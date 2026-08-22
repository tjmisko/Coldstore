package config

import (
	"os"
	"path/filepath"
	"strings"
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
auth_mode = "tailscale"
tailscale_allowed_users = ["owner@example.com", "co-owner@example.com"]

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
	if cfg.Server.AuthenticationMode() != AuthModeTailscale {
		t.Errorf("AuthenticationMode = %q, want %q", cfg.Server.AuthenticationMode(), AuthModeTailscale)
	}
	if len(cfg.Server.TailscaleAllowedUsers) != 2 || cfg.Server.TailscaleAllowedUsers[1] != "co-owner@example.com" {
		t.Errorf("TailscaleAllowedUsers = %v, want the two configured logins", cfg.Server.TailscaleAllowedUsers)
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
	// An omitted listen_addr must default to loopback, never to every
	// interface: silence in the config file must not be the thing that puts an
	// unauthenticated control plane on the LAN (D5).
	if cfg.Server.ListenAddr != DefaultListenAddr {
		t.Errorf("default ListenAddr = %q, want %q", cfg.Server.ListenAddr, DefaultListenAddr)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coldstore.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A folder label is the primary key of the policy table, so two folders
// sharing one would silently merge into a single row with a single budget.
func TestLoadShouldRejectTheConfigWhenTwoFoldersShareALabel(t *testing.T) {
	path := writeConfig(t, `
[[folders]]
label = "Photos"
path = "/srv/sync/photos"
budget = "10GB"

[[folders]]
label = "Photos"
path = "/srv/sync/other"
budget = "5GB"
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("want an error for a duplicate folder label")
	}
	if !strings.Contains(err.Error(), "declared more than once") {
		t.Errorf("error = %q, want it to name the duplicate", err)
	}
}

func TestLoadForInspectionShouldNotRequireOrRetainSecrets(t *testing.T) {
	t.Setenv(EnvUsername, "environment-user-secret")
	t.Setenv(EnvPassword, "environment-password-secret")
	path := writeConfig(t, `
[storage]
cold_storage_path = "/srv/coldstore"

[server]
listen_addr = "0.0.0.0:8090"
auth_mode = "tailscale"
tailscale_allowed_users = ["private-login@example.com"]
username = "file-user-secret"
password = "file-password-secret"

[syncthing]
api_url = "http://private-device:8384"
api_key = "api-key-secret"

[[folders]]
label = "Photos"
syncthing_id = "device-folder-id"
path = "/srv/sync/photos"
budget = "10GB"
`)

	cfg, err := LoadForInspection(path)
	if err != nil {
		t.Fatalf("LoadForInspection rejected an offline report config: %v", err)
	}
	if cfg.Server.Username != "" || cfg.Server.Password != "" || len(cfg.Server.TailscaleAllowedUsers) != 0 ||
		cfg.Syncthing.APIURL != "" || cfg.Syncthing.APIKey != "" ||
		cfg.Folders[0].SyncthingID != "" {
		t.Errorf("inspection config retained secrets or device identifiers: %+v", cfg)
	}
}

// A relative path resolves against whatever directory the daemon was started
// in, which for a systemd unit is not the one the owner had in mind.
func TestLoadShouldRejectTheConfigWhenAFolderPathIsRelative(t *testing.T) {
	path := writeConfig(t, `
[[folders]]
label = "Photos"
path = "sync/photos"
budget = "10GB"
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("want an error for a relative folder path")
	}
	if !strings.Contains(err.Error(), "must be absolute") {
		t.Errorf("error = %q, want it to say the path must be absolute", err)
	}
}
