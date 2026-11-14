package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/fsx"
	"github.com/tjmisko/coldstore/internal/fsx/fsxtest"
)

type diskFault struct {
	name string
	// harmlessAtEOF: the fault can land on the read that only reports io.EOF,
	// which a short read does not change; Put may then succeed, and must verify.
	harmlessAtEOF bool
	apply         func(f *fsxtest.Faulty, callIndex int, trace []fsxtest.Call)
}

// nthOfOp is the 1-based count of calls with the same Op as trace[callIndex-1], up to it.
func nthOfOp(trace []fsxtest.Call, callIndex int) int {
	op := trace[callIndex-1].Op
	n := 0
	for _, call := range trace[:callIndex] {
		if call.Op == op {
			n++
		}
	}
	return n
}

func faultsFor(op fsxtest.Op) []diskFault {
	eio := diskFault{"EIO", false, func(f *fsxtest.Faulty, i int, _ []fsxtest.Call) { f.FailCall(i, syscall.EIO) }}
	enospc := diskFault{"ENOSPC", false, func(f *fsxtest.Faulty, i int, _ []fsxtest.Call) { f.FailCall(i, syscall.ENOSPC) }}
	short := func(err error) func(*fsxtest.Faulty, int, []fsxtest.Call) {
		return func(f *fsxtest.Faulty, i int, trace []fsxtest.Call) {
			f.Inject(fsxtest.Fault{Op: op, Nth: nthOfOp(trace, i), Err: err, Short: true})
		}
	}
	switch op {
	case fsxtest.OpWrite:
		return []diskFault{eio, enospc,
			{"short write", false, short(nil)},
			{"short write with EIO", false, short(syscall.EIO)}}
	case fsxtest.OpRead:
		return []diskFault{eio, {"short read", true, short(io.ErrUnexpectedEOF)}}
	case fsxtest.OpSync:
		return []diskFault{{"Sync failure", false, eio.apply}}
	case fsxtest.OpSyncDir:
		return []diskFault{{"SyncDir failure", false, eio.apply}}
	default:
		return []diskFault{eio}
	}
}

func TestPutInjectedDiskFaults(t *testing.T) {
	content := bytes.Repeat([]byte("fault injection payload "), 200)
	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, "source.bin")
	if err := os.WriteFile(sourcePath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	wantHash := hashOf(content)

	putFromSource := func(disk *Disk) error {
		src, err := os.Open(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		defer src.Close()
		_, _, err = disk.Put(context.Background(), src, nil)
		return err
	}
	newFaultyStore := func() (*Disk, *fsxtest.Faulty, string) {
		root := t.TempDir()
		faulty := fsxtest.NewFaulty(fsx.OS())
		disk, err := Open(faulty, root)
		if err != nil {
			t.Fatal(err)
		}
		faulty.Reset()
		return disk, faulty, root
	}

	cleanDisk, cleanFaulty, _ := newFaultyStore()
	if err := putFromSource(cleanDisk); err != nil {
		t.Fatalf("clean Put: %v", err)
	}
	trace := cleanFaulty.Calls()
	if len(trace) < 10 {
		t.Fatalf("suspiciously short clean trace: %v", trace)
	}
	lastIndex := len(trace)
	if trace[lastIndex-1].Op != fsxtest.OpRemove {
		t.Fatalf("expected the final call to be the staging remove, got %v", trace[lastIndex-1])
	}

	exercised := map[int]bool{}
	for index := 1; index <= len(trace); index++ {
		call := trace[index-1]
		for _, fault := range faultsFor(call.Op) {
			name := fmt.Sprintf("should return an error and link nothing under a hash when %s hits call %d (%s %s)",
				fault.name, index, call.Op, filepath.Base(call.Path))
			t.Run(name, func(t *testing.T) {
				disk, faulty, root := newFaultyStore()
				fault.apply(faulty, index, trace)

				err := putFromSource(disk)
				exercised[index] = true

				if index == lastIndex {
					// Step 9: the blob is durable; the failure is logged, not returned.
					if err != nil {
						t.Fatalf("Put returned %v when only the staging remove failed", err)
					}
					if verr := disk.Verify(context.Background(), wantHash); verr != nil {
						t.Fatalf("blob does not verify: %v", verr)
					}
				} else {
					harmless := err == nil && fault.harmlessAtEOF
					if err == nil && !harmless {
						t.Fatalf("Put returned nil")
					}
					if harmless {
						if verr := disk.Verify(context.Background(), wantHash); verr != nil {
							t.Fatalf("Put succeeded but the blob does not verify: %v", verr)
						}
					} else if n := len(walkAll(t, disk)); n != 0 {
						t.Errorf("Walk visits %d blobs after a failed Put", n)
					}
				}
				// tmp/ is empty or holds only what the next Open removes.
				reopened, openErr := Open(fsx.OS(), root)
				if openErr != nil {
					t.Fatalf("reopen: %v", openErr)
				}
				if names := listDir(t, filepath.Join(root, "tmp")); len(names) != 0 {
					t.Errorf("tmp holds %v after reopen", names)
				}
				if index != lastIndex && err != nil && len(walkAll(t, reopened)) != 0 {
					t.Errorf("blob appeared after reopen")
				}
				got, readErr := os.ReadFile(sourcePath)
				if readErr != nil || !bytes.Equal(got, content) {
					t.Errorf("source file changed (%v)", readErr)
				}
			})
		}
	}
	for index := 1; index <= len(trace); index++ {
		if !exercised[index] {
			t.Errorf("call index %d (%s) was never exercised", index, trace[index-1].Op)
		}
	}
}

// blockingSyncDirFS holds the first fsync of one directory until released, then
// fails it, so a second Put can race the first Put's undo.
type blockingSyncDirFS struct {
	fsx.FS
	target  string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingSyncDirFS) SyncDir(path string) error {
	if path != b.target {
		return b.FS.SyncDir(path)
	}
	first := false
	b.once.Do(func() { first = true })
	if !first {
		return b.FS.SyncDir(path)
	}
	close(b.entered)
	<-b.release
	return syscall.EIO
}

func TestPutConcurrentSameContentNeverReportsABlobAnotherPutRemoves(t *testing.T) {
	content := bytes.Repeat([]byte("racing put "), 100)
	h := hashOf(content)
	root := t.TempDir()
	probe, err := Open(fsx.OS(), root)
	if err != nil {
		t.Fatal(err)
	}
	faulty := &blockingSyncDirFS{
		FS:      fsx.OS(),
		target:  filepath.Dir(probe.Path(h)),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	disk, err := Open(faulty, root)
	if err != nil {
		t.Fatal(err)
	}

	putErrs := make(chan error, 2)
	put := func() {
		_, _, err := disk.Put(context.Background(), bytes.NewReader(content), nil)
		putErrs <- err
	}
	go put()
	<-faulty.entered
	go put()

	// Without per-hash serialisation the second Put returns nil here.
	var results []error
	select {
	case err := <-putErrs:
		results = append(results, err)
	case <-time.After(200 * time.Millisecond):
	}
	close(faulty.release)
	for len(results) < 2 {
		results = append(results, <-putErrs)
	}

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		}
	}
	_, onDisk, statErr := disk.Has(h)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if succeeded > 0 && !onDisk {
		t.Fatalf("%d Put(s) reported the blob stored but it is not on disk", succeeded)
	}
}

