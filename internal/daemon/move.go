package daemon

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Suffixes coldstore appends to files it is not willing to destroy.
//
//   - tmpSuffix names the sibling a cross-device copy writes into. Nothing at
//     the real destination path is ever partially written, so a hard kill can
//     only ever leave one of these behind, and it is never referenced by a
//     catalog row.
//   - supersededSuffix names a complete copy that was set aside because the
//     other side of the move turned out to be a different file. Invariant 9:
//     bytes are kept, never deleted to make the bookkeeping tidy.
const (
	tmpSuffix        = ".coldstore-tmp"
	supersededSuffix = ".coldstore-superseded"
)

var (
	// ErrDestinationExists means something already occupies the destination
	// path. It is never an invitation to overwrite: invariant 10 says a move
	// may not truncate a file it did not create.
	ErrDestinationExists = errors.New("a file already exists at the destination")

	// ErrSourceChanged means the source was rewritten while the copy was in
	// flight -- typically Syncthing renaming a newer version over it. The
	// destination has been set aside as .coldstore-superseded and the source
	// is untouched.
	ErrSourceChanged = errors.New("the source file changed during the move")
)

// syncFile, syncDirPath and destWriter are indirected so tests can observe and
// fault-inject the durability contract: the destination's data and its
// directory entry must both reach stable storage before the source is
// unlinked, and what landed on disk must be re-read to prove it.
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

	// destWriter wraps the temp file the copy writes into. Production returns
	// it unchanged; a test replaces it with a writer that acknowledges more
	// bytes than it stores, which is exactly the failure the re-read
	// verification in step 5 of crossDeviceMove exists to catch.
	destWriter = func(f *os.File) io.Writer { return f }
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

// moveFile moves src to dst, refusing to touch an occupied destination.
//
// It is the ONLY way a file changes place in coldstore. A move is an atomic
// unit of work: it takes no context and cannot be cancelled part-way, because
// a cancellation between the copy and the source removal is exactly the torn
// state the whole primitive exists to make unreachable. Callers check for
// shutdown between moves, never inside one.
func moveFile(src, dst string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	// Invariant 10, cheap half. This Lstat is an early out that gives the
	// caller a clean ErrDestinationExists before any work is done; it is NOT
	// the guard. The guard is commitNoReplace below, which cannot be raced.
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%w: %s", ErrDestinationExists, dst)
	}

	// Same-device fast path. os.Link, not os.Rename: rename REPLACES an
	// occupant silently, and between the Lstat above and the commit Syncthing
	// can deliver a new version of this exact filename. link() fails with
	// EEXIST atomically instead, so the check and the commit are one syscall.
	err := commitNoReplace(src, dst)
	if err == nil {
		// ENOENT means commitNoReplace took its rename fallback and the source
		// name is already gone. Anything else leaves dst and src as two names
		// for one inode: a duplicate directory entry, never any lost bytes,
		// and startup recovery's decision tables resolve it.
		if rmErr := os.Remove(src); rmErr != nil && !os.IsNotExist(rmErr) {
			logger.Warn("cannot remove the source after a same-device move",
				"src", src, "error", rmErr)
		}
		// Best effort only: losing the directory entry to a crash puts the
		// file back under its old name. Nothing is destroyed either way.
		if syncErr := syncDirPath(filepath.Dir(dst)); syncErr != nil {
			logger.Warn("cannot fsync destination directory after rename",
				"path", filepath.Dir(dst), "error", syncErr)
		}
		return nil
	}

	if !isCrossDeviceError(err) {
		return err
	}

	return crossDeviceMove(src, dst, logger)
}

