package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/thumbnail"
)

const (
	// thumbnailBatchSize is how many files one FilesNeedingThumbnails query
	// materialises. The catalog caps this too; keeping it explicit here makes
	// the batching visible at the call site.
	thumbnailBatchSize = 200

	// maxThumbnailBatchesPerPass is a safety stop, not a throughput knob: the
	// pass already terminates when the queue stops yielding files it has not
	// tried. It exists so a catalog that somehow keeps producing rows cannot
	// pin a worker forever.
	maxThumbnailBatchesPerPass = 500

	// initialScanRetryDelay is how long Run waits between failed initial scans.
	initialScanRetryDelay = 5 * time.Second

	// offloadCandidateBatchSize caps how many candidate rows one enforcement
	// pass materialises. Candidates are consumed oldest-first and the pass
	// stops at the budget, so a folder that needs more than this converges
	// over several ticks instead of loading a whole camera roll into memory.
	offloadCandidateBatchSize = 5000

	// bulkEvictionFileThreshold and bulkEvictionFractionPercent are design
	// section 5.10's second fix: mass eviction must be loud whatever caused it.
	// The pre-flight on POST /jobs only covers the restore-shaped cause; a
	// folder that goes over budget because the owner shot a wedding, or lowered
	// the budget in the UI, sheds just as many files with no warning at all.
	bulkEvictionFileThreshold   = 100
	bulkEvictionFractionPercent = 10

	// bulkEvictionMinFiles is the floor under the percentage arm. Without it
	// "more than 10% of the folder's file count" makes every ordinary tick on
	// a small folder a mass eviction: one file out of six is 16%, so a folder
	// of a dozen holiday videos would log a WARN and write a bulk_eviction row
	// every time it shed a single file. The events table is what the Activity
	// list reads, and a warning that fires on the normal case is not a
	// warning. Below this many files the percentage says nothing worth
	// interrupting the owner for; above 100 the absolute arm still fires
	// whatever the folder's size.
	bulkEvictionMinFiles = 10

	// JobRetentionSeconds is how long a terminal job stays readable before the
	// catalog tick purges it. Without a purge the jobs and job_items tables
	// grow without bound (design section 5.7).
	JobRetentionSeconds = 7 * 24 * 60 * 60

	// wedgeWarningRefreshSeconds is how long a wedge warning stays latched
	// before the epilogue says it again. A folder whose pinned bytes exceed
	// its budget is a persistent problem and should keep saying so daily,
	// rather than leaving one row from last Tuesday to scroll off the list.
	wedgeWarningRefreshSeconds = 24 * 60 * 60
)

// thumbnailExts are all extensions the thumbnail generator supports.
var thumbnailExts = []string{
	".jpg", ".jpeg", ".png", ".webp",
	".heif", ".heic",
	".mp4", ".mov", ".avi", ".mkv", ".webm",
}

type Daemon struct {
	cfg                 *config.Config
	cat                 *catalog.Catalog
	logger              *slog.Logger
	scanner             *Scanner
	mover               *Mover
	worker              *Worker
	thumbGen            *thumbnail.Generator
	initialScanComplete bool

	// Thumbnail generation runs off the select loop so a long pass cannot
	// delay SIGTERM. thumbRunning admits one pass at a time; thumbWG lets
	// shutdown wait for the running pass to notice the cancelled context.
	thumbRunning atomic.Bool
	thumbWG      sync.WaitGroup
}

// New builds the daemon around an existing Mover. The mover is a parameter
// rather than something the daemon makes for itself because the API server
// needs the SAME one: two movers over one catalog are two mutexes, and a pin
// arriving through the API would no longer be serialised against the quota
// tick's move (design section 5.2).
func New(cfg *config.Config, cat *catalog.Catalog, mover *Mover, logger *slog.Logger) *Daemon {
	// The daemon owns the scanner and the worker, and hands them out through
	// Scanner() and Worker(), because both are single-instance-per-process for
	// reasons that break silently if violated: a second Scanner is a second
	// scan mutex and two overlapping walks, and a second Worker is a second
	// wake channel that POST /jobs would poke while the running one sleeps.
	scanner := NewScanner(cfg, cat, logger)
	return &Daemon{
		cfg:      cfg,
		cat:      cat,
		logger:   logger,
		scanner:  scanner,
		mover:    mover,
		worker:   NewWorker(cat, mover, scanner, logger),
		thumbGen: thumbnail.NewGenerator(cfg.Storage.ColdStoragePath, cat, logger),
	}
}

