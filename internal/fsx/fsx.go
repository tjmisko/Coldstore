// Package fsx is the filesystem seam of the archive core. Production code in
// store, journal, views and scrub performs every filesystem call through an FS
// so tests can record the calls and inject a fault at any one of them.
package fsx

import (
	"io"
	"io/fs"
	"os"
)

// FS is every filesystem call the archive core makes. Production code in
// store, journal, views and scrub never imports "os" for I/O.
type FS interface {
	OpenFile(name string, flag int, perm fs.FileMode) (File, error)
	Open(name string) (File, error) // O_RDONLY
	Rename(oldpath, newpath string) error
	Link(oldname, newname string) error
	Remove(name string) error
	MkdirAll(path string, perm fs.FileMode) error
	Stat(name string) (fs.FileInfo, error)
	Lstat(name string) (fs.FileInfo, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	Chmod(name string, mode fs.FileMode) error
	Truncate(name string, size int64) error
	// SyncDir opens the directory, fsyncs it and closes it.
	SyncDir(path string) error
}

// File is an open file. *os.File satisfies it.
type File interface {
	io.Reader
	io.ReaderAt
	io.Writer
	io.Seeker
	io.Closer
	Sync() error
	Stat() (fs.FileInfo, error)
	Name() string
}

type osFS struct{}

// OS returns the real filesystem.
func OS() FS { return osFS{} }

func (osFS) OpenFile(name string, flag int, perm fs.FileMode) (File, error) {
	f, err := os.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err // never a typed-nil *os.File inside the interface
	}
	return f, nil
}

func (osFS) Open(name string) (File, error) {
	return osFS{}.OpenFile(name, os.O_RDONLY, 0)
}

func (osFS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }
func (osFS) Link(oldname, newname string) error   { return os.Link(oldname, newname) }
func (osFS) Remove(name string) error             { return os.Remove(name) }

func (osFS) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }

func (osFS) Stat(name string) (fs.FileInfo, error)  { return os.Stat(name) }
func (osFS) Lstat(name string) (fs.FileInfo, error) { return os.Lstat(name) }

func (osFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }

func (osFS) Chmod(name string, mode fs.FileMode) error { return os.Chmod(name, mode) }
func (osFS) Truncate(name string, size int64) error    { return os.Truncate(name, size) }

func (osFS) SyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}
