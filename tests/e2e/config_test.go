//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
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
	// Loopback, not 0.0.0.0: an omitted listen_addr must not be what puts the
	// control plane on the network (D5).
	if cfg.Server.ListenAddr != config.DefaultListenAddr {
		t.Errorf("listen_addr default = %q, want %q", cfg.Server.ListenAddr, config.DefaultListenAddr)
	}
}

// writeCfg writes a config file and returns its path.
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coldstore.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const folderBlock = `
[[folders]]
label = "Photos"
path = "/tmp/photos"
budget = "10GB"
`

func TestConfigLoadShouldRefuseWhenBoundOffLoopbackWithoutCredentials(t *testing.T) {
	path := writeCfg(t, `
[server]
listen_addr = "0.0.0.0:8090"
`+folderBlock)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("config.Load accepted an unauthenticated 0.0.0.0 bind")
	}
	if !strings.Contains(err.Error(), "refusing to start") {
		t.Errorf("error does not read as a refusal: %v", err)
	}
}

func TestConfigLoadShouldAcceptOffLoopbackBindWhenCredentialsComeFromTheEnvironment(t *testing.T) {
	// The password never has to be in the file the owner might paste into a bug
	// report: COLDSTORE_USERNAME / COLDSTORE_PASSWORD supply it instead.
	t.Setenv(config.EnvUsername, "owner")
	t.Setenv(config.EnvPassword, "a-long-random-password")

	path := writeCfg(t, `
[server]
listen_addr = "0.0.0.0:8090"
`+folderBlock)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load = %v, want nil", err)
	}
	if cfg.Server.Username != "owner" || cfg.Server.Password != "a-long-random-password" {
		t.Errorf("credentials = %q/%q, want the environment values",
			cfg.Server.Username, cfg.Server.Password)
	}
}

func TestConfigLoadShouldPreferTheEnvironmentPasswordOverTheFileOne(t *testing.T) {
	t.Setenv(config.EnvPassword, "from-env")

	path := writeCfg(t, `
[server]
listen_addr = "127.0.0.1:8090"
username = "owner"
password = "from-file"
`+folderBlock)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Password != "from-env" {
		t.Errorf("password = %q, want the environment to win", cfg.Server.Password)
	}
}

func TestConfigLoadShouldRefuseWhenNoFoldersAreDeclared(t *testing.T) {
	path := writeCfg(t, `
[storage]
cold_storage_path = "/tmp/cold"
`)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("config.Load accepted a config with no folders")
	}
}

func TestConfigLoadShouldRefuseWhenColdStorageIsInsideAManagedFolder(t *testing.T) {
	path := writeCfg(t, `
[storage]
cold_storage_path = "/tmp/photos/cold"

[[folders]]
label = "Photos"
path = "/tmp/photos"
budget = "10GB"
`)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("config.Load accepted an archive nested inside the folder it archives")
	}
}

func TestShippedExampleConfigShouldLoadAndBeSafeByDefault(t *testing.T) {
	// The documented happy path is `cp coldstore.toml.example ...`. If the
	// shipped file does not load, or loads into an exposed daemon, the runbook
	// is lying.
	cfg, err := config.Load(filepath.Join("..", "..", "coldstore.toml.example"))
	if err != nil {
		t.Fatalf("coldstore.toml.example does not load: %v", err)
	}

	loopback, err := config.IsLoopbackListenAddr(cfg.Server.ListenAddr)
	if err != nil {
		t.Fatal(err)
	}
	authed := cfg.Server.Username != "" && cfg.Server.Password != ""
	if !loopback && !authed {
		t.Errorf("the shipped example binds %q with no credentials", cfg.Server.ListenAddr)
	}
}
