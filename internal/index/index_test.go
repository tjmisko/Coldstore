package index

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/journal"
	"github.com/tjmisko/coldstore/internal/store"
)

var baseTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// script builds a journal-shaped record list with consecutive seqs.
type script struct {
	recs []journal.Record
}

func (s *script) add(rec journal.Record) *script {
	rec.Seq = uint64(len(s.recs) + 1)
	rec.At = baseTime.Add(time.Duration(rec.Seq) * time.Second)
	rec.By = "test"
	s.recs = append(s.recs, rec)
	return s
}

func (s *script) folder(name, kind string) *script {
	return s.add(journal.Record{Type: journal.TypeFolderDeclared, Folder: name, Kind: kind})
}

func (s *script) blob(h store.Hash, size int64) *script {
	return s.add(journal.Record{Type: journal.TypeBlobAdded, Hash: h.String(), Size: size})
}

func (s *script) item(id string, h store.Hash) *script {
	return s.add(journal.Record{Type: journal.TypeItemCreated, Item: journal.ItemID(id), Hash: h.String()})
}

func (s *script) place(folder, path, id string) *script {
	return s.add(journal.Record{Type: journal.TypePlacementSet, Folder: folder, Path: path, Item: journal.ItemID(id)})
}

func (s *script) placeAt(folder, path, id string, mtime time.Time) *script {
	return s.add(journal.Record{Type: journal.TypePlacementSet, Folder: folder, Path: path, Item: journal.ItemID(id), Mtime: &mtime})
}

func (s *script) tag(id string, add, remove []string) *script {
	return s.add(journal.Record{Type: journal.TypeItemTagged, Item: journal.ItemID(id), Add: add, Remove: remove})
}

func (s *script) trash(id string) *script {
	return s.add(journal.Record{Type: journal.TypeItemTrashed, Item: journal.ItemID(id)})
}

func hashOf(s string) store.Hash { return sha256.Sum256([]byte(s)) }

func openTemp(t *testing.T) *Index {
	t.Helper()
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix
}

func mustApplyAll(t *testing.T, ix *Index, s *script) {
	t.Helper()
	if err := ix.ApplyAll(s.recs); err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}
}

