// Package control is the unix-socket channel between the coldstore CLI and the
// running daemon. "Read this server directory" must never be reachable through
// the Tailscale-served HTTP listener, and the CLI cannot satisfy the Tailscale
// identity header, so import and online scrub travel over
// <cold_storage_path>/control.sock, mode 0600, instead.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/tjmisko/coldstore/internal/archive"
)

// SocketName is the control socket's file name under the archive root.
const SocketName = "control.sock"

const (
	socketMode      = 0o600
	maxRequestBytes = 64 << 10
	bytesPerMiB     = 1 << 20
)

// SocketPath is where the daemon listens for a given archive root.
func SocketPath(root string) string { return filepath.Join(root, SocketName) }

// Archive is the part of archive.Service the control routes use.
type Archive interface {
	ImportStart(collection, dir string) (int64, error)
	ImportStatus(id int64) (archive.ImportStatus, bool)
	ImportCancel(id int64) error
	ScrubStart(rateBytesPerSec int64) error
	ScrubStatus() archive.ScrubStatus
}

// Server is the running control listener.
type Server struct {
	path     string
	listener net.Listener
	http     *http.Server
	done     chan struct{}
	logger   *slog.Logger
	closeMu  sync.Once
}

// Start removes a stale socket file, listens on path with mode 0600 and serves
// until ctx ends or Close is called. The socket file is removed on shutdown.
func Start(ctx context.Context, svc Archive, path string, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	// The window between bind and chmod is covered by the archive root, which
	// the deployment guide makes owner-only; a process-wide umask change would
	// race every other file the daemon creates.
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on control socket %s: %w", path, err)
	}
	if err := os.Chmod(path, socketMode); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("setting control socket mode: %w", err)
	}
	s := &Server{
		path: path, listener: listener, done: make(chan struct{}), logger: logger,
		http: &http.Server{Handler: newHandler(svc), ReadHeaderTimeout: 10 * time.Second},
	}
	go func() {
		defer close(s.done)
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("control server stopped", "error", err)
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.done:
		}
	}()
	return s, nil
}

// removeStaleSocket deletes a leftover socket file. It refuses to delete
// anything that is not a socket.
func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking control socket %s: %w", path, err)
	}
	if info.Mode().Type() != os.ModeSocket {
		return fmt.Errorf("control socket path %s exists and is not a socket", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing stale control socket %s: %w", path, err)
	}
	return nil
}

// Close stops serving, removes the socket and waits for the serve loop.
func (s *Server) Close() error {
	var err error
	s.closeMu.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = s.http.Shutdown(ctx)
		if err != nil {
			err = errors.Join(err, s.http.Close())
		}
		if removeErr := os.Remove(s.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	})
	<-s.done
	return err
}

type importRequest struct {
	Collection string `json:"collection"`
	Dir        string `json:"dir"`
}

type scrubRequest struct {
	RateMiBPerSec int64 `json:"rate_mib_per_sec"`
}

func newHandler(svc Archive) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /control/v1/imports", func(w http.ResponseWriter, r *http.Request) { startImport(svc, w, r) })
	mux.HandleFunc("GET /control/v1/imports/{id}", func(w http.ResponseWriter, r *http.Request) { importStatus(svc, w, r) })
	mux.HandleFunc("POST /control/v1/imports/{id}/cancel", func(w http.ResponseWriter, r *http.Request) { cancelImport(svc, w, r) })
	mux.HandleFunc("POST /control/v1/scrub", func(w http.ResponseWriter, r *http.Request) { startScrub(svc, w, r) })
	mux.HandleFunc("GET /control/v1/scrub", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, svc.ScrubStatus())
	})
	return mux
}

func startImport(svc Archive, w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	id, err := svc.ImportStart(req.Collection, req.Dir)
	switch {
	case errors.Is(err, archive.ErrCollectionUnknown):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, archive.ErrImportDir):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, archive.ErrImportRunning):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, archive.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, map[string]int64{"id": id})
	}
}

func importStatus(svc Archive, w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such import")
		return
	}
	status, found := svc.ImportStatus(id)
	if !found {
		writeError(w, http.StatusNotFound, "no such import")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func cancelImport(svc Archive, w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such import")
		return
	}
	err = svc.ImportCancel(id)
	switch {
	case errors.Is(err, archive.ErrImportUnknown):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, archive.ErrImportNotRunning):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, map[string]bool{"cancelling": true})
	}
}

func startScrub(svc Archive, w http.ResponseWriter, r *http.Request) {
	var req scrubRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RateMiBPerSec < 0 {
		writeError(w, http.StatusBadRequest, "rate_mib_per_sec must not be negative")
		return
	}
	err := svc.ScrubStart(req.RateMiBPerSec * bytesPerMiB)
	switch {
	case errors.Is(err, archive.ErrScrubRunning):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, archive.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// ErrDaemonNotRunning means nothing is listening on the control socket.
var ErrDaemonNotRunning = errors.New("the coldstore daemon is not running")

// StatusError is a non-2xx answer from the daemon.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("daemon answered %d: %s", e.Code, e.Message)
}

// Client talks to a daemon's control socket.
type Client struct {
	http *http.Client
}

// NewClient returns a client for the socket at path.
func NewClient(path string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", path)
		},
		DisableKeepAlives: true,
	}
	return &Client{http: &http.Client{Transport: transport}}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://coldstore"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("%w: %v", ErrDaemonNotRunning, err)
		}
		return fmt.Errorf("talking to the daemon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, maxRequestBytes)).Decode(&failure)
		return &StatusError{Code: resp.StatusCode, Message: failure.Error}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("reading the daemon's answer: %w", err)
	}
	return nil
}

// StartImport asks the daemon to import dir into collection and returns the run id.
func (c *Client) StartImport(ctx context.Context, collection, dir string) (int64, error) {
	var answer struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, "/control/v1/imports", importRequest{Collection: collection, Dir: dir}, &answer)
	return answer.ID, err
}

// ImportStatus reads the progress of an import run.
func (c *Client) ImportStatus(ctx context.Context, id int64) (archive.ImportStatus, error) {
	var status archive.ImportStatus
	err := c.do(ctx, http.MethodGet, "/control/v1/imports/"+strconv.FormatInt(id, 10), nil, &status)
	return status, err
}

// CancelImport asks the daemon to stop a running import. A 409 StatusError
// means it had already finished; a 404 that the daemon does not know the id.
func (c *Client) CancelImport(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPost, "/control/v1/imports/"+strconv.FormatInt(id, 10)+"/cancel", nil, nil)
}

// StartScrub asks the daemon to scrub. rateMiBPerSec 0 is unthrottled.
func (c *Client) StartScrub(ctx context.Context, rateMiBPerSec int64) error {
	return c.do(ctx, http.MethodPost, "/control/v1/scrub", scrubRequest{RateMiBPerSec: rateMiBPerSec}, nil)
}

// ScrubStatus reads the scrub progress and the last report.
func (c *Client) ScrubStatus(ctx context.Context) (archive.ScrubStatus, error) {
	var status archive.ScrubStatus
	err := c.do(ctx, http.MethodGet, "/control/v1/scrub", nil, &status)
	return status, err
}
