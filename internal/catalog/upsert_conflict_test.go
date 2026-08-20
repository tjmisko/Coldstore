package catalog

import (
	"testing"
	"time"
)

func seedRow(t *testing.T, cat *Catalog, rel string, size int64, gen int64) *FileRecord {
	t.Helper()
	mtime := time.Now().Add(-90 * 24 * time.Hour).UTC().Truncate(time.Second)
	conflict, err := cat.UpsertFile(&FileRecord{
		FolderLabel:  "Photos",
		RelativePath: rel,
		Filename:     rel,
		Extension:    ".jpg",
		MimeType:     "image/jpeg",
		SizeBytes:    size,
		FileMtime:    mtime,
		FileAtime:    mtime,
		Status:       StatusSynced,
	}, gen)
	if err != nil {
		t.Fatal(err)
	}
	if conflict {
		t.Fatalf("a fresh insert of %q reported a conflict", rel)
	}
	rec, err := cat.GetFile("Photos", rel)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// Design section 4.1b. The 'archived' arm of the old status CASE was a silent
// data-loss engine: the row flipped to synced, size_bytes and file_mtime were
// overwritten, and the cold-storage copy became referenced by no row at all --
// invisible to every listing and destroyed the next time that path was
// offloaded.
func TestUpsertShouldNotAdoptAnArchivedRowsPathWhenAFileReappearsThere(t *testing.T) {
	cat := setupTestCatalog(t)
	rec := seedRow(t, cat, "photo.jpg", 4096, 1000)
	if err := cat.UpdateFileStatus(rec.ID, StatusArchived); err != nil {
		t.Fatal(err)
	}
	before, _ := cat.GetFileByID(rec.ID)

	conflict, err := cat.UpsertFile(&FileRecord{
		FolderLabel:  "Photos",
		RelativePath: "photo.jpg",
		Filename:     "photo.jpg",
		Extension:    ".jpg",
		MimeType:     "image/jpeg",
		SizeBytes:    999,
		FileMtime:    time.Now().UTC(),
		FileAtime:    time.Now().UTC(),
		Status:       StatusSynced,
	}, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !conflict {
		t.Fatal("UpsertFile did not report the conflict")
	}

	after, _ := cat.GetFileByID(rec.ID)
	if after.Status != StatusArchived {
		t.Errorf("status = %q, want archived", after.Status)
	}
	if after.SizeBytes != before.SizeBytes {
		t.Errorf("size_bytes = %d, want the archived %d", after.SizeBytes, before.SizeBytes)
	}
	if !after.FileMtime.Equal(before.FileMtime) {
		t.Errorf("file_mtime = %v, want the archived %v", after.FileMtime, before.FileMtime)
	}
	if after.LastSeen != 2000 {
		t.Errorf("last_seen = %d, want 2000 -- the walk did see the path", after.LastSeen)
	}
}

// A 'missing' row is still adopted: that is a file coming back, not a conflict.
func TestUpsertShouldAdoptAMissingRowWhenTheFileComesBack(t *testing.T) {
	cat := setupTestCatalog(t)
	rec := seedRow(t, cat, "photo.jpg", 4096, 1000)
	if err := cat.UpdateFileStatus(rec.ID, StatusMissing); err != nil {
		t.Fatal(err)
	}

	conflict, err := cat.UpsertFile(&FileRecord{
		FolderLabel:  "Photos",
		RelativePath: "photo.jpg",
		Filename:     "photo.jpg",
		Extension:    ".jpg",
		SizeBytes:    5000,
		FileMtime:    time.Now().UTC(),
		FileAtime:    time.Now().UTC(),
		Status:       StatusSynced,
	}, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if conflict {
		t.Error("a returning missing file is not a conflict")
	}

	after, _ := cat.GetFileByID(rec.ID)
	if after.Status != StatusSynced {
		t.Errorf("status = %q, want synced", after.Status)
	}
	if after.SizeBytes != 5000 {
		t.Errorf("size_bytes = %d, want the new 5000", after.SizeBytes)
	}
}

// The sweep asks "status='synced' AND last_seen < generation". Every
// transition that puts a file back on the device must therefore stamp
// last_seen, or a walk already in flight declares an on-disk file missing.
func TestMarkRestoredShouldStampLastSeenSoAnInFlightWalkCannotSweepIt(t *testing.T) {
	cat := setupTestCatalog(t)
	rec := seedRow(t, cat, "photo.jpg", 4096, 1000)
	if err := cat.UpdateFileStatus(rec.ID, StatusRestoring); err != nil {
		t.Fatal(err)
	}

	generation := time.Now().UnixNano()
	time.Sleep(time.Millisecond)

	ok, err := cat.MarkRestored(rec.ID, time.Now().Unix())
	if err != nil || !ok {
		t.Fatalf("MarkRestored = %v, %v", ok, err)
	}

	swept, err := cat.MarkFilesMissingBeforeGeneration("Photos", generation)
	if err != nil {
		t.Fatal(err)
	}
	if swept != 0 {
		t.Fatalf("the sweep took %d rows, want 0", swept)
	}
	after, _ := cat.GetFileByID(rec.ID)
	if after.Status != StatusSynced {
		t.Errorf("status = %q, want synced", after.Status)
	}
	if !after.Pinned {
		t.Error("a restore must pin (D2)")
	}
}

func TestClaimFileOnDeviceShouldStampLastSeenWhileAPlainClaimDoesNot(t *testing.T) {
	cat := setupTestCatalog(t)
	rec := seedRow(t, cat, "photo.jpg", 4096, 1000)
	if err := cat.UpdateFileStatus(rec.ID, StatusArchiving); err != nil {
		t.Fatal(err)
	}

	generation := time.Now().UnixNano()
	time.Sleep(time.Millisecond)

	ok, err := cat.ClaimFileOnDevice(rec.ID, StatusArchiving, StatusSynced)
	if err != nil || !ok {
		t.Fatalf("ClaimFileOnDevice = %v, %v", ok, err)
	}

	swept, err := cat.MarkFilesMissingBeforeGeneration("Photos", generation)
	if err != nil {
		t.Fatal(err)
	}
	if swept != 0 {
		t.Errorf("the sweep took %d rows, want 0", swept)
	}

	// The plain CAS deliberately keeps its narrow contract, and is the one to
	// use for transitions that do NOT put a file on the device.
	after, _ := cat.GetFileByID(rec.ID)
	if after.LastSeen <= generation {
		t.Errorf("last_seen = %d, want a stamp later than the generation %d",
			after.LastSeen, generation)
	}
}

func TestAdoptDeviceCopyShouldTakeTheOnDiskMetadataWhenTheRowIsArchived(t *testing.T) {
	cat := setupTestCatalog(t)
	rec := seedRow(t, cat, "photo.jpg", 4096, 1000)
	if err := cat.UpdateFileStatus(rec.ID, StatusArchived); err != nil {
		t.Fatal(err)
	}

	mtime := time.Now().UTC().Truncate(time.Second)
	ok, err := cat.AdoptDeviceCopy(rec.ID, 777, mtime, mtime)
	if err != nil || !ok {
		t.Fatalf("AdoptDeviceCopy = %v, %v", ok, err)
	}

	after, _ := cat.GetFileByID(rec.ID)
	if after.Status != StatusSynced {
		t.Errorf("status = %q, want synced", after.Status)
	}
	if after.SizeBytes != 777 {
		t.Errorf("size_bytes = %d, want 777", after.SizeBytes)
	}
	if after.ArchivedAt != nil {
		t.Errorf("archived_at = %v, want nil", after.ArchivedAt)
	}

	// And it refuses a row that is not archived.
	again, err := cat.AdoptDeviceCopy(rec.ID, 1, mtime, mtime)
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Error("AdoptDeviceCopy wrote a row that was not archived")
	}
}
