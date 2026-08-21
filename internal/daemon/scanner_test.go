package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

func TestIsPersistent(t *testing.T) {
	tests := []struct {
		relPath  string
		patterns []string
		want     bool
	}{
		{"Starred/photo.jpg", []string{"Starred/*"}, true},
		// Was false, and asserted the defect design section 4.1a describes: a
		// trailing "/*" now protects the subtree, so a starred album filed
		// into year subfolders is no longer archived out from under the owner
		// one subfolder at a time.
		{"Starred/sub/photo.jpg", []string{"Starred/*"}, true},
		{"Other/photo.jpg", []string{"Starred/*"}, false},
		{"photo.jpg", []string{"Starred/*"}, false},
		{"Favorites/book.pdf", []string{"Favorites/*", "Starred/*"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.relPath, func(t *testing.T) {
			got := IsPersistent(tt.relPath, tt.patterns)
			if got != tt.want {
				t.Errorf("IsPersistent(%q, %v) = %v, want %v", tt.relPath, tt.patterns, got, tt.want)
			}
		})
	}
}

func TestScanFolder(t *testing.T) {
	// Create a temp directory structure
	dir := newSyncthingFolder(t, t.TempDir())
	subdir := filepath.Join(dir, "2024", "01")
	os.MkdirAll(subdir, 0755)

	// Create test files
	os.WriteFile(filepath.Join(subdir, "photo1.jpg"), make([]byte, 1000), 0644)
	os.WriteFile(filepath.Join(subdir, "photo2.png"), make([]byte, 2000), 0644)
	os.WriteFile(filepath.Join(dir, "readme.txt"), make([]byte, 500), 0644)

	// Create a hidden dir that should be skipped
	hiddenDir := filepath.Join(dir, ".stversions")
	os.MkdirAll(hiddenDir, 0755)
	os.WriteFile(filepath.Join(hiddenDir, "old.jpg"), make([]byte, 100), 0644)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	folder := declareFolder(t, cat, "TestFolder", dir)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	scanner := NewScanner(&config.Config{}, cat, logger)

	if err := scanner.ScanFolder(folder); err != nil {
		t.Fatal(err)
	}

	// Should have 3 files (not the one in .stversions)
	count, err := cat.FileCount("")
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("file count = %d, want 3", count)
	}

	// Verify a specific file
	f, err := cat.GetFile("TestFolder", filepath.Join("2024", "01", "photo1.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if f.SizeBytes != 1000 {
		t.Errorf("photo1.jpg size = %d, want 1000", f.SizeBytes)
	}
	if f.Extension != ".jpg" {
		t.Errorf("photo1.jpg extension = %q, want .jpg", f.Extension)
	}
	if f.Status != catalog.StatusSynced {
		t.Errorf("photo1.jpg status = %q, want synced", f.Status)
	}
}

func TestScanDetectsMissingFiles(t *testing.T) {
	dir := newSyncthingFolder(t, t.TempDir())
	os.WriteFile(filepath.Join(dir, "exists.txt"), make([]byte, 100), 0644)
	os.WriteFile(filepath.Join(dir, "willdelete.txt"), make([]byte, 100), 0644)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	folder := declareFolder(t, cat, "Test", dir)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	scanner := NewScanner(&config.Config{}, cat, logger)

	// First scan
	scanner.ScanFolder(folder)

	// Delete a file
	os.Remove(filepath.Join(dir, "willdelete.txt"))

	// Second scan
	scanner.ScanFolder(folder)

	// The deleted file should be marked missing
	f, err := cat.GetFile("Test", "willdelete.txt")
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != catalog.StatusMissing {
		t.Errorf("deleted file status = %q, want missing", f.Status)
	}

	// The existing file should still be synced
	f, _ = cat.GetFile("Test", "exists.txt")
	if f.Status != catalog.StatusSynced {
		t.Errorf("existing file status = %q, want synced", f.Status)
	}
}

// The pattern semantics of design section 4.1a. Rule 1 is the one that matters
// for data safety: under plain filepath.Match, "Albums/Starred/*" protected
// only the loose files directly inside that directory, so a starred album
// organised into year subfolders was archived out from under the owner one
// subfolder at a time.
func TestIsPersistentShouldProtectAWholeSubtreeWhenThePatternEndsInASlash(t *testing.T) {
	cases := []struct {
		name     string
		relPath  string
		patterns []string
		want     bool
	}{
		{"trailing slash protects nested files", "Albums/Starred/2019/IMG_1.jpg", []string{"Albums/Starred/"}, true},
		{"trailing slash-star protects nested files", "Albums/Starred/2019/IMG_1.jpg", []string{"Albums/Starred/*"}, true},
		{"trailing slash protects direct children", "Albums/Starred/IMG_1.jpg", []string{"Albums/Starred/"}, true},
		{"subtree pattern does not protect a sibling", "Albums/Other/IMG_1.jpg", []string{"Albums/Starred/"}, false},
		{"subtree pattern does not protect a prefix-sharing sibling", "Albums/StarredOld/IMG.jpg", []string{"Albums/Starred/"}, false},
		{"full-path glob matches one segment only", "Albums/a.raw", []string{"Albums/*.raw"}, true},
		{"full-path glob does not cross a separator", "Albums/2019/a.raw", []string{"Albums/*.raw"}, false},
		{"a pattern without a separator matches the base name anywhere", "Albums/2019/a.raw", []string{"*.raw"}, true},
		{"a pattern without a separator still matches at the root", "a.raw", []string{"*.raw"}, true},
		{"an exact relative path matches itself", "Favorites/one.jpg", []string{"Favorites/one.jpg"}, true},
		{"a malformed pattern matches nothing rather than everything", "anything.jpg", []string{"[unclosed"}, false},
		{"no patterns protect nothing", "anything.jpg", nil, false},
		{"one matching pattern among several is enough", "Favorites/x.jpg", []string{"Nope/*", "Favorites/"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPersistent(tc.relPath, tc.patterns); got != tc.want {
				t.Errorf("IsPersistent(%q, %v) = %v, want %v", tc.relPath, tc.patterns, got, tc.want)
			}
		})
	}
}
