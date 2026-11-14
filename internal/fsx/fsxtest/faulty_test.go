package fsxtest

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tjmisko/coldstore/internal/fsx"
)

func create(t *testing.T, fsys fsx.FS, path string) fsx.File {
	t.Helper()
	f, err := fsys.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFaulty(t *testing.T) {
	errBoom := errors.New("boom")

	t.Run("should fail exactly the third write when Nth is 3", func(t *testing.T) {
		fsys := NewFaulty(fsx.OS())
		f := create(t, fsys, filepath.Join(t.TempDir(), "a"))
		defer f.Close()
		fsys.Inject(Fault{Op: OpWrite, Nth: 3, Err: errBoom})
		for i := 1; i <= 5; i++ {
			_, err := f.Write([]byte("x"))
			if i == 3 && !errors.Is(err, errBoom) {
				t.Fatalf("write %d err = %v, want boom", i, err)
			}
			if i != 3 && err != nil {
				t.Fatalf("write %d err = %v, want nil", i, err)
			}
		}
	})

	t.Run("should deliver half the bytes then the error when Short is set on a write", func(t *testing.T) {
		fsys := NewFaulty(fsx.OS())
		path := filepath.Join(t.TempDir(), "a")
		f := create(t, fsys, path)
		defer f.Close()
		fsys.Inject(Fault{Op: OpWrite, Nth: 1, Err: errBoom, Short: true})
		n, err := f.Write([]byte("abcdefgh"))
		if n != 4 || !errors.Is(err, errBoom) {
			t.Fatalf("Write = %d, %v; want 4, boom", n, err)
		}
		if got, _ := os.ReadFile(path); string(got) != "abcd" {
			t.Fatalf("file holds %q, want abcd", got)
		}
	})

	t.Run("should return io.ErrShortWrite when Short is set on a write with a nil Err", func(t *testing.T) {
		fsys := NewFaulty(fsx.OS())
		f := create(t, fsys, filepath.Join(t.TempDir(), "a"))
		defer f.Close()
		fsys.Inject(Fault{Op: OpWrite, Nth: 1, Short: true})
		n, err := f.Write([]byte("abcdefgh"))
		if n != 4 || !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("Write = %d, %v; want 4, ErrShortWrite", n, err)
		}
	})

	t.Run("should return fewer bytes and no error when Short is set on a read with a nil Err", func(t *testing.T) {
		fsys := NewFaulty(fsx.OS())
		path := filepath.Join(t.TempDir(), "a")
		if err := os.WriteFile(path, []byte("abcdefgh"), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := fsys.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		fsys.Inject(Fault{Op: OpRead, Nth: 1, Short: true})
		buf := make([]byte, 8)
		n, err := f.Read(buf)
		if n != 4 || err != nil {
			t.Fatalf("Read = %d, %v; want 4, nil", n, err)
		}
		if n, err = f.Read(buf); n != 4 || err != nil {
			t.Fatalf("second Read = %d, %v; want the unfaulted remainder", n, err)
		}
	})

	t.Run("should return the bytes read then the error when Short is set on a read with an Err", func(t *testing.T) {
		fsys := NewFaulty(fsx.OS())
		path := filepath.Join(t.TempDir(), "a")
		if err := os.WriteFile(path, []byte("abcdefgh"), 0o600); err != nil {
			t.Fatal(err)
		}
		f, _ := fsys.Open(path)
		defer f.Close()
		fsys.Inject(Fault{Op: OpRead, Nth: 1, Short: true, Err: errBoom})
		n, err := f.Read(make([]byte, 8))
		if n != 4 || !errors.Is(err, errBoom) {
			t.Fatalf("Read = %d, %v; want 4, boom", n, err)
		}
	})

	t.Run("should record every call in order when no fault is injected", func(t *testing.T) {
		fsys := NewFaulty(fsx.OS())
		dir := t.TempDir()
		a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
		f := create(t, fsys, a)
		_, _ = f.Write([]byte("x"))
		_ = f.Sync()
		_ = f.Close()
		if err := fsys.Rename(a, b); err != nil {
			t.Fatal(err)
		}
		if err := fsys.SyncDir(dir); err != nil {
			t.Fatal(err)
		}
		if _, err := fsys.Stat(b); err != nil {
			t.Fatal(err)
		}
		want := []Call{
			{OpCreate, a}, {OpWrite, a}, {OpSync, a}, {OpClose, a},
			{OpRename, b}, {OpSyncDir, dir}, {OpStat, b},
		}
		if got := fsys.Calls(); !reflect.DeepEqual(got, want) {
			t.Fatalf("Calls = %v\nwant %v", got, want)
		}
	})

	t.Run("should fail the n-th call of any op when FailCall is used", func(t *testing.T) {
		fsys := NewFaulty(fsx.OS())
		dir := t.TempDir()
		fsys.FailCall(2, errBoom)
		if err := fsys.MkdirAll(filepath.Join(dir, "x"), 0o755); err != nil {
			t.Fatalf("call 1 err = %v", err)
		}
		if _, err := fsys.Stat(dir); !errors.Is(err, errBoom) {
			t.Fatalf("call 2 err = %v, want boom", err)
		}
		if _, err := fsys.Stat(dir); err != nil {
			t.Fatalf("call 3 err = %v, want nil", err)
		}
	})

	t.Run("should count only calls matching PathContains when it is set", func(t *testing.T) {
		fsys := NewFaulty(fsx.OS())
		dir := t.TempDir()
		fsys.Inject(Fault{Op: OpStat, PathContains: "target", Nth: 2, Err: errBoom})
		_, _ = fsys.Stat(dir)
		_, _ = fsys.Stat(filepath.Join(dir, "target1"))
		_, _ = fsys.Stat(dir)
		if _, err := fsys.Stat(filepath.Join(dir, "target2")); !errors.Is(err, errBoom) {
			t.Fatalf("second matching call err = %v, want boom", err)
		}
	})

	t.Run("should classify O_CREATE opens as OpCreate and the rest as OpOpen when files are opened", func(t *testing.T) {
		fsys := NewFaulty(fsx.OS())
		path := filepath.Join(t.TempDir(), "a")
		create(t, fsys, path).Close()
		f, err := fsys.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		calls := fsys.Calls()
		if calls[0].Op != OpCreate || calls[2].Op != OpOpen {
			t.Fatalf("Calls = %v", calls)
		}
	})

	t.Run("should match either end of a rename when PathContains names the source", func(t *testing.T) {
		fsys := NewFaulty(fsx.OS())
		dir := t.TempDir()
		src := filepath.Join(dir, "staged")
		if err := os.WriteFile(src, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		fsys.Inject(Fault{Op: OpRename, PathContains: "staged", Nth: 1, Err: errBoom})
		if err := fsys.Rename(src, filepath.Join(dir, "final")); !errors.Is(err, errBoom) {
			t.Fatalf("err = %v, want boom", err)
		}
		if _, err := os.Stat(src); err != nil {
			t.Fatalf("faulted rename still happened: %v", err)
		}
	})

	t.Run("should return ErrInjected when a fault has a nil Err and is not short", func(t *testing.T) {
		fsys := NewFaulty(fsx.OS())
		fsys.Inject(Fault{Op: OpRemove, Nth: 1})
		if err := fsys.Remove(filepath.Join(t.TempDir(), "x")); !errors.Is(err, ErrInjected) {
			t.Fatalf("err = %v, want ErrInjected", err)
		}
	})

	t.Run("should still close the descriptor when a close fault fires", func(t *testing.T) {
		fsys := NewFaulty(fsx.OS())
		f := create(t, fsys, filepath.Join(t.TempDir(), "a"))
		fsys.Inject(Fault{Op: OpClose, Nth: 1, Err: errBoom})
		if err := f.Close(); !errors.Is(err, errBoom) {
			t.Fatalf("err = %v, want boom", err)
		}
		if _, err := f.Write([]byte("x")); err == nil {
			t.Fatal("descriptor was left open")
		}
	})

	t.Run("should clear faults and the trace when Reset is called", func(t *testing.T) {
		fsys := NewFaulty(fsx.OS())
		fsys.Inject(Fault{Op: OpStat, Nth: 1, Err: errBoom})
		fsys.FailCall(1, errBoom)
		fsys.Reset()
		if _, err := fsys.Stat(t.TempDir()); err != nil {
			t.Fatalf("err = %v after Reset", err)
		}
		if n := len(fsys.Calls()); n != 1 {
			t.Fatalf("len(Calls) = %d, want 1", n)
		}
	})

	t.Run("should not return a typed nil File when the open fails", func(t *testing.T) {
		fsys := NewFaulty(fsx.OS())
		f, err := fsys.Open(filepath.Join(t.TempDir(), "missing"))
		if err == nil || f != nil {
			t.Fatalf("Open = %v, %v", f, err)
		}
	})
}
