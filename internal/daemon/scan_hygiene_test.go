package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

func newHygieneScanner(t *testing.T) (*Scanner, *catalog.Catalog, catalog.FolderConfigRecord, string) {
	t.Helper()
	dir := newSyncthingFolder(t, filepath.Join(t.TempDir(), "sync"))
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })

	folder := declareFolder(t, cat, "TestFolder", dir)
	s := NewScanner(&config.Config{}, cat, testLogger())
	return s, cat, folder, dir
}

// Coldstore's own scratch files were catalogued as ordinary user files: a
// .coldstore-tmp partial left in a Syncthing folder by a hard kill mid-restore
// and a .coldstore-superseded copy set aside by a source-changed move were
// both walked, upserted, counted against the budget, offered for download as
// real photos and eventually archived into cold storage.
//
// Syncthing's own temp naming was worse: the old filter tested the RELATIVE
// path for a ".st" prefix, so it only ever fired at the folder root, and it
// never matched ".syncthing.NAME.tmp" at all.
func TestScanShouldNotCatalogueTemporaryOrSupersededFiles(t *testing.T) {
	s, cat, folder, dir := newHygieneScanner(t)

	nested := filepath.Join(dir, "2024", "06")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := map[string]bool{"photo.jpg": true}
	for _, name := range []string{
		"photo.jpg",
		"photo.jpg" + tmpSuffix,
		"photo.jpg" + supersededSuffix,
		".syncthing.VID_0031.mp4.tmp",
		"~syncthing~IMG_2.jpg.tmp",
		"partial.tmp",
	} {
		if err := os.WriteFile(filepath.Join(nested, name), make([]byte, 128), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.ScanFolderContext(context.Background(), folder); err != nil {
		t.Fatalf("scan: %v", err)
	}

	files, err := cat.ListFiles(catalog.FileQuery{Folder: "TestFolder", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !keep[f.Filename] {
			t.Errorf("coldstore's scratch file was catalogued as a real file: %q", f.RelativePath)
		}
	}
	if len(files) != 1 {
		t.Errorf("catalogued %d files, want 1", len(files))
	}
}

// Section 4.1b. When a path under an archived row reappears in the folder the
// row must NOT be flipped to synced: the cold-storage copy would then be
// referenced by no row -- invisible to every listing, every job and every
// cleanup -- and destroyed outright the next time that path was offloaded.
func TestScanShouldNotAdoptAnArchivedFilesPathWhenItReappears(t *testing.T) {
	s, cat, folder, dir := newHygieneScanner(t)

	original := int64(4096)
	mtime := time.Now().Add(-90 * 24 * time.Hour).Truncate(time.Second)
	if _, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel:  "TestFolder",
		RelativePath: "photo.jpg",
		Filename:     "photo.jpg",
		Extension:    ".jpg",
		SizeBytes:    original,
		FileMtime:    mtime,
		FileAtime:    mtime,
		Status:       catalog.StatusSynced,
	}, 1); err != nil {
		t.Fatal(err)
	}
	rec, _ := cat.GetFile("TestFolder", "photo.jpg")
	if err := cat.UpdateFileStatus(rec.ID, catalog.StatusArchived); err != nil {
		t.Fatal(err)
	}

	// The phone re-delivers a re-encode under the same name.
	if err := os.WriteFile(filepath.Join(dir, "photo.jpg"), make([]byte, 999), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.ScanFolderContext(context.Background(), folder); err != nil {
		t.Fatalf("scan: %v", err)
	}

	after, err := cat.GetFile("TestFolder", "photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != catalog.StatusArchived {
		t.Errorf("status = %q, want archived (the cold copy must stay referenced)", after.Status)
	}
	if after.SizeBytes != original {
		t.Errorf("size_bytes = %d, want the archived %d", after.SizeBytes, original)
	}
	events, err := cat.ListEvents("TestFolder", "conflict", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d conflict events, want 1", len(events))
	}

	// And the row must not be swept to missing by the walk that saw it.
	if after.Status == catalog.StatusMissing {
		t.Error("the conflicted row was swept missing")
	}
}

// The mid-walk restore a reviewer reproduced live: MarkRestored set
// status='synced' without refreshing last_seen, and the end-of-walk sweep
// asks only "status='synced' AND last_seen < generation". A file the walk
// passed while it was still in cold storage kept its stale last_seen, so an
// on-disk, pinned, just-restored file was declared missing -- 409 on download,
// 409 on restore, and its bytes dropped out of the folder footprint until the
// next scan, up to a scan interval away.
func TestScanShouldNotSweepAFileThatWasRestoredDuringTheWalk(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "photo.jpg", 2000)
	m := newTestMover(t, cfg, cat)
	if _, err := m.ArchiveByID(file.ID); err != nil {
		t.Fatalf("archiving: %v", err)
	}

	// The generation a walk that is ALREADY RUNNING would be holding.
	generation := time.Now().UnixNano()
	time.Sleep(time.Millisecond)

	if _, err := m.RestoreByID(file.ID); err != nil {
		t.Fatalf("restoring: %v", err)
	}

	swept, err := cat.MarkFilesMissingBeforeGeneration("TestFolder", generation)
	if err != nil {
		t.Fatal(err)
	}
	if swept != 0 {
		t.Errorf("the sweep marked %d rows missing, want 0", swept)
	}

	after, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced (the file is on disk)", after.Status)
	}
	if _, err := os.Stat(filepath.Join(syncDir, "photo.jpg")); err != nil {
		t.Fatalf("the file really is on disk: %v", err)
	}
	total, _, err := cat.FolderSizes("TestFolder")
	if err != nil {
		t.Fatal(err)
	}
	if total != 2000 {
		t.Errorf("folder footprint = %d, want 2000 -- enforcement cannot see the file", total)
	}
}

