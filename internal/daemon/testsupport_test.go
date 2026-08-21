package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

// newSyncthingFolder creates dir the way Syncthing would, marker and all.
// FolderReady refuses to move anything into or out of a directory with no
// .stfolder, because that is how an unmounted volume is told apart from an
// empty one (design section 3.6), so every fixture that stands in for a
// syncthing folder has to go through here.
func newSyncthingFolder(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".stfolder"), 0o755); err != nil {
		t.Fatalf("creating syncthing folder %s: %v", dir, err)
	}
	return dir
}

// declarePolicy folds cfg's folders into folder_configs exactly the way main
// does at startup. Enforcement and scanning read policy from the database now
// (D1), so a test that tunes cfg -- a budget, a grace period, a persistent
// glob -- must declare it before running a tick, or the tick sees the seeded
// values instead.
func declarePolicy(t *testing.T, cfg *config.Config, cat *catalog.Catalog) {
	t.Helper()
	if err := ReconcileFolders(cfg, cat, testLogger()); err != nil {
		t.Fatalf("reconciling folders: %v", err)
	}
}

// plentyOfSpace is the free-space probe a test uses when the test is not about
// disk space. The real statfs would report whatever the developer's machine
// happens to have, which is not something a test should depend on.
func plentyOfSpace(string) (float64, error) { return 90.0, nil }

// newTestDaemon builds a Daemon over its own Mover, the way main does.
func newTestDaemon(t *testing.T, cfg *config.Config, cat *catalog.Catalog, logger *slog.Logger) *Daemon {
	t.Helper()
	mover := NewMover(cfg, cat, logger)
	mover.SetSpaceCheck(plentyOfSpace)
	return New(cfg, cat, mover, logger)
}

// declareFolder registers one folder directly, for tests that care about the
// scanner rather than about policy and have no config.Config to speak of.
func declareFolder(t *testing.T, cat *catalog.Catalog, label, path string) catalog.FolderConfigRecord {
	t.Helper()
	if _, err := cat.ReconcileFolderFromTOML(label, "", path, 1<<30, 30); err != nil {
		t.Fatalf("declaring folder %q: %v", label, err)
	}
	fc, err := cat.FolderConfigByLabel(label)
	if err != nil {
		t.Fatalf("reading folder %q back: %v", label, err)
	}
	return *fc
}
