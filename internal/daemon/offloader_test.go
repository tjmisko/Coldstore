package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

func setupTestEnv(t *testing.T) (syncDir, coldDir string, cfg *config.Config, cat *catalog.Catalog) {
	t.Helper()

	syncDir = filepath.Join(t.TempDir(), "sync")
	coldDir = filepath.Join(t.TempDir(), "cold")
	os.MkdirAll(syncDir, 0755)
	os.MkdirAll(filepath.Join(coldDir, "archive"), 0755)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })

	cfg = &config.Config{
		Storage: config.StorageConfig{ColdStoragePath: coldDir},
		Daemon:  config.DaemonConfig{GracePeriodMinutes: 30},
		Folders: []config.FolderConfig{
			{Label: "TestFolder", Path: syncDir, BudgetBytes: 5000},
		},
	}
	return
}

func TestOffloadAndRestore(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Create a test file in the sync directory
	subdir := filepath.Join(syncDir, "2024", "01")
	os.MkdirAll(subdir, 0755)
	testFile := filepath.Join(subdir, "photo.jpg")
	content := []byte("fake jpeg content for testing")
	os.WriteFile(testFile, content, 0644)

	// Insert into catalog
	record := &catalog.FileRecord{
		FolderLabel:  "TestFolder",
		RelativePath: "2024/01/photo.jpg",
		Filename:     "photo.jpg",
		Extension:    ".jpg",
		SizeBytes:    int64(len(content)),
		FileMtime:    time.Now().UTC(),
		Status:       catalog.StatusSynced,
	}
	cat.UpsertFile(record)
	file, _ := cat.GetFile("TestFolder", "2024/01/photo.jpg")

	// Offload
	offloader := NewOffloader(cfg, cat, logger)
	if err := offloader.Offload(file, syncDir); err != nil {
		t.Fatal(err)
	}

	// Source should be gone
	if _, err := os.Stat(testFile); !os.IsNotExist(err) {
		t.Error("source file should be removed after offload")
	}

	// Destination should exist with correct content
	archivePath := filepath.Join(coldDir, "archive", "TestFolder", "2024", "01", "photo.jpg")
	got, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal("archived file should exist:", err)
	}
	if string(got) != string(content) {
		t.Error("archived file content mismatch")
	}

	// Catalog should show archived
	file, _ = cat.GetFile("TestFolder", "2024/01/photo.jpg")
	if file.Status != catalog.StatusArchived {
		t.Errorf("status = %q, want archived", file.Status)
	}

	// Restore
	restorer := NewRestorer(cfg, cat, logger)
	if err := restorer.Restore(file); err != nil {
		t.Fatal(err)
	}

	// File should be back in sync dir
	got, err = os.ReadFile(testFile)
	if err != nil {
		t.Fatal("restored file should exist:", err)
	}
	if string(got) != string(content) {
		t.Error("restored file content mismatch")
	}

	// Archive copy should be gone
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Error("archive file should be removed after restore")
	}

	// Catalog should show synced
	file, _ = cat.GetFile("TestFolder", "2024/01/photo.jpg")
	if file.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", file.Status)
	}
}

func TestCrossDeviceMove(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")

	content := []byte("cross device test content")
	os.WriteFile(src, content, 0644)

	// crossDeviceMove should work even on same device (it's a copy+delete)
	if err := crossDeviceMove(src, dst); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source should be removed")
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Error("content mismatch")
	}
}

