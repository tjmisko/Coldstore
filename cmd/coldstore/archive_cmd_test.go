package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/archive"
	"github.com/tjmisko/coldstore/internal/control"
	"github.com/tjmisko/coldstore/internal/ingest"
	"github.com/tjmisko/coldstore/internal/store"
)

// shortDir is a directory with a short path: the control socket lives in it and
// unix socket paths are limited to about 100 bytes.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// archiveFixture is a cold storage root, the config that names it and the
// options the daemon would open it with.
type archiveFixture struct {
	root, configPath string
}

func newArchiveFixture(t *testing.T, collections ...string) archiveFixture {
	t.Helper()
	root := shortDir(t)
	var config strings.Builder
	config.WriteString("[storage]\ncold_storage_path = \"" + root + "\"\n")
	for _, name := range collections {
		config.WriteString("[[collections]]\nname = \"" + name + "\"\n")
	}
	configPath := filepath.Join(t.TempDir(), "coldstore.toml")
	if err := os.WriteFile(configPath, []byte(config.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return archiveFixture{root: root, configPath: configPath}
}

func (f archiveFixture) options(collections ...string) archive.Options {
	return archive.Options{Root: f.root, Collections: collections, Sleep: func(time.Duration) {}}
}

func (f archiveFixture) open(t *testing.T) *archive.Service {
	t.Helper()
	service, err := archive.Open(t.Context(), f.options("photos"))
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

// serve opens the archive and listens on its control socket, as the daemon does.
func (f archiveFixture) serve(t *testing.T) *archive.Service {
	t.Helper()
	service := f.open(t)
	server, err := control.Start(t.Context(), service, control.SocketPath(f.root), nil)
	if err != nil {
		t.Fatalf("control.Start: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return service
}

func run(t *testing.T, name string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	previous := importPollInterval
	importPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { importPollInterval = previous })
	var out, errOut bytes.Buffer
	code = runArchiveCommand(t.Context(), name, args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func ingestText(t *testing.T, service *archive.Service, path, body string) {
	t.Helper()
	_, err := service.Ingest(context.Background(), "test", ingest.Arrival{
		Folder: "photos", Path: path, Mtime: time.Now(),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil },
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
}

func TestArchiveSubcommandDispatch(t *testing.T) {
	t.Run("should take the legacy flag path when the first argument is --check", func(t *testing.T) {
		for _, args := range [][]string{
			{"--check"}, {"--config", "x.toml"}, {"--scan"}, {"--status"}, {"--rename-folder-label", "A=B"},
			{}, {"importer"}, {"-config", "import"},
		} {
			if name, ok := archiveSubcommand(args); ok {
				t.Errorf("%v dispatched to the archive subcommand %q", args, name)
			}
		}
	})

	t.Run("should dispatch the four archive subcommands when they are the first argument", func(t *testing.T) {
		for _, name := range []string{"import", "scrub", "rebuild-index", "format-version"} {
			got, ok := archiveSubcommand([]string{name, "--config", "x"})
			if !ok || got != name {
				t.Errorf("%q dispatched to %q, %v", name, got, ok)
			}
		}
	})
}

func TestImportCommand(t *testing.T) {
	t.Run("should exit 1 with a clear message when import is run with no daemon", func(t *testing.T) {
		fixture := newArchiveFixture(t, "photos")

		code, _, stderr := run(t, "import", t.TempDir(), "--collection", "photos", "--config", fixture.configPath)

		if code != 1 || !strings.Contains(stderr, "coldstore import needs the running daemon; it is the only journal writer") {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
	})

	t.Run("should exit 0 and print a summary when the import finishes without failures", func(t *testing.T) {
		fixture := newArchiveFixture(t, "photos")
		fixture.serve(t)
		source := t.TempDir()
		for name, body := range map[string]string{"a.txt": "alpha", "sub/b.txt": "bravo"} {
			full := filepath.Join(source, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		code, stdout, stderr := run(t, "import", "--collection", "photos", "--config", fixture.configPath, source)

		if code != 0 {
			t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "done") || !strings.Contains(stdout, "created 2") {
			t.Fatalf("summary %q", stdout)
		}
	})

	t.Run("should exit 1 when the daemon refuses the directory", func(t *testing.T) {
		fixture := newArchiveFixture(t, "photos")
		fixture.serve(t)
		code, _, stderr := run(t, "import", filepath.Join(fixture.root, "blobs"), "--collection", "photos", "--config", fixture.configPath)
		if code != 1 || !strings.Contains(stderr, "400") {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
	})

	t.Run("should exit 1 when the collection is not declared or an argument is missing", func(t *testing.T) {
		fixture := newArchiveFixture(t, "photos")
		for name, args := range map[string][]string{
			"undeclared":         {"x", "--collection", "books", "--config", fixture.configPath},
			"no collection flag": {"x", "--config", fixture.configPath},
			"no directory":       {"--collection", "photos", "--config", fixture.configPath},
			"two directories":    {"x", "y", "--collection", "photos", "--config", fixture.configPath},
			"missing config":     {"x", "--collection", "photos", "--config", filepath.Join(t.TempDir(), "absent.toml")},
		} {
			if code, _, _ := run(t, "import", args...); code != 1 {
				t.Errorf("%s: exit %d, want 1", name, code)
			}
		}
	})
}

func TestRebuildIndexCommand(t *testing.T) {
	t.Run("should exit 1 when rebuild-index is run while the lock is held", func(t *testing.T) {
		fixture := newArchiveFixture(t, "photos")
		fixture.open(t)

		code, _, stderr := run(t, "rebuild-index", "--config", fixture.configPath)

		if code != 1 || !strings.Contains(stderr, "locked") || !strings.Contains(stderr, "stop the daemon") {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
	})

	t.Run("should print what it rebuilt when no daemon holds the lock", func(t *testing.T) {
		fixture := newArchiveFixture(t, "photos")
		service := fixture.open(t)
		ingestText(t, service, "a.txt", "alpha")
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(fixture.root, "views")); err != nil {
			t.Fatal(err)
		}

		code, stdout, stderr := run(t, "rebuild-index", "--config", fixture.configPath)

		if code != 0 {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
		for _, want := range []string{"records replayed", "entries           1", "views linked      1"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("output lacks %q:\n%s", want, stdout)
			}
		}
	})

	t.Run("should exit 1 and create nothing when no collection is declared and no journal exists", func(t *testing.T) {
		fixture := newArchiveFixture(t)
		folder := t.TempDir()
		withFolder := "[storage]\ncold_storage_path = \"" + fixture.root + "\"\n" +
			"[[folders]]\nlabel = \"Docs\"\npath = \"" + folder + "\"\nbudget = \"1GB\"\n"
		if err := os.WriteFile(fixture.configPath, []byte(withFolder), 0o644); err != nil {
			t.Fatal(err)
		}

		code, _, stderr := run(t, "rebuild-index", "--config", fixture.configPath)

		entries, _ := os.ReadDir(fixture.root)
		if code != 1 || !strings.Contains(stderr, "no archive") || len(entries) != 0 {
			t.Fatalf("exit %d stderr %q created %d entries", code, stderr, len(entries))
		}
	})
}

func TestFormatVersionCommand(t *testing.T) {
	t.Run("should exit 0 and print both versions when the disk is current", func(t *testing.T) {
		fixture := newArchiveFixture(t, "photos")
		fixture.open(t).Close()

		code, stdout, _ := run(t, "format-version", "--config", fixture.configPath)

		if code != 0 || !strings.Contains(stdout, "journal  binary 1  disk 1") || !strings.Contains(stdout, "index    binary 1  disk 1") {
			t.Fatalf("exit %d stdout %q", code, stdout)
		}
	})

	t.Run("should exit 0 when nothing exists on disk yet", func(t *testing.T) {
		fixture := newArchiveFixture(t, "photos")
		code, stdout, _ := run(t, "format-version", "--config", fixture.configPath)
		if code != 0 || !strings.Contains(stdout, "disk 0") {
			t.Fatalf("exit %d stdout %q", code, stdout)
		}
		if entries, _ := os.ReadDir(fixture.root); len(entries) != 0 {
			t.Fatalf("format-version created %d entries", len(entries))
		}
	})

	t.Run("should exit 3 when the disk format is newer than the binary", func(t *testing.T) {
		fixture := newArchiveFixture(t, "photos")
		if err := os.MkdirAll(filepath.Join(fixture.root, "journal"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture.root, "journal", "FORMAT"), []byte("99\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		code, stdout, stderr := run(t, "format-version", "--config", fixture.configPath)

		if code != 3 || !strings.Contains(stdout, "disk 99") || !strings.Contains(stderr, "newer") {
			t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
		}
	})
}

func TestScrubCommand(t *testing.T) {
	cleanArchive := func(t *testing.T) (archiveFixture, string) {
		t.Helper()
		fixture := newArchiveFixture(t, "photos")
		service := fixture.open(t)
		ingestText(t, service, "a.txt", "alpha")
		hashes := blobPaths(t, service)
		if len(hashes) != 1 {
			t.Fatalf("%d blobs", len(hashes))
		}
		blob := hashes[0]
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		return fixture, blob
	}

	t.Run("should exit 0 when offline scrub finds nothing", func(t *testing.T) {
		fixture, _ := cleanArchive(t)
		code, stdout, stderr := run(t, "scrub", "--offline", "--rate-mib", "0", "--config", fixture.configPath)
		if code != 0 || !strings.Contains(stdout, "checked 1 blobs") {
			t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
		}
	})

	t.Run("should exit 2 when offline scrub finds a bad blob", func(t *testing.T) {
		fixture, blob := cleanArchive(t)
		if err := os.Chmod(blob, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(blob, []byte("ALPHA"), 0o644); err != nil {
			t.Fatal(err)
		}

		code, _, stderr := run(t, "scrub", "--offline", "--config", fixture.configPath)

		if code != 2 || !strings.Contains(stderr, "bad") {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
	})

	t.Run("should exit 1 when offline scrub is run while the lock is held", func(t *testing.T) {
		fixture := newArchiveFixture(t, "photos")
		fixture.open(t)
		code, _, stderr := run(t, "scrub", "--offline", "--config", fixture.configPath)
		if code != 1 || !strings.Contains(stderr, "locked") {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
	})

	t.Run("should exit 0 when the daemon's scrub finds nothing", func(t *testing.T) {
		fixture := newArchiveFixture(t, "photos")
		service := fixture.serve(t)
		ingestText(t, service, "a.txt", "alpha")

		code, stdout, stderr := run(t, "scrub", "--rate-mib", "0", "--config", fixture.configPath)

		if code != 0 || !strings.Contains(stdout, "scrub checked 1 blobs") {
			t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
		}
	})

	t.Run("should exit 2 when the daemon's scrub finds a bad blob", func(t *testing.T) {
		fixture := newArchiveFixture(t, "photos")
		service := fixture.serve(t)
		ingestText(t, service, "a.txt", "alpha")
		blob := blobPaths(t, service)[0]
		if err := os.Chmod(blob, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(blob, []byte("ALPHA"), 0o644); err != nil {
			t.Fatal(err)
		}

		code, _, stderr := run(t, "scrub", "--config", fixture.configPath)

		if code != 2 || !strings.Contains(stderr, "bad") {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
	})

	t.Run("should exit 1 when online scrub is run with no daemon", func(t *testing.T) {
		fixture := newArchiveFixture(t, "photos")
		code, _, stderr := run(t, "scrub", "--config", fixture.configPath)
		if code != 1 || !strings.Contains(stderr, "not running") {
			t.Fatalf("exit %d stderr %q", code, stderr)
		}
	})

	t.Run("should exit 1 when the rate is negative", func(t *testing.T) {
		fixture := newArchiveFixture(t, "photos")
		if code, _, _ := run(t, "scrub", "--rate-mib", "-5", "--config", fixture.configPath); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
	})
}

// blobPaths lists the on-disk path of every blob in the service's store.
func blobPaths(t *testing.T, service *archive.Service) []string {
	t.Helper()
	var paths []string
	err := service.Store().Walk(t.Context(), func(h store.Hash, _ int64) error {
		paths = append(paths, service.Store().Path(h))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}
