package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseBudget(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"20GB", 20_000_000_000, false},
		{"500MB", 500_000_000, false},
		{"1TB", 1_000_000_000_000, false},
		{"100KB", 100_000, false},
		{"1024B", 1024, false},
		{"1.5GB", 1_500_000_000, false},
		{"  20GB  ", 20_000_000_000, false},
		{"20gb", 20_000_000_000, false},
		{"", 0, true},
		{"20XB", 0, true},
		{"abcGB", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseBudget(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseBudget(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseBudget(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	toml := `
[storage]
cold_storage_path = "/tmp/coldtest"

[daemon]
scan_interval_minutes = 5
grace_period_minutes = 10

[server]
listen_addr = "127.0.0.1:9090"

[[folders]]
label = "Photos"
syncthing_id = "abc-123"
path = "/tmp/photos"
budget = "10GB"
persistent = ["Starred/*"]

[[folders]]
label = "Docs"
path = "/tmp/docs"
budget = "5GB"
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "coldstore.toml")
	if err := os.WriteFile(cfgPath, []byte(toml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Storage.ColdStoragePath != "/tmp/coldtest" {
		t.Errorf("ColdStoragePath = %q, want /tmp/coldtest", cfg.Storage.ColdStoragePath)
	}
	if cfg.Daemon.ScanIntervalMinutes != 5 {
		t.Errorf("ScanIntervalMinutes = %d, want 5", cfg.Daemon.ScanIntervalMinutes)
	}
	if cfg.Daemon.GracePeriodMinutes != 10 {
		t.Errorf("GracePeriodMinutes = %d, want 10", cfg.Daemon.GracePeriodMinutes)
	}
	// Defaults
	if cfg.Daemon.ThumbnailWorkers != 2 {
		t.Errorf("ThumbnailWorkers = %d, want 2 (default)", cfg.Daemon.ThumbnailWorkers)
	}
	if cfg.Daemon.CatalogSyncIntervalMinutes != 30 {
		t.Errorf("CatalogSyncIntervalMinutes = %d, want 30 (default)", cfg.Daemon.CatalogSyncIntervalMinutes)
	}
	if cfg.Server.ListenAddr != "127.0.0.1:9090" {
		t.Errorf("ListenAddr = %q, want 127.0.0.1:9090", cfg.Server.ListenAddr)
	}

	if len(cfg.Folders) != 2 {
		t.Fatalf("len(Folders) = %d, want 2", len(cfg.Folders))
	}

	photos := cfg.Folders[0]
	if photos.Label != "Photos" {
		t.Errorf("Folders[0].Label = %q, want Photos", photos.Label)
	}
	if photos.BudgetBytes != 10_000_000_000 {
		t.Errorf("Folders[0].BudgetBytes = %d, want 10000000000", photos.BudgetBytes)
	}
	if len(photos.Persistent) != 1 || photos.Persistent[0] != "Starred/*" {
		t.Errorf("Folders[0].Persistent = %v, want [Starred/*]", photos.Persistent)
	}

	docs := cfg.Folders[1]
	if docs.BudgetBytes != 5_000_000_000 {
		t.Errorf("Folders[1].BudgetBytes = %d, want 5000000000", docs.BudgetBytes)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	toml := `
[[folders]]
label = "Test"
path = "/tmp/test"
budget = "1GB"
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "coldstore.toml")
	if err := os.WriteFile(cfgPath, []byte(toml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Storage.ColdStoragePath != "/srv/coldstore" {
		t.Errorf("default ColdStoragePath = %q, want /srv/coldstore", cfg.Storage.ColdStoragePath)
	}
	if cfg.Server.ListenAddr != "0.0.0.0:8090" {
		t.Errorf("default ListenAddr = %q, want 0.0.0.0:8090", cfg.Server.ListenAddr)
	}
}
