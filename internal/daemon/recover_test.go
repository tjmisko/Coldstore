package daemon

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

// recoveryEnv is setupTestEnv plus the database path, so a test can read the
// events table directly. The catalog has no event reader yet.
type recoveryEnv struct {
	syncDir string
	coldDir string
	cfg     *config.Config
	cat     *catalog.Catalog
	dbPath  string
}

func setupRecoveryEnv(t *testing.T) *recoveryEnv {
	t.Helper()

	root := t.TempDir()
	syncDir := filepath.Join(root, "sync")
	coldDir := filepath.Join(root, "cold")
	newSyncthingFolder(t, syncDir)
	os.MkdirAll(filepath.Join(coldDir, "archive", "TestFolder"), 0o755)

	dbPath := filepath.Join(root, "catalog.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })

	return &recoveryEnv{
		syncDir: syncDir,
		coldDir: coldDir,
		dbPath:  dbPath,
		cat:     cat,
		cfg: &config.Config{
			Storage: config.StorageConfig{ColdStoragePath: coldDir},
			Folders: []config.FolderConfig{{Label: "TestFolder", Path: syncDir, BudgetBytes: 5000}},
		},
	}
}

func (e *recoveryEnv) devicePath(rel string) string { return filepath.Join(e.syncDir, rel) }
func (e *recoveryEnv) archivePath(rel string) string {
	return filepath.Join(e.coldDir, "archive", "TestFolder", rel)
}
func (e *recoveryEnv) trashPath(rel string) string {
	return filepath.Join(e.coldDir, "trash", "TestFolder", rel)
}

// plantRow catalogues a file with the given size and status without touching
// the filesystem, so a test can describe any post-crash state exactly.
func (e *recoveryEnv) plantRow(t *testing.T, rel string, size int64, mtime time.Time, status catalog.FileStatus) *catalog.FileRecord {
	t.Helper()
	if _, err := e.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel:  "TestFolder",
		RelativePath: rel,
		Filename:     filepath.Base(rel),
		Extension:    filepath.Ext(rel),
		SizeBytes:    size,
		FileMtime:    mtime,
		Status:       catalog.StatusSynced,
	}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	rec, err := e.cat.GetFile("TestFolder", rel)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cat.UpdateFileStatus(rec.ID, status); err != nil {
		t.Fatal(err)
	}
	rec.Status = status
	return rec
}

func (e *recoveryEnv) recover(t *testing.T) {
	t.Helper()
	if err := RecoverStartupState(e.cfg, e.cat, testLogger()); err != nil {
		t.Fatalf("RecoverStartupState: %v", err)
	}
}

func (e *recoveryEnv) status(t *testing.T, rel string) catalog.FileStatus {
	t.Helper()
	rec, err := e.cat.GetFile("TestFolder", rel)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Status
}

func (e *recoveryEnv) eventTypes(t *testing.T) []string {
	t.Helper()
	db, err := sql.Open("sqlite3", e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT event_type FROM events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var types []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		types = append(types, s)
	}
	return types
}

func hasEvent(types []string, want string) bool {
	for _, s := range types {
		if s == want {
			return true
		}
	}
	return false
}

func writeAt(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func mustNotExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s still exists: %s", path, why)
	}
}

