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

// followedRun is the run every followImport test starts; fakes answer as it
// unless a test is about a daemon that answers as something else.
var followedRun = control.ImportRun{
	ID: 1, Collection: "photos", Dir: "/srv/source",
	StartedAt: time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC),
}

func statusOf(run control.ImportRun, state string) archive.ImportStatus {
	return archive.ImportStatus{ID: run.ID, Collection: run.Collection, Dir: run.Dir, StartedAt: run.StartedAt, State: state}
}

// slowImportArchive is a control.Archive whose one import runs until cancelled.
type slowImportArchive struct {
	cancelled atomic.Bool
}

func (a *slowImportArchive) ImportStart(string, string) (int64, error) { return 1, nil }
func (a *slowImportArchive) ImportStatus(int64) (archive.ImportStatus, bool) {
	if a.cancelled.Load() {
		status := statusOf(followedRun, "cancelled")
		status.FilesSeen = 5
		return status, true
	}
	return statusOf(followedRun, "running"), true
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

		code := followImport(ctx, control.NewClient(control.SocketPath(root)), followedRun, &out, &errOut)

		if !fake.cancelled.Load() {
			t.Fatal("Ctrl-C never reached the daemon as a cancel")
		}
		if code != 1 || !strings.Contains(errOut.String(), "CANCELLED") {
			t.Fatalf("exit %d stderr %q", code, errOut.String())
		}
	})
}

// cannedImportArchive answers every status request with one fixed answer.
type cannedImportArchive struct {
	status archive.ImportStatus
	found  bool
}

func (a cannedImportArchive) ImportStart(string, string) (int64, error) { return a.status.ID, nil }
func (a cannedImportArchive) ImportStatus(int64) (archive.ImportStatus, bool) {
	return a.status, a.found
}
func (a cannedImportArchive) ImportCancel(int64) error         { return nil }
func (a cannedImportArchive) ScrubStart(int64) error           { return nil }
func (a cannedImportArchive) ScrubStatus() archive.ScrubStatus { return archive.ScrubStatus{} }

func followCanned(t *testing.T, fake cannedImportArchive, run control.ImportRun) (int, string, string) {
	t.Helper()
	root := shortDir(t)
	server, err := control.Start(t.Context(), fake, control.SocketPath(root), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	var out, errOut bytes.Buffer
	code := followImport(t.Context(), control.NewClient(control.SocketPath(root)), run, &out, &errOut)
	return code, out.String(), errOut.String()
}

// The daemon numbers imports from 1 in memory, so after a restart another
// import answers to an id a client is still polling. Each case below is a clean
// "done" that belongs to a different run; reporting any of them as this
// command's result would pass an import that never finished.
func TestFollowImportIdentity(t *testing.T) {
	done := func(change func(*archive.ImportStatus)) archive.ImportStatus {
		status := statusOf(followedRun, "done")
		status.FilesSeen, status.FilesCreated = 9, 9
		if change != nil {
			change(&status)
		}
		return status
	}

	t.Run("should exit 0 when the finished run is the one that was started", func(t *testing.T) {
		code, out, errOut := followCanned(t, cannedImportArchive{status: done(nil), found: true}, followedRun)
		if code != 0 || !strings.Contains(out, "import 1 done") {
			t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
		}
	})

	strangers := map[string]func(*archive.ImportStatus){
		"started at a different time":         func(s *archive.ImportStatus) { s.StartedAt = s.StartedAt.Add(time.Second) },
		"imports a different directory":       func(s *archive.ImportStatus) { s.Dir = "/srv/other" },
		"imports into a different collection": func(s *archive.ImportStatus) { s.Collection = "other" },
		"reports no start time at all":        func(s *archive.ImportStatus) { s.StartedAt = time.Time{} },
	}
	for name, change := range strangers {
		t.Run("should exit 1 and call the outcome unknown when the run under the same id "+name, func(t *testing.T) {
			code, out, errOut := followCanned(t, cannedImportArchive{status: done(change), found: true}, followedRun)
			if code != 1 || !strings.Contains(errOut, "UNKNOWN") {
				t.Fatalf("exit %d stderr %q", code, errOut)
			}
			if strings.Contains(out, "done") {
				t.Fatalf("another run's result was printed as this one's: %q", out)
			}
		})
	}

	t.Run("should exit 1 when the daemon that started the import answered with an id and nothing else", func(t *testing.T) {
		code, _, errOut := followCanned(t, cannedImportArchive{status: done(nil), found: true}, control.ImportRun{ID: 1})
		if code != 1 || !strings.Contains(errOut, "UNKNOWN") {
			t.Fatalf("exit %d stderr %q", code, errOut)
		}
	})

	t.Run("should exit 1 and say the import did not finish when the daemon no longer knows the id", func(t *testing.T) {
		code, _, errOut := followCanned(t, cannedImportArchive{found: false}, followedRun)
		if code != 1 || !strings.Contains(errOut, "did NOT finish") {
			t.Fatalf("exit %d stderr %q", code, errOut)
		}
	})
}
