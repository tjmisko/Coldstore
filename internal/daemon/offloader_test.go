package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

func TestEnforcementPagesPastFiveThousandProtectedRowsToAnEligiblePair(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	cfg.Daemon.GracePeriodMinutes = 0
	cfg.Folders[0].BudgetBytes = 5000
	cfg.Folders[0].Persistent = []string{"Protected/*"}
	declarePolicy(t, cfg, cat)

	old := time.Now().UTC().Add(-48 * time.Hour)
	var firstProtected, lastProtected int64
	for i := 0; i < offloadCandidateBatchSize; i++ {
		rel := fmt.Sprintf("Protected/%04d.jpg", i)
		if _, err := cat.UpsertFile(&catalog.FileRecord{
			FolderLabel: "TestFolder", RelativePath: rel, Filename: filepath.Base(rel),
			Extension: ".jpg", SizeBytes: 1, FileMtime: old, FileAtime: old,
			Status: catalog.StatusSynced,
		}, int64(i+1)); err != nil {
			t.Fatal(err)
		}
		got, err := cat.GetFile("TestFolder", rel)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstProtected = got.ID
		}
		lastProtected = got.ID
	}
	still := seedSyncedFile(t, cat, syncDir, "Eligible/IMG_5001.heic", 1)
	movie := seedSyncedFile(t, cat, syncDir, "Eligible/IMG_5001.mov", 1)
	assignTestPair(t, cat, "cid:after-protected-page", still, movie)
	ageFiles(t, cat, syncDir, still.RelativePath, movie.RelativePath)

	d := newTestDaemon(t, cfg, cat, testLogger())
	d.SetSpaceCheck(plentyOfSpace)
	d.EnforceQuotas()

	for _, file := range []*catalog.FileRecord{still, movie} {
		got, err := cat.GetFileByID(file.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != catalog.StatusArchived {
			t.Errorf("eligible pair member %s status = %q", got.RelativePath, got.Status)
		}
	}
	for _, id := range []int64{firstProtected, lastProtected} {
		got, err := cat.GetFileByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != catalog.StatusSynced {
			t.Errorf("protected row %d moved: %q", id, got.Status)
		}
	}
}

func TestGroupPairCandidatesOrdersByNewestMemberAcrossInputPages(t *testing.T) {
	base := time.Now().UTC().Add(-24 * time.Hour)
	files := []catalog.FileRecord{
		{ID: 1, FolderLabel: "TestFolder", PairKey: "cid:split-page", FileMtime: base, FileAtime: base},
		{ID: 2, FolderLabel: "TestFolder", FileMtime: base.Add(time.Hour), FileAtime: base.Add(time.Hour)},
		{ID: 3, FolderLabel: "TestFolder", PairKey: "cid:split-page", FileMtime: base.Add(2 * time.Hour), FileAtime: base.Add(2 * time.Hour)},
	}

	groups := groupPairCandidates(files)
	if len(groups) != 2 {
		t.Fatalf("groups = %#v, want two complete groups", groups)
	}
	if len(groups[0]) != 1 || groups[0][0].ID != 2 {
		t.Fatalf("first group = %#v, want older unpaired id 2", groups[0])
	}
	if len(groups[1]) != 2 || groups[1][0].ID != 1 || groups[1][1].ID != 3 {
		t.Fatalf("second group = %#v, want complete pair [1 3]", groups[1])
	}
}

// testLogger keeps test output quiet unless something actually goes wrong.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func setupTestEnv(t *testing.T) (syncDir, coldDir string, cfg *config.Config, cat *catalog.Catalog) {
	t.Helper()

	syncDir = newSyncthingFolder(t, filepath.Join(t.TempDir(), "sync"))
	coldDir = filepath.Join(t.TempDir(), "cold")
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
	cat.UpsertFile(record, time.Now().UnixNano())
	file, _ := cat.GetFile("TestFolder", "2024/01/photo.jpg")

	declarePolicy(t, cfg, cat)
	mover := NewMover(cfg, cat, logger)
	mover.SetSpaceCheck(plentyOfSpace)

	// Offload
	if _, err := mover.ArchiveByID(file.ID); err != nil {
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
	if _, err := mover.RestoreByID(file.ID); err != nil {
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
	if err := crossDeviceMove(src, dst, testLogger()); err != nil {
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
		cat.UpsertFile(record, time.Now().UnixNano())
	}

	declarePolicy(t, cfg, cat)

	d := newTestDaemon(t, cfg, cat, logger)
	d.EnforceQuotas()

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
	cat.UpsertFile(record, time.Now().UnixNano())

	declarePolicy(t, cfg, cat)

	d := newTestDaemon(t, cfg, cat, logger)
	d.EnforceQuotas()

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
	cat.UpsertFile(record, time.Now().UnixNano())

	declarePolicy(t, cfg, cat)

	d := newTestDaemon(t, cfg, cat, logger)
	d.EnforceQuotas()

	// File should NOT be offloaded (persistent path)
	f, _ := cat.GetFile("TestFolder", "Starred/fav.jpg")
	if f.Status != catalog.StatusSynced {
		t.Errorf("persistent file status = %q, want synced", f.Status)
	}
}

func TestRestoreShouldChangeNothingWhenTheFileIsAlreadySynced(t *testing.T) {
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
	cat.UpsertFile(record, time.Now().UnixNano())
	file, _ := cat.GetFile("TestFolder", "test.txt")

	declarePolicy(t, cfg, cat)

	mover := NewMover(cfg, cat, logger)
	got, err := mover.RestoreByID(file.ID)
	if err != nil {
		t.Fatalf("restoring an already-synced file is a no-op success (invariant 12): %v", err)
	}
	if got.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", got.Status)
	}
	if got.Pinned {
		t.Error("a no-op restore must not pin: nothing was restored")
	}
}
