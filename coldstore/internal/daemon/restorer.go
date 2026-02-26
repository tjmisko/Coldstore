package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

type Restorer struct {
	cfg    *config.Config
	cat    *catalog.Catalog
	logger *slog.Logger
}

func NewRestorer(cfg *config.Config, cat *catalog.Catalog, logger *slog.Logger) *Restorer {
	return &Restorer{cfg: cfg, cat: cat, logger: logger}
}

// Restore moves a file from cold storage back to its syncthing folder.
func (r *Restorer) Restore(file *catalog.FileRecord) error {
	if file.Status != catalog.StatusArchived {
		return fmt.Errorf("file %d is not archived (status: %s)", file.ID, file.Status)
	}

	// Find the syncthing path for this folder
	syncthingPath, err := r.folderPath(file.FolderLabel)
	if err != nil {
		return err
	}

	src := filepath.Join(r.cfg.Storage.ColdStoragePath, "archive", file.FolderLabel, file.RelativePath)
	dst := filepath.Join(syncthingPath, file.RelativePath)

	// Mark as restoring before the move
	if err := r.cat.UpdateFileStatus(file.ID, catalog.StatusRestoring); err != nil {
		return fmt.Errorf("setting restoring status: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("creating syncthing directory: %w", err)
	}

	if err := moveFile(src, dst); err != nil {
		// Revert status on failure
		r.cat.UpdateFileStatus(file.ID, catalog.StatusArchived)
		return fmt.Errorf("moving %s -> %s: %w", src, dst, err)
	}

	if err := r.cat.UpdateFileStatus(file.ID, catalog.StatusSynced); err != nil {
		return fmt.Errorf("updating catalog: %w", err)
	}

	r.cat.LogEvent("restore", file.FolderLabel, file.RelativePath,
		fmt.Sprintf("restored %d bytes", file.SizeBytes))

	r.logger.Info("restored file",
		"folder", file.FolderLabel,
		"path", file.RelativePath,
		"size", file.SizeBytes)

	// Clean up empty parent directories in the archive
	r.cleanEmptyParents(filepath.Dir(src),
		filepath.Join(r.cfg.Storage.ColdStoragePath, "archive", file.FolderLabel))

	return nil
}

func (r *Restorer) folderPath(label string) (string, error) {
	for _, f := range r.cfg.Folders {
		if f.Label == label {
			return f.Path, nil
		}
	}
	return "", fmt.Errorf("no folder config for label %q", label)
}

// cleanEmptyParents removes empty directories from dir up to (but not including) stopAt.
func (r *Restorer) cleanEmptyParents(dir, stopAt string) {
	for dir != stopAt && dir != "." && dir != "/" {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		os.Remove(dir)
		dir = filepath.Dir(dir)
	}
}