// Scanner returns the process's one scanner, whose mutex is what stops the
// catalog tick and a scan job walking the same tree at the same time.
func (d *Daemon) Scanner() *Scanner { return d.scanner }

// Worker returns the process's one job worker. The API server needs it to poke
// the wake channel after committing a job.
func (d *Daemon) Worker() *Worker { return d.worker }

// SetSpaceCheck replaces the disk space check function, for testing. The check
// itself lives in the mover, which is the only thing that moves bytes.
func (d *Daemon) SetSpaceCheck(fn func(string) (float64, error)) {
	d.mover.SetSpaceCheck(fn)
}

// Run starts the daemon and returns when SIGINT or SIGTERM arrives.
func (d *Daemon) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return d.RunContext(ctx)
}

// RunContext is Run driven by a caller-supplied context. The context is
// threaded through scanning, quota enforcement and thumbnail generation, so
// cancellation is honoured mid-work rather than only between ticks. It returns
// once every in-flight unit of work has stopped.
func (d *Daemon) RunContext(ctx context.Context) error {
	// A background thumbnail pass may outlive the loop below by the length of
	// one file; do not return until it has stopped.
	defer d.thumbWG.Wait()

	// Retry initial scan up to 3 times before entering the main loop.
	// Quota enforcement is gated on a successful initial scan.
	const maxRetries = 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			d.logger.Info("shutdown requested during initial scan")
			return nil
		}
		d.logger.Info("running initial catalog scan", "attempt", attempt)
		err := d.scanner.ScanAllContext(ctx)
		if err == nil {
			d.initialScanComplete = true
			break
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			d.logger.Info("shutdown requested during initial scan")
			return nil
		}
		d.logger.Error("initial scan failed", "error", err, "attempt", attempt)
		if attempt == maxRetries {
			d.logger.Warn("initial scan failed after all retries, starting without offload capability")
			break
		}
		if !sleepContext(ctx, initialScanRetryDelay) {
			d.logger.Info("shutdown requested during initial scan")
			return nil
		}
	}

	// The worker starts after the initial scan, not before it. A scan job left
	// pending by a kill would otherwise collide with the initial walk and fail
	// with ErrScanInProgress for no reason; restore and archive jobs waiting a
	// few seconds longer costs nothing.
	var workerWG sync.WaitGroup
	workerWG.Add(1)
	go func() {
		defer workerWG.Done()
		d.worker.Run(ctx)
	}()
	defer workerWG.Wait()

	d.startThumbnailPass(ctx)

	scanTicker := time.NewTicker(time.Duration(d.cfg.Daemon.ScanIntervalMinutes) * time.Minute)
	defer scanTicker.Stop()

	catalogTicker := time.NewTicker(time.Duration(d.cfg.Daemon.CatalogSyncIntervalMinutes) * time.Minute)
	defer catalogTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			d.logger.Info("shutting down, waiting for in-flight work")
			return nil
		case <-scanTicker.C:
			if !d.initialScanComplete {
				d.logger.Warn("skipping quota enforcement: initial scan incomplete")
				continue
			}
			d.logger.Info("quota enforcement tick")
			d.safeEnforceQuotas(ctx)
		case <-catalogTicker.C:
			d.logger.Info("catalog sync tick")
			if err := d.scanner.ScanAllContext(ctx); err != nil {
				if ctx.Err() != nil {
					continue
				}
				d.logger.Error("catalog sync scan failed", "error", err)
			} else if !d.initialScanComplete {
				d.initialScanComplete = true
				d.logger.Info("initial scan completed on catalog sync tick")
			}
			d.startThumbnailPass(ctx)
			d.purgeOldJobs()
		}
	}
}