func mustContain(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

func beginTrashState(t *testing.T, e *recoveryEnv, rel string, size int64, mtime time.Time, from catalog.FileStatus) *catalog.FileRecord {
	t.Helper()
	rec := e.plantRow(t, rel, size, mtime, from)
	claimed, err := e.cat.BeginTrash(rec.ID, from)
	if err != nil || !claimed {
		t.Fatalf("BeginTrash = %v, %v", claimed, err)
	}
	return rec
}

// ---------------------------------------------------------------------------
// rows stuck in trash transitions
// ---------------------------------------------------------------------------

func TestRecoverTrashingShouldPromoteTheLastCopyToRecoverableTrash(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	rec := beginTrashState(t, e, "deleted.txt", 10, now, catalog.StatusSynced)
	writeAt(t, e.trashPath("deleted.txt"), "0123456789", now)

	e.recover(t)

	got, err := e.cat.GetFileByID(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != catalog.StatusTrashed || got.TrashedFrom != catalog.StatusSynced || got.TrashedAt == nil {
		t.Fatalf("recovered row = status %q, from %q, at %v", got.Status, got.TrashedFrom, got.TrashedAt)
	}
	mustContain(t, e.trashPath("deleted.txt"), "0123456789")
}

func TestRecoverUntrashingShouldCommitTheRecordedOriginalState(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	rec := beginTrashState(t, e, "old.txt", 10, now, catalog.StatusArchived)
	if marked, err := e.cat.MarkTrashed(rec.ID, now); err != nil || !marked {
		t.Fatalf("MarkTrashed = %v, %v", marked, err)
	}
	if claimed, err := e.cat.BeginUntrash(rec.ID); err != nil || !claimed {
		t.Fatalf("BeginUntrash = %v, %v", claimed, err)
	}
	writeAt(t, e.archivePath("old.txt"), "0123456789", now)

	e.recover(t)

	got, err := e.cat.GetFileByID(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != catalog.StatusArchived || got.TrashedFrom != "" || got.TrashedAt != nil {
		t.Fatalf("recovered row = status %q, from %q, at %v", got.Status, got.TrashedFrom, got.TrashedAt)
	}
	mustContain(t, e.archivePath("old.txt"), "0123456789")
}

// ---------------------------------------------------------------------------
// rows stuck in "archiving"
// ---------------------------------------------------------------------------

func TestRecoverArchivingShouldRevertToSyncedWhenNothingReachedColdStorage(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	writeAt(t, e.devicePath("a.txt"), "0123456789", now)
	writeAt(t, e.archivePath("a.txt")+tmpSuffix, "012", now)
	e.plantRow(t, "a.txt", 10, now, catalog.StatusArchiving)

	e.recover(t)

	if got := e.status(t, "a.txt"); got != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", got)
	}
	mustContain(t, e.devicePath("a.txt"), "0123456789")
	mustNotExist(t, e.archivePath("a.txt")+tmpSuffix, "a killed copy's temp file is never valid")
}

func TestRecoverArchivingShouldMarkMissingWhenNeitherCopyExists(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	e.plantRow(t, "gone.txt", 10, now, catalog.StatusArchiving)

	e.recover(t)

	if got := e.status(t, "gone.txt"); got != catalog.StatusMissing {
		t.Errorf("status = %q, want missing", got)
	}
	if !hasEvent(e.eventTypes(t), "source_vanished") {
		t.Error("the owner must be told the file disappeared mid-archive")
	}
}

func TestRecoverArchivingShouldPromoteToArchivedWhenTheCopyIsCompleteAndTheSourceIsGone(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	writeAt(t, e.archivePath("b.txt"), "0123456789", now)
	e.plantRow(t, "b.txt", 10, now, catalog.StatusArchiving)

	e.recover(t)

	if got := e.status(t, "b.txt"); got != catalog.StatusArchived {
		// Guessing "the owner deleted it" here would delete the last copy.
		t.Errorf("status = %q, want archived", got)
	}
	mustContain(t, e.archivePath("b.txt"), "0123456789")
}

func TestRecoverArchivingShouldRemoveTheLeftoverSourceWhenBothCopiesMatch(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	writeAt(t, e.devicePath("c.txt"), "0123456789", now)
	writeAt(t, e.archivePath("c.txt"), "0123456789", now)
	e.plantRow(t, "c.txt", 10, now, catalog.StatusArchiving)

	e.recover(t)

	if got := e.status(t, "c.txt"); got != catalog.StatusArchived {
		t.Errorf("status = %q, want archived", got)
	}
	mustNotExist(t, e.devicePath("c.txt"), "the unlink simply never ran")
}

func TestRecoverArchivingShouldKeepBothCopiesWhenTheDeviceCopyIsNewer(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	writeAt(t, e.archivePath("d.txt"), "0123456789", now)
	writeAt(t, e.devicePath("d.txt"), "9876543210", now.Add(2*time.Hour))
	e.plantRow(t, "d.txt", 10, now, catalog.StatusArchiving)

	e.recover(t)

	if got := e.status(t, "d.txt"); got != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", got)
	}
	mustContain(t, e.devicePath("d.txt"), "9876543210")
	mustContain(t, e.archivePath("d.txt")+supersededSuffix, "0123456789")
	if !hasEvent(e.eventTypes(t), "conflict") {
		t.Error("two versions of one file is exactly what a control plane should be loud about")
	}
}

func TestRecoverArchivingShouldNotPromoteAPartialCopyWhenTheSourceIsStillThere(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	writeAt(t, e.devicePath("e.txt"), "0123456789", now)
	writeAt(t, e.archivePath("e.txt"), "0123", now) // a prefix: an interrupted copy
	e.plantRow(t, "e.txt", 10, now, catalog.StatusArchiving)

	e.recover(t)

	if got := e.status(t, "e.txt"); got != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", got)
	}
	mustNotExist(t, e.archivePath("e.txt"), "a prefix of the source holds no unique bytes")
	mustContain(t, e.devicePath("e.txt"), "0123456789")
}

