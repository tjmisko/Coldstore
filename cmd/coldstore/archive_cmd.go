package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tjmisko/coldstore/internal/archive"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/control"
	"github.com/tjmisko/coldstore/internal/journal"
	"github.com/tjmisko/coldstore/internal/scrub"
)

// Exit codes of the archive subcommands. 0 and 1 mean what they mean everywhere
// else in coldstore; 2 and 3 are specific to these commands.
const (
	exitArchiveOK          = 0
	exitArchiveFailure     = 1
	exitArchiveFindings    = 2
	exitArchiveFormatNewer = 3
)

const (
	subcommandImport        = "import"
	subcommandScrub         = "scrub"
	subcommandRebuildIndex  = "rebuild-index"
	subcommandFormatVersion = "format-version"

	defaultConfigPath      = "coldstore.toml"
	failuresPrinted        = 20
	scrubRateUnset         = -1
	bytesPerMiBForReports  = 1 << 20
	daemonRequiredMessage  = "coldstore import needs the running daemon; it is the only journal writer"
	archiveMissingMessage  = "no archive is configured here: declare a [[collections]] block or create the journal first"
	lockedByDaemonTemplate = "the archive is locked by another coldstore (the daemon?): %v\nstop it first, or drop --offline to ask the daemon"
)

// importPollInterval is how often import and online scrub report progress.
var importPollInterval = 2 * time.Second

// archiveSubcommand returns the subcommand named by the first argument. A first
// argument that is anything else, including every flag, goes down the existing
// flag-style path.
func archiveSubcommand(args []string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	switch args[0] {
	case subcommandImport, subcommandScrub, subcommandRebuildIndex, subcommandFormatVersion:
		return args[0], true
	}
	return "", false
}

// runArchiveCommand runs one of the four subcommands and returns its exit code.
// It takes the writers so tests can read what the operator would see.
func runArchiveCommand(ctx context.Context, name string, args []string, stdout, stderr io.Writer) int {
	switch name {
	case subcommandImport:
		return runImport(ctx, args, stdout, stderr)
	case subcommandScrub:
		return runScrub(ctx, args, stdout, stderr)
	case subcommandRebuildIndex:
		return runRebuildIndex(ctx, args, stdout, stderr)
	case subcommandFormatVersion:
		return runFormatVersion(args, stdout, stderr)
	}
	fmt.Fprintf(stderr, "coldstore: unknown subcommand %q\n", name)
	return exitArchiveFailure
}

// parseInterleaved parses flags and returns the positional arguments, accepting
// flags before and after them ("import <dir> --collection x" and the reverse).
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func newFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("coldstore "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "path to config file")
	return fs, configPath
}

func loadArchiveConfig(path string, stderr io.Writer) (*config.Config, bool) {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "coldstore: %v\n", err)
		return nil, false
	}
	return cfg, true
}

// archiveOptions is the archive.Options every entry point shares: the daemon, the
// offline commands and the tests all derive them from the config the same way.
func archiveOptions(cfg *config.Config, logger *slog.Logger) archive.Options {
	names := make([]string, 0, len(cfg.Collections))
	for _, collection := range cfg.Collections {
		names = append(names, collection.Name)
	}
	return archive.Options{
		Root:                 cfg.Storage.ColdStoragePath,
		Collections:          names,
		Logger:               logger,
		UploadConcurrency:    cfg.Archive.UploadConcurrency,
		ScrubInterval:        time.Duration(cfg.Archive.ScrubIntervalHours) * time.Hour,
		ScrubRateBytesPerSec: int64(cfg.Archive.ScrubRateMiBPerSec) * bytesPerMiBForReports,
	}
}

// archiveConfigured is the single condition under which the archive core is
// opened: a declared collection, or a journal directory that already exists.
// With neither, nothing archive-related runs and no directory is created.
func archiveConfigured(cfg *config.Config) bool {
	if len(cfg.Collections) > 0 {
		return true
	}
	info, err := os.Stat(filepath.Join(cfg.Storage.ColdStoragePath, "journal"))
	return err == nil && info.IsDir()
}

func quietLogger(stderr io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func collectionDeclared(cfg *config.Config, name string) bool {
	for _, collection := range cfg.Collections {
		if collection.Name == name {
			return true
		}
	}
	return false
}

func runImport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs, configPath := newFlagSet(subcommandImport, stderr)
	collection := fs.String("collection", "", "collection to import into (required)")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return exitArchiveFailure
	}
	if len(positional) != 1 || *collection == "" {
		fmt.Fprintln(stderr, "usage: coldstore import <dir> --collection <name> [--config coldstore.toml]")
		return exitArchiveFailure
	}
	cfg, ok := loadArchiveConfig(*configPath, stderr)
	if !ok {
		return exitArchiveFailure
	}
	if !collectionDeclared(cfg, *collection) {
		fmt.Fprintf(stderr, "coldstore: collection %q is not declared in %s\n", *collection, *configPath)
		return exitArchiveFailure
	}
	dir, err := filepath.Abs(positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "coldstore: %v\n", err)
		return exitArchiveFailure
	}

	client := control.NewClient(control.SocketPath(cfg.Storage.ColdStoragePath))
	id, err := client.StartImport(ctx, *collection, dir)
	if errors.Is(err, control.ErrDaemonNotRunning) {
		fmt.Fprintf(stderr, "coldstore: %s\n", daemonRequiredMessage)
		return exitArchiveFailure
	}
	if err != nil {
		fmt.Fprintf(stderr, "coldstore: starting the import: %v\n", err)
		return exitArchiveFailure
	}
	fmt.Fprintf(stdout, "import %d started: %s -> %s\n", id, dir, *collection)
	return followImport(ctx, client, id, stdout, stderr)
}

