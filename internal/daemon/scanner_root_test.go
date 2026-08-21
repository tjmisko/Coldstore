package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

// seedScannedFolder catalogs two synced files under a real folder root and
// returns the catalog plus the scanner and folder config pointing at it.
func seedScannedFolder(t *testing.T) (*catalog.Catalog, *Scanner, catalog.FolderConfigRecord, string) {
	t.Helper()

	root := filepath.Join(t.TempDir(), "photos")
	if err := os.MkdirAll(filepath.Join(root, ".stfolder"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one.jpg", "two.jpg"} {
		os.WriteFile(filepath.Join(root, name), make([]byte, 100), 0o644)
	}

	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })

	folder := declareFolder(t, cat, "Photos", root)
	scanner := NewScanner(&config.Config{}, cat, testLogger())

	if err := scanner.ScanFolder(folder); err != nil {
		t.Fatal(err)
	}
	return cat, scanner, folder, root
}

func statusesOf(t *testing.T, cat *catalog.Catalog, folder string, names ...string) map[string]catalog.FileStatus {
	t.Helper()
	out := make(map[string]catalog.FileStatus, len(names))
	for _, n := range names {
		f, err := cat.GetFile(folder, n)
		if err != nil {
			t.Fatalf("getting %s: %v", n, err)
		}
		out[n] = f.Status
	}
	return out
}

func TestScanFolderShouldAbortWithoutMarkingAnythingMissingWhenRootIsAbsent(t *testing.T) {
	cat, scanner, folder, root := seedScannedFolder(t)

	// Simulate an unmounted or renamed syncthing folder.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	err := scanner.ScanFolder(folder)
	if err == nil {
		t.Fatal("scanning a folder whose root is gone should return an error")
	}

	for name, status := range statusesOf(t, cat, "Photos", "one.jpg", "two.jpg") {
		if status != catalog.StatusSynced {
			t.Errorf("%s status = %q, want synced: an unavailable root must not mark the whole folder missing",
				name, status)
		}
	}
}

func TestScanFolderShouldAbortWhenRootIsAFileRatherThanADirectory(t *testing.T) {
	dir := newSyncthingFolder(t, t.TempDir())
	notADir := filepath.Join(dir, "photos")
	os.WriteFile(notADir, []byte("this is not a folder"), 0o644)

	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	folder := declareFolder(t, cat, "Photos", notADir)
	scanner := NewScanner(&config.Config{}, cat, testLogger())

	if err := scanner.ScanFolder(folder); err == nil {
		t.Fatal("scanning a root that is not a directory should return an error")
	}
}

func TestScanFolderShouldAbortWhenRootIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny access")
	}

	cat, scanner, folder, root := seedScannedFolder(t)

	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(root, 0o755) })

	if err := scanner.ScanFolder(folder); err == nil {
		t.Fatal("scanning an unreadable root should return an error")
	}

	os.Chmod(root, 0o755)
	for name, status := range statusesOf(t, cat, "Photos", "one.jpg", "two.jpg") {
		if status != catalog.StatusSynced {
			t.Errorf("%s status = %q, want synced: an unreadable root must not mark the folder missing",
				name, status)
		}
	}
}

func TestScanAllShouldKeepScanningOtherFoldersWhenOneRootIsAbsent(t *testing.T) {
	goodRoot := filepath.Join(t.TempDir(), "good")
	os.MkdirAll(filepath.Join(goodRoot, ".stfolder"), 0o755)
	os.WriteFile(filepath.Join(goodRoot, "kept.jpg"), make([]byte, 50), 0o644)

	missingRoot := filepath.Join(t.TempDir(), "never-created")

	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	declareFolder(t, cat, "Absent", missingRoot)
	declareFolder(t, cat, "Good", goodRoot)
	scanner := NewScanner(&config.Config{}, cat, testLogger())

	if err := scanner.ScanAll(); err != nil {
		t.Fatalf("ScanAll should tolerate one unavailable folder: %v", err)
	}

	f, err := cat.GetFile("Good", "kept.jpg")
	if err != nil {
		t.Fatalf("the healthy folder should still have been scanned: %v", err)
	}
	if f.Status != catalog.StatusSynced {
		t.Errorf("kept.jpg status = %q, want synced", f.Status)
	}
}