// purgeOldJobs drops terminal jobs past their retention window. It is a
// housekeeping call: a failure is worth a log line and nothing more.
func (d *Daemon) purgeOldJobs() {
	cutoff := time.Now().Unix() - JobRetentionSeconds
	purged, err := d.cat.PurgeOldJobs(cutoff)
	if err != nil {
		d.logger.Warn("cannot purge old jobs", "error", err)
		return
	}
	if purged > 0 {
		d.logger.Info("purged finished jobs past retention", "count", purged)
	}
}

// sleepContext waits for d, returning false if the context was cancelled first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// safeEnforceQuotas wraps enforceQuotas with panic recovery so a bug
// in quota enforcement doesn't crash the entire daemon.
func (d *Daemon) safeEnforceQuotas(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			d.logger.Error("panic in quota enforcement", "panic", r)
		}
	}()
	d.EnforceQuotasContext(ctx)
}

// EnforceQuotas runs quota enforcement across all configured folders.
func (d *Daemon) EnforceQuotas() {
	d.EnforceQuotasContext(context.Background())
}

// EnforceQuotasContext is EnforceQuotas with cancellation. It stops between
// folders and between files, and an offload interrupted mid-copy reverts to
// "synced" with the source untouched.
//
// Policy comes from the database, never from d.cfg.Folders (D1, design
// section 3.5). That is what makes a budget edited in the UI take effect on
// the very next tick with no restart, and it is why a read error skips the
// whole tick instead of falling back to TOML: the TOML budget is a seed value
// that may be years out of date, and enforcing it would archive a folder the
// owner had deliberately given more room.
func (d *Daemon) EnforceQuotasContext(ctx context.Context) {
	folders, err := d.cat.ActiveFolderConfigs()
	if err != nil {
		d.logger.Error("cannot read folder policy, skipping enforcement tick", "error", err)
		return
	}

	for _, folder := range folders {
		if ctx.Err() != nil {
			d.logger.Info("quota enforcement interrupted", "reason", ctx.Err())
			return
		}
		if err := d.enforceFolder(ctx, folder); err != nil {
			if ctx.Err() != nil {
				d.logger.Info("quota enforcement interrupted", "folder", folder.FolderLabel)
				return
			}
			d.logger.Error("quota enforcement failed", "folder", folder.FolderLabel, "error", err)
		}
	}
}