// failSyncDirFS fails fsync of the named directories.
type failSyncDirFS struct {
	fsx.FS
	failing map[string]bool
	synced  []string
}

func (f *failSyncDirFS) SyncDir(path string) error {
	f.synced = append(f.synced, path)
	if f.failing[path] {
		return syscall.EIO
	}
	return f.FS.SyncDir(path)
}

func TestPutSyncsParentDirectoriesWhenShardDirectoriesAlreadyExist(t *testing.T) {
	content := []byte("shard directory that exists but was never synced")
	h := hashOf(content)
	root := t.TempDir()
	probe, err := Open(fsx.OS(), root)
	if err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Dir(probe.Path(h))
	shard := filepath.Dir(leaf)
	shaRoot := filepath.Dir(shard)
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, failing := range []string{shaRoot, shard} {
		t.Run("should fail and keep nothing when fsync of "+filepath.Base(failing)+" fails", func(t *testing.T) {
			faulty := &failSyncDirFS{FS: fsx.OS(), failing: map[string]bool{failing: true}}
			disk, err := Open(faulty, root)
			if err != nil {
				t.Fatal(err)
			}
			faulty.failing[failing] = true
			if _, _, err := disk.Put(context.Background(), bytes.NewReader(content), nil); err == nil {
				t.Fatal("Put returned nil though a parent directory could not be synced")
			}
			if _, ok, _ := disk.Has(h); ok {
				t.Error("blob was linked before its parent directories were durable")
			}
		})
	}

	t.Run("should sync blobs/sha256 and the shard on every put when they already exist", func(t *testing.T) {
		faulty := &failSyncDirFS{FS: fsx.OS(), failing: map[string]bool{}}
		disk, err := Open(faulty, root)
		if err != nil {
			t.Fatal(err)
		}
		faulty.synced = nil
		if _, _, err := disk.Put(context.Background(), bytes.NewReader(content), nil); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, p := range faulty.synced {
			seen[p] = true
		}
		for _, want := range []string{shaRoot, shard, leaf} {
			if !seen[want] {
				t.Errorf("%s was not synced; synced %v", want, faulty.synced)
			}
		}
	})
}
