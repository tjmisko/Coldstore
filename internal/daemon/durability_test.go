package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

// recordSyncCalls swaps the fsync hooks for recorders and returns a function
// that reports the ordered log of durability-relevant operations. The hooks are
// restored when the test ends.
func recordSyncCalls(t *testing.T) func() []string {
	t.Helper()

	origFile, origDir := syncFile, syncDirPath
	t.Cleanup(func() { syncFile, syncDirPath = origFile, origDir })

	var mu sync.Mutex
	var calls []string

	syncFile = func(f *os.File) error {
		mu.Lock()
		calls = append(calls, "sync-file:"+filepath.Base(f.Name()))
		mu.Unlock()
		return origFile(f)
	}
	syncDirPath = func(path string) error {
		mu.Lock()
		calls = append(calls, "sync-dir:"+filepath.Base(path))
		mu.Unlock()
		return origDir(path)
	}

	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func indexOfPrefix(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func TestCrossDeviceMoveShouldFsyncDestinationAndItsDirectoryBeforeRemovingSource(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "sync")
	dstDir := filepath.Join(dir, "cold")
	os.MkdirAll(srcDir, 0o755)
	os.MkdirAll(dstDir, 0o755)

	src := filepath.Join(srcDir, "photo.jpg")
	dst := filepath.Join(dstDir, "photo.jpg")
	os.WriteFile(src, []byte("durable content"), 0o644)

	calls := recordSyncCalls(t)

	// The source must still exist while the destination is being made durable;
	// observe that by checking, from inside the directory fsync hook, that the
	// source has not been unlinked yet.
	origDir := syncDirPath
	sourceStillPresentAtDirSync := false
	syncDirPath = func(path string) error {
		if path == dstDir {
			_, err := os.Stat(src)
			sourceStillPresentAtDirSync = err == nil
		}
		return origDir(path)
	}

	if err := crossDeviceMove(src, dst, testLogger()); err != nil {
		t.Fatal(err)
	}

	got := calls()
	fileIdx := indexOfPrefix(got, "sync-file:")
	dirIdx := indexOfPrefix(got, "sync-dir:")

	if fileIdx < 0 {
		t.Fatalf("destination file was never fsynced; calls = %v", got)
	}
	if dirIdx < 0 {
		t.Fatalf("destination directory was never fsynced; calls = %v", got)
	}
	if fileIdx > dirIdx {
		t.Errorf("file fsync must precede directory fsync; calls = %v", got)
	}
	if !sourceStillPresentAtDirSync {
		t.Error("source was removed before the destination directory was fsynced")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source should be removed once the destination is durable")
	}
}

func TestCrossDeviceMoveShouldKeepSourceWhenDestinationFsyncFails(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	content := []byte("the only copy of this file")
	os.WriteFile(src, content, 0o644)

	orig := syncFile
	t.Cleanup(func() { syncFile = orig })
	syncFile = func(f *os.File) error { return errors.New("simulated disk failure") }

	err := crossDeviceMove(src, dst, testLogger())
	if err == nil {
		t.Fatal("move should fail when the destination cannot be made durable")
	}

	got, readErr := os.ReadFile(src)
	if readErr != nil {
		t.Fatalf("source must survive a failed fsync, but it is gone: %v", readErr)
	}
	if string(got) != string(content) {
		t.Error("source content changed")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("the unverified destination copy should be removed")
	}
}

func TestArchiveShouldLeaveTheFileSyncedWhenTheMoveFails(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)

	path := filepath.Join(syncDir, "photo.jpg")
	content := make([]byte, 2<<10)
	os.WriteFile(path, content, 0o644)

	cat.UpsertFile(&catalog.FileRecord{
		FolderLabel:  "TestFolder",
		RelativePath: "photo.jpg",
		Filename:     "photo.jpg",
		Extension:    ".jpg",
		SizeBytes:    int64(len(content)),
		FileMtime:    time.Now().UTC(),
		Status:       catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := cat.GetFile("TestFolder", "photo.jpg")
	declarePolicy(t, cfg, cat)

	mover := NewMover(cfg, cat, testLogger())
	mover.SetSpaceCheck(plentyOfSpace)
	mover.SetMoveFuncs(func(src, dst string) error { return errors.New("simulated move failure") }, nil)

	if _, err := mover.ArchiveByID(file.ID); err == nil {
		t.Fatal("a failed move must return an error")
	}

	after, err := cat.GetFile("TestFolder", "photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced (a failed archive must not strand a file in 'archiving')",
			after.Status)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("source file must still be on disk after a failed archive: %v", err)
	}
}

func TestRestoreShouldLeaveTheFileArchivedWhenTheMoveFails(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)

	// Put the file straight into the archive, as an offload would have.
	archived := filepath.Join(coldDir, "archive", "TestFolder", "photo.jpg")
	os.MkdirAll(filepath.Dir(archived), 0o755)
	os.WriteFile(archived, make([]byte, 1024), 0o644)

	cat.UpsertFile(&catalog.FileRecord{
		FolderLabel:  "TestFolder",
		RelativePath: "photo.jpg",
		Filename:     "photo.jpg",
		Extension:    ".jpg",
		SizeBytes:    1024,
		FileMtime:    time.Now().UTC(),
		Status:       catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := cat.GetFile("TestFolder", "photo.jpg")
	cat.UpdateFileStatus(file.ID, catalog.StatusArchived)
	declarePolicy(t, cfg, cat)

	mover := NewMover(cfg, cat, testLogger())
	mover.SetMoveFuncs(nil, func(src, dst, archiveRoot string) error {
		return errors.New("simulated move failure")
	})

	if _, err := mover.RestoreByID(file.ID); err == nil {
		t.Fatal("a failed move must return an error")
	}

	after, err := cat.GetFile("TestFolder", "photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != catalog.StatusArchived {
		t.Errorf("status = %q, want archived (a failed restore must not strand a file in 'restoring')",
			after.Status)
	}
	if _, err := os.Stat(archived); err != nil {
		t.Errorf("the archive copy must survive a failed restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(syncDir, "photo.jpg")); !os.IsNotExist(err) {
		t.Error("a partial restored file should not be left in the syncthing folder")
	}
}

func TestEnforceQuotasShouldStopOffloadingWhenContextIsCancelled(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)

	// Small but positive: a non-positive budget means "unset" and is skipped
	// outright, which would make this test pass without exercising anything.
	cfg.Folders[0].BudgetBytes = 1
	cfg.Daemon.GracePeriodMinutes = 0

	old := time.Now().Add(-24 * time.Hour)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		p := filepath.Join(syncDir, name)
		os.WriteFile(p, make([]byte, 1000), 0o644)
		os.Chtimes(p, old, old)
		cat.UpsertFile(&catalog.FileRecord{
			FolderLabel:  "TestFolder",
			RelativePath: name,
			Filename:     name,
			Extension:    ".txt",
			SizeBytes:    1000,
			FileMtime:    old,
			Status:       catalog.StatusSynced,
		}, time.Now().UnixNano())
	}

	declarePolicy(t, cfg, cat)

	d := newTestDaemon(t, cfg, cat, testLogger())
	d.SetSpaceCheck(func(string) (float64, error) { return 90.0, nil })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.EnforceQuotasContext(ctx)

	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		f, err := cat.GetFile("TestFolder", name)
		if err != nil {
			t.Fatal(err)
		}
		if f.Status != catalog.StatusSynced {
			t.Errorf("%s status = %q, want synced (enforcement must not run under a cancelled context)",
				name, f.Status)
		}
	}
}

