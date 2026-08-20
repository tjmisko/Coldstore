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
func seedScannedFolder(t *testing.T) (*catalog.Catalog, *Scanner, config.FolderConfig, string) {
	t.Helper()

	root := filepath.Join(t.TempDir(), "photos")
	if err := os.MkdirAll(root, 0o755); err != nil {
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

	folder := config.FolderConfig{Label: "Photos", Path: root}
	cfg := &config.Config{Folders: []config.FolderConfig{folder}}
	scanner := NewScanner(cfg, cat, testLogger())

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
	dir := t.TempDir()
	notADir := filepath.Join(dir, "photos")
	os.WriteFile(notADir, []byte("this is not a folder"), 0o644)

	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	folder := config.FolderConfig{Label: "Photos", Path: notADir}
	cfg := &config.Config{Folders: []config.FolderConfig{folder}}
	scanner := NewScanner(cfg, cat, testLogger())

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
	os.MkdirAll(goodRoot, 0o755)
	os.WriteFile(filepath.Join(goodRoot, "kept.jpg"), make([]byte, 50), 0o644)

	missingRoot := filepath.Join(t.TempDir(), "never-created")

	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	cfg := &config.Config{Folders: []config.FolderConfig{
		{Label: "Absent", Path: missingRoot},
		{Label: "Good", Path: goodRoot},
	}}
	scanner := NewScanner(cfg, cat, testLogger())

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
	os.WriteFile(filepath.Join(root, "top.jpg"), make([]byte, 100), 0o644)
	os.WriteFile(filepath.Join(sub, "inner.jpg"), make([]byte, 100), 0o644)

	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	folder := config.FolderConfig{Label: "Photos", Path: root}
	cfg := &config.Config{Folders: []config.FolderConfig{folder}}
	scanner := NewScanner(cfg, cat, testLogger())

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
	os.MkdirAll(root, 0o755)
	os.WriteFile(filepath.Join(root, "stays.jpg"), make([]byte, 100), 0o644)
	deleted := filepath.Join(root, "goes.jpg")
	os.WriteFile(deleted, make([]byte, 100), 0o644)

	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	folder := config.FolderConfig{Label: "Photos", Path: root}
	cfg := &config.Config{Folders: []config.FolderConfig{folder}}
	scanner := NewScanner(cfg, cat, testLogger())

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
