package catalog

import (
	"path/filepath"
	"testing"
	"time"
)

func setupTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cat, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	return cat
}

func TestUpsertAndGetFile(t *testing.T) {
	cat := setupTestCatalog(t)

	now := time.Now().UTC().Truncate(time.Second)
	record := &FileRecord{
		FolderLabel:  "Photos",
		RelativePath: "2024/01/IMG_001.jpg",
		Filename:     "IMG_001.jpg",
		Extension:    ".jpg",
		MimeType:     "image/jpeg",
		SizeBytes:    1024000,
		FileMtime:    now,
		Status:       StatusSynced,
	}

	if err := cat.UpsertFile(record); err != nil {
		t.Fatal(err)
	}

	got, err := cat.GetFile("Photos", "2024/01/IMG_001.jpg")
	if err != nil {
		t.Fatal(err)
	}

	if got.Filename != "IMG_001.jpg" {
		t.Errorf("Filename = %q, want IMG_001.jpg", got.Filename)
	}
	if got.SizeBytes != 1024000 {
		t.Errorf("SizeBytes = %d, want 1024000", got.SizeBytes)
	}
	if got.Status != StatusSynced {
		t.Errorf("Status = %q, want synced", got.Status)
	}
	if got.ID == 0 {
		t.Error("ID should be non-zero")
	}
}

func TestUpsertFileConflictUpdate(t *testing.T) {
	cat := setupTestCatalog(t)

	now := time.Now().UTC().Truncate(time.Second)
	record := &FileRecord{
		FolderLabel:  "Photos",
		RelativePath: "test.jpg",
		Filename:     "test.jpg",
		Extension:    ".jpg",
		SizeBytes:    1000,
		FileMtime:    now,
		Status:       StatusSynced,
	}
	if err := cat.UpsertFile(record); err != nil {
		t.Fatal(err)
	}

	// Upsert again with different size
	record.SizeBytes = 2000
	if err := cat.UpsertFile(record); err != nil {
		t.Fatal(err)
	}

	got, err := cat.GetFile("Photos", "test.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if got.SizeBytes != 2000 {
		t.Errorf("SizeBytes after upsert = %d, want 2000", got.SizeBytes)
	}
}

func TestUpdateFileStatus(t *testing.T) {
	cat := setupTestCatalog(t)

	record := &FileRecord{
		FolderLabel:  "Photos",
		RelativePath: "test.jpg",
		Filename:     "test.jpg",
		Extension:    ".jpg",
		SizeBytes:    1000,
		FileMtime:    time.Now().UTC(),
		Status:       StatusSynced,
	}
	if err := cat.UpsertFile(record); err != nil {
		t.Fatal(err)
	}

	got, _ := cat.GetFile("Photos", "test.jpg")

	if err := cat.UpdateFileStatus(got.ID, StatusArchived); err != nil {
		t.Fatal(err)
	}

	got, _ = cat.GetFile("Photos", "test.jpg")
	if got.Status != StatusArchived {
		t.Errorf("Status = %q, want archived", got.Status)
	}
	if got.ArchivedAt == nil {
		t.Error("ArchivedAt should be set when archived")
	}

	// Restore
	if err := cat.UpdateFileStatus(got.ID, StatusSynced); err != nil {
		t.Fatal(err)
	}
	got, _ = cat.GetFile("Photos", "test.jpg")
	if got.Status != StatusSynced {
		t.Errorf("Status = %q, want synced", got.Status)
	}
	if got.ArchivedAt != nil {
		t.Error("ArchivedAt should be nil when synced")
	}
}

