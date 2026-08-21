package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// EnvUsername and EnvPassword override [server] username / password. They exist
// so the credential does not have to live in the file an owner is most likely to
// paste into a bug report or a chat window (D5). systemd loads them from
// EnvironmentFile=-/etc/coldstore/coldstore.env, chmod 600.
const (
	EnvUsername = "COLDSTORE_USERNAME"
	EnvPassword = "COLDSTORE_PASSWORD"
)

// DefaultListenAddr is loopback on purpose. An absent or empty listen_addr must
// never mean "every interface": that is how an unauthenticated control plane
// ends up on the LAN by omission rather than by decision.
const DefaultListenAddr = "127.0.0.1:8090"

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

	cfg.applyEnvOverrides()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// applyEnvOverrides lets the environment supply the credential the TOML file
// deliberately does not have to carry. Env wins over TOML: the file is the thing
// that gets copied around, the environment is the thing that is scoped to the
// unit.
func (c *Config) applyEnvOverrides() {
	if v, ok := os.LookupEnv(EnvUsername); ok && v != "" {
		c.Server.Username = v
	}
	if v, ok := os.LookupEnv(EnvPassword); ok && v != "" {
		c.Server.Password = v
	}
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
		c.Server.ListenAddr = DefaultListenAddr
	}

	for i := range c.Folders {
		budget, err := ParseBudget(c.Folders[i].Budget)
		if err != nil {
			return fmt.Errorf("folder %q: %w", c.Folders[i].Label, err)
		}
		c.Folders[i].BudgetBytes = budget
	}

	return nil
}

// Validate checks everything that can be decided from the file alone, with no
// filesystem access. It runs at load so a misconfiguration cannot reach the
// reconciler, the mover or a listening socket. Filesystem-dependent checks live
// in CheckEnvironment, which distinguishes "misconfigured" from "not ready yet"
// and therefore cannot be a pure function.
func (c *Config) Validate() error {
	if err := ValidateServerBinding(&c.Server); err != nil {
		return err
	}

	if !filepath.IsAbs(c.Storage.ColdStoragePath) {
		return fmt.Errorf("[storage] cold_storage_path %q must be absolute",
			c.Storage.ColdStoragePath)
	}

	// A daemon with nothing to manage is always a mistake -- a truncated file,
	// an edit that deleted the wrong block, a config written to the wrong path.
	// It would otherwise start, log nothing interesting and enforce nothing.
	if len(c.Folders) == 0 {
		return errors.New("no [[folders]] are declared: coldstore would start and manage nothing. " +
			"Add at least one [[folders]] block with label, path and budget (see coldstore.toml.example)")
	}

	coldRoot := filepath.Clean(c.Storage.ColdStoragePath)

	// A folder is INFRASTRUCTURE (D1): the label is the primary key of the
	// policy table and the path is the only thing that says where the folder
	// physically is. Both are validated here, at load, so a typo cannot reach
	// the reconciler and quietly merge two folders into one row or resolve a
	// relative path against whatever directory systemd happened to start in.
	seenLabel := make(map[string]bool, len(c.Folders))
	seenPath := make(map[string]string, len(c.Folders))
	for i := range c.Folders {
		label := c.Folders[i].Label
		if strings.TrimSpace(label) == "" {
			return fmt.Errorf("[[folders]] entry %d has no label: the label is the key the "+
				"whole policy table hangs off and cannot be empty", i+1)
		}
		if seenLabel[label] {
			return fmt.Errorf("folder %q is declared more than once", label)
		}
		seenLabel[label] = true

		if !filepath.IsAbs(c.Folders[i].Path) {
			return fmt.Errorf("folder %q: path %q must be absolute",
				label, c.Folders[i].Path)
		}
		path := filepath.Clean(c.Folders[i].Path)
		if other, dup := seenPath[path]; dup {
			return fmt.Errorf("folders %q and %q both declare path %q: "+
				"two policy rows over one directory would fight each other",
				other, label, path)
		}
		seenPath[path] = label

		if c.Folders[i].BudgetBytes <= 0 {
			return fmt.Errorf("folder %q: budget %q is not positive; a zero budget would "+
				"offload the entire folder on the first tick", label, c.Folders[i].Budget)
		}

		// The archive lives under cold_storage_path. If that is inside a managed
		// folder, every archived file is immediately re-scanned as a synced file
		// in the same folder and archived again; if the folder is inside cold
		// storage the same loop runs the other way.
		if pathContains(coldRoot, path) {
			return fmt.Errorf("folder %q path %q is inside [storage] cold_storage_path %q: "+
				"coldstore would archive its own archive", label, path, coldRoot)
		}
		if pathContains(path, coldRoot) {
			return fmt.Errorf("[storage] cold_storage_path %q is inside folder %q (%q): "+
				"coldstore would archive its own archive", coldRoot, label, path)
		}
	}

	return nil
}

