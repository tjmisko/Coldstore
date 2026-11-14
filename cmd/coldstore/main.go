package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tjmisko/coldstore/internal/api"
	"github.com/tjmisko/coldstore/internal/archive"
	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/control"
	"github.com/tjmisko/coldstore/internal/daemon"
	"github.com/tjmisko/coldstore/internal/diagnostic"
	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/journal"
	"github.com/tjmisko/coldstore/internal/thumbnail"
)

func main() {
	// The archive subcommands are dispatched before flag parsing: flag.Parse
	// would stop at "import" and treat everything after it as positional.
	if name, ok := archiveSubcommand(os.Args[1:]); ok {
		ctx, stop := signalContext()
		code := runArchiveCommand(ctx, name, os.Args[2:], os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}

	configPath := flag.String("config", "coldstore.toml", "path to config file")
	scanOnce := flag.Bool("scan", false, "run a single catalog scan and exit")
	checkOnly := flag.Bool("check", false, "validate the config and the machine, print the result, and exit")
	statusOnly := flag.Bool("status", false, "print an offline, read-only archive health report and exit")
	jsonOutput := flag.Bool("json", false, "with --status, emit structured JSON")
	renameFolderLabel := flag.String("rename-folder-label", "",
		"offline catalog identity migration in OLD=NEW form (service must be stopped)")
	renameFolderBudget := flag.String("rename-folder-budget", "",
		"temporary positive budget for --rename-folder-label (for example 2TB)")
	flag.Parse()
	// Anything left after the flags is a subcommand typed in the wrong place
	// ("--config X import dir"). Without this check it would start a second
	// daemon on the live catalog, so refuse before anything is opened.
	if err := rejectPositionalArguments(flag.Args()); err != nil {
		fmt.Fprintf(os.Stderr, "coldstore: %v\n", err)
		os.Exit(1)
	}
	if err := validateModeFlags(*scanOnce, *checkOnly, *statusOnly, *jsonOutput,
		*renameFolderLabel, *renameFolderBudget); err != nil {
		fmt.Fprintf(os.Stderr, "coldstore: %v\n", err)
		os.Exit(diagnostic.ExitFailure)
	}

	// Status dispatches before normal config loading and CheckEnvironment. It
	// neither reads service-only credential environment variables nor performs
	// the create/delete writability probes used by daemon preflight.
	if *statusOnly {
		cfg, err := config.LoadForInspection(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "coldstore: %v\n", err)
			os.Exit(diagnostic.ExitFailure)
		}
		dbPath := filepath.Join(cfg.Storage.ColdStoragePath, "catalog.db")
		code, err := writeStatus(context.Background(), os.Stdout, cfg, dbPath, *jsonOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "coldstore: rendering status: %v\n", err)
			os.Exit(diagnostic.ExitFailure)
		}
		if code != diagnostic.ExitHealthy {
			os.Exit(code)
		}
		return
	}

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

	var renameOld, renameNew string
	var renameBudgetBytes int64
	if *renameFolderLabel != "" {
		renameOld, renameNew, err = parseFolderLabelRename(*renameFolderLabel)
		if err != nil {
			fmt.Fprintf(os.Stderr, "coldstore: %v\n", err)
			os.Exit(1)
		}
		renameBudgetBytes, err = config.ParseBudget(*renameFolderBudget)
		if err != nil {
			fmt.Fprintf(os.Stderr, "coldstore: --rename-folder-budget: %v\n", err)
			os.Exit(1)
		}
		if err := refuseRunningDaemon(cfg.Server.ListenAddr); err != nil {
			fmt.Fprintf(os.Stderr, "coldstore: %v\n", err)
			os.Exit(1)
		}
	}

	warnings, err := cfg.CheckEnvironment()
	if err != nil {
		fmt.Fprintf(os.Stderr, "coldstore: %v\n", err)
		os.Exit(1)
	}
	for _, w := range warnings {
		logger.Warn("preflight", "detail", w)
	}

	dbPath := cfg.Storage.ColdStoragePath + "/catalog.db"
	if *renameFolderLabel != "" {
		cat, err := catalog.Open(dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "coldstore: opening catalog for folder-label migration: %v\n", err)
			os.Exit(1)
		}
		defer cat.Close()

		result, err := daemon.MigrateFolderLabel(cfg, cat, renameOld, renameNew, renameBudgetBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "coldstore: folder-label migration failed: %v\n", err)
			os.Exit(1)
		}
		movedRoots := "none (no source directories existed)"
		if len(result.MovedRoots) != 0 {
			movedRoots = strings.Join(result.MovedRoots, ", ")
		}
		fmt.Printf("folder-label migration complete: %s -> %s\n", renameOld, renameNew)
		fmt.Printf("catalog rows      files=%d persistent_paths=%d events=%d jobs=%d\n",
			result.Catalog.Files, result.Catalog.PersistentPaths,
			result.Catalog.Events, result.Catalog.Jobs)
		fmt.Printf("moved data roots  %s\n", movedRoots)
		fmt.Printf("new budget        %s (%d bytes)\n", *renameFolderBudget, renameBudgetBytes)
		return
	}
	if *checkOnly {
		if err := checkCatalogFolderIdentities(cfg, dbPath); err != nil {
			fmt.Fprintf(os.Stderr, "coldstore: %v\n", err)
			os.Exit(1)
		}
		reportPreflight(os.Stdout, cfg, *configPath, warnings)
		return
	}

	// Take the archive lock BEFORE the legacy catalog is touched, so a second
	// process stops here instead of after it has reconciled folders and resumed
	// jobs on a live daemon's data. archive.Open takes the same lock itself, so
	// it is released just before that call. The kernel drops it on any exit.
	var startupLock *journal.Lock
	if !*scanOnce && archiveConfigured(cfg) {
		startupLock, err = journal.AcquireLock(filepath.Join(cfg.Storage.ColdStoragePath, "journal"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "coldstore: %s\n", archiveOpenAdvice(fmt.Errorf("opening archive: %w", err)))
			os.Exit(1)
		}
		defer startupLock.Release()
	}

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

	// The process owns the signal context: the daemon, the archive's background
	// workers and the control socket all stop on it, and the archive is closed
	// only after the daemon's RunContext has returned.
	ctx, stopSignals := signalContext()
	defer stopSignals()

	// The archive core is opened only when a collection is declared or a journal
	// already exists. Any failure is fatal: a daemon that cannot say which of the
	// archive and the journal is newer must not serve either. It is opened before
	// the listener binds so no request can reach a half-open archive.
	var archiveService *archive.Service
	var controlServer *control.Server
	if archiveConfigured(cfg) {
		if startupLock != nil {
			if err := startupLock.Release(); err != nil {
				logger.Warn("releasing the startup archive lock", "error", err)
			}
		}
		archiveService, controlServer = openArchive(ctx, cfg, cat, logger)
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
		Archive: archiveService,
	})
	go func() {
		if err := srv.ListenAndServe(cfg.Server.ListenAddr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("API server error", "error", err)
		}
	}()

	logger.Info("coldstore daemon starting", "config", *configPath)
	runErr := d.RunContext(ctx)
	shutdownServing(srv, controlServer, archiveService, logger)
	if runErr != nil {
		logger.Error("daemon exited with error", "error", runErr)
		os.Exit(1)
	}
}

