package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	metadatapkg "github.com/tjmisko/coldstore/internal/metadata"
)

const (
	metadataBatchSize          = 200
	maxMetadataBatchesPerPass  = 500
	defaultMetadataWorkerCount = 2
)

// metadataExts comes from the extractor itself so supporting a new format
// cannot leave the catalog queue with a stale hand-maintained allowlist.
var metadataExts = metadatapkg.SupportedExtensions()

// extractMetadata works through the extraction backlog in bounded batches.
// It shares the derived-data guard with thumbnail generation, but keeps its
// own queue and attempt budget so a failure in one pass cannot poison the
// other. A row is attempted at most once per pass even though a failure leaves
// meta_scanned_at NULL and therefore keeps it visible to the next query.
func (d *Daemon) extractMetadata(ctx context.Context) {
	configs, err := d.cat.ListFolderConfigs()
	if err != nil {
		d.logger.Error("cannot read folder paths for metadata", "error", err)
		return
	}
	folderPaths := make(map[string]string, len(configs))
	for _, fc := range configs {
		folderPaths[fc.FolderLabel] = fc.SyncthingPath
	}

	sourcePathFn := func(file *catalog.FileRecord) string {
		if file.Status == catalog.StatusArchived {
			return filepath.Join(d.cfg.Storage.ColdStoragePath, "archive",
				file.FolderLabel, file.RelativePath)
		}
		return filepath.Join(folderPaths[file.FolderLabel], file.RelativePath)
	}

	seen := make(map[int64]bool)
	processed := 0
	for batch := 0; batch < maxMetadataBatchesPerPass; batch++ {
		if ctx.Err() != nil {
			d.logger.Info("metadata pass interrupted", "processed", processed)
			return
		}

		files, err := d.cat.FilesNeedingMetadata(metadataExts, metadataBatchSize)
		if err != nil {
			d.logger.Error("querying files for metadata", "error", err)
			return
		}
		if len(files) == 0 {
			break
		}

		fresh := make([]catalog.FileRecord, 0, len(files))
		for _, file := range files {
			if seen[file.ID] {
				continue
			}
			seen[file.ID] = true
			if _, ok := folderPaths[file.FolderLabel]; !ok && file.Status != catalog.StatusArchived {
				d.logger.Warn("skipping metadata: no folder_configs row for label",
					"folder", file.FolderLabel, "path", file.RelativePath)
				continue
			}
			fresh = append(fresh, file)
		}
		if len(fresh) == 0 {
			break
		}

		d.logger.Info("extracting metadata", "count", len(fresh), "batch", batch+1)
		d.processMetadataQueue(ctx, fresh, sourcePathFn, d.cfg.Daemon.ThumbnailWorkers)
		processed += len(fresh)
	}

	if processed > 0 {
		d.logger.Info("metadata pass complete", "processed", processed)
	}
}

// processMetadataQueue runs one catalog batch with the configured derived-data
// concurrency. Shutdown never burns an attempt: only an extraction error while
// the parent context remains live is attributed to the file.
func (d *Daemon) processMetadataQueue(ctx context.Context, files []catalog.FileRecord,
	sourcePathFn func(*catalog.FileRecord) string, workers int) {

	if len(files) == 0 {
		return
	}
	if workers <= 0 {
		workers = defaultMetadataWorkerCount
	}

	queue := make(chan catalog.FileRecord, len(files))
	for _, file := range files {
		queue <- file
	}
	close(queue)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range queue {
				if ctx.Err() != nil {
					return
				}

				value, err := d.metadataExtractor.Extract(ctx, sourcePathFn(&file), file.Extension)
				if err == nil {
					if writeErr := d.cat.SetExtractedMetadata(file.ID, value, time.Now().Unix()); writeErr != nil {
						d.logger.Error("recording extracted metadata",
							"folder", file.FolderLabel, "path", file.RelativePath, "error", writeErr)
						continue
					}
					d.logger.Debug("metadata extracted",
						"folder", file.FolderLabel, "path", file.RelativePath)
					continue
				}

				if ctx.Err() != nil {
					return
				}
				d.logger.Warn("metadata extraction failed",
					"folder", file.FolderLabel,
					"path", file.RelativePath,
					"attempts", file.MetaAttempts+1,
					"error", err)
				if recordErr := d.cat.RecordMetadataFailure(file.ID, err.Error(), time.Now().Unix()); recordErr != nil {
					d.logger.Error("recording metadata failure",
						"path", file.RelativePath, "error", recordErr)
				}
			}
		}()
	}
	wg.Wait()
}
