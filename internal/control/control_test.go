package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/archive"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// shortDir is a directory with a short path: unix socket paths are limited to
// about 100 bytes and t.TempDir includes the test name.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func openArchive(t *testing.T, root string) *archive.Service {
	t.Helper()
	service, err := archive.Open(t.Context(), archive.Options{
		Root: root, Collections: []string{"photos"}, Logger: quietLogger(),
		Sleep: func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

func startServer(t *testing.T, svc Archive, root string) *Client {
	t.Helper()
	server, err := Start(t.Context(), svc, SocketPath(root), quietLogger())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return NewClient(SocketPath(root))
}

// fakeArchive answers every call with a canned error.
type fakeArchive struct {
	importErr, scrubErr, cancelErr error
}

func (f fakeArchive) ImportStart(string, string) (int64, error) { return 0, f.importErr }
func (f fakeArchive) ImportStatus(int64) (archive.ImportStatus, bool) {
	return archive.ImportStatus{}, false
}
func (f fakeArchive) ImportCancel(int64) error         { return f.cancelErr }
func (f fakeArchive) ScrubStart(int64) error           { return f.scrubErr }
func (f fakeArchive) ScrubStatus() archive.ScrubStatus { return archive.ScrubStatus{} }

func statusCode(t *testing.T, err error) int {
	t.Helper()
	var status *StatusError
	if !errors.As(err, &status) {
		t.Fatalf("error %v is not a StatusError", err)
	}
	return status.Code
}

func TestControl(t *testing.T) {
	t.Run("should start an import and report its progress over the socket", func(t *testing.T) {
		root := shortDir(t)
		client := startServer(t, openArchive(t, root), root)
		source := shortDir(t)
		for name, body := range map[string]string{"a.txt": "alpha", "b.txt": "bravo"} {
			if err := os.WriteFile(filepath.Join(source, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		run, err := client.StartImportRun(t.Context(), "photos", source)
		if err != nil {
			t.Fatalf("StartImportRun: %v", err)
		}
		id := run.ID
		if run.Collection != "photos" || run.Dir == "" || run.StartedAt.IsZero() {
			t.Fatalf("the start answer does not identify the run: %+v", run)
		}
		if first, err := client.ImportStatus(t.Context(), id); err != nil || !run.Matches(first) {
			t.Fatalf("the started run does not match its own status: run %+v status %+v err %v", run, first, err)
		}
		var status archive.ImportStatus
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if status, err = client.ImportStatus(t.Context(), id); err != nil {
				t.Fatalf("ImportStatus: %v", err)
			}
			if status.State != "running" {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}

		if status.State != "done" || status.FilesSeen != 2 || status.FilesCreated != 2 || status.FailedCount != 0 ||
			status.Collection != "photos" || status.ID != id {
			t.Fatalf("status %+v", status)
		}
	})

	t.Run("should answer 409 when an import is already running", func(t *testing.T) {
		root := shortDir(t)
		client := startServer(t, fakeArchive{importErr: archive.ErrImportRunning}, root)
		_, err := client.StartImport(t.Context(), "photos", "/somewhere")
		if code := statusCode(t, err); code != 409 {
			t.Fatalf("code %d, want 409", code)
		}
	})

	t.Run("should answer 400 when dir is inside the archive root", func(t *testing.T) {
		root := shortDir(t)
		client := startServer(t, openArchive(t, root), root)
		inside := filepath.Join(root, "blobs")
		if err := os.MkdirAll(inside, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, dir := range map[string]string{"inside root": inside, "relative": "relative/dir", "missing": filepath.Join(shortDir(t), "nope")} {
			_, err := client.StartImport(t.Context(), "photos", dir)
			if code := statusCode(t, err); code != 400 {
				t.Errorf("%s: code %d, want 400", name, code)
			}
		}
	})

	t.Run("should answer 404 when the collection or the import is unknown", func(t *testing.T) {
		root := shortDir(t)
		client := startServer(t, openArchive(t, root), root)
		if _, err := client.StartImport(t.Context(), "nowhere", shortDir(t)); statusCode(t, err) != 404 {
			t.Fatalf("unknown collection: %v", err)
		}
		if _, err := client.ImportStatus(t.Context(), 999); statusCode(t, err) != 404 {
			t.Fatalf("unknown import: %v", err)
		}
	})

	t.Run("should answer 202, 404 and 409 when a cancel is accepted, unknown or too late", func(t *testing.T) {
		cases := []struct {
			name string
			err  error
			want int
		}{
			{"accepted", nil, 0},
			{"unknown", archive.ErrImportUnknown, 404},
			{"finished", archive.ErrImportNotRunning, 409},
		}
		for _, tc := range cases {
			client := startServer(t, fakeArchive{cancelErr: tc.err}, shortDir(t))
			err := client.CancelImport(t.Context(), 7)
			if tc.want == 0 {
				if err != nil {
					t.Errorf("%s: err = %v", tc.name, err)
				}
				continue
			}
			if got := statusCode(t, err); got != tc.want {
				t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
			}
		}
	})

	t.Run("should create the socket with mode 0600", func(t *testing.T) {
		root := shortDir(t)
		startServer(t, fakeArchive{}, root)
		info, err := os.Lstat(SocketPath(root))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 || info.Mode().Type() != os.ModeSocket {
			t.Fatalf("socket mode %v", info.Mode())
		}
	})

	t.Run("should replace a stale socket file when the daemon starts", func(t *testing.T) {
		root := shortDir(t)
		stale, err := net.Listen("unix", SocketPath(root))
		if err != nil {
			t.Fatal(err)
		}
		stale.(*net.UnixListener).SetUnlinkOnClose(false)
		stale.Close()

		client := startServer(t, fakeArchive{}, root)

		if _, err := client.ScrubStatus(t.Context()); err != nil {
			t.Fatalf("the new server does not answer: %v", err)
		}
	})

	t.Run("should refuse to remove a path that is not a socket", func(t *testing.T) {
		root := shortDir(t)
		if err := os.WriteFile(SocketPath(root), []byte("precious"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Start(t.Context(), fakeArchive{}, SocketPath(root), quietLogger()); err == nil {
			t.Fatal("Start replaced a regular file")
		}
		if body, _ := os.ReadFile(SocketPath(root)); string(body) != "precious" {
			t.Fatal("the file was damaged")
		}
	})

	t.Run("should remove the socket when the context ends", func(t *testing.T) {
		root := shortDir(t)
		ctx, cancel := context.WithCancel(t.Context())
		server, err := Start(ctx, fakeArchive{}, SocketPath(root), quietLogger())
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Lstat(SocketPath(root)); errors.Is(err, os.ErrNotExist) {
				_ = server.Close()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("the socket file outlived the context")
	})

	t.Run("should report ErrDaemonNotRunning when nothing listens on the socket", func(t *testing.T) {
		client := NewClient(SocketPath(shortDir(t)))
		_, err := client.StartImport(t.Context(), "photos", "/x")
		if !errors.Is(err, ErrDaemonNotRunning) {
			t.Fatalf("error %v", err)
		}
	})

	t.Run("should start a scrub and answer 409 when one is already running", func(t *testing.T) {
		root := shortDir(t)
		client := startServer(t, fakeArchive{scrubErr: archive.ErrScrubRunning}, root)
		if code := statusCode(t, client.StartScrub(t.Context(), 20)); code != 409 {
			t.Fatalf("code %d, want 409", code)
		}

		root = shortDir(t)
		service := openArchive(t, root)
		client = startServer(t, service, root)
		if err := client.StartScrub(t.Context(), 0); err != nil {
			t.Fatalf("StartScrub: %v", err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			status, err := client.ScrubStatus(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !status.Running && status.Last != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("the scrub never reported a result")
	})
}