func dumpOf(t *testing.T, ix *Index) string {
	t.Helper()
	var buf bytes.Buffer
	if err := ix.Dump(&buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	return buf.String()
}

// fullScript exercises every record type.
func fullScript() *script {
	a, b, c := hashOf("a"), hashOf("b"), hashOf("c")
	s := &script{}
	s.folder("Photos", journal.FolderKindCollection).
		folder("Docs", journal.FolderKindSync).
		blob(a, 1).blob(b, 2).blob(c, 3).
		item("ITEM-A", a).item("ITEM-B", b).
		place("Photos", "2020/a.jpg", "ITEM-A").
		place("Photos", "2020/b.jpg", "ITEM-B").
		place("Docs", "b.txt", "ITEM-B").
		add(journal.Record{Type: journal.TypeItemVersioned, Item: "ITEM-A", Hash: c.String()}).
		tag("ITEM-A", []string{"Trip/Paris", "family"}, nil).
		tag("ITEM-B", []string{"Family"}, nil).
		tag("ITEM-A", nil, []string{"FAMILY"}).
		add(journal.Record{Type: journal.TypeItemDescribed, Item: "ITEM-A", Text: ptr("sunset")}).
		add(journal.Record{Type: journal.TypePlacementPinned, Folder: "Docs", Path: "b.txt", Pinned: ptr(true)}).
		trash("ITEM-B").
		add(journal.Record{Type: journal.TypeItemUntrashed, Item: "ITEM-B"}).
		trash("ITEM-B").
		add(journal.Record{Type: journal.TypePlacementCleared, Folder: "Docs", Path: "b.txt"})
	return s
}

func ptr[T any](v T) *T { return &v }

func TestApply(t *testing.T) {
	a, b := hashOf("a"), hashOf("b")

	t.Run("should create a folder when folder_declared is applied", func(t *testing.T) {
		ix := openTemp(t)
		mustApplyAll(t, ix, (&script{}).folder("Photos", journal.FolderKindCollection))
		f, ok, err := ix.FolderByName("Photos")
		if err != nil || !ok || f.Kind != journal.FolderKindCollection || f.DeclaredSeq != 1 {
			t.Fatalf("FolderByName = %+v, %v, %v", f, ok, err)
		}
	})

	t.Run("should record the blob once when blob_added is applied twice", func(t *testing.T) {
		ix := openTemp(t)
		mustApplyAll(t, ix, (&script{}).blob(a, 10).blob(a, 10))
		info, ok, err := ix.Blob(a)
		if err != nil || !ok || info.Size != 10 || info.AddedSeq != 1 {
			t.Fatalf("Blob = %+v, %v, %v", info, ok, err)
		}
	})

	t.Run("should create an item when item_created is applied", func(t *testing.T) {
		ix := openTemp(t)
		mustApplyAll(t, ix, (&script{}).folder("P", "collection").blob(a, 1).item("I1", a).place("P", "x", "I1"))
		e, ok, err := ix.Entry(4)
		if err != nil || !ok || e.Hash != a || e.Item != "I1" {
			t.Fatalf("Entry = %+v, %v, %v", e, ok, err)
		}
	})

	t.Run("should return an error when an item names an unknown blob", func(t *testing.T) {
		ix := openTemp(t)
		if err := ix.ApplyAll((&script{}).item("I1", a).recs); err == nil {
			t.Fatal("expected an error")
		}
		if ix.AppliedSeq() != 0 {
			t.Fatalf("AppliedSeq = %d after a failed apply", ix.AppliedSeq())
		}
	})

	t.Run("should move the old hash to versions when an item is versioned", func(t *testing.T) {
		ix := openTemp(t)
		s := (&script{}).folder("P", "collection").blob(a, 1).blob(b, 2).item("I1", a).place("P", "x", "I1").
			add(journal.Record{Type: journal.TypeItemVersioned, Item: "I1", Hash: b.String()})
		mustApplyAll(t, ix, s)
		e, _, _ := ix.Entry(5)
		if e.Hash != b {
			t.Fatalf("current hash = %s, want %s", e.Hash, b)
		}
		if !strings.Contains(dumpOf(t, ix), "version\t\"I1\"\t6\t\""+a.String()) {
			t.Fatalf("dump has no version row for the old hash:\n%s", dumpOf(t, ix))
		}
	})

	t.Run("should keep the entry id when a placed path is set again", func(t *testing.T) {
		ix := openTemp(t)
		s := (&script{}).folder("P", "collection").blob(a, 1).blob(b, 2).item("I1", a).item("I2", b).
			place("P", "x", "I1").place("P", "x", "I2")
		mustApplyAll(t, ix, s)
		p, ok, err := ix.PlacementAt("P", "x")
		if err != nil || !ok || p.EntryID != 6 || p.Item != "I2" {
			t.Fatalf("PlacementAt = %+v, %v, %v", p, ok, err)
		}
	})

	t.Run("should return an error when a placement names an unknown item", func(t *testing.T) {
		ix := openTemp(t)
		mustApplyAll(t, ix, (&script{}).folder("P", "collection"))
		bad := journal.Record{Seq: 2, At: baseTime, By: "t", Type: journal.TypePlacementSet, Folder: "P", Path: "x", Item: "NOPE"}
		if err := ix.Apply(bad); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("should return an error when a placement names an unknown folder", func(t *testing.T) {
		ix := openTemp(t)
		s := (&script{}).blob(a, 1).item("I1", a).place("Nowhere", "x", "I1")
		if err := ix.ApplyAll(s.recs); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("should remove the placement when placement_cleared is applied", func(t *testing.T) {
		ix := openTemp(t)
		s := (&script{}).folder("P", "collection").blob(a, 1).item("I1", a).place("P", "x", "I1").
			add(journal.Record{Type: journal.TypePlacementCleared, Folder: "P", Path: "x"})
		mustApplyAll(t, ix, s)
		if _, ok, _ := ix.PlacementAt("P", "x"); ok {
			t.Fatal("placement still present")
		}
	})

	t.Run("should set the pin when placement_pinned is applied", func(t *testing.T) {
		ix := openTemp(t)
		s := (&script{}).folder("P", "collection").blob(a, 1).item("I1", a).place("P", "x", "I1").
			add(journal.Record{Type: journal.TypePlacementPinned, Folder: "P", Path: "x", Pinned: ptr(true)})
		mustApplyAll(t, ix, s)
		if p, _, _ := ix.PlacementAt("P", "x"); !p.Pinned {
			t.Fatal("placement not pinned")
		}
	})

	t.Run("should fold tag case when the same tag is added twice with different case", func(t *testing.T) {
		ix := openTemp(t)
		s := (&script{}).folder("P", "collection").blob(a, 1).item("I1", a).place("P", "x", "I1").
			tag("I1", []string{"Family"}, nil).tag("I1", []string{"FAMILY"}, nil)
		mustApplyAll(t, ix, s)
		e, _, _ := ix.Entry(4)
		if len(e.Tags) != 1 || e.Tags[0] != "Family" {
			t.Fatalf("Tags = %v, want [Family]", e.Tags)
		}
	})

	t.Run("should delete a tag when its last assignment is removed", func(t *testing.T) {
		ix := openTemp(t)
		s := (&script{}).folder("P", "collection").blob(a, 1).item("I1", a).place("P", "x", "I1").
			tag("I1", []string{"Family"}, nil).tag("I1", nil, []string{"family"})
		mustApplyAll(t, ix, s)
		if strings.Contains(dumpOf(t, ix), "tag\t") {
			t.Fatalf("a tag row remains:\n%s", dumpOf(t, ix))
		}
	})

	t.Run("should return an error when a tag name is invalid", func(t *testing.T) {
		ix := openTemp(t)
		s := (&script{}).folder("P", "collection").blob(a, 1).item("I1", a).tag("I1", []string{"a//b"}, nil)
		if err := ix.ApplyAll(s.recs); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("should set the description when item_described is applied", func(t *testing.T) {
		ix := openTemp(t)
		s := (&script{}).folder("P", "collection").blob(a, 1).item("I1", a).place("P", "x", "I1").
			add(journal.Record{Type: journal.TypeItemDescribed, Item: "I1", Text: ptr("hello")})
		mustApplyAll(t, ix, s)
		if e, _, _ := ix.Entry(4); e.Description != "hello" {
			t.Fatalf("Description = %q", e.Description)
		}
	})

	t.Run("should set and clear trashed_at when an item is trashed and untrashed", func(t *testing.T) {
		ix := openTemp(t)
		s := (&script{}).folder("P", "collection").blob(a, 1).item("I1", a).place("P", "x", "I1").trash("I1")
		mustApplyAll(t, ix, s)
		e, _, _ := ix.Entry(4)
		if e.TrashedAt == nil || !e.TrashedAt.Equal(baseTime.Add(5*time.Second)) {
			t.Fatalf("TrashedAt = %v", e.TrashedAt)
		}
		if err := ix.Apply(journal.Record{Seq: 6, At: baseTime, By: "t", Type: journal.TypeItemUntrashed, Item: "I1"}); err != nil {
			t.Fatal(err)
		}
		if e, _, _ := ix.Entry(4); e.TrashedAt != nil {
			t.Fatalf("TrashedAt = %v after untrash", e.TrashedAt)
		}
	})

	t.Run("should use the record time as mtime when the placement has none", func(t *testing.T) {
		ix := openTemp(t)
		mustApplyAll(t, ix, (&script{}).folder("P", "collection").blob(a, 1).item("I1", a).place("P", "x", "I1"))
		p, _, _ := ix.PlacementAt("P", "x")
		if !p.Mtime.Equal(baseTime.Add(4 * time.Second)) {
			t.Fatalf("Mtime = %v", p.Mtime)
		}
	})

	t.Run("should skip a record when its seq is already applied", func(t *testing.T) {
		ix := openTemp(t)
		s := (&script{}).folder("P", "collection").blob(a, 1)
		mustApplyAll(t, ix, s)
		before := dumpOf(t, ix)
		if err := ix.Apply(s.recs[0]); err != nil {
			t.Fatalf("re-applying: %v", err)
		}
		if err := ix.ApplyAll(s.recs); err != nil {
			t.Fatalf("re-applying all: %v", err)
		}
		if after := dumpOf(t, ix); after != before {
			t.Fatalf("dump changed:\n%s\n%s", before, after)
		}
	})

	t.Run("should return an error when a seq is skipped", func(t *testing.T) {
		ix := openTemp(t)
		rec := journal.Record{Seq: 2, At: baseTime, By: "t", Type: journal.TypeFolderDeclared, Folder: "P", Kind: "collection"}
		if err := ix.Apply(rec); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("should return an error and apply nothing when a seq is skipped mid-slice", func(t *testing.T) {
		ix := openTemp(t)
		s := (&script{}).folder("P", "collection").blob(a, 1)
		s.recs[1].Seq = 3
		if err := ix.ApplyAll(s.recs); err == nil {
			t.Fatal("expected an error")
		}
		if ix.AppliedSeq() != 0 {
			t.Fatalf("AppliedSeq = %d", ix.AppliedSeq())
		}
	})

	t.Run("should return an error when the record type is unknown", func(t *testing.T) {
		ix := openTemp(t)
		if err := ix.Apply(journal.Record{Seq: 1, At: baseTime, By: "t", Type: "from_the_future"}); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("should leave an identical dump when the same journal is applied twice", func(t *testing.T) {
		ix := openTemp(t)
		s := fullScript()
		mustApplyAll(t, ix, s)
		first := dumpOf(t, ix)
		mustApplyAll(t, ix, s)
		if second := dumpOf(t, ix); second != first {
			t.Fatalf("dump changed after re-apply")
		}
	})

	t.Run("should leave an identical dump when replay resumes from the applied seq", func(t *testing.T) {
		s := fullScript()
		whole := openTemp(t)
		mustApplyAll(t, whole, s)

		resumed := openTemp(t)
		half := len(s.recs) / 2
		if err := resumed.ApplyAll(s.recs[:half]); err != nil {
			t.Fatal(err)
		}
		if err := resumed.ApplyAll(s.recs[half-2:]); err != nil {
			t.Fatal(err)
		}
		if dumpOf(t, resumed) != dumpOf(t, whole) {
			t.Fatal("dumps differ")
		}
	})
}

func TestOpen(t *testing.T) {
	t.Run("should open with the pragmas of the resource table", func(t *testing.T) {
		ix := openTemp(t)
		check := func(db *sql.DB, pragma, want string) {
			t.Helper()
			var got string
			if err := db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
				t.Fatalf("PRAGMA %s: %v", pragma, err)
			}
			if got != want {
				t.Fatalf("PRAGMA %s = %q, want %q", pragma, got, want)
			}
		}
		check(ix.write, "journal_mode", "wal")
		check(ix.write, "synchronous", "1")
		check(ix.write, "cache_size", "-8000")
		check(ix.read, "cache_size", "-8000")
		check(ix.read, "query_only", "1")
		check(ix.write, "user_version", fmt.Sprint(FormatVersion))
		if got := ix.write.Stats().MaxOpenConnections; got != 1 {
			t.Fatalf("writer connections = %d, want 1", got)
		}
		if got := ix.read.Stats().MaxOpenConnections; got != 2 {
			t.Fatalf("reader connections = %d, want 2", got)
		}
	})

	t.Run("should resume at the applied seq when the index is reopened", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "index.db")
		ix, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		mustApplyAll(t, ix, (&script{}).folder("P", "collection").blob(hashOf("a"), 1))
		if err := ix.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		if reopened.AppliedSeq() != 2 {
			t.Fatalf("AppliedSeq = %d, want 2", reopened.AppliedSeq())
		}
	})

	t.Run("should create an empty index when the file is zero bytes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "index.db")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		ix, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		ix.Close()
	})
}