func (d *Daemon) enforceFolder(ctx context.Context, folder catalog.FolderConfigRecord) error {
	// The epilogue runs on EVERY exit -- the happy path, the unset budget, an
	// unavailable folder, an abandoned pass -- because it does two things that
	// must not depend on why the loop did or did not run: it stamps
	// last_enforced_at, so "last checked N minutes ago" never freezes on a
	// live daemon, and it applies the wedge latch, which has to be able to
	// CLEAR on the within-budget path (design section 4.4).
	defer d.enforceEpilogue(folder.FolderLabel)

	// Nothing may be moved into or out of a folder whose root is not really
	// there. Deduped at an hour: an unmounted drive stays unmounted, and one
	// row per folder per tick buries everything else in the events table.
	if err := FolderReady(folder.SyncthingPath); err != nil {
		d.cat.LogEventDeduped("folder_unavailable", folder.FolderLabel, "", err.Error(), 60)
		return err
	}

	// A zero or negative budget is not "archive everything": it is a folder
	// whose budget has never been set, and the only safe reading of an unset
	// cap is no cap at all.
	if folder.BudgetBytes <= 0 {
		d.logger.Warn("folder has no positive budget, not enforcing",
			"folder", folder.FolderLabel, "budget", folder.BudgetBytes)
		return nil
	}

	// FolderSizes: enforcement budgets the folder's ON-DEVICE FOOTPRINT, which
	// is status IN ('synced','archiving','restoring'). A file being moved out
	// still occupies the device until the move commits, and a file being moved
	// in already claims its bytes. Counting only 'synced' understates the
	// footprint exactly when moves are in flight and makes the daemon offload
	// past the budget.
	onDevice, _, err := d.cat.FolderSizes(folder.FolderLabel)
	if err != nil {
		return fmt.Errorf("getting folder size: %w", err)
	}

	if onDevice <= folder.BudgetBytes {
		d.logger.Debug("folder within budget",
			"folder", folder.FolderLabel,
			"size", onDevice,
			"budget", folder.BudgetBytes)
		return nil
	}

	d.logger.Info("folder over budget, offloading",
		"folder", folder.FolderLabel,
		"size", onDevice,
		"budget", folder.BudgetBytes,
		"excess", onDevice-folder.BudgetBytes)

	// Persistent globs come from the database only. The TOML globs were copied
	// into persistent_paths when the folder was first seeded (section 3.2), so
	// merging cfg here would resurrect a glob the user has since deleted.
	persistentPatterns, err := d.cat.ListPersistentPaths(folder.FolderLabel)
	if err != nil {
		return err
	}
	allPatterns := make([]string, 0, len(persistentPatterns))
	for _, p := range persistentPatterns {
		allPatterns = append(allPatterns, p.Pattern)
	}

	candidates, err := d.cat.OffloadCandidates(folder.FolderLabel, offloadCandidateBatchSize)
	if err != nil {
		return fmt.Errorf("getting offload candidates: %w", err)
	}

	// Grace is per folder and comes from the database too. A negative value
	// clamps to zero rather than reaching into the future for a cutoff.
	graceMinutes := folder.GraceMinutes
	if graceMinutes < 0 {
		graceMinutes = 0
	}
	graceCutoff := time.Now().Add(-time.Duration(graceMinutes) * time.Minute)

	// Counted for design section 5.10's second fix, and reported from a defer
	// so the cold-storage-full and interrupted exits report what they managed
	// to evict rather than saying nothing at all.
	evicted := 0
	var evictedBytes int64
	defer func() { d.reportBulkEviction(folder.FolderLabel, evicted, evictedBytes) }()

	for _, file := range candidates {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("offload loop interrupted: %w", err)
		}

		// Redundant with OffloadCandidates' `pinned = 0` and deliberately so:
		// a future caller that forgets the SQL filter still cannot offload a
		// pinned file. The authoritative layer is ClaimUnpinnedFile, inside
		// the mover, which also catches a pin that lands after this query ran
		// (design section 4.1).
		if file.Pinned {
			continue
		}

		// Re-query rather than decrementing a running total. Another actor --
		// a restore, or a manual archive -- can change the folder's footprint
		// between two iterations, and a stale local total makes this loop
		// offload past the budget. The aggregate is index-covered and costs
		// microseconds against a move that costs milliseconds to seconds.
		onDevice, _, err := d.cat.FolderSizes(folder.FolderLabel)
		if err != nil {
			return fmt.Errorf("getting folder size: %w", err)
		}
		if onDevice <= folder.BudgetBytes {
			break
		}

		// Skip persistent paths
		if IsPersistent(file.RelativePath, allPatterns) {
			continue
		}

		// Skip files within grace period, using max(mtime, atime)
		lastActivity := file.FileMtime
		if file.FileAtime.After(lastActivity) {
			lastActivity = file.FileAtime
		}
		if lastActivity.After(graceCutoff) {
			continue
		}

		// Re-check mtime on disk to avoid racing with active transfers
		diskPath := filepath.Join(folder.SyncthingPath, file.RelativePath)
		info, err := os.Stat(diskPath)
		if err != nil {
			d.logger.Warn("cannot stat file, skipping", "path", diskPath, "error", err)
			continue
		}
		if info.ModTime().After(graceCutoff) {
			continue
		}

		// The mover owns the cold-storage free-space guard, the claim and
		// every row transition. The automatic variant claims with
		// ClaimUnpinnedFile, so a pin that commits between the candidate
		// query above and the claim inside wins and the file stays (D2).
		if _, err := d.mover.ArchiveAutomatic(file.ID); err != nil {
			switch {
			case errors.Is(err, ErrColdStorageFull), errors.Is(err, ErrFolderUnavailable):
				// Neither condition gets better by trying the next file.
				// Abandon the folder for this tick; the epilogue still runs.
				return nil
			case errors.Is(err, ErrFileStateChanged):
				// Somebody else -- a pin, a restore, a manual archive -- got
				// to this row first. Entirely ordinary.
				d.logger.Debug("skipping file claimed by another actor",
					"path", file.RelativePath, "error", err)
			default:
				d.logger.Error("offload failed", "path", file.RelativePath, "error", err)
			}
			continue
		}

		evicted++
		evictedBytes += file.SizeBytes
	}

	return nil
}

