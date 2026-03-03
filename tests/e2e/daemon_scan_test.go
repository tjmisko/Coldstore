//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func TestScanDetectsNewFiles(t *testing.T) {
	env := newTestEnv(t)

	// Create files in sync dir
	genFiles(t, env.syncDir, 5, ".jpg", 1000)

	if err := env.scanner.ScanAll(); err != nil {
		t.Fatal(err)
	}

	files, err := env.cat.ListFiles(catalog.FileQuery{Folder: "TestFolder", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 5 {
		t.Errorf("scanned %d files, want 5", len(files))
	}
	for _, f := range files {
		if f.Status != catalog.StatusSynced {
			t.Errorf("file %s status = %q, want synced", f.Filename, f.Status)
		}
	}
}

func TestScanMarksMissingOnRescan(t *testing.T) {
	env := newTestEnv(t)

	paths := genFiles(t, env.syncDir, 3, ".txt", 500)
	env.scanner.ScanAll()

	// Remove one file
	os.Remove(paths[0])

	// Rescan
	env.scanner.ScanAll()

	files, _ := env.cat.ListFiles(catalog.FileQuery{Folder: "TestFolder", Status: catalog.StatusMissing, Limit: 100})
	if len(files) != 1 {
		t.Errorf("missing files = %d, want 1", len(files))
	}

	synced, _ := env.cat.ListFiles(catalog.FileQuery{Folder: "TestFolder", Status: catalog.StatusSynced, Limit: 100})
	if len(synced) != 2 {
		t.Errorf("synced files = %d, want 2", len(synced))
	}
}

func TestScanSkipsHiddenDirs(t *testing.T) {
	env := newTestEnv(t)

	// Create files in a hidden dir
	hiddenDir := filepath.Join(env.syncDir, ".stversions")
	os.MkdirAll(hiddenDir, 0755)
	os.WriteFile(filepath.Join(hiddenDir, "old.txt"), make([]byte, 100), 0644)

	// Create a normal file
	os.WriteFile(filepath.Join(env.syncDir, "normal.txt"), make([]byte, 100), 0644)

	env.scanner.ScanAll()

	files, _ := env.cat.ListFiles(catalog.FileQuery{Folder: "TestFolder", Limit: 100})
	if len(files) != 1 {
		t.Errorf("scanned %d files, want 1 (hidden dir should be skipped)", len(files))
	}
	if len(files) > 0 && files[0].Filename != "normal.txt" {
		t.Errorf("expected normal.txt, got %s", files[0].Filename)
	}
}

func TestScanRecordsAtime(t *testing.T) {
	env := newTestEnv(t)

	p := filepath.Join(env.syncDir, "accessed.txt")
	os.WriteFile(p, make([]byte, 100), 0644)

	// Set a specific mtime in the past
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(p, past, past)

	env.scanner.ScanAll()

	file, err := env.cat.GetFile("TestFolder", "accessed.txt")
	if err != nil {
		t.Fatal(err)
	}

	// atime should be recorded (may equal mtime depending on mount options,
	// but it should be a non-zero time)
	if file.FileAtime.IsZero() {
		t.Error("file atime should be recorded")
	}
}
