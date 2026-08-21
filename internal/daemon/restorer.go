package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tjmisko/coldstore/internal/config"
)

// Restorer is the mirror of Offloader and is equally free of catalog access:
// it owns the cold-storage path layout, the destination MkdirAll and the empty
// -directory cleanup, and nothing else (design section 5.2).
type Restorer struct {
	cfg    *config.Config
	logger *slog.Logger
}

func NewRestorer(cfg *config.Config, logger *slog.Logger) *Restorer {
	return &Restorer{cfg: cfg, logger: logger}
}

// ColdPath returns where a file of this folder lives while archived.
func (r *Restorer) ColdPath(folderLabel, relativePath string) string {
	return filepath.Join(r.ArchiveRoot(folderLabel), relativePath)
}

// ArchiveRoot is the per-folder root of the cold-storage tree. MoveIn prunes
// empty directories up to, but never including, this path.
func (r *Restorer) ArchiveRoot(folderLabel string) string {
	return filepath.Join(r.cfg.Storage.ColdStoragePath, "archive", folderLabel)
}

// MoveIn moves one file from cold storage back into a syncthing folder.
//
// archiveRoot is a parameter rather than something derived from src because
// pruning must stop at the folder's archive root and src alone does not say
// where that is; the caller has the label and computes it with ArchiveRoot.
func (r *Restorer) MoveIn(src, dst, archiveRoot string) error {
	if err := mkdirAllSync(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("creating syncthing directory: %w", err)
	}
	if err := moveFile(src, dst, r.logger); err != nil {
		return fmt.Errorf("moving %s -> %s: %w", src, dst, err)
	}
	r.cleanEmptyParents(filepath.Dir(src), archiveRoot)
	return nil
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