// openArchive opens the archive core and the control socket, or exits 1.
func openArchive(ctx context.Context, cfg *config.Config, cat *catalog.Catalog, logger *slog.Logger) (*archive.Service, *control.Server) {
	opts := archiveOptions(cfg, logger)
	opts.Render = thumbnail.NewGenerator(cfg.Storage.ColdStoragePath, cat, logger).RenderTo
	service, err := archive.Open(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "coldstore: %v\n", archiveOpenAdvice(err))
		os.Exit(1)
	}
	service.Start(ctx)
	server, err := control.Start(ctx, service, control.SocketPath(cfg.Storage.ColdStoragePath), logger)
	if err != nil {
		logger.Error("cannot start the control socket", "error", err)
		_ = service.Close()
		os.Exit(1)
	}
	return service, server
}

// archiveOpenAdvice names which side is newer or who holds the lock; every
// other error is printed as it is.
func archiveOpenAdvice(err error) string {
	switch {
	case errors.Is(err, journal.ErrFormatNewer):
		return fmt.Sprintf("the archive on disk was written by a newer coldstore than this binary; "+
			"refusing to open it (run `coldstore format-version` for the numbers): %v", err)
	case errors.Is(err, index.ErrFormatNewer):
		return fmt.Sprintf("index.db was written by a newer coldstore than this binary; "+
			"refusing to open it: %v", err)
	case errors.Is(err, journal.ErrLocked):
		return fmt.Sprintf("another coldstore holds the archive (journal/LOCK): %v", err)
	}
	return err.Error()
}

