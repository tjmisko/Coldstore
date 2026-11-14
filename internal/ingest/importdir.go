package ingest

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unicode/utf8"
)

// ImportDir is the importdir Source: a streamed WalkDir of root. Regular files
// only; symlinks, devices, FIFOs and names that are not valid UTF-8 are
// reported through skipped and never opened. The file list is never
// materialised. The source directory is only ever read.
//
// Paths that could not be READ (an unreadable directory, a failed lstat, an
// aborted walk) are also reported through skipped, with an "unreadable: "
// reason. Callers that must not lose data silently use ImportDirReporting.
func ImportDir(root, collection string, skipped func(path, reason string)) Source {
	if skipped == nil {
		skipped = func(string, string) {}
	}
	return ImportDirReporting(root, collection, skipped, func(path string, err error) {
		skipped(path, "unreadable: "+err.Error())
	})
}

// ImportDirReporting separates the two reasons a path is not imported. skipped
// is for benign, expected skips (a symlink entry, a FIFO, socket or device, a
// name that is not valid UTF-8). unreadable is for a path the walk could not
// read: the caller must treat it as a failure, because the files beneath it
// were not imported.
func ImportDirReporting(root, collection string, skipped func(path, reason string), unreadable func(path string, err error)) Source {
	if skipped == nil {
		skipped = func(string, string) {}
	}
	if unreadable == nil {
		unreadable = func(string, error) {}
	}
	return dirSource{root: root, collection: collection, skipped: skipped, unreadable: unreadable}
}

type dirSource struct {
	root       string
	collection string
	skipped    func(path, reason string)
	unreadable func(path string, err error)
}

func (s dirSource) Arrivals(ctx context.Context) <-chan Arrival {
	out := make(chan Arrival)
	go func() {
		defer close(out)
		// A root that is itself a symlink is followed: WalkDir would otherwise
		// see one non-directory entry and import nothing.
		resolved, err := filepath.EvalSymlinks(s.root)
		if err != nil {
			s.unreadable(s.root, err)
			return
		}
		s.root = resolved
		walkErr := filepath.WalkDir(s.root, func(full string, entry fs.DirEntry, err error) error {
			return s.visit(ctx, out, full, entry, err)
		})
		if walkErr != nil && ctx.Err() == nil {
			s.unreadable(s.root, fmt.Errorf("walk stopped: %w", walkErr))
		}
	}()
	return out
}

func (s dirSource) visit(ctx context.Context, out chan<- Arrival, full string, entry fs.DirEntry, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		// Returning nil, not fs.SkipDir: after a ReadDir error WalkDir still
		// walks the entries it did read, and SkipDir would discard them.
		s.unreadable(full, err)
		return nil
	}
	if !utf8.ValidString(entry.Name()) {
		// Not benign: this is real content that was not imported, and for a
		// directory it is a whole subtree. It must fail the run, never pass
		// as a clean one.
		s.unreadable(full, fmt.Errorf("name is not valid UTF-8; not imported"))
		if entry.IsDir() {
			return fs.SkipDir
		}
		return nil
	}
	if entry.IsDir() {
		return nil
	}
	if reason := nonRegularReason(entry.Type()); reason != "" {
		s.skipped(full, reason)
		return nil
	}
	info, err := entry.Info()
	if err != nil {
		s.unreadable(full, err)
		return nil
	}
	rel, err := filepath.Rel(s.root, full)
	if err != nil {
		s.unreadable(full, fmt.Errorf("not below the import root: %w", err))
		return nil
	}
	arrival := Arrival{
		Folder: s.collection,
		Path:   filepath.ToSlash(rel),
		Mtime:  info.ModTime().UTC(),
		Open:   func() (io.ReadCloser, error) { return openStable(full) },
	}
	select {
	case out <- arrival:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func nonRegularReason(mode fs.FileMode) string {
	switch {
	case mode.IsRegular():
		return ""
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	case mode&fs.ModeNamedPipe != 0:
		return "fifo"
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeDevice != 0:
		return "device"
	}
	return "not a regular file"
}

// openStable opens path read-only without following a symlink swapped in after
// the walk, refuses anything that is not a regular file, and returns a reader
// that fails with ErrChangedWhileReading instead of reporting EOF when the
// file's size or mtime differ from what they were at open.
func openStable(path string) (io.ReadCloser, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSourceRead, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %w", ErrSourceRead, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrSourceRead, path)
	}
	return &stableFile{file: f, size: info.Size(), mtime: info.ModTime()}, nil
}

type stableFile struct {
	file  *os.File
	size  int64
	mtime time.Time
	read  int64
}

func (s *stableFile) Read(p []byte) (int, error) {
	n, err := s.file.Read(p)
	s.read += int64(n)
	if err != nil && err != io.EOF {
		return n, fmt.Errorf("%w: %w", ErrSourceRead, err)
	}
	if err != io.EOF {
		return n, err
	}
	info, statErr := s.file.Stat()
	if statErr != nil {
		return n, fmt.Errorf("%w: %w", ErrSourceRead, statErr)
	}
	if info.Size() != s.size || !info.ModTime().Equal(s.mtime) || s.read != s.size {
		return n, fmt.Errorf("%s: %w", s.file.Name(), ErrChangedWhileReading)
	}
	return n, io.EOF
}

func (s *stableFile) Close() error { return s.file.Close() }
