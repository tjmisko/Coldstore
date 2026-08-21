package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

// seedConflict produces the exact shape section 4.1b describes: an archived
// row, its bytes in cold storage, and a DIFFERENT file back at the syncthing
// path -- a photo recovered from Recently Deleted, or a laptop reconnecting
// with an offline edit. It returns the row and the two paths.
func seedConflict(t *testing.T, m *Mover, cat *catalog.Catalog, syncDir, relPath string,
	deviceBytes []byte) (rec *catalog.FileRecord, devicePath, coldPath string) {

	t.Helper()
	file := seedSyncedFile(t, cat, syncDir, relPath, 128)
	if _, err := m.ArchiveByID(file.ID); err != nil {
		t.Fatalf("archiving %s: %v", relPath, err)
	}

	devicePath = filepath.Join(syncDir, relPath)
	if err := os.WriteFile(devicePath, deviceBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	coldPath = m.restorer.ColdPath("TestFolder", relPath)
	if _, err := os.Stat(coldPath); err != nil {
		t.Fatalf("the archived copy should be in cold storage: %v", err)
	}

	rec, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	return rec, devicePath, coldPath
}

// The documented keep=device contract: the owner keeps what is in the folder
// and the cold copy is deleted. Nothing is left behind -- not the cold copy,
// and not the .coldstore-superseded sibling the resolution passes through on
// its way there.
func TestResolveShouldDeleteTheColdCopyWhenTheOwnerKeepsTheDeviceFile(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	m := newTestMover(t, cfg, cat)
	deviceBytes := []byte("the version the laptop had all along")
	rec, _, coldPath := seedConflict(t, m, cat, syncDir, "shared.jpg", deviceBytes)

	after, err := m.ResolveConflict(rec.ID, ResolveKeepDevice)
	if err != nil {
		t.Fatalf("resolving in the device's favour: %v", err)
	}

	if after.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", after.Status)
	}
	if after.SizeBytes != int64(len(deviceBytes)) {
		t.Errorf("size = %d, want %d: the row must describe the file on disk",
			after.SizeBytes, len(deviceBytes))
	}
	if _, err := os.Lstat(coldPath); !os.IsNotExist(err) {
		t.Errorf("the cold copy must be gone, got %v", err)
	}
	assertNoSupersededFiles(t, coldDir)
}

// Defect 4. keep=device deleted the cold copy BEFORE the row stopped pointing
// at it, so an adopt that lost its race destroyed the bytes an archived row
// still referenced: a file that can never be restored, that every subsequent
// walk re-reports as the same conflict, and that no endpoint can repair.
//
// The adopt seam stands in for the only thing that can produce that outcome --
// the row leaving 'archived' between the read at the top of ResolveConflict
// and the compare-and-swap at the bottom.
func TestResolveShouldLeaveTheColdCopyInPlaceWhenAdoptingTheDeviceFileFails(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	m := newTestMover(t, cfg, cat)
	rec, _, coldPath := seedConflict(t, m, cat, syncDir, "raced.jpg", []byte("device bytes"))

	archived, err := os.ReadFile(coldPath)
	if err != nil {
		t.Fatal(err)
	}

	// The row left 'archived' while we were resolving, so the swap matches
	// nothing.
	m.adoptRow = func(int64, int64, time.Time, time.Time) (bool, error) { return false, nil }

	if _, err := m.ResolveConflict(rec.ID, ResolveKeepDevice); !errors.Is(err, ErrFileStateChanged) {
		t.Fatalf("err = %v, want ErrFileStateChanged", err)
	}

	// The resolution did not happen, so the archived bytes must still be where
	// the still-archived row says they are.
	got, err := os.ReadFile(coldPath)
	if err != nil {
		t.Fatalf("the cold copy was destroyed by a resolution that failed: %v", err)
	}
	if string(got) != string(archived) {
		t.Errorf("cold copy = %q, want the original %q", got, archived)
	}
	assertNoSupersededFiles(t, coldDir)

	after, err := cat.GetFileByID(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != catalog.StatusArchived {
		t.Errorf("status = %q, want archived: the row must be exactly as it was", after.Status)
	}
}

// The same window, reached through a database error rather than a lost race.
func TestResolveShouldLeaveTheColdCopyInPlaceWhenTheCatalogWriteErrors(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	m := newTestMover(t, cfg, cat)
	rec, _, coldPath := seedConflict(t, m, cat, syncDir, "broken.jpg", []byte("device bytes"))

	boom := errors.New("database is locked")
	m.adoptRow = func(int64, int64, time.Time, time.Time) (bool, error) { return false, boom }

	if _, err := m.ResolveConflict(rec.ID, ResolveKeepDevice); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
	if _, err := os.Stat(coldPath); err != nil {
		t.Errorf("the cold copy was destroyed by a resolution that failed: %v", err)
	}
	assertNoSupersededFiles(t, coldDir)
}

// A resolution the owner can still retry: after the failure above, the same
// call with a working catalog must succeed and reach the documented end state.
func TestResolveShouldStillSucceedWhenRetriedAfterAFailedAttempt(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	m := newTestMover(t, cfg, cat)
	rec, _, coldPath := seedConflict(t, m, cat, syncDir, "retried.jpg", []byte("device bytes"))

	m.adoptRow = func(int64, int64, time.Time, time.Time) (bool, error) {
		return false, errors.New("database is locked")
	}
	if _, err := m.ResolveConflict(rec.ID, ResolveKeepDevice); err == nil {
		t.Fatal("the first attempt should have failed")
	}

	m.adoptRow = cat.AdoptDeviceCopy
	after, err := m.ResolveConflict(rec.ID, ResolveKeepDevice)
	if err != nil {
		t.Fatalf("the retry must succeed: %v", err)
	}
	if after.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", after.Status)
	}
	if _, err := os.Lstat(coldPath); !os.IsNotExist(err) {
		t.Errorf("the cold copy must be gone after the successful retry, got %v", err)
	}
	assertNoSupersededFiles(t, coldDir)
}

// assertNoSupersededFiles fails if any .coldstore-superseded file is left under
// root. keep=device passes the cold copy through that name; it must not still
// be wearing it when the call returns, in either direction.
func assertNoSupersededFiles(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.Contains(filepath.Base(path), supersededSuffix) {
			t.Errorf("a %s file was left behind at %s", supersededSuffix, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}
