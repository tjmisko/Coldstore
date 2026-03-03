package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/thumbnail"
)

const minFreeSpacePercent = 5.0

// thumbnailExts are all extensions the thumbnail generator supports.
var thumbnailExts = []string{
	".jpg", ".jpeg", ".png", ".webp",
	".heif", ".heic",
	".mp4", ".mov", ".avi", ".mkv", ".webm",
}

type Daemon struct {
	cfg                  *config.Config
	cat                  *catalog.Catalog
	logger               *slog.Logger
	scanner              *Scanner
	offloader            *Offloader
	restorer             *Restorer
	thumbGen             *thumbnail.Generator
	spaceCheck           func(string) (float64, error)
	initialScanComplete  bool
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

func (d *Daemon) Run() error {
	// Retry initial scan up to 3 times before entering the main loop.
	// Quota enforcement is gated on a successful initial scan.
	const maxRetries = 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		d.logger.Info("running initial catalog scan", "attempt", attempt)
		if err := d.scanner.ScanAll(); err != nil {
			d.logger.Error("initial scan failed", "error", err, "attempt", attempt)
			if attempt < maxRetries {
				time.Sleep(5 * time.Second)
				continue
			}
			d.logger.Warn("initial scan failed after all retries, starting without offload capability")
		} else {
			d.initialScanComplete = true
		}
		break
	}
	d.generateThumbnails()

	scanTicker := time.NewTicker(time.Duration(d.cfg.Daemon.ScanIntervalMinutes) * time.Minute)
	defer scanTicker.Stop()

	catalogTicker := time.NewTicker(time.Duration(d.cfg.Daemon.CatalogSyncIntervalMinutes) * time.Minute)
	defer catalogTicker.Stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-scanTicker.C:
			if !d.initialScanComplete {
				d.logger.Warn("skipping quota enforcement: initial scan incomplete")
				continue
			}
			d.logger.Info("quota enforcement tick")
			d.safeEnforceQuotas()
		case <-catalogTicker.C:
			d.logger.Info("catalog sync tick")
			if err := d.scanner.ScanAll(); err != nil {
				d.logger.Error("catalog sync scan failed", "error", err)
			} else if !d.initialScanComplete {
				d.initialScanComplete = true
				d.logger.Info("initial scan completed on catalog sync tick")
			}
			d.generateThumbnails()
		case sig := <-sigCh:
			d.logger.Info("received signal, shutting down", "signal", sig)
			return nil
		}
	}
}

// safeEnforceQuotas wraps enforceQuotas with panic recovery so a bug
// in quota enforcement doesn't crash the entire daemon.
func (d *Daemon) safeEnforceQuotas() {
	defer func() {
		if r := recover(); r != nil {
			d.logger.Error("panic in quota enforcement", "panic", r)
		}
	}()
	d.EnforceQuotas()
}

// EnforceQuotas runs quota enforcement across all configured folders.
func (d *Daemon) EnforceQuotas() {
	for _, folder := range d.cfg.Folders {
		if err := d.enforceFolder(folder); err != nil {
			d.logger.Error("quota enforcement failed", "folder", folder.Label, "error", err)
		}
	}
}

func (d *Daemon) enforceFolder(folder config.FolderConfig) error {
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

		if err := d.offloader.Offload(&file, folder.Path); err != nil {
			d.logger.Error("offload failed", "path", file.RelativePath, "error", err)
			continue
		}

		syncedSize -= file.SizeBytes
	}

	return nil
}

func (d *Daemon) generateThumbnails() {
	files, err := d.cat.FilesNeedingThumbnails(thumbnailExts)
	if err != nil {
		d.logger.Error("querying files for thumbnails", "error", err)
		return
	}

	if len(files) == 0 {
		return
	}

	d.logger.Info("generating thumbnails", "count", len(files))

	// Build a map of folder label -> syncthing path for quick lookup
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

	d.thumbGen.ProcessQueue(files, sourcePathFn, d.cfg.Daemon.ThumbnailWorkers)
	d.logger.Info("thumbnail generation complete", "processed", len(files))
}
