package archive

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/ingest"
)

// sleepLog records the durations the service asked to sleep, without sleeping.
type sleepLog struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (l *sleepLog) sleep(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.waits = append(l.waits, d)
}

func (l *sleepLog) snapshot() []time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Duration(nil), l.waits...)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testOptions returns options on a fresh root with two declared collections.
func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Root:        filepath.Join(t.TempDir(), "archive"),
		Collections: []string{"photos", "books"},
		Logger:      quietLogger(),
		Sleep:       (&sleepLog{}).sleep,
	}
}

func openService(t *testing.T, opts Options) *Service {
	t.Helper()
	s, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// memArrival is an Arrival whose bytes are in memory.
func memArrival(folder, path, body string) ingest.Arrival {
	return ingest.Arrival{
		Folder: folder, Path: path,
		Mtime: time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC),
		Open:  func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil },
	}
}

func mustIngest(t *testing.T, s *Service, folder, path, body string) ingest.Result {
	t.Helper()
	res, err := s.Ingest(context.Background(), "test", memArrival(folder, path, body))
	if err != nil {
		t.Fatalf("Ingest %s/%s: %v", folder, path, err)
	}
	return res
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func waitImport(t *testing.T, s *Service, id int64) ImportStatus {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		status, ok := s.ImportStatus(id)
		if !ok {
			t.Fatalf("import %d is unknown", id)
		}
		if status.State != "running" {
			return status
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("import %d did not finish", id)
	return ImportStatus{}
}

func runImport(t *testing.T, s *Service, collection, dir string) ImportStatus {
	t.Helper()
	id, err := s.ImportStart(collection, dir)
	if err != nil {
		t.Fatalf("ImportStart: %v", err)
	}
	return waitImport(t, s, id)
}

// snapshotTree describes every file under root: path, mode, mtime, size and content.
func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		line := rel + "|" + info.Mode().String() + "|" + info.ModTime().UTC().Format(time.RFC3339Nano)
		if info.Mode().IsRegular() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			line += "|" + string(body)
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return lines
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