// commitNoReplace publishes `from` at `to` without ever replacing whatever may
// already be at `to`, and without a window between the check and the write.
//
// os.Link is the atomic primitive: on Linux link(2) fails with EEXIST if the
// destination exists, so there is no TOCTOU to lose. It leaves `from` in place;
// the caller unlinks it once the destination is durable.
//
// A filesystem that cannot hard-link (exfat, some network mounts) falls back to
// Lstat-then-rename, which is racy but is the best that filesystem offers. The
// pool and the Syncthing folder are ordinary Linux filesystems in every
// supported deployment, so the fallback is a safety net, not a normal path.
func commitNoReplace(from, to string) error {
	err := os.Link(from, to)
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%w: %s", ErrDestinationExists, to)
	}
	if isCrossDeviceError(err) || !linkUnsupported(err) {
		return err
	}

	if _, statErr := os.Lstat(to); statErr == nil {
		return fmt.Errorf("%w: %s", ErrDestinationExists, to)
	}
	return os.Rename(from, to)
}

// linkUnsupported reports whether a link(2) failure means "this filesystem
// does not do hard links" rather than "this particular link was refused".
func linkUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.ENOSYS) ||
		errors.Is(err, syscall.EMLINK)
}

func isCrossDeviceError(err error) bool {
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return errors.Is(linkErr.Err, syscall.EXDEV)
	}
	return false
}

// fileIdentity is what a move remembers about its source so that step 8 can
// tell "the file I copied" from "a different file that now has that name".
// Syncthing updates a file by renaming a temp over it, so the path can be
// rebound to another inode at any moment during a long copy.
type fileIdentity struct {
	dev   uint64
	ino   uint64
	size  int64
	mtime time.Time
	atime time.Time
	mode  os.FileMode
}

func identityFromInfo(info os.FileInfo) fileIdentity {
	id := fileIdentity{size: info.Size(), mtime: info.ModTime(), mode: info.Mode()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		id.dev = uint64(st.Dev)
		id.ino = uint64(st.Ino)
		id.atime = time.Unix(st.Atim.Sec, st.Atim.Nsec)
	} else {
		id.atime = info.ModTime()
	}
	return id
}

// sameFile reports whether two observations describe the same unmodified file.
func (a fileIdentity) sameFile(b fileIdentity) bool {
	return a.dev == b.dev && a.ino == b.ino && a.size == b.size && a.mtime.Equal(b.mtime)
}

// copyAndVerify streams srcFile into w, feeding h as it goes, and reports how
// many bytes the writer accepted. It is split out so tests can drive the copy
// against a writer that lies about what it stored.
func copyAndVerify(srcFile *os.File, w io.Writer, h hash.Hash) (int64, error) {
	return io.Copy(io.MultiWriter(w, h), srcFile)
}

