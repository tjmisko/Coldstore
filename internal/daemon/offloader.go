package daemon

import (
	"context"
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
	return o.OffloadContext(context.Background(), file, syncthingPath)
}

// OffloadContext is Offload with cancellation. A cancelled context aborts the
// copy, removes the partial destination and reverts the catalog row to
// "synced", so an interrupted offload never leaves a half-written archive or a
// file stranded in the transient "archiving" state.
func (o *Offloader) OffloadContext(ctx context.Context, file *catalog.FileRecord, syncthingPath string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("offload cancelled before start: %w", err)
	}

	src := filepath.Join(syncthingPath, file.RelativePath)
	dst := o.ArchivePath(file.FolderLabel, file.RelativePath)

	if err := mkdirAllSync(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("creating archive directory: %w", err)
	}

	// Set transient "archiving" status before the move so crash recovery
	// can detect incomplete offloads.
	if err := o.cat.UpdateFileStatus(file.ID, catalog.StatusArchiving); err != nil {
		return fmt.Errorf("setting archiving status: %w", err)
	}

	if err := moveFile(ctx, src, dst, o.logger); err != nil {
		// Revert status on failure. The source is still in place: moveFile
		// removes it only after the destination is verified and durable.
		o.cat.UpdateFileStatus(file.ID, catalog.StatusSynced)
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

// syncFile and syncDirPath are indirected so tests can observe and fault-inject
// the durability contract: the destination's data and its directory entry must
// both reach stable storage before the source is unlinked.
var (
	syncFile = func(f *os.File) error { return f.Sync() }

	syncDirPath = func(path string) error {
		d, err := os.Open(path)
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	}
)

// mkdirAllSync creates dir and any missing parents, then fsyncs each directory
// it had to create along with the nearest pre-existing ancestor. Without that,
// a freshly created archive subtree can vanish on power loss even though the
// file inside it was fsynced. When every directory already exists it does no
// I/O beyond the stat walk.
func mkdirAllSync(dir string, perm os.FileMode) error {
	var created []string
	existing := dir
	for {
		_, err := os.Stat(existing)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("stating %s: %w", existing, err)
		}
		created = append(created, existing)
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		existing = parent
	}

	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	if len(created) == 0 {
		return nil
	}

	// Outermost first: a directory entry is only durable once its parent is.
	if err := syncDirPath(existing); err != nil {
		return fmt.Errorf("syncing %s: %w", existing, err)
	}
	for i := len(created) - 1; i >= 0; i-- {
		if err := syncDirPath(created[i]); err != nil {
			return fmt.Errorf("syncing %s: %w", created[i], err)
		}
	}
	return nil
}

// moveFile tries os.Rename first, falls back to copy+verify+delete for cross-device moves.
func moveFile(ctx context.Context, src, dst string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	err := os.Rename(src, dst)
	if err == nil {
		// Best effort only: a rename is atomic, so losing it to a crash puts
		// the file back under its old name. Nothing is destroyed either way.
		if syncErr := syncDirPath(filepath.Dir(dst)); syncErr != nil {
			logger.Warn("cannot fsync destination directory after rename",
				"path", filepath.Dir(dst), "error", syncErr)
		}
		return nil
	}

	if !isCrossDeviceError(err) {
		return err
	}

	return crossDeviceMove(ctx, src, dst, logger)
}

func isCrossDeviceError(err error) bool {
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return errors.Is(linkErr.Err, syscall.EXDEV)
	}
	return false
}

// ctxReader aborts a copy as soon as the context is cancelled. io.Copy reads in
// 32 KiB chunks, so cancellation is observed promptly even on a large file.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// crossDeviceMove copies the file, verifies size and hash, makes the copy
// durable, and only then removes the original.
//
// This is the NORMAL path in production: a Syncthing folder and the cold
// storage pool are different filesystems, so os.Rename always returns EXDEV.
// The destination file and its containing directory are both fsynced before
// the source is unlinked; otherwise a power loss in that window destroys the
// only copy of the file.
func crossDeviceMove(ctx context.Context, src, dst string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

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
	tee := io.TeeReader(&ctxReader{ctx: ctx, r: srcFile}, srcHash)
	written, err := io.Copy(io.MultiWriter(dstFile, dstHash), tee)
	if err != nil {
		dstFile.Close()
		os.Remove(dst)
		return fmt.Errorf("copying: %w", err)
	}

	// Verify size before spending an fsync on a copy we already know is wrong.
	if written != srcInfo.Size() {
		dstFile.Close()
		os.Remove(dst)
		return fmt.Errorf("size mismatch: wrote %d, expected %d", written, srcInfo.Size())
	}

	// Verify hash
	srcSum := srcHash.Sum(nil)
	dstSum := dstHash.Sum(nil)
	if string(srcSum) != string(dstSum) {
		dstFile.Close()
		os.Remove(dst)
		return fmt.Errorf("hash mismatch after copy")
	}

	// Preserve original permissions before the fsync so the mode is durable too.
	if err := os.Chmod(dst, srcInfo.Mode()); err != nil {
		logger.Warn("cannot preserve mode on archived copy", "path", dst, "error", err)
	}

	// Flush the copy to stable storage. A failure here means we do NOT know the
	// data landed, so the source must stay exactly where it is.
	if err := syncFile(dstFile); err != nil {
		dstFile.Close()
		os.Remove(dst)
		return fmt.Errorf("syncing destination: %w", err)
	}

	if err := dstFile.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("closing destination: %w", err)
	}

	// The file's data is durable; its directory entry may not be. fsync the
	// containing directory so the name survives a crash as well.
	if err := syncDirPath(filepath.Dir(dst)); err != nil {
		os.Remove(dst)
		return fmt.Errorf("syncing destination directory: %w", err)
	}

	// Remove original only after the copy is verified AND durable.
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
