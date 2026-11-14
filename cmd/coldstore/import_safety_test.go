package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/archive"
	"github.com/tjmisko/coldstore/internal/control"
)

func TestRejectPositionalArguments(t *testing.T) {
	t.Run("should accept no arguments when the daemon is started with flags only", func(t *testing.T) {
		if err := rejectPositionalArguments(nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("should name the argument and the correct form when a subcommand follows the flags", func(t *testing.T) {
		err := rejectPositionalArguments([]string{"import", "/photos", "--collection", "c"})
		if err == nil {
			t.Fatal("accepted a leftover argument")
		}
		for _, want := range []string{`"import"`, "subcommand must come first", "coldstore import <dir>"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("message %q lacks %q", err, want)
			}
		}
	})
}

func TestDaemonStartupSafety(t *testing.T) {
	t.Run("should exit 1 and open nothing when a subcommand is typed after --config", func(t *testing.T) {
		root := shortDir(t)
		config := filepath.Join(t.TempDir(), "coldstore.toml")
		body := "[storage]\ncold_storage_path = \"" + root + "\"\n[[collections]]\nname = \"photos\"\n"
		if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonHelperProcess$", "--",
			"--config", config, "import", t.TempDir(), "--collection", "photos")
		cmd.Env = append(os.Environ(), daemonHelperEnv+"=1")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		err := cmd.Run()

		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("exit %v, stderr %q", err, stderr.String())
		}
		if !strings.Contains(stderr.String(), `unexpected argument "import"`) {
			t.Fatalf("stderr %q", stderr.String())
		}
		entries, _ := os.ReadDir(root)
		if len(entries) != 0 {
			t.Fatalf("the refused invocation created %d entries in cold storage", len(entries))
		}
	})

	t.Run("should stop on the archive lock before touching the legacy catalog when another process holds it", func(t *testing.T) {
		root := shortDir(t)
		archiveFixture{root: root}.open(t)
		cmd, _ := startDaemon(t, root, []string{"photos"})

		err := cmd.Wait()

		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("exit %v, want 1", err)
		}
		if _, err := os.Stat(filepath.Join(root, "catalog.db")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the second process opened the legacy catalog before taking the lock: %v", err)
		}
	})
}

func TestSummariseImport(t *testing.T) {
	t.Run("should exit 0 and print every count when the import is clean", func(t *testing.T) {
		var out, errOut bytes.Buffer
		code := summariseImport(archive.ImportStatus{
			ID: 3, State: "done", FilesSeen: 9, FilesCreated: 4, FilesUnchanged: 3, FilesOther: 1, SkippedCount: 1,
		}, &out, &errOut)
		if code != 0 {
			t.Fatalf("exit %d stderr %q", code, errOut.String())
		}
		for _, want := range []string{"created 4", "unchanged 3", "other 1", "skipped (benign) 1", "failed 0"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("summary %q lacks %q", out.String(), want)
			}
		}
	})

	t.Run("should exit 1 and say plainly the import is incomplete when a path was unreadable", func(t *testing.T) {
		var out, errOut bytes.Buffer
		code := summariseImport(archive.ImportStatus{
			State: "done", FailedCount: 1, UnreadableCount: 1,
			Failed:     []archive.ImportFailure{{Path: "/src/locked", Error: "permission denied"}},
			ReportPath: "/cold/import-reports/r-1.jsonl",
		}, &out, &errOut)
		if code != 1 {
			t.Fatalf("exit %d", code)
		}
		for _, want := range []string{"/src/locked", "INCOMPLETE", "could not be read", "/cold/import-reports/r-1.jsonl"} {
			if !strings.Contains(errOut.String(), want) {
				t.Errorf("stderr %q lacks %q", errOut.String(), want)
			}
		}
	})

	t.Run("should exit 1 when the run was cancelled even though nothing failed", func(t *testing.T) {
		var out, errOut bytes.Buffer
		code := summariseImport(archive.ImportStatus{State: "cancelled", FilesSeen: 2}, &out, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "CANCELLED") || !strings.Contains(errOut.String(), "run it again to resume") {
			t.Fatalf("exit %d stderr %q", code, errOut.String())
		}
	})
}

// slowImportArchive is a control.Archive whose one import runs until cancelled.
type slowImportArchive struct {
	cancelled atomic.Bool
}

func (a *slowImportArchive) ImportStart(string, string) (int64, error) { return 1, nil }
func (a *slowImportArchive) ImportStatus(id int64) (archive.ImportStatus, bool) {
	if a.cancelled.Load() {
		return archive.ImportStatus{ID: id, State: "cancelled", FilesSeen: 5}, true
	}
	return archive.ImportStatus{ID: id, State: "running"}, true
}
func (a *slowImportArchive) ImportCancel(int64) error         { a.cancelled.Store(true); return nil }
func (a *slowImportArchive) ScrubStart(int64) error           { return nil }
func (a *slowImportArchive) ScrubStatus() archive.ScrubStatus { return archive.ScrubStatus{} }

func TestFollowImportInterrupt(t *testing.T) {
	t.Run("should request a cancel and report the run as cancelled when Ctrl-C arrives mid-import", func(t *testing.T) {
		root := shortDir(t)
		fake := &slowImportArchive{}
		server, err := control.Start(t.Context(), fake, control.SocketPath(root), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		previous := importPollInterval
		importPollInterval = 5 * time.Millisecond
		t.Cleanup(func() { importPollInterval = previous })
		ctx, interrupt := context.WithCancel(context.Background())
		time.AfterFunc(30*time.Millisecond, interrupt)
		var out, errOut bytes.Buffer

		code := followImport(ctx, control.NewClient(control.SocketPath(root)), 1, &out, &errOut)

		if !fake.cancelled.Load() {
			t.Fatal("Ctrl-C never reached the daemon as a cancel")
		}
		if code != 1 || !strings.Contains(errOut.String(), "CANCELLED") {
			t.Fatalf("exit %d stderr %q", code, errOut.String())
		}
	})
}