func TestRunContextShouldReturnPromptlyWhenContextIsCancelled(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	os.WriteFile(filepath.Join(syncDir, "a.txt"), make([]byte, 10), 0o644)

	// Tickers far enough out that the loop can only exit via the context.
	cfg.Daemon.ScanIntervalMinutes = 60
	cfg.Daemon.CatalogSyncIntervalMinutes = 60
	declarePolicy(t, cfg, cat)

	d := newTestDaemon(t, cfg, cat, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.RunContext(ctx) }()

	// Let the initial scan get going, then ask for shutdown.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunContext returned %v, want nil on clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunContext did not return within 5s of cancellation")
	}
}

func TestEnforcementShouldCountAMoveInFlightWhenMeasuringTheFolderAgainstItsBudget(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)

	cfg.Folders[0].BudgetBytes = 5000
	cfg.Daemon.GracePeriodMinutes = 0

	old := time.Now().Add(-24 * time.Hour)
	for _, name := range []string{"inflight.txt", "eligible.txt"} {
		p := filepath.Join(syncDir, name)
		os.WriteFile(p, make([]byte, 3000), 0o644)
		os.Chtimes(p, old, old)
		cat.UpsertFile(&catalog.FileRecord{
			FolderLabel:  "TestFolder",
			RelativePath: name,
			Filename:     name,
			Extension:    ".txt",
			SizeBytes:    3000,
			FileMtime:    old,
			FileAtime:    old,
			Status:       catalog.StatusSynced,
		}, time.Now().UnixNano())
	}

	// A file whose move out has been claimed but not committed still occupies
	// the device, so the folder is at 6000 against a 5000 budget. Counting
	// only 'synced' would report 3000 and archive nothing at all.
	inflight, _ := cat.GetFile("TestFolder", "inflight.txt")
	cat.UpdateFileStatus(inflight.ID, catalog.StatusArchiving)

	declarePolicy(t, cfg, cat)

	d := newTestDaemon(t, cfg, cat, testLogger())
	d.SetSpaceCheck(func(string) (float64, error) { return 90.0, nil })
	d.EnforceQuotas()

	eligible, err := cat.GetFile("TestFolder", "eligible.txt")
	if err != nil {
		t.Fatal(err)
	}
	if eligible.Status != catalog.StatusArchived {
		t.Errorf("status = %q, want archived: the in-flight file's bytes count against the budget",
			eligible.Status)
	}
}
