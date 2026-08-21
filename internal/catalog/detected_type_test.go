package catalog

import (
	"testing"
	"time"
)

// addTypedFile inserts a row with an explicit extension, so the tests below
// can build the case the whole feature exists for: a file with no extension
// at all.
func addTypedFile(t *testing.T, cat *Catalog, folder, relPath, ext string, mtime time.Time) *FileRecord {
	t.Helper()
	rec := &FileRecord{
		FolderLabel:  folder,
		RelativePath: relPath,
		Filename:     relPath,
		Extension:    ext,
		SizeBytes:    1,
		FileMtime:    mtime,
		FileAtime:    mtime,
		Status:       StatusSynced,
	}
	if _, err := cat.UpsertFile(rec, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	got, err := cat.GetFile(folder, relPath)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestUpsertFileShouldLeaveDetectedTypeEmptyWhenTheRowIsNew(t *testing.T) {
	cat := setupTestCatalog(t)
	f := addTypedFile(t, cat, "Photos", "bookmarks", "", time.Now().UTC())

	if f.DetectedType != "" {
		t.Errorf("DetectedType = %q, want empty: a new row has not been sniffed yet", f.DetectedType)
	}
}

func TestFilesNeedingSniffShouldReturnARowWhenItHasNeverBeenSniffed(t *testing.T) {
	cat := setupTestCatalog(t)
	addTypedFile(t, cat, "Photos", "bookmarks", "", time.Now().UTC())

	queued, err := cat.FilesNeedingSniff("Photos", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("got %d queued, want 1", len(queued))
	}
	if queued[0].RelativePath != "bookmarks" {
		t.Errorf("queued %q, want bookmarks", queued[0].RelativePath)
	}
}

func TestFilesNeedingSniffShouldSkipARowWhenItHasAlreadyBeenSniffed(t *testing.T) {
	cat := setupTestCatalog(t)
	f := addTypedFile(t, cat, "Photos", "bookmarks", "", time.Now().UTC())

	if err := cat.SetDetectedType(f.ID, "text/plain"); err != nil {
		t.Fatal(err)
	}

	queued, err := cat.FilesNeedingSniff("Photos", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Errorf("got %d queued, want 0: a sniffed row must leave the queue", len(queued))
	}
}

func TestFilesNeedingSniffShouldSkipARowWhenTheSniffRecognisedNothing(t *testing.T) {
	// The whole reason TypeUnknown is not the empty string. A file the sniff
	// cannot identify must leave the queue, or it is re-opened on every scan
	// for the life of the installation.
	cat := setupTestCatalog(t)
	f := addTypedFile(t, cat, "Photos", "firmware", "", time.Now().UTC())

	if err := cat.SetDetectedType(f.ID, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}

	queued, err := cat.FilesNeedingSniff("Photos", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Errorf("got %d queued, want 0: an unidentifiable file must still leave the queue", len(queued))
	}
}

func TestSetDetectedTypeShouldFailWhenTheTypeIsEmpty(t *testing.T) {
	cat := setupTestCatalog(t)
	f := addTypedFile(t, cat, "Photos", "bookmarks", "", time.Now().UTC())

	if err := cat.SetDetectedType(f.ID, ""); err == nil {
		t.Error("storing an empty detected_type must fail: it would re-queue the row forever")
	}
}

func TestFilesNeedingSniffShouldStopAtTheLimitWhenTheQueueIsLarge(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()
	for i := range 50 {
		addTypedFile(t, cat, "Photos", "f"+itoa(i), "", now)
	}

	queued, err := cat.FilesNeedingSniff("Photos", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 10 {
		t.Errorf("got %d queued, want 10: the sniff opens a file per row and must stay bounded", len(queued))
	}
}

func TestFilesNeedingSniffShouldSkipAFileWhenItIsArchived(t *testing.T) {
	// An archived file's bytes are in cold storage. The scanner walks the
	// syncthing folder, so it has no path to read and must not queue one.
	cat := setupTestCatalog(t)
	f := addTypedFile(t, cat, "Photos", "gone", "", time.Now().UTC())
	if _, err := cat.db.Exec(`UPDATE files SET status = 'archived' WHERE id = ?`, f.ID); err != nil {
		t.Fatal(err)
	}

	queued, err := cat.FilesNeedingSniff("Photos", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Errorf("got %d queued, want 0", len(queued))
	}
}

func TestUpsertFileShouldClearDetectedTypeWhenTheMtimeChanges(t *testing.T) {
	cat := setupTestCatalog(t)
	mtime := time.Now().UTC().Truncate(time.Second)
	f := addTypedFile(t, cat, "Photos", "notes", "", mtime)
	if err := cat.SetDetectedType(f.ID, "text/plain"); err != nil {
		t.Fatal(err)
	}

	// The same path, replaced in place with different content.
	addTypedFile(t, cat, "Photos", "notes", "", mtime.Add(time.Hour))

	got, err := cat.GetFile("Photos", "notes")
	if err != nil {
		t.Fatal(err)
	}
	if got.DetectedType != "" {
		t.Errorf("DetectedType = %q, want empty: changed bytes must invalidate the sniff", got.DetectedType)
	}
}

func TestUpsertFileShouldClearDetectedTypeWhenTheSizeChanges(t *testing.T) {
	cat := setupTestCatalog(t)
	mtime := time.Now().UTC().Truncate(time.Second)
	f := addTypedFile(t, cat, "Photos", "notes", "", mtime)
	if err := cat.SetDetectedType(f.ID, "text/plain"); err != nil {
		t.Fatal(err)
	}

	rec := &FileRecord{
		FolderLabel: "Photos", RelativePath: "notes", Filename: "notes",
		SizeBytes: 99999, FileMtime: mtime, FileAtime: mtime, Status: StatusSynced,
	}
	if _, err := cat.UpsertFile(rec, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}

	got, err := cat.GetFile("Photos", "notes")
	if err != nil {
		t.Fatal(err)
	}
	if got.DetectedType != "" {
		t.Errorf("DetectedType = %q, want empty: a size change must invalidate the sniff", got.DetectedType)
	}
}

func TestUpsertFileShouldKeepDetectedTypeWhenNothingAboutTheBytesChanged(t *testing.T) {
	// The gate that stops the sniff running on every scan. Without it the
	// daemon re-opens every file in every folder on every tick.
	cat := setupTestCatalog(t)
	mtime := time.Now().UTC().Truncate(time.Second)
	f := addTypedFile(t, cat, "Photos", "notes", "", mtime)
	if err := cat.SetDetectedType(f.ID, "text/plain"); err != nil {
		t.Fatal(err)
	}

	addTypedFile(t, cat, "Photos", "notes", "", mtime)

	got, err := cat.GetFile("Photos", "notes")
	if err != nil {
		t.Fatal(err)
	}
	if got.DetectedType != "text/plain" {
		t.Errorf("DetectedType = %q, want text/plain: an unchanged file must not be re-sniffed", got.DetectedType)
	}
	queued, err := cat.FilesNeedingSniff("Photos", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Errorf("got %d queued, want 0", len(queued))
	}
}

func TestFilesNeedingThumbnailsByTypeShouldSelectAFileWhenItHasNoExtensionButAKnownType(t *testing.T) {
	// H0b's whole point. This row is invisible to extension IN (...) because
	// its extension is the empty string.
	cat := setupTestCatalog(t)
	f := addTypedFile(t, cat, "Photos", "screenshot-no-extension", "", time.Now().UTC())
	if err := cat.SetDetectedType(f.ID, "image/png"); err != nil {
		t.Fatal(err)
	}

	byExtension, err := cat.FilesNeedingThumbnails([]string{".jpg", ".png"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(byExtension) != 0 {
		t.Fatalf("the extension arm found %d rows; the fixture is wrong", len(byExtension))
	}

	byType, err := cat.FilesNeedingThumbnailsByType([]string{".jpg", ".png"}, []string{"image/png"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(byType) != 1 {
		t.Fatalf("got %d rows, want 1: an extension-less file must be reachable by its sniffed type", len(byType))
	}
	if byType[0].DetectedType != "image/png" {
		t.Errorf("DetectedType = %q, want image/png", byType[0].DetectedType)
	}
}

func TestFilesNeedingThumbnailsByTypeShouldSelectAFileWhenOnlyItsExtensionMatches(t *testing.T) {
	cat := setupTestCatalog(t)
	addTypedFile(t, cat, "Photos", "holiday.jpg", ".jpg", time.Now().UTC())

	files, err := cat.FilesNeedingThumbnailsByType([]string{".jpg"}, []string{"image/png"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Errorf("got %d rows, want 1: adding the type arm must not narrow the extension arm", len(files))
	}
}

func TestFilesNeedingThumbnailsByTypeShouldReturnEachFileOnceWhenBothArmsMatch(t *testing.T) {
	cat := setupTestCatalog(t)
	f := addTypedFile(t, cat, "Photos", "holiday.jpg", ".jpg", time.Now().UTC())
	if err := cat.SetDetectedType(f.ID, "image/jpeg"); err != nil {
		t.Fatal(err)
	}

	files, err := cat.FilesNeedingThumbnailsByType([]string{".jpg"}, []string{"image/jpeg"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Errorf("got %d rows, want 1: an OR of two arms must not duplicate the row", len(files))
	}
}

func TestFilesNeedingThumbnailsByTypeShouldReturnNothingWhenBothArmsAreEmpty(t *testing.T) {
	cat := setupTestCatalog(t)
	addTypedFile(t, cat, "Photos", "holiday.jpg", ".jpg", time.Now().UTC())

	files, err := cat.FilesNeedingThumbnailsByType(nil, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("got %d rows, want 0: an empty predicate must not select the whole table", len(files))
	}
}

func TestFilesNeedingThumbnailsByTypeShouldSkipAFileWhenItsSniffRecognisedNothing(t *testing.T) {
	cat := setupTestCatalog(t)
	f := addTypedFile(t, cat, "Photos", "firmware", "", time.Now().UTC())
	if err := cat.SetDetectedType(f.ID, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}

	files, err := cat.FilesNeedingThumbnailsByType([]string{".jpg"}, []string{"image/png", "image/jpeg"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("got %d rows, want 0", len(files))
	}
}
