package config

import (
	"strings"
	"testing"
)

// validCfg is the smallest configuration that passes Validate. Each test mutates
// exactly the field it is about, so a failure names one cause.
func validCfg() *Config {
	return &Config{
		Storage: StorageConfig{ColdStoragePath: "/srv/coldstore"},
		Server:  ServerConfig{ListenAddr: DefaultListenAddr},
		Folders: []FolderConfig{{
			Label:       "CameraRoll",
			Path:        "/home/owner/CameraRoll",
			Budget:      "20GB",
			BudgetBytes: 20_000_000_000,
		}},
	}
}

func TestShouldRefuseToStartWhenBoundOffLoopbackWithoutCredentials(t *testing.T) {
	addrs := []string{
		"0.0.0.0:8090",
		":8090",
		"192.168.1.10:8090",
		"[::]:8090",
		"10.0.0.5:8090",
		"coldstore.lan:8090",
	}

	for _, addr := range addrs {
		t.Run(addr, func(t *testing.T) {
			err := ValidateServerBinding(&ServerConfig{ListenAddr: addr})
			if err == nil {
				t.Fatalf("ValidateServerBinding(%q) with no credentials returned nil, want refusal", addr)
			}
			// The message has to tell the owner what to change, not just that
			// something is wrong.
			for _, want := range []string{addr, "listen_addr", EnvPassword, DefaultListenAddr} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal message does not mention %q:\n%v", want, err)
				}
			}
		})
	}
}

func TestShouldStartWhenBoundOffLoopbackWithCredentials(t *testing.T) {
	s := &ServerConfig{ListenAddr: "192.168.1.10:8090", Username: "owner", Password: "hunter2hunter2"}
	if err := ValidateServerBinding(s); err != nil {
		t.Fatalf("ValidateServerBinding with credentials = %v, want nil", err)
	}
}

func TestShouldStartWhenBoundToLoopbackWithoutCredentials(t *testing.T) {
	addrs := []string{"127.0.0.1:8090", "localhost:8090", "[::1]:8090", "127.0.0.53:8090"}
	for _, addr := range addrs {
		t.Run(addr, func(t *testing.T) {
			if err := ValidateServerBinding(&ServerConfig{ListenAddr: addr}); err != nil {
				t.Fatalf("ValidateServerBinding(%q) = %v, want nil", addr, err)
			}
		})
	}
}

func TestShouldAcceptTailscaleAuthOnlyOnLoopbackWithAnAllowlist(t *testing.T) {
	s := &ServerConfig{
		ListenAddr:            DefaultListenAddr,
		AuthMode:              AuthModeTailscale,
		TailscaleAllowedUsers: []string{"owner@example.com", "co-owner@example.com"},
	}
	if err := ValidateServerBinding(s); err != nil {
		t.Fatalf("valid Tailscale Serve auth config = %v", err)
	}
	if got := s.AuthenticationMode(); got != AuthModeTailscale {
		t.Errorf("AuthenticationMode = %q, want %q", got, AuthModeTailscale)
	}
}