// pathContains reports whether child is parent or lives beneath it. Both
// arguments must already be absolute and Clean.
func pathContains(parent, child string) bool {
	if parent == child {
		return true
	}
	return strings.HasPrefix(child, strings.TrimSuffix(parent, string(filepath.Separator))+string(filepath.Separator))
}

// ValidateServerBinding is the D5 refusal. A control plane that can pin, archive,
// restore, run bulk jobs and rewrite folder policy must not be reachable from the
// network without a credential, and the previous behaviour -- one WARN line that
// scrolls out of the journal -- is not a decision the owner ever consciously made.
//
// There is deliberately no override flag. The two safe shapes are cheap: bind
// loopback, or set a username and password. A flag would exist only to be
// copy-pasted out of a forum post.
func ValidateServerBinding(s *ServerConfig) error {
	loopback, err := IsLoopbackListenAddr(s.ListenAddr)
	if err != nil {
		return err
	}

	hasUser := s.Username != ""
	hasPass := s.Password != ""

	// Half a credential is worse than none: Handler() falls back to the bare mux,
	// so the owner believes auth is on when it is off.
	if hasUser != hasPass {
		missing, present := "password", "username"
		if hasPass {
			missing, present = "username", "password"
		}
		return fmt.Errorf("[server] %s is set but %s is empty, so basic auth would be "+
			"silently disabled. Set both, or neither (see %s / %s)",
			present, missing, EnvUsername, EnvPassword)
	}

	if hasUser || loopback {
		return nil
	}

	return fmt.Errorf(`refusing to start: [server] listen_addr = %q is reachable from the network but no credentials are set.

Anyone who can reach that address can pin, archive, restore and permanently move your
files, and coldstore's archives propagate as deletions to every Syncthing device.

Fix it in one of two ways:

  1. Set credentials. Preferred, so the password is not in the config file:
       echo 'COLDSTORE_USERNAME=coldstore'   >  /etc/coldstore/coldstore.env
       echo 'COLDSTORE_PASSWORD=<a long random password>' >> /etc/coldstore/coldstore.env
       chmod 600 /etc/coldstore/coldstore.env
     and keep listen_addr on the LAN interface, never 0.0.0.0.

  2. Or bind loopback and reach the UI over an SSH tunnel:
       listen_addr = %q
       ssh -N -L 8090:127.0.0.1:8090 you@this-host

There is no flag to skip this check.`, s.ListenAddr, DefaultListenAddr)
}

// IsLoopbackListenAddr reports whether addr can only be reached from this
// machine. It fails closed: anything it cannot positively identify as loopback
// -- a wildcard bind, a bare port, a hostname -- counts as network-reachable.
func IsLoopbackListenAddr(addr string) (bool, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Errorf("[server] listen_addr %q is not a valid host:port "+
			"(examples: %q, %q): %w", addr, DefaultListenAddr, "192.168.1.10:8090", err)
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return false, fmt.Errorf("[server] listen_addr %q has an invalid port %q", addr, port)
	}

	// ":8090" and "0.0.0.0:8090" are the same thing: every interface.
	if host == "" {
		return false, nil
	}
	if host == "localhost" {
		return true, nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A hostname. It may well resolve to a LAN address, and resolving it here
		// would make startup depend on DNS. Treat it as network-reachable.
		return false, nil
	}
	if ip.IsUnspecified() {
		return false, nil
	}
	return ip.IsLoopback(), nil
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
