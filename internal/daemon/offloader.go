package daemon

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"syscall"

	"github.com/tjmisko/coldstore/internal/config"
)

// Offloader is a PURE FILESYSTEM operation: it owns the archive path layout
// and the directory creation a move out needs, and it holds no catalog handle
// at all. Every row transition belongs to Mover, so that there is exactly one
// place where a file's status and its pin can change (design section 5.2).
type Offloader struct {
	cfg    *config.Config
	logger *slog.Logger
}

func NewOffloader(cfg *config.Config, logger *slog.Logger) *Offloader {
	return &Offloader{cfg: cfg, logger: logger}
}

// ArchivePath returns the cold storage destination for a file.
func (o *Offloader) ArchivePath(folderLabel, relativePath string) string {
	return filepath.Join(o.cfg.Storage.ColdStoragePath, "archive", folderLabel, relativePath)
}

// MoveOut moves one file from a syncthing folder into cold storage. It creates
// the destination directory and delegates the move itself to moveFile, which
// refuses an occupied destination and never leaves a partial file behind.
func (o *Offloader) MoveOut(src, dst string) error {
	if err := mkdirAllSync(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("creating archive directory: %w", err)
	}
	if err := moveFile(src, dst, o.logger); err != nil {
		return fmt.Errorf("moving %s -> %s: %w", src, dst, err)
	}
	return nil
}

// AvailableSpaceBytes returns the available bytes on the filesystem containing the given path.
func AvailableSpaceBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

// AvailableSpacePercent returns the percentage of available space on the filesystem.
func AvailableSpacePercent(path string) (float64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Blocks == 0 {
		return 0, nil
	}
	return float64(stat.Bavail) / float64(stat.Blocks) * 100, nil
}
