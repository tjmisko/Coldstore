package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"syscall"

	"github.com/tjmisko/coldstore/internal/crashpoint"
)

var (
	crashPutStagedCreated  = crashpoint.Register("store.put.staged_created")
	crashPutStagedWritten  = crashpoint.Register("store.put.staged_written")
	crashPutStagedSynced   = crashpoint.Register("store.put.staged_synced")
	crashPutStagedVerified = crashpoint.Register("store.put.staged_verified")
	crashPutLinked         = crashpoint.Register("store.put.linked")
	crashPutDirSynced      = crashpoint.Register("store.put.dir_synced")
	crashPutTmpRemoved     = crashpoint.Register("store.put.tmp_removed")
)

// Put stages r under tmp/, hashes it, makes it durable, then hard-links it to
// its content address. Nothing under blobs/ is ever replaced.
func (d *Disk) Put(ctx context.Context, r io.Reader, want *Hash) (Hash, int64, error) {
	tmpPath, err := d.newStagingPath()
	if err != nil {
		return Hash{}, 0, err
	}
	// Steps 1-3: stage, hash while copying, sync. On any error tmp is removed.
	streamed, size, err := d.stage(ctx, tmpPath, r)
	if err != nil {
		d.removeStaging(tmpPath)
		return Hash{}, 0, err
	}
	if want != nil && *want != streamed {
		d.removeStaging(tmpPath)
		return Hash{}, 0, ErrHashMismatch
	}
	if err := d.reverify(ctx, tmpPath, streamed); err != nil {
		d.removeStaging(tmpPath)
		return Hash{}, 0, err
	}
	crashPutStagedVerified.Hit()

	if err := d.fsys.Chmod(tmpPath, 0o444); err != nil {
		d.removeStaging(tmpPath)
		return Hash{}, 0, fmt.Errorf("storing blob %s: chmod staged file: %w", streamed, err)
	}
	finalPath := d.Path(streamed)

	// The lock stripes on the shard byte, so same-shard Puts serialise through
	// directory creation and its fsyncs.
	lock := d.lockFor(streamed)
	lock.Lock()
	defer lock.Unlock()

	if err := d.makeBlobDirs(finalPath); err != nil {
		d.removeStaging(tmpPath)
		return Hash{}, 0, fmt.Errorf("storing blob %s: %w", streamed, err)
	}

	linkErr := d.fsys.Link(tmpPath, finalPath)
	if errors.Is(linkErr, fs.ErrExist) {
		return d.settleExisting(ctx, tmpPath, streamed, size)
	}
	if linkErr != nil {
		d.removeStaging(tmpPath)
		return Hash{}, 0, fmt.Errorf("storing blob %s: linking: %w", streamed, linkErr)
	}
	crashPutLinked.Hit()

	if err := d.fsys.SyncDir(filepath.Dir(finalPath)); err != nil {
		// The link may not be durable and must not be believed: undo it.
		err = fmt.Errorf("storing blob %s: syncing blob directory: %w", streamed, err)
		if rmErr := d.fsys.Remove(finalPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("removing unsynced link: %w", rmErr))
		}
		d.removeStaging(tmpPath)
		return Hash{}, 0, err
	}
	crashPutDirSynced.Hit()

	if err := d.fsys.Remove(tmpPath); err != nil {
		// The blob is durable; the next Open sweeps tmp/.
		logger().Warn("store: removing staging file after put", "path", tmpPath, "err", err)
	}
	crashPutTmpRemoved.Hit()
	return streamed, size, nil
}

func (d *Disk) newStagingPath() (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("storing blob: choosing staging name: %w", err)
	}
	return filepath.Join(d.tmpDir(), "put-"+hex.EncodeToString(random[:])), nil
}

// removeStaging is best effort; a leftover file is swept by the next Open.
func (d *Disk) removeStaging(tmpPath string) {
	if err := d.fsys.Remove(tmpPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logger().Warn("store: removing staging file", "path", tmpPath, "err", err)
	}
}

