package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/tjmisko/coldstore/internal/api"
	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/daemon"
)

func main() {
	configPath := flag.String("config", "coldstore.toml", "path to config file")
	scanOnce := flag.Bool("scan", false, "run a single catalog scan and exit")
	checkOnly := flag.Bool("check", false, "validate the config and the machine, print the result, and exit")
	flag.Parse()

	// Config is loaded and validated before anything else exists -- before the
	// catalog is opened and long before a socket is bound. config.Load performs
	// the D5 binding refusal, so an unauthenticated control plane cannot reach a
	// listening port even for the instant it takes the daemon to fail later on
	// (design 3.2, steps 1-2).
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "coldstore: %v\n", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	warnings, err := cfg.CheckEnvironment()
	if err != nil {
		fmt.Fprintf(os.Stderr, "coldstore: %v\n", err)
		os.Exit(1)
	}
	for _, w := range warnings {
		logger.Warn("preflight", "detail", w)
	}

	if *checkOnly {
		reportPreflight(os.Stdout, cfg, *configPath, warnings)
		return
	}

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

// reportPreflight prints what --check found. It exists so the owner can answer
// "is this box ready?" without starting the daemon, touching the catalog or
// binding a port -- everything above this point is read-only apart from one
// create-and-delete writability probe per directory.
func reportPreflight(w io.Writer, cfg *config.Config, configPath string, warnings []string) {
	loopback, _ := config.IsLoopbackListenAddr(cfg.Server.ListenAddr)
	reach := "LAN"
	if loopback {
		reach = "this host only"
	}
	auth := "DISABLED"
	if cfg.Server.Username != "" && cfg.Server.Password != "" {
		auth = "basic auth as " + cfg.Server.Username
	}

	fmt.Fprintf(w, "config           %s\n", configPath)
	fmt.Fprintf(w, "cold storage     %s (exists, writable%s)\n",
		cfg.Storage.ColdStoragePath, describeColdStorageMount(cfg.Storage.ColdStoragePath))
	fmt.Fprintf(w, "listen           %s -- reachable from %s\n", cfg.Server.ListenAddr, reach)
	fmt.Fprintf(w, "auth             %s\n", auth)
	fmt.Fprintf(w, "folders          %d declared\n", len(cfg.Folders))
	for _, f := range cfg.Folders {
		fmt.Fprintf(w, "  %-20s %s (seed budget %s)\n", f.Label, f.Path, f.Budget)
	}

	printReadWritePaths(w, cfg)

	if len(warnings) == 0 {
		fmt.Fprintln(w, "\npreflight OK, no warnings")
		return
	}
	fmt.Fprintf(w, "\npreflight OK with %d warning(s):\n", len(warnings))
	for _, warning := range warnings {
		fmt.Fprintf(w, "  - %s\n", warning)
	}
	fmt.Fprintln(w, "\nWarnings are not fatal. A folder that is missing or has no .stfolder is")
	fmt.Fprintln(w, "skipped until it appears, which is what lets the daemon survive being")
	fmt.Fprintln(w, "started before its mount.")
}

// describeColdStorageMount says whether cold storage is its own mounted
// filesystem, because --check has no other way to raise the one mistake that
// costs data outright.
//
// checkColdStorage passes on an UNMOUNTED fstab mountpoint: the directory has
// to exist for mount(8) to work, so it is there, it is a directory, and it is
// writable -- on the root filesystem. The daemon then writes a fresh catalog
// and every archived file there while Syncthing propagates the deletions to
// every device the owner has, and the real archive reappears shadowed and
// invisible when the volume is finally mounted. The unit's RequiresMountsFor=
// is what prevents that at boot; nothing prevents it if the owner starts the
// daemon by hand.
//
// A plain directory is a supported layout, so this is a description and not a
// verdict. It is worded so that reading past it is a choice.
func describeColdStorageMount(path string) string {
	state, err := config.DescribeMount(path)
	if err != nil {
		return ""
	}
	if !state.IsMountPoint {
		return "; NOT a separate mount -- confirm this is deliberate"
	}
	if state.FSType == "" {
		return "; mounted"
	}
	return "; mounted, " + state.FSType
}

// printReadWritePaths emits the exact ReadWritePaths= line this config needs,
// and says plainly that --check cannot verify the one in the unit.
//
// This exists because --check is otherwise a liar about the single most common
// first-deployment mistake. Run by hand it is NOT inside the unit's sandbox, so
// every path looks writable and it prints "preflight OK" for a configuration
// that will exit 1 the moment systemd starts it with ProtectSystem=strict and a
// folder missing from ReadWritePaths=. Printing the line the owner should have
// is the most --check can honestly do from outside; the daemon itself performs
// the same probe inside the sandbox on every real start, which is where that
// mistake is actually caught.
func printReadWritePaths(w io.Writer, cfg *config.Config) {
	paths := make([]string, 0, len(cfg.Folders)+1)
	paths = append(paths, cfg.Storage.ColdStoragePath)
	for _, f := range cfg.Folders {
		paths = append(paths, f.Path)
	}

	fmt.Fprintf(w, "\nsystemd          ReadWritePaths=%s\n", strings.Join(paths, " "))
	fmt.Fprintln(w, "                 --check runs OUTSIDE the unit's sandbox and cannot verify")
	fmt.Fprintln(w, "                 this line. Copy it into /etc/systemd/system/coldstore.service;")
	fmt.Fprintln(w, "                 a path missing from it makes the daemon exit 1 at startup")
	fmt.Fprintln(w, "                 with EROFS, not silently misbehave later.")
}
