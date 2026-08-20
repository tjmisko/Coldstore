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

	if err := crossDeviceMove(context.Background(), src, dst, testLogger()); err != nil {
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

	err := crossDeviceMove(context.Background(), src, dst, testLogger())
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

func TestCrossDeviceMoveShouldKeepSourceWhenContextIsCancelledMidCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "big.bin")
	dst := filepath.Join(dir, "big.archived.bin")

	// Large enough that io.Copy makes many 32 KiB reads, so cancellation is
	// observed part-way through rather than after the whole file.
	content := make([]byte, 4<<20)
	for i := range content {
		content[i] = byte(i)
	}
	os.WriteFile(src, content, 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := crossDeviceMove(ctx, src, dst, testLogger())
	if err == nil {
		t.Fatal("cancelled move should fail")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	if _, statErr := os.Stat(src); statErr != nil {
		t.Errorf("source must survive a cancelled move: %v", statErr)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Error("partial destination should be cleaned up")
	}
}

func TestOffloadShouldLeaveFileSyncedWhenContextIsCancelled(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)

	path := filepath.Join(syncDir, "photo.jpg")
	content := make([]byte, 2<<20)
	os.WriteFile(path, content, 0o644)

	cat.UpsertFile(&catalog.FileRecord{
		FolderLabel:  "TestFolder",
		RelativePath: "photo.jpg",
		Filename:     "photo.jpg",
		Extension:    ".jpg",
		SizeBytes:    int64(len(content)),
		FileMtime:    time.Now().UTC(),
		Status:       catalog.StatusSynced,
	})
	file, _ := cat.GetFile("TestFolder", "photo.jpg")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	offloader := NewOffloader(cfg, cat, testLogger())
	if err := offloader.OffloadContext(ctx, file, syncDir); err == nil {
		t.Fatal("cancelled offload should return an error")
	}

	after, err := cat.GetFile("TestFolder", "photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced (an interrupted offload must not strand a file in 'archiving')",
			after.Status)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("source file must still be on disk after a cancelled offload: %v", err)
	}
}

func TestRestoreShouldLeaveFileArchivedWhenContextIsCancelled(t *testing.T) {
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
	})
	file, _ := cat.GetFile("TestFolder", "photo.jpg")
	cat.UpdateFileStatus(file.ID, catalog.StatusArchived)
	file, _ = cat.GetFile("TestFolder", "photo.jpg")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	restorer := NewRestorer(cfg, cat, testLogger())
	if err := restorer.RestoreContext(ctx, file); err == nil {
		t.Fatal("cancelled restore should return an error")
	}

	after, err := cat.GetFile("TestFolder", "photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != catalog.StatusArchived {
		t.Errorf("status = %q, want archived (an interrupted restore must not strand a file in 'restoring')",
			after.Status)
	}
	if _, err := os.Stat(archived); err != nil {
		t.Errorf("the archive copy must survive a cancelled restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(syncDir, "photo.jpg")); !os.IsNotExist(err) {
		t.Error("a partial restored file should not be left in the syncthing folder")
	}
}

func TestEnforceQuotasShouldStopOffloadingWhenContextIsCancelled(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)

	cfg.Folders[0].BudgetBytes = 0
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
		})
	}

	d := New(cfg, cat, testLogger())
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

	d := New(cfg, cat, testLogger())

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
