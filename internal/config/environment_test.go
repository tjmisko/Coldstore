package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// envCfg builds a config whose cold storage really exists, so each test can
// break exactly one thing.
func envCfg(t *testing.T) *Config {
	t.Helper()
	cold := filepath.Join(t.TempDir(), "cold")
	if err := os.MkdirAll(cold, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Config{
		Storage: StorageConfig{ColdStoragePath: cold},
		Server:  ServerConfig{ListenAddr: DefaultListenAddr},
	}
}

func mountedFolder(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(path, stfolderMarker), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestShouldRefuseToStartWhenColdStoragePathDoesNotExist(t *testing.T) {
	cfg := envCfg(t)
	cfg.Storage.ColdStoragePath = filepath.Join(t.TempDir(), "nope")
	cfg.Folders = []FolderConfig{{Label: "F", Path: mountedFolder(t, "f"), BudgetBytes: 1}}

	_, err := cfg.CheckEnvironment()
	if err == nil {
		t.Fatal("a missing cold_storage_path started successfully")
	}
	// Creating it would put the archive on the root filesystem when the pool is
	// not mounted, which is the failure the message has to prevent.
	if !strings.Contains(err.Error(), "will not create it") {
		t.Errorf("message does not explain why it is not created: %v", err)
	}
}

func TestShouldRefuseToStartWhenColdStoragePathIsAFile(t *testing.T) {
	cfg := envCfg(t)
	file := filepath.Join(t.TempDir(), "cold-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Storage.ColdStoragePath = file
	cfg.Folders = []FolderConfig{{Label: "F", Path: mountedFolder(t, "f"), BudgetBytes: 1}}

	if _, err := cfg.CheckEnvironment(); err == nil {
		t.Fatal("a cold_storage_path pointing at a file started successfully")
	}
}

func TestShouldRefuseToStartWhenColdStoragePathIsNotWritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores the mode bits this test relies on")
	}
	cfg := envCfg(t)
	if err := os.Chmod(cfg.Storage.ColdStoragePath, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(cfg.Storage.ColdStoragePath, 0o755) })
	cfg.Folders = []FolderConfig{{Label: "F", Path: mountedFolder(t, "f"), BudgetBytes: 1}}

	_, err := cfg.CheckEnvironment()
	if err == nil {
		t.Fatal("an unwritable cold_storage_path started successfully")
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Errorf("message does not say what is wrong: %v", err)
	}
}

func TestShouldWarnButStartWhenFolderPathDoesNotExist(t *testing.T) {
	cfg := envCfg(t)
	missing := filepath.Join(t.TempDir(), "not-mounted-yet")
	cfg.Folders = []FolderConfig{{Label: "CameraRoll", Path: missing, BudgetBytes: 1}}

	warnings, err := cfg.CheckEnvironment()
	// A boot-ordering race with the mount unit must not keep the service down.
	if err != nil {
		t.Fatalf("a folder path that is not mounted yet was fatal: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], missing) {
		t.Fatalf("warnings = %v, want one naming %q", warnings, missing)
	}
}

func TestShouldWarnButStartWhenFolderHasNoStfolderMarker(t *testing.T) {
	cfg := envCfg(t)
	bare := filepath.Join(t.TempDir(), "bare")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Folders = []FolderConfig{{Label: "CameraRoll", Path: bare, BudgetBytes: 1}}

	warnings, err := cfg.CheckEnvironment()
	if err != nil {
		t.Fatalf("a folder without .stfolder was fatal: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], stfolderMarker) {
		t.Fatalf("warnings = %v, want one naming %s", warnings, stfolderMarker)
	}
}

func TestShouldRefuseToStartWhenFolderPathIsAFile(t *testing.T) {
	cfg := envCfg(t)
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Folders = []FolderConfig{{Label: "CameraRoll", Path: file, BudgetBytes: 1}}

	// A file can never become a folder by waiting, so this is misconfiguration,
	// not a race.
	if _, err := cfg.CheckEnvironment(); err == nil {
		t.Fatal("a folder path pointing at a file started successfully")
	}
}

func TestShouldRefuseToStartWhenMountedFolderIsNotWritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores the mode bits this test relies on")
	}
	cfg := envCfg(t)
	path := mountedFolder(t, "CameraRoll")
	if err := os.Chmod(path, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o755) })
	cfg.Folders = []FolderConfig{{Label: "CameraRoll", Path: path, BudgetBytes: 1}}

	// .stfolder is present, so the volume is demonstrably mounted and a
	// permission problem is a deployment mistake rather than an ordering race.
	_, err := cfg.CheckEnvironment()
	if err == nil {
		t.Fatal("a mounted but unwritable folder started successfully")
	}
	if !strings.Contains(err.Error(), "same account Syncthing runs as") {
		t.Errorf("message does not name the fix: %v", err)
	}
	// An ownership problem must NOT be described as a sandbox problem.
	if strings.Contains(err.Error(), "ReadWritePaths") {
		t.Errorf("EACCES was misdiagnosed as a read-only sandbox: %v", err)
	}
}

// The reviewer's failing case: run the real binary under the shipped unit's
// ProtectSystem=strict with one [[folders]].path left out of ReadWritePaths=.
// The probe fails with EROFS, and the old single message told the owner to
// chown a directory they already owned. The unit-testable half of that is which
// remedy the errno selects; a read-only bind mount needs root, so it is not
// something this tier can stage.
func TestShouldNameReadWritePathsWhenThePathIsReadOnlyToThisProcess(t *testing.T) {
	got := writeFailureHint(
		fmt.Errorf("open /srv/x/.coldstore-writecheck-1: %w", syscall.EROFS),
		"Run it as the same account Syncthing runs as.")

	if !strings.Contains(got, "ReadWritePaths") {
		t.Errorf("EROFS hint does not name ReadWritePaths=: %q", got)
	}
	if strings.Contains(got, "Syncthing runs as") {
		t.Errorf("EROFS hint still prescribes the ownership fix, which cannot work: %q", got)
	}
	if !strings.Contains(got, "chown will not fix it") {
		t.Errorf("EROFS hint does not rule out the fix an owner reaches for first: %q", got)
	}
}

func TestShouldNameTheOwnershipFixWhenThePathIsMerelyUnwritable(t *testing.T) {
	const ownership = "Run it as the same account Syncthing runs as."
	got := writeFailureHint(
		fmt.Errorf("open /srv/x/.coldstore-writecheck-1: %w", syscall.EACCES), ownership)

	if got != ownership {
		t.Errorf("EACCES hint = %q, want the ownership fix %q", got, ownership)
	}
}

func TestShouldReportNoWarningsWhenEverythingIsReady(t *testing.T) {
	cfg := envCfg(t)
	cfg.Folders = []FolderConfig{
		{Label: "CameraRoll", Path: mountedFolder(t, "CameraRoll"), BudgetBytes: 1},
		{Label: "Books", Path: mountedFolder(t, "Books"), BudgetBytes: 1},
	}

	warnings, err := cfg.CheckEnvironment()
	if err != nil {
		t.Fatalf("a healthy config failed the environment check: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

func TestShouldLeaveNoProbeFilesBehindWhenCheckSucceeds(t *testing.T) {
	cfg := envCfg(t)
	path := mountedFolder(t, "CameraRoll")
	cfg.Folders = []FolderConfig{{Label: "CameraRoll", Path: path, BudgetBytes: 1}}

	if _, err := cfg.CheckEnvironment(); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{cfg.Storage.ColdStoragePath, path} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".coldstore-writecheck") {
				t.Errorf("writability probe left %s in %s", e.Name(), dir)
			}
		}
	}
}
