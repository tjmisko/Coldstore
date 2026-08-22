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

	AuthModeNone      = "none"
	AuthModeBasic     = "basic"
	AuthModeTailscale = "tailscale"
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

	// RequireMountpoint makes a cold_storage_path that is not its own mount
	// point a fatal startup error.
	//
	// It exists because "coldstore will not create cold_storage_path" is not
	// the mount check it is usually read as. mount(8) requires the mountpoint
	// DIRECTORY to exist, so an fstab volume that failed to mount leaves an
	// empty, writable directory on the root filesystem, and every check in
	// checkColdStorage passes. The daemon then builds a fresh catalog there --
	// discarding the budgets and pins the real one held -- and archives into
	// the root filesystem while Syncthing propagates each deletion to every
	// device the owner has. When the volume is finally mounted, those copies
	// are shadowed and the files are gone everywhere else.
	//
	// Only a ZFS dataset with a native mountpoint= is self-protecting, because
	// zfs mount creates the directory itself.
	//
	// Default false: a plain directory on an existing filesystem is a
	// supported layout and is what the tests and the demo use, so this cannot
	// be the default without breaking them. Set it to true on any deployment
	// whose cold storage is a separate volume -- which is every deployment
	// that follows the runbook.
	RequireMountpoint bool `toml:"require_mountpoint"`
}

type DaemonConfig struct {
	ScanIntervalMinutes        int `toml:"scan_interval_minutes"`
	CatalogSyncIntervalMinutes int `toml:"catalog_sync_interval_minutes"`
	ThumbnailWorkers           int `toml:"thumbnail_workers"`
	GracePeriodMinutes         int `toml:"grace_period_minutes"`
}

type ServerConfig struct {
	ListenAddr            string   `toml:"listen_addr"`
	AuthMode              string   `toml:"auth_mode"`
	TailscaleAllowedUsers []string `toml:"tailscale_allowed_users"`
	Username              string   `toml:"username"`
	Password              string   `toml:"password"`
}

// AuthenticationMode returns the effective server authentication mode. An
// omitted auth_mode retains the original configuration contract: two Basic
// Auth credentials enable Basic Auth, while a credential-free loopback listener
// is local-only and unauthenticated. Callers use this only after validation, so
// an unknown explicit mode cannot reach the HTTP server.
func (s ServerConfig) AuthenticationMode() string {
	if s.AuthMode != "" {
		return s.AuthMode
	}
	if s.Username != "" && s.Password != "" {
		return AuthModeBasic
	}
	return AuthModeNone
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
	return load(path, false)
}

// LoadForInspection parses the configuration for read-only diagnostics. It
// deliberately does not read credential environment variables and does not
// require authentication for a non-loopback listener: --status never starts a
// listener, so a missing service-only environment file must not prevent an
// offline health report. Secrets and Syncthing device identifiers are scrubbed
// from the returned value as a second line of defence against accidental
// serialization.
func LoadForInspection(path string) (*Config, error) {
	return load(path, true)
}

func load(path string, inspection bool) (*Config, error) {
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

	if !inspection {
		cfg.applyEnvOverrides()
	}

	if err := cfg.validate(!inspection); err != nil {
		return nil, err
	}
	if inspection {
		cfg.scrubInspectionSecrets()
	}

	return &cfg, nil
}

func (c *Config) scrubInspectionSecrets() {
	c.Server.Username = ""
	c.Server.Password = ""
	c.Server.TailscaleAllowedUsers = nil
	c.Syncthing.APIURL = ""
	c.Syncthing.APIKey = ""
	for i := range c.Folders {
		c.Folders[i].SyncthingID = ""
	}
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
	return c.validate(true)
}