func followImport(ctx context.Context, client *control.Client, id int64, stdout, stderr io.Writer) int {
	ticker := time.NewTicker(importPollInterval)
	defer ticker.Stop()
	for {
		status, err := client.ImportStatus(ctx, id)
		if err != nil {
			fmt.Fprintf(stderr, "coldstore: reading import progress (the import may still be running): %v\n", err)
			return exitArchiveFailure
		}
		if status.State != "running" {
			return summariseImport(status, stdout, stderr)
		}
		fmt.Fprintf(stdout, "  seen %d  created %d  unchanged %d  other %d  failed %d  %d bytes\n",
			status.FilesSeen, status.FilesCreated, status.FilesUnchanged, status.FilesOther,
			status.FailedCount, status.BytesIngested)
		select {
		case <-ticker.C:
		case <-ctx.Done():
			fmt.Fprintln(stderr, "coldstore: interrupted; the import continues in the daemon")
			return exitArchiveFailure
		}
	}
}

func summariseImport(status archive.ImportStatus, stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "import %d %s: seen %d, created %d, unchanged %d, other %d, skipped %d, failed %d, %d bytes\n",
		status.ID, status.State, status.FilesSeen, status.FilesCreated, status.FilesUnchanged,
		status.FilesOther, status.SkippedCount, status.FailedCount, status.BytesIngested)
	for i, failure := range status.Failed {
		if i == failuresPrinted {
			fmt.Fprintf(stderr, "  ... and %d more failures\n", status.FailedCount-failuresPrinted)
			break
		}
		fmt.Fprintf(stderr, "  failed %s: %s\n", failure.Path, failure.Error)
	}
	if status.Error != "" {
		fmt.Fprintf(stderr, "coldstore: the import failed: %s\n", status.Error)
	}
	if status.State == "done" && status.FailedCount == 0 {
		return exitArchiveOK
	}
	return exitArchiveFailure
}

func runScrub(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs, configPath := newFlagSet(subcommandScrub, stderr)
	offline := fs.Bool("offline", false, "take the archive lock and scrub in this process (the daemon must be stopped)")
	rateMiB := fs.Int64("rate-mib", scrubRateUnset, "read rate limit in MiB/s; 0 is unthrottled (default: [archive] scrub_rate_mib_per_sec)")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return exitArchiveFailure
	}
	if len(positional) != 0 || (*rateMiB < 0 && *rateMiB != scrubRateUnset) {
		fmt.Fprintln(stderr, "usage: coldstore scrub [--offline] [--rate-mib N] [--config coldstore.toml]")
		return exitArchiveFailure
	}
	cfg, ok := loadArchiveConfig(*configPath, stderr)
	if !ok {
		return exitArchiveFailure
	}
	rate := int64(cfg.Archive.ScrubRateMiBPerSec)
	if *rateMiB != scrubRateUnset {
		rate = *rateMiB
	}
	if *offline {
		return scrubOffline(ctx, cfg, rate, stdout, stderr)
	}
	return scrubOnline(ctx, cfg, rate, stdout, stderr)
}

func scrubOffline(ctx context.Context, cfg *config.Config, rateMiB int64, stdout, stderr io.Writer) int {
	if !archiveConfigured(cfg) {
		fmt.Fprintf(stderr, "coldstore: %s\n", archiveMissingMessage)
		return exitArchiveFailure
	}
	opts := archiveOptions(cfg, quietLogger(stderr))
	opts.ScrubRateBytesPerSec = rateMiB * bytesPerMiBForReports
	report, err := archive.ScrubOffline(ctx, opts)
	if errors.Is(err, journal.ErrLocked) {
		fmt.Fprintf(stderr, "coldstore: "+lockedByDaemonTemplate+"\n", err)
		return exitArchiveFailure
	}
	if err != nil {
		fmt.Fprintf(stderr, "coldstore: scrubbing: %v\n", err)
		return exitArchiveFailure
	}
	return reportScrub(report, stdout, stderr)
}