func TestShouldRejectUnsafeTailscaleAuthConfigurations(t *testing.T) {
	tests := map[string]struct {
		server ServerConfig
		want   string
	}{
		"missing allowlist": {
			server: ServerConfig{ListenAddr: DefaultListenAddr, AuthMode: AuthModeTailscale},
			want:   "at least one exact Tailscale login",
		},
		"network listener": {
			server: ServerConfig{ListenAddr: "192.168.1.10:8090", AuthMode: AuthModeTailscale, TailscaleAllowedUsers: []string{"owner@example.com"}},
			want:   "could forge Tailscale identity headers",
		},
		"Basic credentials remain": {
			server: ServerConfig{ListenAddr: DefaultListenAddr, AuthMode: AuthModeTailscale, TailscaleAllowedUsers: []string{"owner@example.com"}, Username: "old", Password: "old"},
			want:   "cannot be combined with Basic Auth credentials",
		},
		"blank login": {
			server: ServerConfig{ListenAddr: DefaultListenAddr, AuthMode: AuthModeTailscale, TailscaleAllowedUsers: []string{""}},
			want:   "entry 1 is empty",
		},
		"login with whitespace": {
			server: ServerConfig{ListenAddr: DefaultListenAddr, AuthMode: AuthModeTailscale, TailscaleAllowedUsers: []string{" owner@example.com"}},
			want:   "leading or trailing whitespace",
		},
		"duplicate login": {
			server: ServerConfig{ListenAddr: DefaultListenAddr, AuthMode: AuthModeTailscale, TailscaleAllowedUsers: []string{"owner@example.com", "owner@example.com"}},
			want:   "duplicate login",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidateServerBinding(&tt.server)
			if err == nil {
				t.Fatal("unsafe Tailscale authentication config was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestShouldRejectAmbiguousOrUnknownAuthenticationModes(t *testing.T) {
	tests := map[string]struct {
		server ServerConfig
		want   string
	}{
		"explicit Basic without credentials": {
			server: ServerConfig{ListenAddr: DefaultListenAddr, AuthMode: AuthModeBasic},
			want:   EnvPassword,
		},
		"Tailscale users without mode": {
			server: ServerConfig{ListenAddr: DefaultListenAddr, TailscaleAllowedUsers: []string{"owner@example.com"}},
			want:   "requires auth_mode",
		},
		"Tailscale users in Basic mode": {
			server: ServerConfig{ListenAddr: DefaultListenAddr, AuthMode: AuthModeBasic, TailscaleAllowedUsers: []string{"owner@example.com"}, Username: "owner", Password: "secret"},
			want:   "cannot be combined",
		},
		"unknown mode": {
			server: ServerConfig{ListenAddr: DefaultListenAddr, AuthMode: "proxy-magic"},
			want:   "not supported",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidateServerBinding(&tt.server)
			if err == nil {
				t.Fatal("ambiguous authentication config was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestAuthenticationModeShouldPreserveLegacyBasicAuthInference(t *testing.T) {
	if got := (ServerConfig{Username: "owner", Password: "secret"}).AuthenticationMode(); got != AuthModeBasic {
		t.Errorf("credentials inferred mode %q, want %q", got, AuthModeBasic)
	}
	if got := (ServerConfig{}).AuthenticationMode(); got != AuthModeNone {
		t.Errorf("credential-free mode = %q, want %q", got, AuthModeNone)
	}
}

func TestShouldRefuseToStartWhenOnlyOneCredentialIsSet(t *testing.T) {
	cases := map[string]ServerConfig{
		"username without password": {ListenAddr: DefaultListenAddr, Username: "owner"},
		"password without username": {ListenAddr: DefaultListenAddr, Password: "secret"},
	}

	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateServerBinding(&s)
			if err == nil {
				t.Fatal("half a credential was accepted")
			}
			if !strings.Contains(err.Error(), "Basic Auth is incomplete") {
				t.Errorf("message does not explain the consequence: %v", err)
			}
		})
	}
}

func TestShouldRefuseToStartWhenListenAddrIsNotHostPort(t *testing.T) {
	cases := []string{"8090", "192.168.1.10", "192.168.1.10:notaport", ""}
	for _, addr := range cases {
		t.Run(addr, func(t *testing.T) {
			if err := ValidateServerBinding(&ServerConfig{ListenAddr: addr}); err == nil {
				t.Fatalf("ValidateServerBinding(%q) = nil, want a parse error", addr)
			}
		})
	}
}

func TestIsLoopbackListenAddrShouldFailClosedWhenAddressCannotBeJudged(t *testing.T) {
	// A hostname could resolve anywhere, and resolving it would make startup
	// depend on DNS. It must not be treated as loopback.
	loopback, err := IsLoopbackListenAddr("some-host:8090")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loopback {
		t.Error("a hostname was judged loopback; the D5 refusal would not fire")
	}
}

func TestShouldRejectConfigWhenNoFoldersAreDeclared(t *testing.T) {
	cfg := validCfg()
	cfg.Folders = nil

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a config with no folders was accepted; the daemon would manage nothing")
	}
	if !strings.Contains(err.Error(), "[[folders]]") {
		t.Errorf("message does not name the missing section: %v", err)
	}
}

func TestShouldRejectConfigWhenColdStoragePathIsRelative(t *testing.T) {
	cfg := validCfg()
	cfg.Storage.ColdStoragePath = "coldstore"

	if err := cfg.Validate(); err == nil {
		t.Fatal("a relative cold_storage_path was accepted")
	}
}

func TestShouldRejectConfigWhenTwoFoldersShareAPath(t *testing.T) {
	cfg := validCfg()
	cfg.Folders = append(cfg.Folders, FolderConfig{
		Label: "Duplicate", Path: "/home/owner/CameraRoll/", Budget: "1GB", BudgetBytes: 1e9,
	})

	err := cfg.Validate()
	if err == nil {
		t.Fatal("two folders over one directory were accepted")
	}
	if !strings.Contains(err.Error(), "Duplicate") || !strings.Contains(err.Error(), "CameraRoll") {
		t.Errorf("message does not name both folders: %v", err)
	}
}

func TestShouldRejectConfigWhenFolderHasNoLabel(t *testing.T) {
	cfg := validCfg()
	cfg.Folders[0].Label = "   "

	if err := cfg.Validate(); err == nil {
		t.Fatal("a folder with a blank label was accepted")
	}
}

func TestShouldRejectConfigWhenBudgetIsNotPositive(t *testing.T) {
	cfg := validCfg()
	cfg.Folders[0].Budget = "0B"
	cfg.Folders[0].BudgetBytes = 0

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a zero budget was accepted; the first tick would offload the whole folder")
	}
	if !strings.Contains(err.Error(), "CameraRoll") {
		t.Errorf("message does not name the folder: %v", err)
	}
}

func TestShouldRejectConfigWhenFolderAndColdStorageOverlap(t *testing.T) {
	cases := map[string]func(*Config){
		"folder inside cold storage": func(c *Config) {
			c.Storage.ColdStoragePath = "/srv/coldstore"
			c.Folders[0].Path = "/srv/coldstore/CameraRoll"
		},
		"cold storage inside folder": func(c *Config) {
			c.Storage.ColdStoragePath = "/home/owner/CameraRoll/cold"
			c.Folders[0].Path = "/home/owner/CameraRoll"
		},
		"folder is cold storage": func(c *Config) {
			c.Storage.ColdStoragePath = "/home/owner/CameraRoll"
			c.Folders[0].Path = "/home/owner/CameraRoll"
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validCfg()
			mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("an overlapping archive and folder were accepted")
			}
			if !strings.Contains(err.Error(), "archive its own archive") {
				t.Errorf("message does not explain the loop: %v", err)
			}
		})
	}
}

func TestShouldAcceptSiblingPathsThatSharePrefixWhenNeitherContainsTheOther(t *testing.T) {
	cfg := validCfg()
	cfg.Storage.ColdStoragePath = "/srv/cold"
	cfg.Folders[0].Path = "/srv/coldstuff"

	if err := cfg.Validate(); err != nil {
		t.Fatalf("sibling paths with a shared string prefix were rejected: %v", err)
	}
}

// "coldstore will not create cold_storage_path" reads like a mount check and is
// not one. mount(8) needs the mountpoint directory to exist, so an fstab volume
// that failed to mount leaves an empty writable stub on the root filesystem and
// every check in checkColdStorage passes: exists, directory, writable. The
// daemon then builds a fresh catalog there -- discarding the budgets and pins
// the real one holds -- and archives into root while Syncthing propagates each
// deletion to every device the owner has.
func TestCheckEnvironmentShouldRefuseAnUnmountedColdStorageStubWhenRequireMountpointIsSet(t *testing.T) {
	cfg := envCfg(t)
	cfg.Storage.RequireMountpoint = true
	cfg.Folders = []FolderConfig{{Label: "Books", Path: mountedFolder(t, "Books"), BudgetBytes: 1}}

	_, err := cfg.CheckEnvironment()
	if err == nil {
		t.Fatal("a plain directory passed require_mountpoint")
	}
	for _, want := range []string{"not a mount point", "require_mountpoint", "findmnt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

// The default has to stay false: a plain directory is a supported layout and is
// what the demo and every test use.
func TestCheckEnvironmentShouldAcceptAPlainDirectoryByDefault(t *testing.T) {
	cfg := envCfg(t)
	cfg.Folders = []FolderConfig{{Label: "Books", Path: mountedFolder(t, "Books"), BudgetBytes: 1}}

	if cfg.Storage.RequireMountpoint {
		t.Fatal("require_mountpoint defaults to true; the demo and the tests would break")
	}
	if _, err := cfg.CheckEnvironment(); err != nil {
		t.Fatalf("a plain cold storage directory was refused by default: %v", err)
	}
}

// "/" is a mount point, so it is the one path that exercises the passing branch
// without needing a real volume to mount.
func TestCheckEnvironmentShouldAcceptAMountPointWhenRequireMountpointIsSet(t *testing.T) {
	if err := requireMountpoint("/"); err != nil {
		t.Errorf(`requireMountpoint("/") = %v, want nil`, err)
	}
}
