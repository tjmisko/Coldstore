package daemon

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

type Offloader struct {
	cfg    *config.Config
	cat    *catalog.Catalog
	logger *slog.Logger
}

func NewOffloader(cfg *config.Config, cat *catalog.Catalog, logger *slog.Logger) *Offloader {
	return &Offloader{cfg: cfg, cat: cat, logger: logger}
}

// ArchivePath returns the cold storage destination for a file.
func (o *Offloader) ArchivePath(folderLabel, relativePath string) string {
	return filepath.Join(o.cfg.Storage.ColdStoragePath, "archive", folderLabel, relativePath)
}

// Offload moves a file from its syncthing location to cold storage.
func (o *Offloader) Offload(file *catalog.FileRecord, syncthingPath string) error {
	src := filepath.Join(syncthingPath, file.RelativePath)
	dst := o.ArchivePath(file.FolderLabel, file.RelativePath)

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("creating archive directory: %w", err)
	}

	if err := moveFile(src, dst); err != nil {
		return fmt.Errorf("moving %s -> %s: %w", src, dst, err)
	}

	if err := o.cat.UpdateFileStatus(file.ID, catalog.StatusArchived); err != nil {
		return fmt.Errorf("updating catalog: %w", err)
	}

	o.cat.LogEvent("archive", file.FolderLabel, file.RelativePath,
		fmt.Sprintf("offloaded %d bytes", file.SizeBytes))

	o.logger.Info("offloaded file",
		"folder", file.FolderLabel,
		"path", file.RelativePath,
		"size", file.SizeBytes)

	return nil
}

// moveFile tries os.Rename first, falls back to copy+verify+delete for cross-device moves.
func moveFile(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}

	if !isCrossDeviceError(err) {
		return err
	}

	return crossDeviceMove(src, dst)
}

func isCrossDeviceError(err error) bool {
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return errors.Is(linkErr.Err, syscall.EXDEV)
	}
	return false
}

// crossDeviceMove copies the file, verifies the copy by size, then removes the original.
func crossDeviceMove(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}

	srcHash := sha256.New()
	dstHash := sha256.New()

	// Copy while computing source hash
	tee := io.TeeReader(srcFile, srcHash)
	written, err := io.Copy(io.MultiWriter(dstFile, dstHash), tee)
	if err != nil {
		dstFile.Close()
		os.Remove(dst)
		return fmt.Errorf("copying: %w", err)
	}

	if err := dstFile.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("closing destination: %w", err)
	}

	// Verify size
	if written != srcInfo.Size() {
		os.Remove(dst)
		return fmt.Errorf("size mismatch: wrote %d, expected %d", written, srcInfo.Size())
	}

	// Verify hash
	srcSum := srcHash.Sum(nil)
	dstSum := dstHash.Sum(nil)
	if string(srcSum) != string(dstSum) {
		os.Remove(dst)
		return fmt.Errorf("hash mismatch after copy")
	}

	// Preserve original permissions
	if err := os.Chmod(dst, srcInfo.Mode()); err != nil {
		// Non-fatal, log but continue
	}

	// Remove original only after successful verification
	srcFile.Close()
	return os.Remove(src)
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