func TestRecoverArchivingShouldKeepAShortArchiveCopyThatIsNotAPrefixOfTheSource(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	writeAt(t, e.devicePath("f.txt"), "0123456789", now)
	writeAt(t, e.archivePath("f.txt"), "zzzz", now) // a different file, not a partial
	e.plantRow(t, "f.txt", 10, now, catalog.StatusArchiving)

	e.recover(t)

	if got := e.status(t, "f.txt"); got != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", got)
	}
	mustContain(t, e.archivePath("f.txt")+supersededSuffix, "zzzz")
	if !hasEvent(e.eventTypes(t), "conflict") {
		t.Error("want a conflict event")
	}
}

func TestRecoverArchivingShouldKeepAnUnexpectedArchiveCopyWhenItIsTheOnlyOne(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	writeAt(t, e.archivePath("g.txt"), "0123", now)
	e.plantRow(t, "g.txt", 10, now, catalog.StatusArchiving)

	e.recover(t)

	if got := e.status(t, "g.txt"); got != catalog.StatusArchived {
		t.Errorf("status = %q, want archived (invariant 9: keep the bytes and keep them referenced)", got)
	}
	mustContain(t, e.archivePath("g.txt"), "0123")
	if !hasEvent(e.eventTypes(t), "archive_unconfirmed") {
		t.Error("an archive whose size does not match must be reported")
	}
}

// ---------------------------------------------------------------------------
// rows stuck in "restoring"
// ---------------------------------------------------------------------------

func TestRecoverRestoringShouldPreferTheColdSourceWhenTheDeviceCopyIsTruncated(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	writeAt(t, e.archivePath("h.jpg"), "0123456789", now)
	writeAt(t, e.devicePath("h.jpg"), "0123", now) // what a pre-5.2b kill left behind
	e.plantRow(t, "h.jpg", 10, now, catalog.StatusRestoring)

	e.recover(t)

	// An existence test would have committed the truncation here: marked the
	// row synced and pinned, let syncthing propagate 4 bytes over a 10-byte
	// photo on every device, and left the intact cold copy referenced by no
	// row at all.
	if got := e.status(t, "h.jpg"); got != catalog.StatusArchived {
		t.Errorf("status = %q, want archived (the cold copy is authoritative)", got)
	}
	mustNotExist(t, e.devicePath("h.jpg"), "the truncated file must never reach syncthing")
	mustContain(t, e.archivePath("h.jpg"), "0123456789")
}

func TestRecoverRestoringShouldRevertToArchivedWhenNothingLandedOnTheDevice(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	writeAt(t, e.archivePath("i.jpg"), "0123456789", now)
	writeAt(t, e.devicePath("i.jpg")+tmpSuffix, "012", now)
	e.plantRow(t, "i.jpg", 10, now, catalog.StatusRestoring)

	e.recover(t)

	if got := e.status(t, "i.jpg"); got != catalog.StatusArchived {
		t.Errorf("status = %q, want archived", got)
	}
	mustNotExist(t, e.devicePath("i.jpg")+tmpSuffix, "the temp sibling is never valid")
	mustContain(t, e.archivePath("i.jpg"), "0123456789")
}

func TestRecoverRestoringShouldFinishTheRestoreWhenOnlyTheSourceRemovalWasLost(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	writeAt(t, e.archivePath("j.jpg"), "0123456789", now)
	writeAt(t, e.devicePath("j.jpg"), "0123456789", now)
	e.plantRow(t, "j.jpg", 10, now, catalog.StatusRestoring)

	e.recover(t)

	rec, err := e.cat.GetFile("TestFolder", "j.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", rec.Status)
	}
	if !rec.Pinned {
		t.Error("a recovered restore must be pinned, or the next quota tick takes it straight back")
	}
	mustNotExist(t, e.archivePath("j.jpg"), "the cold copy is the leftover here")
}