// crossDeviceMove copies src to dst through a temp sibling, verifies what
// actually landed on disk, renames it into place, and only then removes the
// source.
//
// This is the NORMAL path in production: a Syncthing folder and the cold
// storage pool are different filesystems, so os.Rename always returns EXDEV.
//
// The ordering IS the specification (design section 5.2b) -- do not reorder it:
//
//  1. Steps 2-7 never write to dst itself, so at every instant dst either does
//     not exist or is a complete, re-hashed copy. A hard kill can leave a
//     partial only at dst+tmpSuffix, which no catalog row references, which the
//     scanner skips by name, and which startup recovery deletes. That is what
//     makes recovery decidable by inspection, and it is what stops Syncthing
//     from propagating a truncated file to every one of the owner's devices.
//     (Syncthing itself will still SEE that sibling for the duration of a
//     restore's copy; keeping it out of Syncthing's way is the .stignore
//     warning of design section 3.8, which is a separate step.)
//  2. The verification re-opens the temp file and hashes it FROM DISK. Hashing
//     the stream on its way past proves nothing about what the filesystem kept.
//  3. mtime and atime are restored onto the copy. Without that, an archive or a
//     restore re-dates the file to now, Syncthing propagates the new mtime
//     everywhere, and the next scan writes it over the last remaining record of
//     when the photo was actually taken.
//  4. The source is re-stat'd before it is unlinked, so a file that was
//     rewritten under us is never deleted.
func crossDeviceMove(src, dst string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	// 1. Identity comes from the descriptor, not a second stat of the path.
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}
	before := identityFromInfo(srcInfo)

	// 2. A leftover temp from a previous kill is never valid; it is an
	// incomplete copy of something and nothing references it.
	tmp := dst + tmpSuffix
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale temp file %s: %w", tmp, err)
	}
	tmpFile, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("creating temp file %s: %w", tmp, err)
	}

	abandonTmp := func(cause error) error {
		tmpFile.Close()
		os.Remove(tmp)
		return cause
	}

	// 3. Copy.
	streamHash := sha256.New()
	written, err := copyAndVerify(srcFile, destWriter(tmpFile), streamHash)
	if err != nil {
		return abandonTmp(fmt.Errorf("copying: %w", err))
	}
	if written != before.size {
		return abandonTmp(fmt.Errorf("size mismatch: wrote %d, expected %d", written, before.size))
	}

	// 4. Make the bytes and the temp's directory entry durable.
	if err := syncFile(tmpFile); err != nil {
		return abandonTmp(fmt.Errorf("syncing destination: %w", err))
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("closing destination: %w", err)
	}
	if err := syncDirPath(filepath.Dir(tmp)); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("syncing destination directory: %w", err)
	}

	// 5. Verify what the filesystem actually kept, by reading it back.
	diskSum, diskSize, err := hashFile(tmp)
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("re-reading destination: %w", err)
	}
	if diskSize != before.size {
		os.Remove(tmp)
		return fmt.Errorf("size mismatch on disk: %d bytes landed, expected %d", diskSize, before.size)
	}
	if string(diskSum) != string(streamHash.Sum(nil)) {
		os.Remove(tmp)
		return errors.New("hash mismatch: the copy on disk differs from the bytes that were sent")
	}

	// 6. Metadata. Both are fatal: a wrong mode is a permissions bug and a
	// wrong mtime destroys the only surviving record of the file's date.
	if err := os.Chmod(tmp, before.mode.Perm()); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("preserving mode on %s: %w", tmp, err)
	}
	if err := os.Chtimes(tmp, before.atime, before.mtime); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("preserving timestamps on %s: %w", tmp, err)
	}

	// 7. Commit. Same directory, and NON-REPLACING: the destination was clear
	// when moveFile looked, but the copy above can take minutes and Syncthing
	// can deliver a new version of this filename at any point inside that
	// window. An unconditional rename here destroys those bytes with no
	// .coldstore-superseded sibling, no event and a 200 from the API.
	if err := commitNoReplace(tmp, dst); err != nil {
		if errors.Is(err, ErrDestinationExists) {
			return abandonTmpOnCollision(src, tmp, dst, before, logger)
		}
		os.Remove(tmp)
		return fmt.Errorf("committing %s: %w", dst, err)
	}
	// commitNoReplace linked rather than renamed; drop the temp name. Failing
	// here costs one stray directory entry, never any bytes.
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		logger.Warn("cannot remove the temp sibling after committing",
			"path", tmp, "error", err)
	}
	if err := syncDirPath(filepath.Dir(dst)); err != nil {
		logger.Warn("cannot fsync destination directory after commit",
			"path", filepath.Dir(dst), "error", err)
	}

	// --- the destination is now committed and verified ---

	// 8. Only now may the source be touched.
	afterInfo, err := os.Lstat(src)
	if os.IsNotExist(err) {
		// Someone else deleted the source while we copied. The destination
		// stays: invariant 9 keeps the bytes and keeps them referenced.
		logger.Warn("source vanished during move, keeping the copy", "src", src, "dst", dst)
		return nil
	}
	if err != nil {
		logger.Warn("cannot re-stat source after move, leaving it in place",
			"src", src, "error", err)
		return nil
	}
	if !before.sameFile(identityFromInfo(afterInfo)) {
		aside := asidePath(dst)
		if renameErr := os.Rename(dst, aside); renameErr != nil {
			logger.Error("cannot set aside a superseded copy", "dst", dst, "error", renameErr)
		}
		logger.Warn("source changed during move, keeping both copies",
			"src", src, "superseded", aside)
		return fmt.Errorf("%w: %s", ErrSourceChanged, src)
	}

	return os.Remove(src)
}