// The same rule for the revert path: a failed archive puts the row back to
// synced with the file still in the folder, so a walk in flight must not
// sweep it either.
func TestScanShouldNotSweepAFileWhoseArchiveFailedDuringTheWalk(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "photo.jpg", 1500)
	m := newTestMover(t, cfg, cat)

	generation := time.Now().UnixNano()
	time.Sleep(time.Millisecond)

	m.SetMoveFuncs(func(src, dst string) error { return os.ErrPermission }, nil)
	if _, err := m.ArchiveByID(file.ID); err == nil {
		t.Fatal("expected the archive to fail")
	}

	swept, err := cat.MarkFilesMissingBeforeGeneration("TestFolder", generation)
	if err != nil {
		t.Fatal(err)
	}
	if swept != 0 {
		t.Errorf("the sweep marked %d rows missing, want 0", swept)
	}
}

// asidePath does not stop at one suffix: a second collision on the same path
// produces "IMG.jpg.coldstore-superseded.2", a third ".3", and a pathological
// one a nanosecond stamp. The skip table matched only the bare suffix, so
// every aside after the first was walked, upserted, counted against the
// budget, offered for download as a real photo and eventually archived -- the
// precise outcome the skip table was written to prevent, reachable on the
// second occurrence.
//
// This is also the rule the runbook already asks owners to put in .stignore
// (`*.coldstore-superseded*`, trailing star deliberate). The scanner now
// applies the same rule.
func TestScanShouldNotCatalogueNumberedSupersededCopies(t *testing.T) {
	s, cat, folder, dir := newHygieneScanner(t)

	for _, name := range []string{
		"photo.jpg",
		"photo.jpg" + supersededSuffix,
		"photo.jpg" + supersededSuffix + ".2",
		"photo.jpg" + supersededSuffix + ".3",
		"photo.jpg" + supersededSuffix + ".1782000000000000000",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, 128), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.ScanFolderContext(context.Background(), folder); err != nil {
		t.Fatalf("scan: %v", err)
	}

	files, err := cat.ListFiles(catalog.FileQuery{Folder: "TestFolder", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].RelativePath != "photo.jpg" {
		got := make([]string, 0, len(files))
		for _, f := range files {
			got = append(got, f.RelativePath)
		}
		t.Errorf("catalogued %v, want only [photo.jpg]", got)
	}
}

// The name asidePath actually generates, rather than a hand-written
// approximation of it.
func TestSkipScanEntryShouldMatchEveryNameAsidePathCanProduce(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "IMG_0031.jpg")

	for i := 0; i < 4; i++ {
		aside := asidePath(target)
		if err := os.WriteFile(aside, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if !skipScanEntry(filepath.Base(aside)) {
			t.Errorf("aside %d: skipScanEntry(%q) = false, want true",
				i, filepath.Base(aside))
		}
	}
}