func TestRecoverRestoringShouldKeepBothCopiesWhenTheDeviceFileIsADifferentFile(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	writeAt(t, e.archivePath("k.jpg"), "0123456789", now)
	writeAt(t, e.devicePath("k.jpg"), "zzzz", now)
	e.plantRow(t, "k.jpg", 10, now, catalog.StatusRestoring)

	e.recover(t)

	if got := e.status(t, "k.jpg"); got != catalog.StatusSynced {
		t.Errorf("status = %q, want synced (adopt what is on the device)", got)
	}
	mustContain(t, e.devicePath("k.jpg"), "zzzz")
	mustContain(t, e.archivePath("k.jpg")+supersededSuffix, "0123456789")
	if !hasEvent(e.eventTypes(t), "conflict") {
		t.Error("want a conflict event")
	}
}

func TestRecoverRestoringShouldCommitWhenOnlyTheCatalogWriteWasLost(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	writeAt(t, e.devicePath("l.jpg"), "0123456789", now)
	e.plantRow(t, "l.jpg", 10, now, catalog.StatusRestoring)

	e.recover(t)

	rec, err := e.cat.GetFile("TestFolder", "l.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", rec.Status)
	}
	if !rec.Pinned || rec.PinSource != catalog.PinSourceRestore {
		t.Errorf("pinned = %v, pin_source = %q, want a restore pin", rec.Pinned, rec.PinSource)
	}
}

func TestRecoverRestoringShouldNotTrustTheSizeWhenOnlyTheDeviceCopyRemains(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	writeAt(t, e.devicePath("m.jpg"), "0123", now)
	e.plantRow(t, "m.jpg", 10, now, catalog.StatusRestoring)

	e.recover(t)

	if got := e.status(t, "m.jpg"); got != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", got)
	}
	if !hasEvent(e.eventTypes(t), "conflict") {
		t.Error("want a conflict event naming both sizes")
	}
}

func TestRecoverRestoringShouldMarkMissingWhenNeitherCopyExists(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	e.plantRow(t, "n.jpg", 10, now, catalog.StatusRestoring)

	e.recover(t)

	if got := e.status(t, "n.jpg"); got != catalog.StatusMissing {
		t.Errorf("status = %q, want missing", got)
	}
	if !hasEvent(e.eventTypes(t), "source_vanished") {
		t.Error("want a source_vanished event")
	}
}

// ---------------------------------------------------------------------------
// the two halves together
// ---------------------------------------------------------------------------

func TestRecoveryShouldResolveAHardKillMidArchiveWithoutLosingAnyBytes(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)

	content := make([]byte, 256<<10)
	for i := range content {
		content[i] = byte(i)
	}
	src := e.devicePath("photo.jpg")
	os.WriteFile(src, content, 0o644)
	os.Chtimes(src, now, now)
	rec := e.plantRow(t, "photo.jpg", int64(len(content)), now, catalog.StatusSynced)

	// The offloader claims the row and starts the copy; then the process dies
	// mid-copy with no deferred cleanup, no revert and no catalog write. That
	// is the state a SIGKILL or a power cut leaves behind. crossDeviceMove is
	// driven directly because a test temp dir is a single filesystem, so a
	// real offload would take the rename fast path and never copy anything.
	if err := e.cat.UpdateFileStatus(rec.ID, catalog.StatusArchiving); err != nil {
		t.Fatal(err)
	}
	withDestWriter(t, func(f *os.File) io.Writer { return &killAfterWriter{f: f, after: 2} })
	func() {
		defer func() { recover() }()
		crossDeviceMove(src, e.archivePath("photo.jpg"), testLogger())
	}()

	if got := e.status(t, "photo.jpg"); got != catalog.StatusArchiving {
		t.Fatalf("status = %q, want archiving: the kill must land inside the transient state", got)
	}

	destWriter = func(f *os.File) io.Writer { return f }
	e.recover(t)

	if got := e.status(t, "photo.jpg"); got != catalog.StatusSynced {
		t.Errorf("status = %q, want synced: nothing complete ever reached cold storage", got)
	}
	got, err := os.ReadFile(src)
	if err != nil || len(got) != len(content) {
		t.Fatalf("the source must be intact: %d bytes, err %v", len(got), err)
	}
	mustNotExist(t, e.archivePath("photo.jpg"), "no partial copy may be left where a restore could find it")
	mustNotExist(t, e.archivePath("photo.jpg")+tmpSuffix, "recovery removes the temp sibling")
}