func scrubOnline(ctx context.Context, cfg *config.Config, rateMiB int64, stdout, stderr io.Writer) int {
	client := control.NewClient(control.SocketPath(cfg.Storage.ColdStoragePath))
	err := client.StartScrub(ctx, rateMiB)
	var refused *control.StatusError
	switch {
	case errors.Is(err, control.ErrDaemonNotRunning):
		fmt.Fprintln(stderr, "coldstore: the daemon is not running; stop here and use --offline to scrub without it")
		return exitArchiveFailure
	case errors.As(err, &refused) && refused.Code == 409:
		fmt.Fprintln(stdout, "a scrub is already running; waiting for it")
	case err != nil:
		fmt.Fprintf(stderr, "coldstore: starting the scrub: %v\n", err)
		return exitArchiveFailure
	default:
		fmt.Fprintln(stdout, "scrub started")
	}

	ticker := time.NewTicker(importPollInterval)
	defer ticker.Stop()
	for {
		status, err := client.ScrubStatus(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "coldstore: reading scrub progress: %v\n", err)
			return exitArchiveFailure
		}
		if !status.Running && status.Last != nil {
			return reportScrub(*status.Last, stdout, stderr)
		}
		fmt.Fprintf(stdout, "  checked %d of %d blobs\n", status.Checked, status.Total)
		select {
		case <-ticker.C:
		case <-ctx.Done():
			fmt.Fprintln(stderr, "coldstore: interrupted; the scrub continues in the daemon")
			return exitArchiveFailure
		}
	}
}

func reportScrub(report scrub.Report, stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "scrub checked %d blobs, %d bytes; %d findings, %d stray files\n",
		report.Checked, report.CheckedBytes, len(report.Findings), len(report.Stray))
	for _, finding := range report.Findings {
		fmt.Fprintf(stderr, "  %s %s %s\n", finding.Kind, finding.Hash, finding.Reason)
	}
	for _, stray := range report.Stray {
		fmt.Fprintf(stderr, "  stray %s\n", stray)
	}
	if len(report.Findings) > 0 {
		return exitArchiveFindings
	}
	return exitArchiveOK
}

func runRebuildIndex(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs, configPath := newFlagSet(subcommandRebuildIndex, stderr)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return exitArchiveFailure
	}
	if len(positional) != 0 {
		fmt.Fprintln(stderr, "usage: coldstore rebuild-index [--config coldstore.toml]")
		return exitArchiveFailure
	}
	cfg, ok := loadArchiveConfig(*configPath, stderr)
	if !ok {
		return exitArchiveFailure
	}
	if !archiveConfigured(cfg) {
		fmt.Fprintf(stderr, "coldstore: %s\n", archiveMissingMessage)
		return exitArchiveFailure
	}
	report, err := archive.RebuildIndex(ctx, archiveOptions(cfg, quietLogger(stderr)))
	if errors.Is(err, journal.ErrLocked) {
		fmt.Fprintf(stderr, "coldstore: the archive is locked by another coldstore (the daemon?): %v\n"+
			"rebuild-index is offline only: stop the daemon first\n", err)
		return exitArchiveFailure
	}
	if err != nil {
		fmt.Fprintf(stderr, "coldstore: rebuilding the index: %v\n", err)
		return exitArchiveFailure
	}
	fmt.Fprintf(stdout, "records replayed  %d\n", report.Records)
	fmt.Fprintf(stdout, "entries           %d\n", report.Entries)
	fmt.Fprintf(stdout, "views linked      %d\n", report.ViewsLinked)
	if torn := report.Torn; torn != nil {
		fmt.Fprintf(stdout, "warning: the journal tail was torn: %s truncated at offset %d (%d bytes, %d intact records dropped)\n",
			torn.Segment, torn.Offset, torn.DroppedBytes, torn.DroppedRecs)
	}
	return exitArchiveOK
}

func runFormatVersion(args []string, stdout, stderr io.Writer) int {
	fs, configPath := newFlagSet(subcommandFormatVersion, stderr)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return exitArchiveFailure
	}
	if len(positional) != 0 {
		fmt.Fprintln(stderr, "usage: coldstore format-version [--config coldstore.toml]")
		return exitArchiveFailure
	}
	// Inspection load: this is a read-only report and must work on a box whose
	// config the daemon would refuse.
	cfg, err := config.LoadForInspection(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "coldstore: %v\n", err)
		return exitArchiveFailure
	}
	report, err := archive.FormatVersions(cfg.Storage.ColdStoragePath)
	if err != nil {
		fmt.Fprintf(stderr, "coldstore: reading format versions: %v\n", err)
		return exitArchiveFailure
	}
	fmt.Fprintf(stdout, "journal  binary %d  disk %d\n", report.JournalBinary, report.JournalDisk)
	fmt.Fprintf(stdout, "index    binary %d  disk %d\n", report.IndexBinary, report.IndexDisk)
	if report.Newer {
		fmt.Fprintln(stderr, "coldstore: the archive on disk was written by a newer coldstore than this binary")
		return exitArchiveFormatNewer
	}
	return exitArchiveOK
}

// signalContext is cancelled by SIGINT or SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