// stage runs steps 1 to 3: create, copy through the hasher, sync and close.
func (d *Disk) stage(ctx context.Context, tmpPath string, r io.Reader) (Hash, int64, error) {
	file, err := d.fsys.OpenFile(tmpPath, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, 0o600)
	if err != nil {
		return Hash{}, 0, fmt.Errorf("storing blob: creating staging file: %w", err)
	}
	crashPutStagedCreated.Hit()

	hash, size, err := copyAndHash(ctx, file, r)
	if err != nil {
		_ = file.Close()
		return Hash{}, 0, fmt.Errorf("storing blob: staging bytes: %w", err)
	}
	crashPutStagedWritten.Hit()

	if err := file.Sync(); err != nil {
		_ = file.Close()
		return Hash{}, 0, fmt.Errorf("storing blob: syncing staging file: %w", err)
	}
	if err := file.Close(); err != nil {
		return Hash{}, 0, fmt.Errorf("storing blob: closing staging file: %w", err)
	}
	crashPutStagedSynced.Hit()
	return hash, size, nil
}

// copyAndHash copies r to w and hashes the bytes as they pass, one pooled
// buffer, ctx checked between reads. A short write is an error.
func copyAndHash(ctx context.Context, w io.Writer, r io.Reader) (Hash, int64, error) {
	bufPtr := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bufPtr)
	buf := *bufPtr

	hasher := sha256.New()
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return Hash{}, total, err
		}
		n, readErr := r.Read(buf)
		if n > 0 {
			written, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				return Hash{}, total, writeErr
			}
			if written != n {
				return Hash{}, total, io.ErrShortWrite
			}
			hasher.Write(buf[:n])
			total += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return Hash{}, total, readErr
		}
	}
	var sum Hash
	hasher.Sum(sum[:0])
	return sum, total, nil
}

// reverify is step 5: the bytes that reached the disk must hash to what was streamed.
func (d *Disk) reverify(ctx context.Context, tmpPath string, streamed Hash) error {
	file, err := d.fsys.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("storing blob %s: reopening staged file: %w", streamed, err)
	}
	onDisk, _, err := hashStream(ctx, file)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("storing blob %s: re-reading staged file: %w", streamed, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("storing blob %s: closing re-read staged file: %w", streamed, err)
	}
	if onDisk != streamed {
		return fmt.Errorf("storing blob %s: staged file re-hashes to %s", streamed, onDisk)
	}
	return nil
}

// makeBlobDirs creates blobs/sha256/ab/cd and unconditionally syncs the parent
// of the shard and of the leaf. A directory that already exists may have been
// created by a Put that died before its fsync, so existence proves nothing.
// The caller holds the shard's lock.
func (d *Disk) makeBlobDirs(finalPath string) error {
	leaf := filepath.Dir(finalPath)
	shard := filepath.Dir(leaf)
	if err := d.fsys.MkdirAll(leaf, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", leaf, err)
	}
	for _, dir := range []string{filepath.Dir(shard), shard} {
		if err := d.fsys.SyncDir(dir); err != nil {
			return fmt.Errorf("syncing %s: %w", dir, err)
		}
	}
	return nil
}

// settleExisting is step 7's EEXIST branch: the content is already held. The
// existing blob is verified, never replaced.
func (d *Disk) settleExisting(ctx context.Context, tmpPath string, h Hash, size int64) (Hash, int64, error) {
	d.removeStaging(tmpPath)
	if err := d.verify(ctx, h, size); err != nil {
		return Hash{}, 0, err // *CorruptError, ErrNotFound or ctx error, unwrapped
	}
	// A previous Put may have died between its link and its directory sync.
	if err := d.fsys.SyncDir(filepath.Dir(d.Path(h))); err != nil {
		return Hash{}, 0, fmt.Errorf("storing blob %s: syncing blob directory: %w", h, err)
	}
	return h, size, nil
}