func TestListFilesFiltering(t *testing.T) {
	cat := setupTestCatalog(t)

	files := []FileRecord{
		{FolderLabel: "Photos", RelativePath: "a.jpg", Filename: "a.jpg", Extension: ".jpg", SizeBytes: 100, FileMtime: time.Now().UTC(), Status: StatusSynced},
		{FolderLabel: "Photos", RelativePath: "b.png", Filename: "b.png", Extension: ".png", SizeBytes: 200, FileMtime: time.Now().UTC(), Status: StatusSynced},
		{FolderLabel: "Photos", RelativePath: "c.jpg", Filename: "c.jpg", Extension: ".jpg", SizeBytes: 300, FileMtime: time.Now().UTC(), Status: StatusArchived},
		{FolderLabel: "Docs", RelativePath: "d.pdf", Filename: "d.pdf", Extension: ".pdf", SizeBytes: 400, FileMtime: time.Now().UTC(), Status: StatusSynced},
	}
	for i := range files {
		if err := cat.UpsertFile(&files[i]); err != nil {
			t.Fatal(err)
		}
	}

	// Filter by folder
	result, err := cat.ListFiles(FileQuery{Folder: "Photos"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 3 {
		t.Errorf("Photos folder count = %d, want 3", len(result))
	}

	// Filter by status
	result, err = cat.ListFiles(FileQuery{Status: StatusArchived})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Errorf("archived count = %d, want 1", len(result))
	}

	// Filter by extension
	result, err = cat.ListFiles(FileQuery{Extension: ".jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Errorf(".jpg count = %d, want 2", len(result))
	}

	// Search by filename
	result, err = cat.ListFiles(FileQuery{Search: "b.p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].Filename != "b.png" {
		t.Errorf("search 'b.p' got %v, want [b.png]", result)
	}
}

func TestFolderSyncedSize(t *testing.T) {
	cat := setupTestCatalog(t)

	files := []FileRecord{
		{FolderLabel: "Photos", RelativePath: "a.jpg", Filename: "a.jpg", Extension: ".jpg", SizeBytes: 1000, FileMtime: time.Now().UTC(), Status: StatusSynced},
		{FolderLabel: "Photos", RelativePath: "b.jpg", Filename: "b.jpg", Extension: ".jpg", SizeBytes: 2000, FileMtime: time.Now().UTC(), Status: StatusSynced},
		{FolderLabel: "Photos", RelativePath: "c.jpg", Filename: "c.jpg", Extension: ".jpg", SizeBytes: 500, FileMtime: time.Now().UTC(), Status: StatusArchived},
	}
	for i := range files {
		cat.UpsertFile(&files[i])
	}

	size, err := cat.FolderSyncedSize("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if size != 3000 {
		t.Errorf("FolderSyncedSize = %d, want 3000", size)
	}
}

func TestMarkMissingSyncedFiles(t *testing.T) {
	cat := setupTestCatalog(t)

	record := &FileRecord{
		FolderLabel:  "Photos",
		RelativePath: "gone.jpg",
		Filename:     "gone.jpg",
		Extension:    ".jpg",
		SizeBytes:    100,
		FileMtime:    time.Now().UTC(),
		Status:       StatusSynced,
	}
	cat.UpsertFile(record)

	if err := cat.MarkMissingSyncedFiles("Photos"); err != nil {
		t.Fatal(err)
	}

	got, _ := cat.GetFile("Photos", "gone.jpg")
	if got.Status != StatusMissing {
		t.Errorf("Status = %q, want missing", got.Status)
	}
}

func TestPersistentPaths(t *testing.T) {
	cat := setupTestCatalog(t)

	if err := cat.AddPersistentPath("Photos", "Starred/*"); err != nil {
		t.Fatal(err)
	}
	if err := cat.AddPersistentPath("Photos", "Favorites/*"); err != nil {
		t.Fatal(err)
	}

	paths, err := cat.ListPersistentPaths("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("len(paths) = %d, want 2", len(paths))
	}

	if err := cat.DeletePersistentPath(paths[0].ID); err != nil {
		t.Fatal(err)
	}
	paths, _ = cat.ListPersistentPaths("Photos")
	if len(paths) != 1 {
		t.Errorf("len(paths) after delete = %d, want 1", len(paths))
	}
}

func TestFolderConfig(t *testing.T) {
	cat := setupTestCatalog(t)

	fc := &FolderConfigRecord{
		FolderLabel:   "Photos",
		SyncthingID:   "abc-123",
		SyncthingPath: "/home/user/Photos",
		BudgetBytes:   20_000_000_000,
		GraceMinutes:  30,
		Enabled:       true,
	}
	if err := cat.UpsertFolderConfig(fc); err != nil {
		t.Fatal(err)
	}

	configs, err := cat.ListFolderConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 1 {
		t.Fatalf("len(configs) = %d, want 1", len(configs))
	}
	if configs[0].BudgetBytes != 20_000_000_000 {
		t.Errorf("BudgetBytes = %d, want 20000000000", configs[0].BudgetBytes)
	}
}

func TestLogEvent(t *testing.T) {
	cat := setupTestCatalog(t)

	if err := cat.LogEvent("archive", "Photos", "old.jpg", "moved to cold storage"); err != nil {
		t.Fatal(err)
	}
}

func TestFileCount(t *testing.T) {
	cat := setupTestCatalog(t)

	files := []FileRecord{
		{FolderLabel: "Photos", RelativePath: "a.jpg", Filename: "a.jpg", Extension: ".jpg", SizeBytes: 100, FileMtime: time.Now().UTC(), Status: StatusSynced},
		{FolderLabel: "Photos", RelativePath: "b.jpg", Filename: "b.jpg", Extension: ".jpg", SizeBytes: 200, FileMtime: time.Now().UTC(), Status: StatusArchived},
	}
	for i := range files {
		cat.UpsertFile(&files[i])
	}

	total, _ := cat.FileCount("")
	if total != 2 {
		t.Errorf("total count = %d, want 2", total)
	}
	synced, _ := cat.FileCount(StatusSynced)
	if synced != 1 {
		t.Errorf("synced count = %d, want 1", synced)
	}
}
