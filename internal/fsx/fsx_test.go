package fsx

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestOS(t *testing.T) {
	fsys := OS()

	t.Run("should write read and seek a file when created through OpenFile", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a")
		f, err := fsys.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("hello world")); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 5)
		if _, err := f.ReadAt(buf, 6); err != nil || string(buf) != "world" {
			t.Fatalf("ReadAt = %q, %v", buf, err)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		all, err := io.ReadAll(f)
		if err != nil || string(all) != "hello world" {
			t.Fatalf("ReadAll = %q, %v", all, err)
		}
		if f.Name() != path {
			t.Errorf("Name = %q, want %q", f.Name(), path)
		}
		info, err := f.Stat()
		if err != nil || info.Size() != 11 {
			t.Fatalf("Stat = %v, %v", info, err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("should return a nil File and an ErrNotExist error when Open names a missing file", func(t *testing.T) {
		f, err := fsys.Open(filepath.Join(t.TempDir(), "missing"))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want ErrNotExist", err)
		}
		if f != nil {
			t.Error("File is non-nil on error (typed nil)")
		}
	})

	t.Run("should fail the second create when O_EXCL names an existing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a")
		f, err := fsys.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		if _, err := fsys.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("err = %v, want ErrExist", err)
		}
	})

	t.Run("should move link remove chmod and truncate when the matching methods are called", func(t *testing.T) {
		dir := t.TempDir()
		a, b, c := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "c")
		if err := os.WriteFile(a, []byte("0123456789"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := fsys.Rename(a, b); err != nil {
			t.Fatal(err)
		}
		if err := fsys.Link(b, c); err != nil {
			t.Fatal(err)
		}
		if err := fsys.Truncate(c, 4); err != nil {
			t.Fatal(err)
		}
		if info, err := fsys.Stat(b); err != nil || info.Size() != 4 {
			t.Fatalf("hard link did not share the truncate: %v, %v", info, err)
		}
		if err := fsys.Chmod(c, 0o400); err != nil {
			t.Fatal(err)
		}
		if info, err := fsys.Lstat(b); err != nil || info.Mode().Perm() != 0o400 {
			t.Fatalf("mode = %v, %v", info, err)
		}
		if err := fsys.Remove(b); err != nil {
			t.Fatal(err)
		}
		if _, err := fsys.Stat(a); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Stat(a) err = %v, want ErrNotExist", err)
		}
	})

	t.Run("should create nested directories and list them when MkdirAll and ReadDir are used", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "x", "y")
		if err := fsys.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		entries, err := fsys.ReadDir(filepath.Dir(dir))
		if err != nil || len(entries) != 1 || entries[0].Name() != "y" {
			t.Fatalf("ReadDir = %v, %v", entries, err)
		}
	})

	t.Run("should sync a directory when SyncDir names one and fail when it is missing", func(t *testing.T) {
		dir := t.TempDir()
		if err := fsys.SyncDir(dir); err != nil {
			t.Fatal(err)
		}
		if err := fsys.SyncDir(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want ErrNotExist", err)
		}
	})
}
