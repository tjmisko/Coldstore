package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

func newScanEnv(t *testing.T) (string, *catalog.Catalog, *Scanner) {
	t.Helper()

	syncDir := newSyncthingFolder(t, filepath.Join(t.TempDir(), "sync"))
	coldDir := filepath.Join(t.TempDir(), "cold")
	if err := os.MkdirAll(filepath.Join(coldDir, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	cat, _ := openCatalog(t)
	cfg := &config.Config{
		Storage: config.StorageConfig{ColdStoragePath: coldDir},
		Folders: []config.FolderConfig{{Label: "TestFolder", Path: syncDir, BudgetBytes: 50000}},
	}
	declarePolicy(t, cfg, cat)
	return syncDir, cat, NewScanner(cfg, cat, testLogger())
}

// Two overlapping walks of the same tree interleave their generation stamps,
// and the loser's final sweep marks the winner's fresh rows missing. The second
// caller is told to come back rather than being allowed to do that.
func TestScanShouldReportInProgressWhenAnotherScanIsAlreadyRunning(t *testing.T) {
	syncDir, cat, scanner := newScanEnv(t)
	if err := os.WriteFile(filepath.Join(syncDir, "a.jpg"), make([]byte, 10), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scanner.ScanAll(); err != nil {
		t.Fatal(err)
	}

	scanner.mu.Lock()
	defer scanner.mu.Unlock()

	if err := scanner.ScanAll(); !errors.Is(err, ErrScanInProgress) {
		t.Errorf("ScanAll error = %v, want ErrScanInProgress", err)
	}

	fc, err := cat.FolderConfigByLabel("TestFolder")
	if err != nil {
		t.Fatal(err)
	}
	if err := scanner.ScanFolder(*fc); !errors.Is(err, ErrScanInProgress) {
		t.Errorf("ScanFolder error = %v, want ErrScanInProgress", err)
	}

	// The refused scan must not have swept anything on its way out.
	rec, err := cat.GetFile("TestFolder", "a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced: a refused scan marked a file missing", rec.Status)
	}
}

// ScanAll holds the lock for the whole pass and calls the per-folder body
// directly. A TryLock in the inner call would make every folder after the first
// return ErrScanInProgress against itself.
func TestScanAllShouldWalkEveryFolderWhenSeveralAreDeclared(t *testing.T) {
	root := t.TempDir()
	coldDir := filepath.Join(t.TempDir(), "cold")
	if err := os.MkdirAll(filepath.Join(coldDir, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	cat, _ := openCatalog(t)

	cfg := &config.Config{
		Storage: config.StorageConfig{ColdStoragePath: coldDir},
	}
	for _, label := range []string{"One", "Two", "Three"} {
		dir := newSyncthingFolder(t, filepath.Join(root, label))
		if err := os.WriteFile(filepath.Join(dir, "a.jpg"), make([]byte, 10), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg.Folders = append(cfg.Folders, config.FolderConfig{
			Label: label, Path: dir, BudgetBytes: 50000,
		})
	}
	declarePolicy(t, cfg, cat)

	scanner := NewScanner(cfg, cat, testLogger())
	if err := scanner.ScanAll(); err != nil {
		t.Fatal(err)
	}

	for _, label := range []string{"One", "Two", "Three"} {
		if _, err := cat.GetFile(label, "a.jpg"); err != nil {
			t.Errorf("folder %q was not scanned: %v", label, err)
		}
	}
}

func TestScanShouldRunAgainWhenThePreviousScanHasFinished(t *testing.T) {
	syncDir, cat, scanner := newScanEnv(t)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := scanner.ScanAllContext(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()

	if err := os.WriteFile(filepath.Join(syncDir, "later.jpg"), make([]byte, 10), 0o644); err != nil {
		t.Fatal(err)
	}
	// A different generation stamp: the scanner uses nanoseconds so two scans
	// inside one wall-clock second cannot share one.
	time.Sleep(time.Millisecond)
	if err := scanner.ScanAll(); err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	if _, err := cat.GetFile("TestFolder", "later.jpg"); err != nil {
		t.Errorf("the second scan did not run: %v", err)
	}
}