// reportBulkEviction writes one loud event when a single enforcement pass took
// an unusual number of files out of a folder.
//
// The restore pre-flight (design section 5.10 fix 1) only covers the case where
// a restore caused the pressure. This covers the rest: a wedding shoot, a
// budget lowered in the UI, a folder that was never within budget to begin
// with. Without it the only signal anywhere is a red bar, while Syncthing
// deletes hundreds of files from every one of the owner's devices.
func (d *Daemon) reportBulkEviction(label string, count int, bytes int64) {
	if count == 0 {
		return
	}

	counts, err := d.cat.FolderCounts(label)
	if err != nil {
		d.logger.Warn("cannot size the folder for the bulk-eviction check",
			"folder", label, "error", err)
		return
	}

	big := count > bulkEvictionFileThreshold
	if !big && count >= bulkEvictionMinFiles && counts.Total > 0 {
		big = int64(count)*100 > counts.Total*bulkEvictionFractionPercent
	}
	if !big {
		return
	}

	details := fmt.Sprintf("one enforcement pass archived %d files (%s) out of %d; "+
		"syncthing will delete them from your other devices",
		count, catalog.HumanSI(bytes), counts.Total)
	d.cat.LogEvent("bulk_eviction", label, "", details)
	d.logger.Warn("bulk eviction", "folder", label, "files", count, "bytes", bytes)
}

// enforceEpilogue recomputes the budget picture from scratch and applies the
// wedge latch. It never trusts a value the offload loop carried.
//
// The latch lives in folder_configs.wedge_warned_at rather than in a map on
// the Daemon, because an in-memory flag resets on every restart and a systemd
// restart loop would re-flood the events table with the same warning.
func (d *Daemon) enforceEpilogue(label string) {
	now := time.Now().Unix()

	// Deferred, not tail-called. last_enforced_at is what "last checked N
	// minutes ago" reads, and the two reads below can fail transiently. If a
	// failure skipped the stamp, the UI would freeze that timestamp at the
	// moment the trouble started and report a healthy daemon as a stopped one
	// -- the exact symptom the stamp exists to expose.
	defer func() {
		if err := d.cat.SetLastEnforcedAt(label, now); err != nil {
			d.logger.Warn("cannot record enforcement time", "folder", label, "error", err)
		}
	}()

	// Re-read, FIRST: the UI may have changed the policy while the pass was
	// running, and wedge_warned_at may have been written by an earlier tick.
	// The budget the tick started with is stale by definition, so the wedge
	// decision and the number quoted in the warning both come from here. A
	// budget raised mid-pass must not produce a wedge warning quoting the old
	// one, and a budget lowered mid-pass must not hide a real wedge until the
	// next tick.
	fc, err := d.cat.FolderConfigByLabel(label)
	if err != nil {
		d.logger.Error("epilogue: folder config", "folder", label, "error", err)
		return
	}
	budget := fc.BudgetBytes

	total, pinned, err := d.cat.FolderSizes(label)
	if err != nil {
		d.logger.Error("epilogue: folder sizes", "folder", label, "error", err)
		return
	}
	b := catalog.FolderBudget{BudgetBytes: budget, TotalBytes: total, PinnedBytes: pinned}

	switch {
	case b.Wedged():
		// One event per wedge episode, refreshed daily. A folder that stays
		// wedged for a week says so seven times, not once last Tuesday.
		if fc.WedgeWarnedAt == nil || now-*fc.WedgeWarnedAt >= wedgeWarningRefreshSeconds {
			d.cat.LogEvent("folder_wedged", label, "", fmt.Sprintf(
				"%s pinned exceeds the %s budget - nothing left to offload",
				catalog.HumanSI(pinned), catalog.HumanSI(budget)))
			d.logger.Warn("folder wedged: pinned bytes exceed budget",
				"folder", label, "pinned", pinned, "budget", budget)
			if err := d.cat.SetWedgeWarnedAt(label, &now); err != nil {
				d.logger.Error("epilogue: cannot latch wedge warning",
					"folder", label, "error", err)
			}
		}
	default:
		// Clearing the latch when the condition resolves is what makes the
		// next genuine wedge warn immediately instead of being swallowed.
		if fc.WedgeWarnedAt != nil {
			d.cat.LogEvent("folder_unwedged", label, "", "folder no longer wedged")
			if err := d.cat.SetWedgeWarnedAt(label, nil); err != nil {
				d.logger.Error("epilogue: cannot clear wedge latch",
					"folder", label, "error", err)
			}
		}
	}
}

