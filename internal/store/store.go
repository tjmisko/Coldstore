// Package store is the content-addressed blob store of the archive core. A blob
// lives at blobs/sha256/ab/cd/<hash> under the archive root, is written once,
// is mode 0444, and is never replaced or deleted by this package.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"crypto/sha256"

	"github.com/tjmisko/coldstore/internal/fsx"
)

// Store is the blob store (design 4.1).
type Store interface {
	// Put streams r into the store and returns its hash and length. When want
	// is non-nil and the bytes hash to something else, it returns ErrHashMismatch
	// and keeps nothing.
	Put(ctx context.Context, r io.Reader, want *Hash) (Hash, int64, error)
	Open(h Hash) (io.ReadSeekCloser, error)
	Verify(ctx context.Context, h Hash) error
	Walk(ctx context.Context, fn func(h Hash, size int64) error) error
}

var (
	ErrHashMismatch = errors.New("store: bytes do not match the expected hash")
	ErrNotFound     = errors.New("store: no such blob")
)

// Reasons carried by CorruptError.
const (
	ReasonHashMismatch = "hash_mismatch"
	ReasonTruncated    = "truncated"
	ReasonUnreadable   = "unreadable"
)

// CorruptError is returned by Verify, and by Put when the store already holds
// a blob under the hash and that blob no longer verifies.
type CorruptError struct {
	Hash   Hash
	Got    Hash   // zero when the blob could not be read to the end
	Reason string // "hash_mismatch" | "truncated" | "unreadable"
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("store: blob %s is corrupt: %s", e.Hash, e.Reason)
}

const copyBufferBytes = 1 << 20

var bufferPool = sync.Pool{New: func() any {
	b := make([]byte, copyBufferBytes)
	return &b
}}

// Disk is the on-disk Store.
type Disk struct {
	fsys fsx.FS
	root string
	// locks serialise link, directory fsync and undo per hash (striped by the
	// first hash byte), so a concurrent Put never accepts a link whose creator
	// may still remove it.
	locks [256]sync.Mutex
}

func (d *Disk) lockFor(h Hash) *sync.Mutex { return &d.locks[h[0]] }

var _ Store = (*Disk)(nil)

// Open creates blobs/sha256 and tmp/ under root if absent and removes stale
// files from tmp/ (callers hold journal/LOCK, so nothing else is staging).
func Open(fsys fsx.FS, root string) (*Disk, error) {
	d := &Disk{fsys: fsys, root: root}
	if err := fsys.MkdirAll(d.blobsDir(), 0o755); err != nil {
		return nil, fmt.Errorf("opening store: creating blobs directory: %w", err)
	}
	if err := fsys.MkdirAll(d.tmpDir(), 0o755); err != nil {
		return nil, fmt.Errorf("opening store: creating tmp directory: %w", err)
	}
	// Make the directory entries just created durable, bottom up.
	for _, dir := range []string{filepath.Dir(d.blobsDir()), d.root} {
		if err := fsys.SyncDir(dir); err != nil {
			return nil, fmt.Errorf("opening store: syncing %s: %w", dir, err)
		}
	}
	if err := fsys.SyncDir(d.tmpDir()); err != nil {
		return nil, fmt.Errorf("opening store: syncing tmp directory: %w", err)
	}
	if err := d.removeStaleStaging(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Disk) blobsDir() string { return filepath.Join(d.root, "blobs", "sha256") }
func (d *Disk) tmpDir() string   { return filepath.Join(d.root, "tmp") }

func (d *Disk) removeStaleStaging() error {
	entries, err := d.fsys.ReadDir(d.tmpDir())
	if err != nil {
		return fmt.Errorf("opening store: listing tmp: %w", err)
	}
	for _, entry := range entries {
		name := filepath.Join(d.tmpDir(), entry.Name())
		if err := d.fsys.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("opening store: removing stale staging file: %w", err)
		}
	}
	return nil
}

// Path is the absolute path of the blob file for h.
func (d *Disk) Path(h Hash) string {
	return filepath.Join(d.root, "blobs", filepath.FromSlash(h.RelPath()))
}

// Has stats the blob file and nothing else.
func (d *Disk) Has(h Hash) (size int64, ok bool, err error) {
	info, err := d.fsys.Stat(d.Path(h))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("stat blob %s: %w", h, err)
	}
	return info.Size(), true, nil
}

// Open returns the blob for reading, or ErrNotFound.
func (d *Disk) Open(h Hash) (io.ReadSeekCloser, error) {
	file, err := d.fsys.Open(d.Path(h))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("opening blob %s: %w", h, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("opening blob %s: %w", h, err)
	}
	return file, nil
}

