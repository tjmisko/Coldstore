//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tjmisko/coldstore/internal/config"
)

func TestConfigMissingColdStoragePath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "test.toml")
	os.WriteFile(cfgPath, []byte(`
[storage]
cold_storage_path = ""

[[folders]]
label = "Test"
path = "/tmp/test"
budget = "10GB"
`), 0644)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// Default should be applied
	if cfg.Storage.ColdStoragePath != "/srv/coldstore" {
		t.Errorf("cold_storage_path = %q, want /srv/coldstore (default)", cfg.Storage.ColdStoragePath)
	}
}

func TestConfigInvalidBudgetUnit(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "test.toml")
	os.WriteFile(cfgPath, []byte(`
[storage]
cold_storage_path = "/tmp/cold"

[[folders]]
label = "Bad"
path = "/tmp/bad"
budget = "10XB"
`), 0644)

	_, err := config.Load(cfgPath)
	if err == nil {
		t.Error("expected error for invalid budget unit")
	}
}

func TestConfigMultiFolderBudgets(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "test.toml")
	os.WriteFile(cfgPath, []byte(`
[storage]
cold_storage_path = "/tmp/cold"

[[folders]]
label = "Photos"
path = "/tmp/photos"
budget = "20GB"

[[folders]]
label = "Videos"
path = "/tmp/videos"
budget = "100GB"
`), 0644)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Folders) != 2 {
		t.Fatalf("expected 2 folders, got %d", len(cfg.Folders))
	}
	if cfg.Folders[0].BudgetBytes != 20_000_000_000 {
		t.Errorf("Photos budget = %d, want 20000000000", cfg.Folders[0].BudgetBytes)
	}
	if cfg.Folders[1].BudgetBytes != 100_000_000_000 {
		t.Errorf("Videos budget = %d, want 100000000000", cfg.Folders[1].BudgetBytes)
	}
}

func TestConfigBudgetZero(t *testing.T) {
	_, err := config.ParseBudget("0B")
	if err != nil {
		t.Fatal(err)
	}
}

func TestConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "test.toml")
	os.WriteFile(cfgPath, []byte(`
[[folders]]
label = "Test"
path = "/tmp/test"
budget = "1GB"
`), 0644)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Storage.ColdStoragePath != "/srv/coldstore" {
		t.Errorf("cold_storage_path default = %q", cfg.Storage.ColdStoragePath)
	}
	if cfg.Daemon.ScanIntervalMinutes != 15 {
		t.Errorf("scan_interval default = %d", cfg.Daemon.ScanIntervalMinutes)
	}
	if cfg.Daemon.CatalogSyncIntervalMinutes != 30 {
		t.Errorf("catalog_sync_interval default = %d", cfg.Daemon.CatalogSyncIntervalMinutes)
	}
	if cfg.Daemon.GracePeriodMinutes != 30 {
		t.Errorf("grace_period default = %d", cfg.Daemon.GracePeriodMinutes)
	}
	if cfg.Daemon.ThumbnailWorkers != 2 {
		t.Errorf("thumbnail_workers default = %d", cfg.Daemon.ThumbnailWorkers)
	}
	if cfg.Server.ListenAddr != "0.0.0.0:8090" {
		t.Errorf("listen_addr default = %q", cfg.Server.ListenAddr)
	}
}