// The archiving half of the recovery decision table guards its symmetric
// delete with size AND mtime; the restoring half used to accept a size match
// alone -- and then deleted the cold-storage copy, the only copy of the
// archived bytes.
//
// A same-length rewrite landing at the destination during the restore window
// is not exotic: an EXIF-orientation rewrite of a JPEG is exactly
// byte-length-preserving.
func TestRecoveryShouldKeepTheColdCopyWhenTheDeviceFileIsADifferentFileOfTheSameSize(t *testing.T) {
	e := setupRecoveryEnv(t)
	mtime := time.Now().Add(-72 * time.Hour).Truncate(time.Second)

	const archived = "the photograph coldstore put into cold storage in may"
	const rewritten = "a DIFFERENT file of exactly the same byte length!!!!!"
	if len(archived) != len(rewritten) {
		t.Fatalf("fixture is wrong: %d vs %d bytes", len(archived), len(rewritten))
	}

	e.plantRow(t, "photo.jpg", int64(len(archived)), mtime, catalog.StatusRestoring)
	writeAt(t, e.archivePath("photo.jpg"), archived, mtime)
	// Rewritten now, so its mtime has moved forward past the catalogued one.
	writeAt(t, e.devicePath("photo.jpg"), rewritten, time.Now())

	e.recover(t)

	if got := e.status(t, "photo.jpg"); got != catalog.StatusSynced {
		t.Errorf("status = %q, want synced (the device copy is adopted)", got)
	}
	entries, err := os.ReadDir(filepath.Dir(e.archivePath("photo.jpg")))
	if err != nil {
		t.Fatal(err)
	}
	var aside string
	for _, entry := range entries {
		if strings.Contains(entry.Name(), supersededSuffix) {
			aside = entry.Name()
		}
	}
	if aside == "" {
		t.Fatalf("the archived bytes were deleted; cold dir holds %v", entries)
	}
	kept, _ := os.ReadFile(filepath.Join(filepath.Dir(e.archivePath("photo.jpg")), aside))
	if string(kept) != archived {
		t.Errorf("the set-aside copy holds %q, want the archived bytes", kept)
	}
	if !hasEvent(e.eventTypes(t), "conflict") {
		t.Errorf("no conflict event was written; got %v", e.eventTypes(t))
	}
}

// The other side of the same predicate: a restore that really did complete
// preserves size AND mtime (crossDeviceMove chtimes the copy), so it must
// still be recognised and the leftover cold copy removed.
func TestRecoveryShouldRemoveTheColdCopyWhenTheRestoreGenuinelyCompleted(t *testing.T) {
	e := setupRecoveryEnv(t)
	mtime := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	const body = "the photograph, restored intact"

	e.plantRow(t, "photo.jpg", int64(len(body)), mtime, catalog.StatusRestoring)
	writeAt(t, e.archivePath("photo.jpg"), body, mtime)
	writeAt(t, e.devicePath("photo.jpg"), body, mtime)

	e.recover(t)

	if got := e.status(t, "photo.jpg"); got != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", got)
	}
	if _, err := os.Lstat(e.archivePath("photo.jpg")); !os.IsNotExist(err) {
		t.Errorf("the leftover cold copy was not removed: %v", err)
	}
	rec, _ := e.cat.GetFile("TestFolder", "photo.jpg")
	if !rec.Pinned {
		t.Error("a recovered restore must be pinned (D2)")
	}
}

// Boot with the syncthing volume not yet mounted. The device copy is really
// there, but nothing under an unmounted root is visible, so every branch that
// reads "the source is gone" is reading noise. Promoting the row to archived
// here strands a file that is still on the device: the archive and the device
// both hold it, and the next scan reports a conflict the owner has to settle by
// hand. Leaving it in archiving costs one more recovery pass and nothing else.
func TestRecoverArchivingShouldNotPromoteToArchivedWhenTheFolderRootIsUnmounted(t *testing.T) {
	e := setupRecoveryEnv(t)
	now := time.Now().Truncate(time.Second)
	writeAt(t, e.archivePath("b.txt"), "0123456789", now)
	e.plantRow(t, "b.txt", 10, now, catalog.StatusArchiving)

	// Present the folder the way an unmounted mountpoint does: the directory
	// exists and is readable, but the marker and the files are absent.
	if err := os.Remove(filepath.Join(e.syncDir, ".stfolder")); err != nil {
		t.Fatal(err)
	}

	e.recover(t)

	if got := e.status(t, "b.txt"); got != catalog.StatusArchiving {
		t.Errorf("status = %q, want archiving: an unmounted root is not evidence the source was removed", got)
	}
	mustContain(t, e.archivePath("b.txt"), "0123456789")
}
