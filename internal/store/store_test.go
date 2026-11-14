package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tjmisko/coldstore/internal/fsx"
)

func openTestStore(t *testing.T) (*Disk, string) {
	t.Helper()
	root := t.TempDir()
	disk, err := Open(fsx.OS(), root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return disk, root
}

func hashOf(b []byte) Hash { return Hash(sha256.Sum256(b)) }

func mustPut(t *testing.T, disk *Disk, content []byte) Hash {
	t.Helper()
	h, _, err := disk.Put(context.Background(), bytes.NewReader(content), nil)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return h
}

func walkAll(t *testing.T, disk *Disk) map[Hash]int64 {
	t.Helper()
	seen := map[Hash]int64{}
	err := disk.Walk(context.Background(), func(h Hash, size int64) error {
		if _, dup := seen[h]; dup {
			t.Errorf("Walk visited %s twice", h)
		}
		seen[h] = size
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return seen
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestPut(t *testing.T) {
	t.Run("should store the bytes under their sha256 when Put succeeds", func(t *testing.T) {
		disk, root := openTestStore(t)
		content := []byte("hello archive")
		h, size, err := disk.Put(context.Background(), bytes.NewReader(content), nil)
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if h != hashOf(content) || size != int64(len(content)) {
			t.Fatalf("Put returned %s, %d", h, size)
		}
		want := filepath.Join(root, "blobs", "sha256", h.String()[:2], h.String()[2:4], h.String())
		if disk.Path(h) != want {
			t.Fatalf("Path = %s, want %s", disk.Path(h), want)
		}
		info, err := os.Stat(want)
		if err != nil {
			t.Fatalf("blob missing: %v", err)
		}
		if info.Mode().Perm() != 0o444 {
			t.Errorf("blob mode = %v, want 0444", info.Mode().Perm())
		}
		got, err := os.ReadFile(want)
		if err != nil || !bytes.Equal(got, content) {
			t.Errorf("blob bytes differ: %v", err)
		}
		if names := listDir(t, filepath.Join(root, "tmp")); len(names) != 0 {
			t.Errorf("tmp not empty: %v", names)
		}
		if err := disk.Verify(context.Background(), h); err != nil {
			t.Errorf("Verify: %v", err)
		}
	})

	t.Run("should be a no-op that verifies the existing blob when the content is already held", func(t *testing.T) {
		disk, root := openTestStore(t)
		content := []byte("held twice")
		h := mustPut(t, disk, content)
		before, err := os.Stat(disk.Path(h))
		if err != nil {
			t.Fatal(err)
		}
		h2, size, err := disk.Put(context.Background(), bytes.NewReader(content), &h)
		if err != nil || h2 != h || size != int64(len(content)) {
			t.Fatalf("second Put = %s, %d, %v", h2, size, err)
		}
		after, err := os.Stat(disk.Path(h))
		if err != nil || !os.SameFile(before, after) {
			t.Errorf("blob inode changed on a repeated Put (%v)", err)
		}
		if names := listDir(t, filepath.Join(root, "tmp")); len(names) != 0 {
			t.Errorf("tmp not empty: %v", names)
		}
		if len(walkAll(t, disk)) != 1 {
			t.Errorf("want exactly one blob")
		}
	})

	t.Run("should return ErrHashMismatch and keep nothing when want differs", func(t *testing.T) {
		disk, root := openTestStore(t)
		wrong := hashOf([]byte("something else"))
		_, _, err := disk.Put(context.Background(), bytes.NewReader([]byte("actual")), &wrong)
		if !errors.Is(err, ErrHashMismatch) {
			t.Fatalf("err = %v, want ErrHashMismatch", err)
		}
		if n := len(walkAll(t, disk)); n != 0 {
			t.Errorf("%d blobs kept", n)
		}
		if names := listDir(t, filepath.Join(root, "tmp")); len(names) != 0 {
			t.Errorf("tmp not empty: %v", names)
		}
	})

	t.Run("should accept a matching want when the hash is right", func(t *testing.T) {
		disk, _ := openTestStore(t)
		content := []byte("matching")
		want := hashOf(content)
		if _, _, err := disk.Put(context.Background(), bytes.NewReader(content), &want); err != nil {
			t.Fatalf("Put: %v", err)
		}
	})

	t.Run("should store a zero-byte blob when the reader is empty", func(t *testing.T) {
		disk, _ := openTestStore(t)
		h, size, err := disk.Put(context.Background(), bytes.NewReader(nil), nil)
		if err != nil || size != 0 || h != hashOf(nil) {
			t.Fatalf("Put = %s, %d, %v", h, size, err)
		}
		if err := disk.Verify(context.Background(), h); err != nil {
			t.Errorf("Verify: %v", err)
		}
		if sz, ok, err := disk.Has(h); err != nil || !ok || sz != 0 {
			t.Errorf("Has = %d, %v, %v", sz, ok, err)
		}
	})

	t.Run("should return CorruptError and not replace the blob when the held copy is damaged", func(t *testing.T) {
		disk, _ := openTestStore(t)
		content := []byte("will be damaged, 0123456789")
		h := mustPut(t, disk, content)
		damage(t, disk.Path(h), func(b []byte) []byte { b[3] ^= 0xff; return b })

		_, _, err := disk.Put(context.Background(), bytes.NewReader(content), nil)
		var corrupt *CorruptError
		if !errors.As(err, &corrupt) || corrupt.Reason != ReasonHashMismatch {
			t.Fatalf("err = %v, want CorruptError hash_mismatch", err)
		}
		got, readErr := os.ReadFile(disk.Path(h))
		if readErr != nil || bytes.Equal(got, content) {
			t.Errorf("damaged blob was replaced (%v)", readErr)
		}
	})

	t.Run("should stop and keep nothing when the context is cancelled mid-copy", func(t *testing.T) {
		disk, root := openTestStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		reader := &cancellingReader{cancel: cancel, remaining: 3 * copyBufferBytes}
		_, _, err := disk.Put(ctx, reader, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if n := len(walkAll(t, disk)); n != 0 {
			t.Errorf("%d blobs kept", n)
		}
		if names := listDir(t, filepath.Join(root, "tmp")); len(names) != 0 {
			t.Errorf("tmp not empty: %v", names)
		}
	})

	t.Run("should keep exactly one blob when two goroutines Put the same content concurrently", func(t *testing.T) {
		disk, root := openTestStore(t)
		content := bytes.Repeat([]byte("concurrent"), 5000)
		want := hashOf(content)
		var wg sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h, _, err := disk.Put(context.Background(), bytes.NewReader(content), nil)
				if err == nil && h != want {
					err = errors.New("wrong hash")
				}
				errs[i] = err
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("Put %d: %v", i, err)
			}
		}
		if n := len(walkAll(t, disk)); n != 1 {
			t.Errorf("%d blobs, want 1", n)
		}
		if names := listDir(t, filepath.Join(root, "tmp")); len(names) != 0 {
			t.Errorf("tmp not empty: %v", names)
		}
	})

	t.Run("should hand the reader no buffer larger than 1 MiB when a large blob is put", func(t *testing.T) {
		disk, _ := openTestStore(t)
		total := int64(8 << 20)
		if os.Getenv("COLDSTORE_TEST_BIG") != "" {
			total = 256 << 20 // RAM-backed temp dir: opt-in only
		}
		reader := &countingReader{remaining: total}
		_, size, err := disk.Put(context.Background(), reader, nil)
		if err != nil || size != total {
			t.Fatalf("Put = %d, %v", size, err)
		}
		if reader.largestSlice > copyBufferBytes {
			t.Errorf("reader was handed a %d byte slice, want <= %d", reader.largestSlice, copyBufferBytes)
		}
	})
}

func TestOpenConstructor(t *testing.T) {
	t.Run("should remove stale staging files when the store is opened", func(t *testing.T) {
		root := t.TempDir()
		tmp := filepath.Join(root, "tmp")
		if err := os.MkdirAll(tmp, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, "put-stale"), []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(fsx.OS(), root); err != nil {
			t.Fatalf("Open: %v", err)
		}
		if names := listDir(t, tmp); len(names) != 0 {
			t.Errorf("stale files remain: %v", names)
		}
		if _, err := os.Stat(filepath.Join(root, "blobs", "sha256")); err != nil {
			t.Errorf("blobs/sha256 not created: %v", err)
		}
	})
}

func TestStoreOpenBlob(t *testing.T) {
	t.Run("should return the bytes when the blob is opened", func(t *testing.T) {
		disk, _ := openTestStore(t)
		content := []byte("read me back")
		h := mustPut(t, disk, content)
		rsc, err := disk.Open(h)
		if err != nil {
			t.Fatal(err)
		}
		defer rsc.Close()
		got, err := io.ReadAll(rsc)
		if err != nil || !bytes.Equal(got, content) {
			t.Errorf("read %q, %v", got, err)
		}
	})

	t.Run("should return ErrNotFound when the blob is absent", func(t *testing.T) {
		disk, _ := openTestStore(t)
		if _, err := disk.Open(hashOf([]byte("nope"))); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
		if _, ok, err := disk.Has(hashOf([]byte("nope"))); ok || err != nil {
			t.Errorf("Has = %v, %v", ok, err)
		}
	})
}

func TestWalk(t *testing.T) {
	t.Run("should visit every hash-named blob exactly once when blobs are stored", func(t *testing.T) {
		disk, _ := openTestStore(t)
		want := map[Hash]int64{}
		for _, s := range []string{"a", "bb", "ccc", "dddd", ""} {
			want[mustPut(t, disk, []byte(s))] = int64(len(s))
		}
		got := walkAll(t, disk)
		if len(got) != len(want) {
			t.Fatalf("walked %d blobs, want %d", len(got), len(want))
		}
		for h, size := range want {
			if got[h] != size {
				t.Errorf("blob %s size %d, want %d", h, got[h], size)
			}
		}
	})

	t.Run("should report a non-hash file under blobs as stray and not as a blob", func(t *testing.T) {
		disk, root := openTestStore(t)
		h := mustPut(t, disk, []byte("real"))
		other := hashOf([]byte("misfiled")).String()
		strays := map[string]string{
			filepath.Join(root, "blobs", "notes.txt"):                            "x",
			filepath.Join(root, "blobs", "sha256", "zz", "zz", "junk"):           "x",
			filepath.Join(root, "blobs", "sha256", "ab", "cd", "short"):          "x",
			filepath.Join(root, "blobs", "sha256", "00", "00", other):            "x", // right name, wrong shard
			filepath.Join(root, "blobs", "sha256", h.String()[:2], "loose-file"): "x",
		}
		for path, body := range strays {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got := walkAll(t, disk); len(got) != 1 {
			t.Errorf("Walk saw %d blobs, want 1", len(got))
		}
		reported := map[string]bool{}
		err := disk.WalkStray(context.Background(), func(path string) error {
			reported[path] = true
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(reported) != len(strays) {
			t.Errorf("reported %v, want %d strays", reported, len(strays))
		}
		for path := range strays {
			if !reported[path] {
				t.Errorf("stray %s not reported", path)
			}
		}
		if reported[disk.Path(h)] {
			t.Errorf("real blob reported as stray")
		}
	})

	t.Run("should stop and return the error when the callback fails", func(t *testing.T) {
		disk, _ := openTestStore(t)
		mustPut(t, disk, []byte("one"))
		mustPut(t, disk, []byte("two"))
		sentinel := errors.New("stop")
		calls := 0
		err := disk.Walk(context.Background(), func(Hash, int64) error { calls++; return sentinel })
		if !errors.Is(err, sentinel) || calls != 1 {
			t.Errorf("err = %v after %d calls", err, calls)
		}
	})

	t.Run("should return nothing when the store is empty", func(t *testing.T) {
		disk, _ := openTestStore(t)
		if n := len(walkAll(t, disk)); n != 0 {
			t.Errorf("%d blobs in an empty store", n)
		}
	})

	t.Run("should return the context error when the context is cancelled", func(t *testing.T) {
		disk, _ := openTestStore(t)
		mustPut(t, disk, []byte("x"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := disk.Walk(ctx, func(Hash, int64) error { return nil }); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestVerifyCorruption(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789abcdef"), 100)

	t.Run("should report hash_mismatch when one byte of a blob is flipped", func(t *testing.T) {
		disk, _ := openTestStore(t)
		h := mustPut(t, disk, content)
		damage(t, disk.Path(h), func(b []byte) []byte { b[len(b)/2] ^= 0x01; return b })
		err := disk.Verify(context.Background(), h)
		var corrupt *CorruptError
		if !errors.As(err, &corrupt) || corrupt.Reason != ReasonHashMismatch {
			t.Fatalf("err = %v", err)
		}
		if corrupt.Hash != h || corrupt.Got == (Hash{}) || corrupt.Got == h {
			t.Errorf("CorruptError fields wrong: %+v", corrupt)
		}
	})

	t.Run("should report truncated when a blob is cut short", func(t *testing.T) {
		disk, _ := openTestStore(t)
		h := mustPut(t, disk, content)
		damage(t, disk.Path(h), func(b []byte) []byte { return b[:len(b)/2] })
		err := disk.VerifySize(context.Background(), h, int64(len(content)))
		var corrupt *CorruptError
		if !errors.As(err, &corrupt) || corrupt.Reason != ReasonTruncated {
			t.Fatalf("err = %v", err)
		}
		if corrupt.Got != (Hash{}) {
			t.Errorf("Got should be zero for a blob not read to the end")
		}
	})

	t.Run("should report truncated when the file ends before the length its stat promised", func(t *testing.T) {
		root := t.TempDir()
		real, err := Open(fsx.OS(), root)
		if err != nil {
			t.Fatal(err)
		}
		h := mustPut(t, real, content)
		overstating := &overstatingFS{FS: fsx.OS(), extra: 100}
		disk, err := Open(overstating, root)
		if err != nil {
			t.Fatal(err)
		}
		err = disk.Verify(context.Background(), h)
		var corrupt *CorruptError
		if !errors.As(err, &corrupt) || corrupt.Reason != ReasonTruncated {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("should report hash_mismatch when a blob is longer than its known size", func(t *testing.T) {
		disk, _ := openTestStore(t)
		h := mustPut(t, disk, content)
		damage(t, disk.Path(h), func(b []byte) []byte { return append(b, 'x') })
		err := disk.VerifySize(context.Background(), h, int64(len(content)))
		var corrupt *CorruptError
		if !errors.As(err, &corrupt) || corrupt.Reason != ReasonHashMismatch {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("should return ErrNotFound when a blob is deleted", func(t *testing.T) {
		disk, _ := openTestStore(t)
		h := mustPut(t, disk, content)
		if err := os.Remove(disk.Path(h)); err != nil {
			t.Fatal(err)
		}
		if err := disk.Verify(context.Background(), h); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("should report unreadable when the blob cannot be read", func(t *testing.T) {
		root := t.TempDir()
		real, err := Open(fsx.OS(), root)
		if err != nil {
			t.Fatal(err)
		}
		h := mustPut(t, real, content)
		disk, err := Open(&failingOpenFS{FS: fsx.OS()}, root)
		if err != nil {
			t.Fatal(err)
		}
		err = disk.Verify(context.Background(), h)
		var corrupt *CorruptError
		if !errors.As(err, &corrupt) || corrupt.Reason != ReasonUnreadable {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("should still verify every other blob when one is damaged", func(t *testing.T) {
		disk, _ := openTestStore(t)
		hashes := []Hash{
			mustPut(t, disk, []byte("alpha")),
			mustPut(t, disk, content),
			mustPut(t, disk, []byte("gamma")),
		}
		damage(t, disk.Path(hashes[1]), func(b []byte) []byte { b[0] ^= 0xff; return b })
		for i, h := range hashes {
			err := disk.Verify(context.Background(), h)
			if i == 1 && err == nil {
				t.Errorf("damaged blob verified")
			}
			if i != 1 && err != nil {
				t.Errorf("blob %d: %v", i, err)
			}
		}
	})
}

// damage rewrites a (read-only) blob file in place.
func damage(t *testing.T, path string, mutate func([]byte) []byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, mutate(b), 0o644); err != nil {
		t.Fatal(err)
	}
}

type cancellingReader struct {
	cancel    context.CancelFunc
	remaining int
	reads     int
}

func (c *cancellingReader) Read(p []byte) (int, error) {
	c.reads++
	if c.reads == 2 {
		c.cancel()
	}
	if c.remaining <= 0 {
		return 0, io.EOF
	}
	n := min(len(p), c.remaining)
	c.remaining -= n
	return n, nil
}

// countingReader yields a repeating pattern and records the largest slice it was handed.
type countingReader struct {
	remaining    int64
	largestSlice int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.largestSlice = max(c.largestSlice, len(p))
	if c.remaining <= 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), c.remaining))
	for i := 0; i < n; i++ {
		p[i] = byte(i)
	}
	c.remaining -= int64(n)
	return n, nil
}

// overstatingFS makes every opened file's Stat claim extra bytes.
type overstatingFS struct {
	fsx.FS
	extra int64
}

func (o *overstatingFS) Open(name string) (fsx.File, error) {
	f, err := o.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return &overstatingFile{File: f, extra: o.extra}, nil
}

type overstatingFile struct {
	fsx.File
	extra int64
}

func (f *overstatingFile) Stat() (fs.FileInfo, error) {
	info, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	return overstatingInfo{FileInfo: info, extra: f.extra}, nil
}

type overstatingInfo struct {
	fs.FileInfo
	extra int64
}

func (i overstatingInfo) Size() int64 { return i.FileInfo.Size() + i.extra }

// failingOpenFS fails every Open (read-only) with EIO.
type failingOpenFS struct{ fsx.FS }

func (f *failingOpenFS) Open(name string) (fsx.File, error) {
	return nil, errors.New("input/output error")
}
