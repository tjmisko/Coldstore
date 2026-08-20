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
		{"Starred/sub/photo.jpg", []string{"Starred/*"}, false}, // filepath.Match doesn't recurse
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
