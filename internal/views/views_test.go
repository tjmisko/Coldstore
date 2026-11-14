package views

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/fsx"
	"github.com/tjmisko/coldstore/internal/fsx/fsxtest"
	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/journal"
	"github.com/tjmisko/coldstore/internal/store"
)

var baseTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

type fixture struct {
	t      *testing.T
	root   string
	st     *store.Disk
	ix     *index.Index
	tree   *Tree
	recs   []journal.Record
	faulty *fsxtest.Faulty
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	faulty := fsxtest.NewFaulty(fsx.OS())
	st, err := store.Open(fsx.OS(), root)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := index.Open(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	f := &fixture{t: t, root: root, st: st, ix: ix, faulty: faulty}
	f.tree = New(faulty, filepath.Join(root, "views"), st.Path)
	f.apply(journal.Record{Type: journal.TypeFolderDeclared, Folder: "Photos", Kind: journal.FolderKindCollection})
	return f
}

// apply gives rec the next seq, applies it to the index and remembers it.
func (f *fixture) apply(rec journal.Record) journal.Record {
	f.t.Helper()
	rec.Seq = uint64(len(f.recs) + 1)
	rec.At = baseTime.Add(time.Duration(rec.Seq) * time.Second)
	rec.By = "test"
	if err := f.ix.Apply(rec); err != nil {
		f.t.Fatalf("Apply %s: %v", rec.Type, err)
	}
	f.recs = append(f.recs, rec)
	return rec
}

func (f *fixture) blob(content string) store.Hash {
	f.t.Helper()
	h, size, err := f.st.Put(context.Background(), strings.NewReader(content), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.apply(journal.Record{Type: journal.TypeBlobAdded, Hash: h.String(), Size: size})
	return h
}

// place creates an item with the content at folder/path and syncs views.
func (f *fixture) place(item, path, content string) store.Hash {
	f.t.Helper()
	h := f.blob(content)
	f.apply(journal.Record{Type: journal.TypeItemCreated, Item: journal.ItemID(item), Hash: h.String()})
	set := f.apply(journal.Record{Type: journal.TypePlacementSet, Folder: "Photos", Path: path, Item: journal.ItemID(item)})
	if err := f.tree.Sync(f.ix, []journal.Record{set}); err != nil {
		f.t.Fatalf("Sync: %v", err)
	}
	return h
}

func (f *fixture) viewFile(parts ...string) string {
	return filepath.Join(append([]string{f.root, "views"}, parts...)...)
}

func (f *fixture) manifest() string {
	f.t.Helper()
	var buf bytes.Buffer
	if err := f.tree.Manifest(context.Background(), f.ix, &buf); err != nil {
		f.t.Fatal(err)
	}
	return buf.String()
}

func sameInode(t *testing.T, a, b string) bool {
	t.Helper()
	ai, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	bi, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(ai, bi)
}

func TestSync(t *testing.T) {
	t.Run("should hardlink the blob at views/<folder>/<path> when a placement is set", func(t *testing.T) {
		f := newFixture(t)
		f.place("I1", "2020/a.jpg", "alpha")
		got, err := os.ReadFile(f.viewFile("Photos", "2020", "a.jpg"))
		if err != nil || string(got) != "alpha" {
			t.Fatalf("view content = %q, %v", got, err)
		}
	})

	t.Run("should share the blob inode when a placement is set", func(t *testing.T) {
		f := newFixture(t)
		h := f.place("I1", "a.jpg", "alpha")
		if !sameInode(t, f.viewFile("Photos", "a.jpg"), f.st.Path(h)) {
			t.Fatal("view link and blob are different inodes")
		}
	})

	t.Run("should remove the link when the item is trashed", func(t *testing.T) {
		f := newFixture(t)
		f.place("I1", "2020/a.jpg", "alpha")
		trashed := f.apply(journal.Record{Type: journal.TypeItemTrashed, Item: "I1"})
		if err := f.tree.Sync(f.ix, []journal.Record{trashed}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(f.viewFile("Photos", "2020", "a.jpg")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("link still present: %v", err)
		}
		if _, err := os.Lstat(f.viewFile("Photos")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("empty directories were left behind: %v", err)
		}
	})

	t.Run("should restore the link when the item is untrashed", func(t *testing.T) {
		f := newFixture(t)
		h := f.place("I1", "a.jpg", "alpha")
		trashed := f.apply(journal.Record{Type: journal.TypeItemTrashed, Item: "I1"})
		must(t, f.tree.Sync(f.ix, []journal.Record{trashed}))
		untrashed := f.apply(journal.Record{Type: journal.TypeItemUntrashed, Item: "I1"})
		must(t, f.tree.Sync(f.ix, []journal.Record{untrashed}))
		if !sameInode(t, f.viewFile("Photos", "a.jpg"), f.st.Path(h)) {
			t.Fatal("link not restored")
		}
	})

	t.Run("should remove the link when the placement is cleared", func(t *testing.T) {
		f := newFixture(t)
		f.place("I1", "a.jpg", "alpha")
		cleared := f.apply(journal.Record{Type: journal.TypePlacementCleared, Folder: "Photos", Path: "a.jpg"})
		must(t, f.tree.Sync(f.ix, []journal.Record{cleared}))
		if _, err := os.Lstat(f.viewFile("Photos", "a.jpg")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("link still present: %v", err)
		}
	})

	t.Run("should point the link at the new bytes when the item is versioned", func(t *testing.T) {
		f := newFixture(t)
		f.place("I1", "a.jpg", "alpha")
		h2 := f.blob("beta")
		versioned := f.apply(journal.Record{Type: journal.TypeItemVersioned, Item: "I1", Hash: h2.String()})
		must(t, f.tree.Sync(f.ix, []journal.Record{versioned}))
		if !sameInode(t, f.viewFile("Photos", "a.jpg"), f.st.Path(h2)) {
			t.Fatal("link still points at the old bytes")
		}
	})

	t.Run("should do nothing when the record type does not affect views", func(t *testing.T) {
		f := newFixture(t)
		f.faulty.Reset()
		tag := f.recs[0]
		tag.Type = journal.TypeItemTagged
		must(t, f.tree.Sync(f.ix, []journal.Record{tag}))
		if len(f.faulty.Calls()) != 0 {
			t.Fatalf("calls = %v", f.faulty.Calls())
		}
	})

	t.Run("should reject a path that escapes views when a segment is ..", func(t *testing.T) {
		f := newFixture(t)
		h := f.blob("alpha")
		f.apply(journal.Record{Type: journal.TypeItemCreated, Item: "I1", Hash: h.String()})
		set := f.apply(journal.Record{Type: journal.TypePlacementSet, Folder: "Photos", Path: "../../escape.txt", Item: "I1"})
		err := f.tree.Sync(f.ix, []journal.Record{set})
		if !errors.Is(err, ErrBadPath) {
			t.Fatalf("Sync error = %v, want ErrBadPath", err)
		}
		if _, err := os.Lstat(filepath.Join(f.root, "escape.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a file escaped views: %v", err)
		}
	})

	t.Run("should reject empty and dot segments", func(t *testing.T) {
		for _, path := range []string{"a//b", "./a", "a/./b", "a/", "/a", "a/.."} {
			if _, err := segments("Photos", path); !errors.Is(err, ErrBadPath) {
				t.Errorf("segments(%q) error = %v, want ErrBadPath", path, err)
			}
		}
		if _, err := segments("a/b", "x"); !errors.Is(err, ErrBadPath) {
			t.Errorf("a folder with a slash was accepted")
		}
	})

	t.Run("should not follow a symlink when creating parent directories", func(t *testing.T) {
		f := newFixture(t)
		outside := t.TempDir()
		must(t, os.MkdirAll(filepath.Join(f.root, "views"), 0o755))
		must(t, os.Symlink(outside, f.viewFile("Photos")))
		h := f.blob("alpha")
		f.apply(journal.Record{Type: journal.TypeItemCreated, Item: "I1", Hash: h.String()})
		set := f.apply(journal.Record{Type: journal.TypePlacementSet, Folder: "Photos", Path: "sub/a.jpg", Item: "I1"})
		if err := f.tree.Sync(f.ix, []journal.Record{set}); err == nil {
			t.Fatal("expected an error")
		}
		entries, _ := os.ReadDir(outside)
		if len(entries) != 0 {
			t.Fatalf("a symlinked directory was written through: %v", entries)
		}
	})

	t.Run("should return an error and leave the previous link intact when Link fails", func(t *testing.T) {
		f := newFixture(t)
		h1 := f.place("I1", "a.jpg", "alpha")
		h2 := f.blob("beta")
		versioned := f.apply(journal.Record{Type: journal.TypeItemVersioned, Item: "I1", Hash: h2.String()})
		f.faulty.Inject(fsxtest.Fault{Op: fsxtest.OpLink, Nth: 1, Err: errors.New("injected link failure")})
		if err := f.tree.Sync(f.ix, []journal.Record{versioned}); err == nil {
			t.Fatal("expected an error")
		}
		if !sameInode(t, f.viewFile("Photos", "a.jpg"), f.st.Path(h1)) {
			t.Fatal("the previous link was damaged")
		}
		entries, _ := os.ReadDir(f.viewFile("Photos"))
		if len(entries) != 1 {
			t.Fatalf("directory has %d entries, want only the old link", len(entries))
		}
	})

	t.Run("should return an error and leave no temp file when Rename fails", func(t *testing.T) {
		f := newFixture(t)
		h := f.blob("alpha")
		f.apply(journal.Record{Type: journal.TypeItemCreated, Item: "I1", Hash: h.String()})
		set := f.apply(journal.Record{Type: journal.TypePlacementSet, Folder: "Photos", Path: "a.jpg", Item: "I1"})
		f.faulty.Inject(fsxtest.Fault{Op: fsxtest.OpRename, Nth: 1, Err: errors.New("injected rename failure")})
		if err := f.tree.Sync(f.ix, []journal.Record{set}); err == nil {
			t.Fatal("expected an error")
		}
		entries, _ := os.ReadDir(f.viewFile("Photos"))
		if len(entries) != 0 {
			t.Fatalf("leftovers: %v", entries)
		}
	})
}

func TestRegenerate(t *testing.T) {
	ctx := context.Background()

	t.Run("should regenerate an identical manifest when views is deleted", func(t *testing.T) {
		f := newFixture(t)
		f.place("I1", "2020/a.jpg", "alpha")
		f.place("I2", "2020/b.jpg", "beta")
		f.place("I3", "c.jpg", "gamma")
		trashed := f.apply(journal.Record{Type: journal.TypeItemTrashed, Item: "I3"})
		must(t, f.tree.Sync(f.ix, []journal.Record{trashed}))
		before := f.manifest()
		if strings.Count(before, "\n") != 2 {
			t.Fatalf("manifest should have two lines:\n%s", before)
		}

		must(t, os.RemoveAll(filepath.Join(f.root, "views")))
		report, err := f.tree.Regenerate(ctx, f.ix)
		must(t, err)
		if report.Linked != 2 {
			t.Fatalf("report = %+v", report)
		}
		if after := f.manifest(); after != before {
			t.Fatalf("manifest changed:\n%s\n--- before\n%s", after, before)
		}
	})

	t.Run("should list the blob hash on every manifest line", func(t *testing.T) {
		f := newFixture(t)
		h := f.place("I1", "a.jpg", "alpha")
		if got, want := f.manifest(), "Photos/a.jpg\t"+h.String()+"\n"; got != want {
			t.Fatalf("manifest = %q, want %q", got, want)
		}
	})

	t.Run("should remove a file the index does not name when regenerating", func(t *testing.T) {
		f := newFixture(t)
		h := f.place("I1", "a.jpg", "alpha")
		must(t, os.WriteFile(f.viewFile("Photos", "stray.txt"), []byte("x"), 0o644))
		must(t, os.MkdirAll(f.viewFile("Ghost", "deep"), 0o755))
		must(t, os.WriteFile(f.viewFile("Ghost", "deep", "f"), []byte("x"), 0o644))
		must(t, os.WriteFile(f.viewFile("top.txt"), []byte("x"), 0o644))
		must(t, os.Symlink("/nonexistent", f.viewFile("Photos", "dangling")))
		blobBefore, _ := os.ReadFile(f.st.Path(h))

		report, err := f.tree.Regenerate(ctx, f.ix)
		must(t, err)
		if report.Removed != 4 {
			t.Fatalf("report = %+v, want 4 removed", report)
		}
		for _, gone := range [][]string{{"Photos", "stray.txt"}, {"Ghost"}, {"top.txt"}, {"Photos", "dangling"}} {
			if _, err := os.Lstat(f.viewFile(gone...)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%v still present: %v", gone, err)
			}
		}
		if blobAfter, _ := os.ReadFile(f.st.Path(h)); !bytes.Equal(blobAfter, blobBefore) {
			t.Fatal("regenerate modified a blob")
		}
		if !sameInode(t, f.viewFile("Photos", "a.jpg"), f.st.Path(h)) {
			t.Fatal("the legitimate link was lost")
		}
	})

	t.Run("should replace a link to the wrong inode when regenerating", func(t *testing.T) {
		f := newFixture(t)
		h := f.place("I1", "a.jpg", "alpha")
		must(t, os.Remove(f.viewFile("Photos", "a.jpg")))
		must(t, os.WriteFile(f.viewFile("Photos", "a.jpg"), []byte("copy"), 0o644))
		if got := f.manifest(); !strings.HasSuffix(got, "\t-\n") {
			t.Fatalf("manifest should flag the wrong inode: %q", got)
		}
		report, err := f.tree.Regenerate(ctx, f.ix)
		must(t, err)
		if report.Linked != 1 || !sameInode(t, f.viewFile("Photos", "a.jpg"), f.st.Path(h)) {
			t.Fatalf("report = %+v", report)
		}
	})

	t.Run("should leave no temp file when linking is interrupted and Regenerate runs", func(t *testing.T) {
		f := newFixture(t)
		h := f.place("I1", "a.jpg", "alpha")
		// A crash between Link and Rename leaves exactly this behind.
		stale := f.viewFile("Photos", "a.jpg") + TempSuffix
		must(t, os.Link(f.st.Path(h), stale))
		_, err := f.tree.Regenerate(ctx, f.ix)
		must(t, err)
		if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temp file survived: %v", err)
		}
	})

	t.Run("should clear a stale temp link when the same path is linked again", func(t *testing.T) {
		f := newFixture(t)
		h := f.blob("alpha")
		f.apply(journal.Record{Type: journal.TypeItemCreated, Item: "I1", Hash: h.String()})
		set := f.apply(journal.Record{Type: journal.TypePlacementSet, Folder: "Photos", Path: "a.jpg", Item: "I1"})
		must(t, os.MkdirAll(f.viewFile("Photos"), 0o755))
		must(t, os.Link(f.st.Path(h), f.viewFile("Photos", "a.jpg")+TempSuffix))
		must(t, f.tree.Sync(f.ix, []journal.Record{set}))
		entries, _ := os.ReadDir(f.viewFile("Photos"))
		if len(entries) != 1 {
			t.Fatalf("entries = %v", entries)
		}
	})

	t.Run("should skip a placement whose blob is missing and still link the rest", func(t *testing.T) {
		f := newFixture(t)
		gone := f.place("I1", "a.jpg", "alpha")
		f.place("I2", "b.jpg", "beta")
		must(t, os.RemoveAll(filepath.Join(f.root, "views")))
		must(t, os.Chmod(filepath.Dir(f.st.Path(gone)), 0o755))
		must(t, os.Remove(f.st.Path(gone)))
		if _, err := f.tree.Regenerate(ctx, f.ix); err != nil {
			t.Fatalf("Regenerate: %v", err)
		}
		if _, err := os.Lstat(f.viewFile("Photos", "b.jpg")); err != nil {
			t.Fatalf("healthy link missing: %v", err)
		}
	})

	t.Run("should stop when the context is cancelled", func(t *testing.T) {
		f := newFixture(t)
		f.place("I1", "a.jpg", "alpha")
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := f.tree.Regenerate(cancelled, f.ix); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
