//go:build e2e

package e2e

import (
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
	syncDir  string
	coldDir  string
	cfg      *config.Config
	cat      *catalog.Catalog
	scanner  *daemon.Scanner
	offloader *daemon.Offloader
	restorer *daemon.Restorer
	daemon   *daemon.Daemon
	server   *api.Server
	ts       *httptest.Server
	logger   *slog.Logger
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
			dir := filepath.Join(env.syncDir, label)
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			env.cfg.Folders = append(env.cfg.Folders, config.FolderConfig{
				Label:       label,
				Path:        dir,
				BudgetBytes: 50000,
			})
		}
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

	syncDir := filepath.Join(t.TempDir(), "sync")
	coldDir := filepath.Join(t.TempDir(), "cold")
	os.MkdirAll(syncDir, 0755)
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

	env.scanner = daemon.NewScanner(cfg, cat, logger)
	env.offloader = daemon.NewOffloader(cfg, cat, logger)
	env.restorer = daemon.NewRestorer(cfg, cat, logger)
	env.daemon = daemon.New(cfg, cat, logger)
	env.server = api.NewServer(cfg, cat, env.restorer, logger)

	return env
}
