package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/daemon"
)

// Deps is what the API server needs to do its job. It is a struct rather than
// a positional parameter list because that list has already broken the e2e
// helper once and this work keeps adding to it (design section 6.14).
type Deps struct {
	Cfg   *config.Config
	Cat   *catalog.Catalog
	Mover *daemon.Mover
	// Worker is needed only for the enqueue poke. It must be the SAME worker
	// the daemon is running, or POST /jobs wakes a goroutine nobody started and
	// every job waits for the running worker's 2 s ticker instead.
	Worker  *daemon.Worker
	Scanner *daemon.Scanner
	Logger  *slog.Logger
}

type Server struct {
	cfg *config.Config
	cat *catalog.Catalog
	// mover is the process-wide serialisation point. The server must be
	// handed the SAME one the daemon uses, or a pin arriving over HTTP is no
	// longer serialised against the quota tick's move.
	mover  *daemon.Mover
	worker *daemon.Worker
	// scanner is held for design section 6.14's Deps shape. Nothing in the API
	// calls it any more: POST /api/v1/scan enqueues a job and the worker runs
	// the walk, which is what removed the unguarded inline scan from the
	// request path.
	scanner *daemon.Scanner
	logger  *slog.Logger
	mux     *http.ServeMux
}

func NewServer(d Deps) *Server {
	s := &Server{
		cfg:     d.Cfg,
		cat:     d.Cat,
		mover:   d.Mover,
		worker:  d.Worker,
		scanner: d.Scanner,
		logger:  d.Logger,
		mux:     http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) Handler() http.Handler {
	next := s.auditMutations(s.mux)
	switch s.cfg.Server.AuthenticationMode() {
	case config.AuthModeBasic:
		if s.cfg.Server.Username == "" || s.cfg.Server.Password == "" ||
			len(s.cfg.Server.TailscaleAllowedUsers) != 0 {
			return invalidAuthConfiguration()
		}
		return s.basicAuth(next)
	case config.AuthModeTailscale:
		loopback, err := config.IsLoopbackListenAddr(s.cfg.Server.ListenAddr)
		if err != nil || !loopback || len(s.cfg.Server.TailscaleAllowedUsers) == 0 ||
			s.cfg.Server.Username != "" || s.cfg.Server.Password != "" {
			return invalidAuthConfiguration()
		}
		return s.tailscaleAuth(s.requireSameOriginMutations(next))
	case config.AuthModeNone:
		if s.cfg.Server.AuthMode != "" || s.cfg.Server.Username != "" ||
			s.cfg.Server.Password != "" || len(s.cfg.Server.TailscaleAllowedUsers) != 0 {
			return invalidAuthConfiguration()
		}
		// Reaching the unauthenticated shape with a non-loopback listen_addr is
		// impossible in a daemon started through main: config validation refuses
		// it at load.
		return withRequestIdentity(next, requestIdentity{
			Provider: config.AuthModeNone,
			Login:    "loopback",
		})
	default:
		return invalidAuthConfiguration()
	}
}

func invalidAuthConfiguration() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusServiceUnavailable, "server authentication configuration is invalid")
	})
}

// basicAuth guards the whole mux, the SPA and the thumbnail tree included.
//
// Both fields are compared in constant time, and both comparisons always run.
// Credentials are hashed first so the comparison is over two fixed-length
// digests: subtle.ConstantTimeCompare returns 0 immediately for inputs of
// differing length, which would otherwise leak the password length. A request
// with no Authorization header at all takes the same path as a wrong password --
// it just compares the empty string -- so "no header" and "bad password" are not
// distinguishable by timing or by response.
func (s *Server) basicAuth(next http.Handler) http.Handler {
	wantUser := sha256.Sum256([]byte(s.cfg.Server.Username))
	wantPass := sha256.Sum256([]byte(s.cfg.Server.Password))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, parsed := r.BasicAuth()
		gotUser := sha256.Sum256([]byte(user))
		gotPass := sha256.Sum256([]byte(pass))

		userOK := subtle.ConstantTimeCompare(gotUser[:], wantUser[:])
		passOK := subtle.ConstantTimeCompare(gotPass[:], wantPass[:])
		parsedOK := 0
		if parsed {
			parsedOK = 1
		}

		if parsedOK&userOK&passOK != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="coldstore"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		withRequestIdentity(next, requestIdentity{
			Provider: config.AuthModeBasic,
			Login:    user,
		}).ServeHTTP(w, r)
	})
}

func (s *Server) ListenAndServe(addr string) error {
	authMode := s.cfg.Server.AuthenticationMode()
	if authMode == config.AuthModeNone {
		// config.ValidateServerBinding has already established that this is a
		// loopback bind, so the exposure is limited to this machine.
		s.logger.Warn("API server starting without authentication; reachable only from this host",
			"addr", addr)
	}
	s.logger.Info("API server starting", "addr", addr, "auth_mode", authMode)

	srv := &http.Server{
		Addr:    addr,
		Handler: s.Handler(),
		// A LAN daemon should not be held open by a client that opens a socket
		// and never finishes its request headers. There is deliberately no
		// WriteTimeout: file downloads and the initial thumbnail pass can
		// legitimately take longer than any value that would be safe to pick.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	return srv.ListenAndServe()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