func TestScanFolderShouldNotMarkFilesMissingWhenASubdirectoryIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny access")
	}

	root := filepath.Join(t.TempDir(), "photos")
	sub := filepath.Join(root, "2024")
	os.MkdirAll(sub, 0o755)
	newSyncthingFolder(t, root)
	os.WriteFile(filepath.Join(root, "top.jpg"), make([]byte, 100), 0o644)
	os.WriteFile(filepath.Join(sub, "inner.jpg"), make([]byte, 100), 0o644)

	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	folder := declareFolder(t, cat, "Photos", root)
	scanner := NewScanner(&config.Config{}, cat, testLogger())

	if err := scanner.ScanFolder(folder); err != nil {
		t.Fatal(err)
	}

	// Deny access to the subtree, then rescan.
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })

	if err := scanner.ScanFolder(folder); err != nil {
		t.Fatalf("a partially unreadable tree should not fail the scan outright: %v", err)
	}

	os.Chmod(sub, 0o755)
	f, err := cat.GetFile("Photos", filepath.Join("2024", "inner.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != catalog.StatusSynced {
		t.Errorf("inner.jpg status = %q, want synced: a permission error is not proof of deletion", f.Status)
	}
}

func TestScanFolderShouldMarkOnlyDeletedFilesMissingWhenRescannedTwiceInTheSameSecond(t *testing.T) {
	root := filepath.Join(t.TempDir(), "photos")
	os.MkdirAll(filepath.Join(root, ".stfolder"), 0o755)
	os.WriteFile(filepath.Join(root, "stays.jpg"), make([]byte, 100), 0o644)
	deleted := filepath.Join(root, "goes.jpg")
	os.WriteFile(deleted, make([]byte, 100), 0o644)

	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	folder := declareFolder(t, cat, "Photos", root)
	scanner := NewScanner(&config.Config{}, cat, testLogger())

	start := time.Now()
	if err := scanner.ScanFolder(folder); err != nil {
		t.Fatal(err)
	}
	os.Remove(deleted)
	if err := scanner.ScanFolder(folder); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Logf("scans took %s, so this run did not exercise the same-second case", elapsed)
	}

	got := statusesOf(t, cat, "Photos", "stays.jpg", "goes.jpg")
	if got["goes.jpg"] != catalog.StatusMissing {
		t.Errorf("goes.jpg status = %q, want missing", got["goes.jpg"])
	}
	if got["stays.jpg"] != catalog.StatusSynced {
		t.Errorf("stays.jpg status = %q, want synced", got["stays.jpg"])
	}
}

// An unmounted mountpoint is the case a bare os.Stat cannot see: the directory
// exists, it is readable, and it is empty. Reading that as "every file was
// deleted" marks the whole folder missing, which breaks downloads and restores
// and drops the folder's bytes out of the enforcement budget until someone
// notices. Syncthing's .stfolder marker is what tells the two apart, so the
// scanner has to consult it before it trusts an empty walk (design 3.6).
func TestScanFolderShouldNotMarkFilesMissingWhenTheRootIsAnUnmountedMountpoint(t *testing.T) {
	cat, scanner, folder, root := seedScannedFolder(t)

	// Wipe the root the way an unmounted volume presents: the directory is
	// still there and still readable, but the marker and the files are not.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := scanner.ScanFolder(folder); err == nil {
		t.Fatal("scanning an unmounted root should return an error, not report an empty folder")
	}

	for name, status := range statusesOf(t, cat, "Photos", "one.jpg", "two.jpg") {
		if status != catalog.StatusSynced {
			t.Errorf("%s status = %q, want synced: an empty directory with no .stfolder is an unmounted volume, not a deletion",
				name, status)
		}
	}
}
