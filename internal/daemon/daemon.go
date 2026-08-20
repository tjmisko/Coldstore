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

const minFreeSpacePercent = 5.0

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
	offloader           *Offloader
	restorer            *Restorer
	thumbGen            *thumbnail.Generator
	spaceCheck          func(string) (float64, error)
	initialScanComplete bool

	// Thumbnail generation runs off the select loop so a long pass cannot
	// delay SIGTERM. thumbRunning admits one pass at a time; thumbWG lets
	// shutdown wait for the running pass to notice the cancelled context.
	thumbRunning atomic.Bool
	thumbWG      sync.WaitGroup
}

func New(cfg *config.Config, cat *catalog.Catalog, logger *slog.Logger) *Daemon {
	return &Daemon{
		cfg:        cfg,
		cat:        cat,
		logger:     logger,
		scanner:    NewScanner(cfg, cat, logger),
		offloader:  NewOffloader(cfg, cat, logger),
		restorer:   NewRestorer(cfg, cat, logger),
		thumbGen:   thumbnail.NewGenerator(cfg.Storage.ColdStoragePath, cat, logger),
		spaceCheck: AvailableSpacePercent,
	}
}

// SetSpaceCheck replaces the disk space check function, for testing.
func (d *Daemon) SetSpaceCheck(fn func(string) (float64, error)) {
	d.spaceCheck = fn
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
		}
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
func (d *Daemon) EnforceQuotasContext(ctx context.Context) {
	for _, folder := range d.cfg.Folders {
		if ctx.Err() != nil {
			d.logger.Info("quota enforcement interrupted", "reason", ctx.Err())
			return
		}
		if err := d.enforceFolder(ctx, folder); err != nil {
			if ctx.Err() != nil {
				d.logger.Info("quota enforcement interrupted", "folder", folder.Label)
				return
			}
			d.logger.Error("quota enforcement failed", "folder", folder.Label, "error", err)
		}
	}
}

func (d *Daemon) enforceFolder(ctx context.Context, folder config.FolderConfig) error {
	syncedSize, err := d.cat.FolderSyncedSize(folder.Label)
	if err != nil {
		return fmt.Errorf("getting folder size: %w", err)
	}

	if syncedSize <= folder.BudgetBytes {
		d.logger.Debug("folder within budget",
			"folder", folder.Label,
			"size", syncedSize,
			"budget", folder.BudgetBytes)
		return nil
	}

	d.logger.Info("folder over budget, offloading",
		"folder", folder.Label,
		"size", syncedSize,
		"budget", folder.BudgetBytes,
		"excess", syncedSize-folder.BudgetBytes)

	// Gather persistent patterns
	persistentPatterns, err := d.cat.ListPersistentPaths(folder.Label)
	if err != nil {
		return err
	}
	allPatterns := make([]string, 0, len(folder.Persistent)+len(persistentPatterns))
	allPatterns = append(allPatterns, folder.Persistent...)
	for _, p := range persistentPatterns {
		allPatterns = append(allPatterns, p.Pattern)
	}

	candidates, err := d.cat.OffloadCandidates(folder.Label)
	if err != nil {
		return fmt.Errorf("getting offload candidates: %w", err)
	}

	graceCutoff := time.Now().Add(-time.Duration(d.cfg.Daemon.GracePeriodMinutes) * time.Minute)

	for _, file := range candidates {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("offload loop interrupted: %w", err)
		}
		if syncedSize <= folder.BudgetBytes {
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
		diskPath := filepath.Join(folder.Path, file.RelativePath)
		info, err := os.Stat(diskPath)
		if err != nil {
			d.logger.Warn("cannot stat file, skipping", "path", diskPath, "error", err)
			continue
		}
		if info.ModTime().After(graceCutoff) {
			continue
		}

		// Check cold storage disk space
		freePercent, err := d.spaceCheck(d.cfg.Storage.ColdStoragePath)
		if err != nil {
			d.logger.Error("cannot check disk space", "error", err)
			return fmt.Errorf("disk space check: %w", err)
		}
		if freePercent < minFreeSpacePercent {
			d.logger.Warn("cold storage below 5% free, pausing offload",
				"free_percent", freePercent)
			d.cat.LogEvent("warning", folder.Label, "",
				fmt.Sprintf("offload paused: cold storage at %.1f%% free", freePercent))
			return nil
		}

		if err := d.offloader.OffloadContext(ctx, &file, folder.Path); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("offload loop interrupted: %w", ctx.Err())
			}
			d.logger.Error("offload failed", "path", file.RelativePath, "error", err)
			continue
		}

		syncedSize -= file.SizeBytes
	}

	return nil
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
	folderPaths := make(map[string]string, len(d.cfg.Folders))
	for _, f := range d.cfg.Folders {
		folderPaths[f.Label] = f.Path
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

		files, err := d.cat.FilesNeedingThumbnailsLimited(thumbnailExts, thumbnailBatchSize)
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