// abandonTmpOnCollision handles the one outcome invariant 10 exists for: the
// destination was clear when the move started and something else -- in
// practice Syncthing delivering a newer version -- occupied it while the copy
// was in flight.
//
// Nothing at dst is touched. The only question is what to do with the verified
// copy sitting at tmp:
//
//   - the source is still there, unchanged: tmp holds no bytes the source does
//     not, so it is deleted and the caller simply retries later.
//   - the source is gone or has itself changed: tmp is the only copy of those
//     bytes. Invariant 9 -- keep them, under a .coldstore-superseded name, and
//     say so loudly.
//
// Either way the caller gets ErrDestinationExists, which the mover turns into
// the adopt-the-device-copy branch and the API into a 409.
func abandonTmpOnCollision(src, tmp, dst string, before fileIdentity, logger *slog.Logger) error {
	srcInfo, srcErr := os.Lstat(src)
	if srcErr == nil && before.sameFile(identityFromInfo(srcInfo)) {
		if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
			logger.Warn("cannot remove the temp sibling after a destination collision",
				"path", tmp, "error", err)
		}
		logger.Warn("destination was occupied during the copy, keeping both existing files",
			"src", src, "dst", dst)
		return fmt.Errorf("%w: %s", ErrDestinationExists, dst)
	}

	aside := asidePath(dst)
	if err := os.Rename(tmp, aside); err != nil {
		logger.Error("cannot set aside a copy whose destination was taken",
			"tmp", tmp, "aside", aside, "error", err)
		return fmt.Errorf("%w: %s", ErrDestinationExists, dst)
	}
	logger.Warn("destination was occupied during the copy and the source is gone; kept the copy",
		"src", src, "dst", dst, "superseded", aside)
	return fmt.Errorf("%w: %s", ErrDestinationExists, dst)
}

// asidePath names a free .coldstore-superseded slot next to path. Reusing an
// occupied one would silently destroy a copy that was set aside earlier, which
// is the exact outcome invariant 9 forbids.
func asidePath(path string) string {
	base := path + supersededSuffix
	if _, err := os.Lstat(base); os.IsNotExist(err) {
		return base
	}
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s.%d", base, i)
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return fmt.Sprintf("%s.%d", base, time.Now().UnixNano())
}

// hashFile reads a file back from disk and returns its sha256 and its size.
func hashFile(path string) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, 0, err
	}
	return h.Sum(nil), n, nil
}

// isPrefixOf reports whether the whole of the file at short is the leading
// bytes of the file at long. Recovery uses it to tell an interrupted copy from
// a genuinely different file: a prefix contains no bytes the other copy does
// not already have, so deleting it destroys nothing.
func isPrefixOf(short, long string) (bool, error) {
	shortFile, err := os.Open(short)
	if err != nil {
		return false, err
	}
	defer shortFile.Close()

	longFile, err := os.Open(long)
	if err != nil {
		return false, err
	}
	defer longFile.Close()

	shortInfo, err := shortFile.Stat()
	if err != nil {
		return false, err
	}
	longInfo, err := longFile.Stat()
	if err != nil {
		return false, err
	}
	if shortInfo.Size() > longInfo.Size() {
		return false, nil
	}

	const chunk = 64 << 10
	a := make([]byte, chunk)
	b := make([]byte, chunk)
	remaining := shortInfo.Size()
	for remaining > 0 {
		n := int64(chunk)
		if remaining < n {
			n = remaining
		}
		if _, err := io.ReadFull(shortFile, a[:n]); err != nil {
			return false, err
		}
		if _, err := io.ReadFull(longFile, b[:n]); err != nil {
			return false, err
		}
		if string(a[:n]) != string(b[:n]) {
			return false, nil
		}
		remaining -= n
	}
	return true, nil
}
