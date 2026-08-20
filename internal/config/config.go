package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Storage   StorageConfig   `toml:"storage"`
	Daemon    DaemonConfig    `toml:"daemon"`
	Server    ServerConfig    `toml:"server"`
	Syncthing SyncthingConfig `toml:"syncthing"`
	Folders   []FolderConfig  `toml:"folders"`
}

type StorageConfig struct {
	ColdStoragePath string `toml:"cold_storage_path"`
}

type DaemonConfig struct {
	ScanIntervalMinutes        int `toml:"scan_interval_minutes"`
	CatalogSyncIntervalMinutes int `toml:"catalog_sync_interval_minutes"`
	ThumbnailWorkers           int `toml:"thumbnail_workers"`
	GracePeriodMinutes         int `toml:"grace_period_minutes"`
}

type ServerConfig struct {
	ListenAddr string `toml:"listen_addr"`
	Username   string `toml:"username"`
	Password   string `toml:"password"`
}

type SyncthingConfig struct {
	APIURL string `toml:"api_url"`
	APIKey string `toml:"api_key"`
}

type FolderConfig struct {
	Label       string   `toml:"label"`
	SyncthingID string   `toml:"syncthing_id"`
	Path        string   `toml:"path"`
	Budget      string   `toml:"budget"`
	Persistent  []string `toml:"persistent"`

	BudgetBytes int64 `toml:"-"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := cfg.setDefaults(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) setDefaults() error {
	if c.Storage.ColdStoragePath == "" {
		c.Storage.ColdStoragePath = "/srv/coldstore"
	}
	if c.Daemon.ScanIntervalMinutes == 0 {
		c.Daemon.ScanIntervalMinutes = 15
	}
	if c.Daemon.CatalogSyncIntervalMinutes == 0 {
		c.Daemon.CatalogSyncIntervalMinutes = 30
	}
	if c.Daemon.ThumbnailWorkers == 0 {
		c.Daemon.ThumbnailWorkers = 2
	}
	if c.Daemon.GracePeriodMinutes == 0 {
		c.Daemon.GracePeriodMinutes = 30
	}
	if c.Server.ListenAddr == "" {
		c.Server.ListenAddr = "0.0.0.0:8090"
	}

	// A folder is INFRASTRUCTURE (D1): the label is the primary key of the
	// policy table and the path is the only thing that says where the folder
	// physically is. Both are validated here, at load, so a typo cannot reach
	// the reconciler and quietly merge two folders into one row or resolve a
	// relative path against whatever directory systemd happened to start in.
	seen := make(map[string]bool, len(c.Folders))
	for i := range c.Folders {
		label := c.Folders[i].Label
		if seen[label] {
			return fmt.Errorf("folder %q is declared more than once", label)
		}
		seen[label] = true

		if !filepath.IsAbs(c.Folders[i].Path) {
			return fmt.Errorf("folder %q: path %q must be absolute",
				label, c.Folders[i].Path)
		}

		budget, err := ParseBudget(c.Folders[i].Budget)
		if err != nil {
			return fmt.Errorf("folder %q: %w", label, err)
		}
		c.Folders[i].BudgetBytes = budget
	}

	return nil
}

// ParseBudget parses human-readable size strings like "20GB", "500MB", "1TB".
func ParseBudget(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty budget string")
	}

	s = strings.ToUpper(s)

	// Ordered longest-suffix-first to avoid "B" matching before "GB"
	suffixes := []struct {
		suffix string
		mult   int64
	}{
		{"TB", 1000 * 1000 * 1000 * 1000},
		{"GB", 1000 * 1000 * 1000},
		{"MB", 1000 * 1000},
		{"KB", 1000},
		{"B", 1},
	}

	for _, entry := range suffixes {
		if strings.HasSuffix(s, entry.suffix) {
			numStr := strings.TrimSpace(strings.TrimSuffix(s, entry.suffix))
			val, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid budget %q: %w", s, err)
			}
			return int64(val * float64(entry.mult)), nil
		}
	}

	return 0, fmt.Errorf("invalid budget %q: no recognized unit (B, KB, MB, GB, TB)", s)
}
