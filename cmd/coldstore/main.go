package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/tjmisko/coldstore/internal/api"
	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/daemon"
)

func main() {
	configPath := flag.String("config", "coldstore.toml", "path to config file")
	scanOnce := flag.Bool("scan", false, "run a single catalog scan and exit")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	dbPath := cfg.Storage.ColdStoragePath + "/catalog.db"
	cat, err := catalog.Open(dbPath)
	if err != nil {
		logger.Error("failed to open catalog", "error", err)
		os.Exit(1)
	}
	defer cat.Close()

	// Fold the folders declared in coldstore.toml into folder_configs before
	// anything reads policy. It runs before recovery because recovery resolves
	// file rows through folder paths that only reconciliation guarantees are
	// current, and a daemon that cannot establish its policy table must not
	// start enforcing against whatever is in the database (design 3.2).
	if err := daemon.ReconcileFolders(cfg, cat, logger); err != nil {
		logger.Error("folder reconciliation failed", "error", err)
		os.Exit(1)
	}

	// Resolve rows left mid-move by a previous run BEFORE anything that could
	// move a file exists. Recovery writes the filesystem without a mover lock,
	// so it is only safe while nothing else is running (invariant 8), and a
	// catalog whose transient rows cannot be resolved is not one to enforce
	// quotas against.
	if err := daemon.RecoverStartupState(cfg, cat, logger); err != nil {
		logger.Error("startup recovery failed", "error", err)
		os.Exit(1)
	}

	// Re-queue the job work a kill left in flight. It runs AFTER the file-level
	// recovery above, never before: recovery settles each file's status first,
	// so a resumed item cannot re-claim a row that is still transient and fail
	// its CAS for a reason that has nothing to do with the job (design 5.6).
	resumedItems, resumedJobs, err := cat.ResumeInterruptedJobs()
	if err != nil {
		logger.Error("cannot resume interrupted jobs", "error", err)
		os.Exit(1)
	}
	if resumedJobs > 0 {
		logger.Info("re-queued jobs interrupted by a previous run",
			"jobs", resumedJobs, "items", resumedItems)
	}
	if purged, err := cat.PurgeOldJobs(time.Now().Unix() - daemon.JobRetentionSeconds); err != nil {
		logger.Warn("cannot purge old jobs", "error", err)
	} else if purged > 0 {
		logger.Info("purged finished jobs past retention", "count", purged)
	}

	if *scanOnce {
		scanner := daemon.NewScanner(cfg, cat, logger)
		if err := scanner.ScanAll(); err != nil {
			logger.Error("scan failed", "error", err)
			os.Exit(1)
		}
		logger.Info("scan complete")
		return
	}

	// ONE mover for the process. The API server and the daemon share it, and
	// its mutex is what stops a pin arriving over HTTP from racing the quota
	// tick's move (design section 5.2).
	mover := daemon.NewMover(cfg, cat, logger)

	// The daemon is built first so the API server can be handed the SAME
	// scanner and the SAME job worker it runs. A second scanner would be a
	// second scan mutex; a second worker would be a wake channel nobody reads.
	d := daemon.New(cfg, cat, mover, logger)

	// Start API server in background
	srv := api.NewServer(api.Deps{
		Cfg:     cfg,
		Cat:     cat,
		Mover:   mover,
		Worker:  d.Worker(),
		Scanner: d.Scanner(),
		Logger:  logger,
	})
	go func() {
		if err := srv.ListenAndServe(cfg.Server.ListenAddr); err != nil {
			logger.Error("API server error", "error", err)
		}
	}()

	logger.Info("coldstore daemon starting", "config", *configPath)
	if err := d.Run(); err != nil {
		logger.Error("daemon exited with error", "error", err)
		os.Exit(1)
	}
}