func TestQuotaEnforcementOrdering(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Budget is 5000 bytes. Create 3 files totaling 9000 bytes.
	// Oldest should be offloaded first.
	cfg.Daemon.GracePeriodMinutes = 0 // Disable grace period for test

	files := []struct {
		name  string
		size  int
		mtime time.Time
	}{
		{"oldest.txt", 3000, time.Now().Add(-3 * time.Hour)},
		{"middle.txt", 3000, time.Now().Add(-2 * time.Hour)},
		{"newest.txt", 3000, time.Now().Add(-1 * time.Hour)},
	}

	for _, f := range files {
		path := filepath.Join(syncDir, f.name)
		os.WriteFile(path, make([]byte, f.size), 0644)
		os.Chtimes(path, f.mtime, f.mtime)

		record := &catalog.FileRecord{
			FolderLabel:  "TestFolder",
			RelativePath: f.name,
			Filename:     f.name,
			Extension:    ".txt",
			SizeBytes:    int64(f.size),
			FileMtime:    f.mtime,
			Status:       catalog.StatusSynced,
		}
		cat.UpsertFile(record)
	}

	d := New(cfg, cat, logger)
	d.enforceQuotas()

	// oldest.txt and middle.txt should be offloaded (9000 - 3000 = 6000, still > 5000, so offload another: 6000 - 3000 = 3000 <= 5000)
	f1, _ := cat.GetFile("TestFolder", "oldest.txt")
	if f1.Status != catalog.StatusArchived {
		t.Errorf("oldest.txt status = %q, want archived", f1.Status)
	}

	f2, _ := cat.GetFile("TestFolder", "middle.txt")
	if f2.Status != catalog.StatusArchived {
		t.Errorf("middle.txt status = %q, want archived", f2.Status)
	}

	f3, _ := cat.GetFile("TestFolder", "newest.txt")
	if f3.Status != catalog.StatusSynced {
		t.Errorf("newest.txt status = %q, want synced", f3.Status)
	}
}

func TestGracePeriodSkipsRecentFiles(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cfg.Folders[0].BudgetBytes = 1000
	cfg.Daemon.GracePeriodMinutes = 60 // 1 hour grace period

	// Create a recent file that exceeds budget
	path := filepath.Join(syncDir, "recent.txt")
	os.WriteFile(path, make([]byte, 5000), 0644)
	// mtime is now (within grace period)

	record := &catalog.FileRecord{
		FolderLabel:  "TestFolder",
		RelativePath: "recent.txt",
		Filename:     "recent.txt",
		Extension:    ".txt",
		SizeBytes:    5000,
		FileMtime:    time.Now().UTC(),
		Status:       catalog.StatusSynced,
	}
	cat.UpsertFile(record)

	d := New(cfg, cat, logger)
	d.enforceQuotas()

	// File should NOT be offloaded (within grace period)
	f, _ := cat.GetFile("TestFolder", "recent.txt")
	if f.Status != catalog.StatusSynced {
		t.Errorf("recent file status = %q, want synced (grace period should protect it)", f.Status)
	}
}

func TestPersistentPathsSkipped(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cfg.Folders[0].BudgetBytes = 1000
	cfg.Folders[0].Persistent = []string{"Starred/*"}
	cfg.Daemon.GracePeriodMinutes = 0

	// Create a file in the persistent path
	starredDir := filepath.Join(syncDir, "Starred")
	os.MkdirAll(starredDir, 0755)
	oldTime := time.Now().Add(-24 * time.Hour)

	path := filepath.Join(starredDir, "fav.jpg")
	os.WriteFile(path, make([]byte, 5000), 0644)
	os.Chtimes(path, oldTime, oldTime)

	record := &catalog.FileRecord{
		FolderLabel:  "TestFolder",
		RelativePath: "Starred/fav.jpg",
		Filename:     "fav.jpg",
		Extension:    ".jpg",
		SizeBytes:    5000,
		FileMtime:    oldTime,
		Status:       catalog.StatusSynced,
	}
	cat.UpsertFile(record)

	d := New(cfg, cat, logger)
	d.enforceQuotas()

	// File should NOT be offloaded (persistent path)
	f, _ := cat.GetFile("TestFolder", "Starred/fav.jpg")
	if f.Status != catalog.StatusSynced {
		t.Errorf("persistent file status = %q, want synced", f.Status)
	}
}

func TestRestoreNonArchivedFails(t *testing.T) {
	_, _, cfg, cat := setupTestEnv(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	record := &catalog.FileRecord{
		FolderLabel:  "TestFolder",
		RelativePath: "test.txt",
		Filename:     "test.txt",
		Extension:    ".txt",
		SizeBytes:    100,
		FileMtime:    time.Now().UTC(),
		Status:       catalog.StatusSynced,
	}
	cat.UpsertFile(record)
	file, _ := cat.GetFile("TestFolder", "test.txt")

	restorer := NewRestorer(cfg, cat, logger)
	err := restorer.Restore(file)
	if err == nil {
		t.Error("restoring a synced file should fail")
	}
}