func (c *Config) validate(checkServerBinding bool) error {
	if checkServerBinding {
		if err := ValidateServerBinding(&c.Server); err != nil {
			return err
		}
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
	// policy table, the Syncthing ID is its identity on the local Syncthing
	// device, and the path says where it physically is. They are validated here,
	// at load, so a typo cannot reach the reconciler and quietly merge two
	// folders into one row or resolve a relative path against whatever directory
	// systemd happened to start in.
	seenLabel := make(map[string]bool, len(c.Folders))
	seenSyncthingID := make(map[string]string, len(c.Folders))
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

		if syncthingID := c.Folders[i].SyncthingID; syncthingID != "" {
			if other, dup := seenSyncthingID[syncthingID]; dup {
				return fmt.Errorf("folders %q and %q both declare Syncthing ID %q: "+
					"one Syncthing folder cannot back two Coldstore identities",
					other, label, syncthingID)
			}
			seenSyncthingID[syncthingID] = label
		}

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
// network without an authentication boundary, and the previous behaviour -- one
// WARN line that scrolls out of the journal -- is not a decision the owner ever
// consciously made.
//
// Tailscale Serve identity headers are safe to trust only when the backend is
// loopback-only. Serve removes caller-supplied identity headers and inserts its
// authenticated value, but a client that can reach the backend directly can
// supply any header it wants. The loopback refusal therefore is part of the
// Tailscale authentication mechanism, not merely a deployment preference.
func ValidateServerBinding(s *ServerConfig) error {
	loopback, err := IsLoopbackListenAddr(s.ListenAddr)
	if err != nil {
		return err
	}

	hasUser := s.Username != ""
	hasPass := s.Password != ""

	// Half a credential is worse than none. Retain the original refusal even when
	// auth_mode is explicit so a stale environment override cannot be overlooked.
	if hasUser != hasPass {
		missing, present := "password", "username"
		if hasPass {
			missing, present = "username", "password"
		}
		return fmt.Errorf("[server] %s is set but %s is empty, so Basic Auth is incomplete. "+
			"Set both, or neither (see %s / %s)",
			present, missing, EnvUsername, EnvPassword)
	}

	switch s.AuthMode {
	case "":
		if len(s.TailscaleAllowedUsers) != 0 {
			return errors.New("[server] tailscale_allowed_users requires auth_mode = \"tailscale\"")
		}
		if hasUser || loopback {
			return nil
		}
	case AuthModeBasic:
		if len(s.TailscaleAllowedUsers) != 0 {
			return errors.New("[server] tailscale_allowed_users cannot be combined with auth_mode = \"basic\"")
		}
		if !hasUser {
			return fmt.Errorf("[server] auth_mode = %q requires both %s and %s",
				AuthModeBasic, EnvUsername, EnvPassword)
		}
		return nil
	case AuthModeTailscale:
		if hasUser {
			return errors.New("[server] auth_mode = \"tailscale\" cannot be combined with Basic Auth credentials; remove username/password and their environment overrides")
		}
		if !loopback {
			return fmt.Errorf("[server] auth_mode = %q requires a loopback listen_addr (for example %q); clients that reach %q directly could forge Tailscale identity headers",
				AuthModeTailscale, DefaultListenAddr, s.ListenAddr)
		}
		if err := validateTailscaleAllowedUsers(s.TailscaleAllowedUsers); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("[server] auth_mode %q is not supported; use %q or %q (or omit it for the legacy default)",
			s.AuthMode, AuthModeBasic, AuthModeTailscale)
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

func validateTailscaleAllowedUsers(users []string) error {
	if len(users) == 0 {
		return errors.New("[server] auth_mode = \"tailscale\" requires at least one exact Tailscale login in tailscale_allowed_users")
	}
	seen := make(map[string]struct{}, len(users))
	for i, user := range users {
		if user == "" {
			return fmt.Errorf("[server] tailscale_allowed_users entry %d is empty", i+1)
		}
		if user != strings.TrimSpace(user) {
			return fmt.Errorf("[server] tailscale_allowed_users entry %d has leading or trailing whitespace", i+1)
		}
		if _, exists := seen[user]; exists {
			return fmt.Errorf("[server] tailscale_allowed_users contains duplicate login %q", user)
		}
		seen[user] = struct{}{}
	}
	return nil
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
