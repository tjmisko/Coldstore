//go:build e2e

package e2e

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjmisko/coldstore/internal/api"
	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/daemon"
)

// testEnv bundles all components needed for an e2e test.
type testEnv struct {
	syncDir string
	coldDir string
	cfg     *config.Config
	cat     *catalog.Catalog
	scanner *daemon.Scanner
	// mover is the process-wide serialisation point, shared by the daemon and
	// the API server exactly as main shares it.
	mover  *daemon.Mover
	worker *daemon.Worker
	daemon *daemon.Daemon
	server *api.Server
	ts     *httptest.Server
	logger *slog.Logger
}

// drainJobs runs the job worker synchronously until the queue is empty. Tests
// use it instead of starting the worker goroutine and sleeping: the assertion
// is about what the worker did, not about how fast it noticed.
func (env *testEnv) drainJobs(t *testing.T) {
	t.Helper()
	env.worker.RunOnce(context.Background())
}

type envOption func(*testEnv, *testing.T)

func withBudget(label string, bytes int64) envOption {
	return func(env *testEnv, t *testing.T) {
		for i, f := range env.cfg.Folders {
			if f.Label == label {
				env.cfg.Folders[i].BudgetBytes = bytes
				return
			}
		}
		t.Fatalf("withBudget: folder %q not found", label)
	}
}

func withGracePeriod(mins int) envOption {
	return func(env *testEnv, _ *testing.T) {
		env.cfg.Daemon.GracePeriodMinutes = mins
	}
}

func withPersistent(label, pattern string) envOption {
	return func(env *testEnv, t *testing.T) {
		for i, f := range env.cfg.Folders {
			if f.Label == label {
				env.cfg.Folders[i].Persistent = append(env.cfg.Folders[i].Persistent, pattern)
				return
			}
		}
		t.Fatalf("withPersistent: folder %q not found", label)
	}
}

func withAuth(user, pass string) envOption {
	return func(env *testEnv, _ *testing.T) {
		env.cfg.Server.Username = user
		env.cfg.Server.Password = pass
	}
}

func withMultiFolders(n int) envOption {
	return func(env *testEnv, t *testing.T) {
		env.cfg.Folders = nil
		for i := 0; i < n; i++ {
			label := "Folder" + string(rune('A'+i))
			dir := newSyncthingFolder(t, filepath.Join(env.syncDir, label))
			env.cfg.Folders = append(env.cfg.Folders, config.FolderConfig{
				Label:       label,
				Path:        dir,
				BudgetBytes: 50000,
			})
		}
	}
}

// setPolicy edits folder policy in the database the way the UI does, after the
// daemon is already up. Nothing needs restarting: the next tick reads it.
func setPolicy(t *testing.T, env *testEnv, label string, budget *int64, grace *int, enabled *bool) {
	t.Helper()
	found, err := env.cat.UpdateFolderPolicy(label, budget, grace, enabled)
	if err != nil {
		t.Fatalf("updating policy for %q: %v", label, err)
	}
	if !found {
		t.Fatalf("no folder_configs row for %q", label)
	}
}

func int64Ptr(v int64) *int64 { return &v }

// newSyncthingFolder creates a directory the way Syncthing would, marker and
// all. FolderReady refuses to move anything into or out of a directory with no
// .stfolder, because that is how an unmounted volume is told apart from an
// empty one (design section 3.6).
func newSyncthingFolder(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".stfolder"), 0o755); err != nil {
		t.Fatalf("creating syncthing folder %s: %v", dir, err)
	}
	return dir
}

// archive drives one file to cold storage through the mover, which is how
// every production offload happens. Tests use it to reach the state they are
// actually about.
func (env *testEnv) archive(t *testing.T, id int64) {
	t.Helper()
	if _, err := env.mover.ArchiveByID(id); err != nil {
		t.Fatalf("archiving file %d: %v", id, err)
	}
}

// restore is archive's mirror.
func (env *testEnv) restore(t *testing.T, id int64) {
	t.Helper()
	if _, err := env.mover.RestoreByID(id); err != nil {
		t.Fatalf("restoring file %d: %v", id, err)
	}
}

func withLiveServer() envOption {
	return func(env *testEnv, t *testing.T) {
		env.ts = httptest.NewServer(env.server.Handler())
		t.Cleanup(func() { env.ts.Close() })
	}
}

func newTestEnv(t *testing.T, opts ...envOption) *testEnv {
	t.Helper()

	syncDir := newSyncthingFolder(t, filepath.Join(t.TempDir(), "sync"))
	coldDir := filepath.Join(t.TempDir(), "cold")
	os.MkdirAll(filepath.Join(coldDir, "archive"), 0755)
	os.MkdirAll(filepath.Join(coldDir, "thumbnails"), 0755)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := &config.Config{
		Storage: config.StorageConfig{ColdStoragePath: coldDir},
		Daemon: config.DaemonConfig{
			ScanIntervalMinutes:        15,
			CatalogSyncIntervalMinutes: 30,
			ThumbnailWorkers:           1,
			GracePeriodMinutes:         30,
		},
		Server: config.ServerConfig{ListenAddr: "127.0.0.1:0"},
		Folders: []config.FolderConfig{
			{Label: "TestFolder", Path: syncDir, BudgetBytes: 50000},
		},
	}

	env := &testEnv{
		syncDir: syncDir,
		coldDir: coldDir,
		cfg:     cfg,
		cat:     cat,
		logger:  logger,
	}

	for _, opt := range opts {
		opt(env, t)
	}

	// Policy lives in the database (D1). Options above tune cfg; this is the
	// startup reconciliation that hands those values to the daemon, exactly as
	// main does. Anything that wants to change policy AFTER this point has to
	// go through cat.UpdateFolderPolicy or the API, which is the behaviour
	// under test in several of these files.
	if err := daemon.ReconcileFolders(cfg, cat, logger); err != nil {
		t.Fatal(err)
	}

	env.mover = daemon.NewMover(cfg, cat, logger)
	// Disk space is not what most of these tests are about, and the real
	// statfs would report whatever the machine happens to have.
	env.mover.SetSpaceCheck(func(string) (float64, error) { return 90.0, nil })
	env.daemon = daemon.New(cfg, cat, env.mover, logger)
	// The scanner and the worker come FROM the daemon, exactly as main takes
	// them: one scan mutex and one wake channel per process, or the guards they
	// exist to provide guard nothing.
	env.scanner = env.daemon.Scanner()
	env.worker = env.daemon.Worker()
	env.server = api.NewServer(api.Deps{
		Cfg:     cfg,
		Cat:     cat,
		Mover:   env.mover,
		Worker:  env.worker,
		Scanner: env.scanner,
		Logger:  logger,
	})

	return env
}
