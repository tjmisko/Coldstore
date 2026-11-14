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
func ImportDir(root, collection string, skipped func(path, reason string)) Source {
	if skipped == nil {
		skipped = func(string, string) {}
	}
	return dirSource{root: root, collection: collection, skipped: skipped}
}

type dirSource struct {
	root       string
	collection string
	skipped    func(path, reason string)
}

func (s dirSource) Arrivals(ctx context.Context) <-chan Arrival {
	out := make(chan Arrival)
	go func() {
		defer close(out)
		walkErr := filepath.WalkDir(s.root, func(full string, entry fs.DirEntry, err error) error {
			return s.visit(ctx, out, full, entry, err)
		})
		if walkErr != nil && ctx.Err() == nil {
			s.skipped(s.root, fmt.Sprintf("walk stopped: %v", walkErr))
		}
	}()
	return out
}

func (s dirSource) visit(ctx context.Context, out chan<- Arrival, full string, entry fs.DirEntry, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		s.skipped(full, fmt.Sprintf("unreadable: %v", err))
		if entry != nil && entry.IsDir() {
			return fs.SkipDir
		}
		return nil
	}
	if !utf8.ValidString(entry.Name()) {
		s.skipped(full, "name is not valid UTF-8")
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
		s.skipped(full, fmt.Sprintf("unreadable: %v", err))
		return nil
	}
	rel, err := filepath.Rel(s.root, full)
	if err != nil {
		s.skipped(full, fmt.Sprintf("not below the import root: %v", err))
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
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
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
	if err != io.EOF {
		return n, err
	}
	info, statErr := s.file.Stat()
	if statErr != nil {
		return n, statErr
	}
	if info.Size() != s.size || !info.ModTime().Equal(s.mtime) || s.read != s.size {
		return n, fmt.Errorf("%s: %w", s.file.Name(), ErrChangedWhileReading)
	}
	return n, io.EOF
}

func (s *stableFile) Close() error { return s.file.Close() }
