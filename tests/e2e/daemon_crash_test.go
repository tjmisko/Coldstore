//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/daemon"
)

// recover runs the startup recovery phase the way main does: once, with
// nothing else touching the filesystem.
func recoverStartup(t *testing.T, env *testEnv) {
	t.Helper()
	if err := daemon.RecoverStartupState(env.cfg, env.cat, env.logger); err != nil {
		t.Fatalf("RecoverStartupState: %v", err)
	}
}

func TestRecoveryShouldMarkArchivedWhenTheArchiveCopyIsCompleteAndTheSourceIsGone(t *testing.T) {
	env := newTestEnv(t)

	archiveDir := filepath.Join(env.coldDir, "archive", "TestFolder")
	os.MkdirAll(archiveDir, 0755)
	os.WriteFile(filepath.Join(archiveDir, "crashed.txt"), []byte("archived"), 0644)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "crashed.txt", Filename: "crashed.txt",
		Extension: ".txt", SizeBytes: 8, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := env.cat.GetFile("TestFolder", "crashed.txt")
	env.cat.UpdateFileStatus(file.ID, catalog.StatusArchiving)

	recoverStartup(t, env)

	file, _ = env.cat.GetFile("TestFolder", "crashed.txt")
	if file.Status != catalog.StatusArchived {
		t.Errorf("status = %q, want archived (the archive copy is complete)", file.Status)
	}
}

func TestRecoveryShouldRevertToSyncedWhenNoArchiveCopyWasWritten(t *testing.T) {
	env := newTestEnv(t)

	os.WriteFile(filepath.Join(env.syncDir, "incomplete.txt"), []byte("still here"), 0644)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "incomplete.txt", Filename: "incomplete.txt",
		Extension: ".txt", SizeBytes: 10, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := env.cat.GetFile("TestFolder", "incomplete.txt")
	env.cat.UpdateFileStatus(file.ID, catalog.StatusArchiving)

	recoverStartup(t, env)

	file, _ = env.cat.GetFile("TestFolder", "incomplete.txt")
	if file.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced (nothing was written to cold storage)", file.Status)
	}
}

func TestRecoveryShouldResolveEveryStuckRowWhenSeveralWereInFlight(t *testing.T) {
	env := newTestEnv(t)

	archiveDir := filepath.Join(env.coldDir, "archive", "TestFolder")
	os.MkdirAll(archiveDir, 0755)

	// a.txt: archive copy complete, source gone -- becomes archived.
	os.WriteFile(filepath.Join(archiveDir, "a.txt"), []byte("a"), 0644)
	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "a.txt", Filename: "a.txt",
		Extension: ".txt", SizeBytes: 1, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	fa, _ := env.cat.GetFile("TestFolder", "a.txt")
	env.cat.UpdateFileStatus(fa.ID, catalog.StatusArchiving)

	// b.txt: no archive copy, source present -- reverts to synced.
	os.WriteFile(filepath.Join(env.syncDir, "b.txt"), []byte("b"), 0644)
	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "b.txt", Filename: "b.txt",
		Extension: ".txt", SizeBytes: 1, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	fb, _ := env.cat.GetFile("TestFolder", "b.txt")
	env.cat.UpdateFileStatus(fb.ID, catalog.StatusArchiving)

	// c.txt: a restore that was killed after writing a truncated destination.
	os.WriteFile(filepath.Join(archiveDir, "c.txt"), []byte("cccccccc"), 0644)
	os.WriteFile(filepath.Join(env.syncDir, "c.txt"), []byte("cc"), 0644)
	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "c.txt", Filename: "c.txt",
		Extension: ".txt", SizeBytes: 8, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	fc, _ := env.cat.GetFile("TestFolder", "c.txt")
	env.cat.UpdateFileStatus(fc.ID, catalog.StatusRestoring)

	recoverStartup(t, env)

	fa, _ = env.cat.GetFile("TestFolder", "a.txt")
	if fa.Status != catalog.StatusArchived {
		t.Errorf("a.txt status = %q, want archived", fa.Status)
	}

	fb, _ = env.cat.GetFile("TestFolder", "b.txt")
	if fb.Status != catalog.StatusSynced {
		t.Errorf("b.txt status = %q, want synced", fb.Status)
	}

	fc, _ = env.cat.GetFile("TestFolder", "c.txt")
	if fc.Status != catalog.StatusArchived {
		t.Errorf("c.txt status = %q, want archived (the cold copy is authoritative)", fc.Status)
	}
	if _, err := os.Stat(filepath.Join(env.syncDir, "c.txt")); !os.IsNotExist(err) {
		t.Error("the truncated restore must not be left in the syncthing folder for syncthing to propagate")
	}
	if got, _ := os.ReadFile(filepath.Join(archiveDir, "c.txt")); string(got) != "cccccccc" {
		t.Errorf("the cold copy must survive untouched, got %q", got)
	}
}

func TestRecoveryShouldNotRunFromAScan(t *testing.T) {
	env := newTestEnv(t)

	os.WriteFile(filepath.Join(env.syncDir, "stuck.txt"), []byte("still here"), 0644)
	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "stuck.txt", Filename: "stuck.txt",
		Extension: ".txt", SizeBytes: 10, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := env.cat.GetFile("TestFolder", "stuck.txt")
	env.cat.UpdateFileStatus(file.ID, catalog.StatusArchiving)

	// A scan runs on a ticker alongside in-flight moves. If it recovered
	// transient rows it would race the mover and could delete the only copy
	// of a file, so it must leave them exactly as they are.
	if err := env.scanner.ScanAll(); err != nil {
		t.Fatal(err)
	}

	file, _ = env.cat.GetFile("TestFolder", "stuck.txt")
	if file.Status != catalog.StatusArchiving {
		t.Errorf("status = %q, want archiving (a scan must not recover transient rows)", file.Status)
	}
}