// Verify re-hashes the blob from disk. It returns ErrNotFound when the blob is
// absent and *CorruptError when it does not hash to h.
//
// The store keeps no length beside a blob, so Verify can only call a blob
// "truncated" when the file ends before the length its own stat promised.
// A caller that knows the length (the index does) should use VerifySize.
func (d *Disk) Verify(ctx context.Context, h Hash) error {
	return d.verify(ctx, h, -1)
}

// VerifySize is Verify plus a known length: a blob shorter than wantSize is
// reported as truncated, longer as hash_mismatch.
func (d *Disk) VerifySize(ctx context.Context, h Hash, wantSize int64) error {
	return d.verify(ctx, h, wantSize)
}

func (d *Disk) verify(ctx context.Context, h Hash, wantSize int64) error {
	file, err := d.fsys.Open(d.Path(h))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("verifying blob %s: %w", h, ErrNotFound)
	}
	if err != nil {
		return &CorruptError{Hash: h, Reason: ReasonUnreadable}
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return &CorruptError{Hash: h, Reason: ReasonUnreadable}
	}
	promised := info.Size()
	if wantSize >= 0 {
		promised = wantSize
	}

	got, n, err := hashStream(ctx, file)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("verifying blob %s: %w", h, ctx.Err())
		}
		return &CorruptError{Hash: h, Reason: ReasonUnreadable}
	}
	if n < promised {
		return &CorruptError{Hash: h, Reason: ReasonTruncated}
	}
	if got != h {
		return &CorruptError{Hash: h, Got: got, Reason: ReasonHashMismatch}
	}
	return nil
}

// hashStream hashes r to EOF through one pooled buffer, checking ctx between reads.
func hashStream(ctx context.Context, r io.Reader) (Hash, int64, error) {
	bufPtr := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bufPtr)
	buf := *bufPtr

	hasher := sha256.New()
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return Hash{}, total, err
		}
		n, err := r.Read(buf)
		hasher.Write(buf[:n])
		total += int64(n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Hash{}, total, err
		}
	}
	var sum Hash
	hasher.Sum(sum[:0])
	return sum, total, nil
}

// Walk visits every hash-named blob exactly once, in hash order, and nothing else.
func (d *Disk) Walk(ctx context.Context, fn func(h Hash, size int64) error) error {
	return d.walkBlobs(ctx, func(path string, h Hash, isBlob bool, size int64) error {
		if !isBlob {
			return nil
		}
		return fn(h, size)
	})
}

// WalkStray reports files under blobs/ whose name is not a valid hash path.
func (d *Disk) WalkStray(ctx context.Context, fn func(path string) error) error {
	return d.walkBlobs(ctx, func(path string, _ Hash, isBlob bool, _ int64) error {
		if isBlob {
			return nil
		}
		return fn(path)
	})
}

// walkBlobs classifies every non-directory under blobs/. A directory in the
// wrong place is descended into with everything below it stray.
func (d *Disk) walkBlobs(ctx context.Context, visit func(path string, h Hash, isBlob bool, size int64) error) error {
	blobsRoot := filepath.Join(d.root, "blobs")
	return d.walkDir(ctx, blobsRoot, nil, visit)
}

// walkDir lists dir. parts are the directory names below blobs/ so far.
// parts == nil means dir is blobs/ itself.
func (d *Disk) walkDir(ctx context.Context, dir string, parts []string, visit func(string, Hash, bool, int64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := d.fsys.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) && parts == nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("walking blobs: listing %s: %w", dir, err)
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			childParts := append(append([]string(nil), parts...), entry.Name())
			if err := d.walkDir(ctx, path, childParts, visit); err != nil {
				return err
			}
			continue
		}
		if err := d.visitFile(ctx, entry, path, parts, visit); err != nil {
			return err
		}
	}
	return nil
}

func (d *Disk) visitFile(ctx context.Context, entry fs.DirEntry, path string, parts []string, visit func(string, Hash, bool, int64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h, ok := blobIdentity(entry, parts)
	if !ok {
		return visit(path, Hash{}, false, 0)
	}
	info, err := entry.Info()
	if errors.Is(err, fs.ErrNotExist) {
		return nil // removed while walking
	}
	if err != nil {
		return fmt.Errorf("walking blobs: stat %s: %w", path, err)
	}
	return visit(path, h, true, info.Size())
}

// blobIdentity reports the hash of a regular file at sha256/ab/cd/<hash>.
func blobIdentity(entry fs.DirEntry, parts []string) (Hash, bool) {
	if len(parts) != 3 || parts[0] != "sha256" || !entry.Type().IsRegular() {
		return Hash{}, false
	}
	h, err := ParseHash(entry.Name())
	if err != nil {
		return Hash{}, false
	}
	if !strings.HasPrefix(entry.Name(), parts[1]+parts[2]) {
		return Hash{}, false
	}
	return h, true
}

func logger() *slog.Logger { return slog.Default() }