// shutdownServing stops what serves requests, then closes the archive. The HTTP
// server goes first so an in-flight upload finishes (or is cut) before the
// journal and index close under it.
func shutdownServing(srv *api.Server, controlServer *control.Server, service *archive.Service, logger *slog.Logger) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("API server shutdown", "error", err)
	}
	if controlServer != nil {
		if err := controlServer.Close(); err != nil {
			logger.Warn("control socket shutdown", "error", err)
		}
	}
	if service == nil {
		return
	}
	if err := service.Close(); err != nil {
		logger.Error("closing the archive", "error", err)
	}
}

// rejectPositionalArguments refuses arguments left over after flag parsing.
func rejectPositionalArguments(args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("unexpected argument %q: coldstore takes no positional arguments in daemon mode; "+
		"a subcommand must come first, e.g. `coldstore import <dir> --collection <name> --config <file>` "+
		"(subcommands: import, scrub, rebuild-index, format-version)", args[0])
}

func validateModeFlags(scan, check, status, jsonOutput bool, renameSpec, renameBudget string) error {
	modes := 0
	for _, selected := range []bool{scan, check, status, renameSpec != ""} {
		if selected {
			modes++
		}
	}
	if modes > 1 {
		return fmt.Errorf("--scan, --check, --status and --rename-folder-label are mutually exclusive")
	}
	if jsonOutput && !status {
		return fmt.Errorf("--json requires --status")
	}
	if renameSpec == "" && renameBudget != "" {
		return fmt.Errorf("--rename-folder-budget requires --rename-folder-label")
	}
	if renameSpec != "" && renameBudget == "" {
		return fmt.Errorf("--rename-folder-label requires --rename-folder-budget")
	}
	return nil
}

func parseFolderLabelRename(spec string) (string, string, error) {
	oldLabel, newLabel, ok := strings.Cut(spec, "=")
	if !ok || oldLabel == "" || newLabel == "" || strings.Contains(newLabel, "=") {
		return "", "", fmt.Errorf("--rename-folder-label must be exactly OLD=NEW")
	}
	return oldLabel, newLabel, nil
}

// The migration is an offline maintenance operation. A successful connection
// to the configured listener is strong evidence that the daemon was not
// stopped; refusing here prevents the common operator error. The catalog also
// rejects in-flight file moves and jobs as a second, state-based guard.
func refuseRunningDaemon(listenAddr string) error {
	conn, err := net.DialTimeout("tcp", listenAddr, 300*time.Millisecond)
	if err != nil {
		return nil
	}
	conn.Close()
	return fmt.Errorf("a process is listening at %s; stop coldstore before --rename-folder-label",
		listenAddr)
}

func writeStatus(ctx context.Context, w io.Writer, cfg *config.Config, dbPath string, jsonOutput bool) (int, error) {
	report := diagnostic.NewBuilder().Build(ctx, cfg, dbPath)
	var err error
	if jsonOutput {
		err = diagnostic.WriteJSON(w, report)
	} else {
		err = diagnostic.WriteHuman(w, report)
	}
	return report.ExitCode, err
}

// checkCatalogFolderIdentities is the only catalog access performed by
// --check. ReadFolderIdentities uses a read-only, query-only SQLite connection
// and does not create or migrate a catalog.
func checkCatalogFolderIdentities(cfg *config.Config, dbPath string) error {
	identities, err := catalog.ReadFolderIdentities(dbPath)
	if err != nil {
		return fmt.Errorf("checking catalog folder identities: %w", err)
	}
	return daemon.ValidateFolderIdentityChanges(cfg, identities)
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
	switch cfg.Server.AuthenticationMode() {
	case config.AuthModeBasic:
		auth = "basic auth as " + cfg.Server.Username
	case config.AuthModeTailscale:
		count := len(cfg.Server.TailscaleAllowedUsers)
		principal := "users"
		if count == 1 {
			principal = "user"
		}
		auth = fmt.Sprintf("Tailscale Serve identity allowlist (%d %s)", count, principal)
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

	if len(cfg.Collections) > 0 {
		fmt.Fprintf(w, "collections      %d declared\n", len(cfg.Collections))
		for _, collection := range cfg.Collections {
			fmt.Fprintf(w, "  %s\n", collection.Name)
		}
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