// startThumbnailPass runs one thumbnail pass in the background. At most one
// pass runs at a time: a tick that arrives while the previous pass is still
// working is dropped rather than queued.
func (d *Daemon) startThumbnailPass(ctx context.Context) {
	if !d.thumbRunning.CompareAndSwap(false, true) {
		d.logger.Debug("thumbnail pass already running, skipping this tick")
		return
	}
	d.thumbWG.Add(1)
	go func() {
		defer d.thumbWG.Done()
		defer d.thumbRunning.Store(false)
		d.generateThumbnails(ctx)
	}()
}

// generateThumbnails works through the thumbnail backlog in bounded batches.
//
// It never asks for the whole backlog at once, it stops as soon as the context
// is cancelled, and it refuses to look at a file twice in one pass: a file that
// fails keeps its place in the query result until its attempt counter runs out,
// and re-processing it inside the same pass would spin.
func (d *Daemon) generateThumbnails(ctx context.Context) {
	// Paths come from folder_configs, not from cfg.Folders. Orphaned and
	// disabled folders are included deliberately: their files remain
	// browsable, so they remain worth a thumbnail. A label with no row at all
	// used to yield "" here and make filepath.Join read relative to the
	// daemon's working directory; such files are now dropped from the batch.
	configs, err := d.cat.ListFolderConfigs()
	if err != nil {
		d.logger.Error("cannot read folder paths for thumbnails", "error", err)
		return
	}
	folderPaths := make(map[string]string, len(configs))
	for _, fc := range configs {
		folderPaths[fc.FolderLabel] = fc.SyncthingPath
	}

	sourcePathFn := func(f *catalog.FileRecord) string {
		switch f.Status {
		case catalog.StatusArchived:
			return filepath.Join(d.cfg.Storage.ColdStoragePath, "archive", f.FolderLabel, f.RelativePath)
		default:
			return filepath.Join(folderPaths[f.FolderLabel], f.RelativePath)
		}
	}

	seen := make(map[int64]bool)
	processed := 0

	for batch := 0; batch < maxThumbnailBatchesPerPass; batch++ {
		if ctx.Err() != nil {
			d.logger.Info("thumbnail pass interrupted", "processed", processed)
			return
		}

		files, err := d.cat.FilesNeedingThumbnails(thumbnailExts, thumbnailBatchSize)
		if err != nil {
			d.logger.Error("querying files for thumbnails", "error", err)
			return
		}
		if len(files) == 0 {
			break
		}

		fresh := make([]catalog.FileRecord, 0, len(files))
		for _, f := range files {
			if seen[f.ID] {
				continue
			}
			seen[f.ID] = true
			if _, ok := folderPaths[f.FolderLabel]; !ok && f.Status != catalog.StatusArchived {
				d.logger.Warn("skipping thumbnail: no folder_configs row for label",
					"folder", f.FolderLabel, "path", f.RelativePath)
				continue
			}
			fresh = append(fresh, f)
		}
		if len(fresh) == 0 {
			// Every row this query can still see has already been tried in
			// this pass. The rest waits for the next tick.
			break
		}

		d.logger.Info("generating thumbnails", "count", len(fresh), "batch", batch+1)
		d.thumbGen.ProcessQueueContext(ctx, fresh, sourcePathFn, d.cfg.Daemon.ThumbnailWorkers)
		processed += len(fresh)
	}

	if processed > 0 {
		d.logger.Info("thumbnail pass complete", "processed", processed)
	}
}
