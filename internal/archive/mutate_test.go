package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/store"
)

func entryTags(t *testing.T, s *Service, id int64) []string {
	t.Helper()
	entry, found, err := s.Index().Entry(id)
	if err != nil || !found {
		t.Fatalf("entry %d: found=%v err=%v", id, found, err)
	}
	tags := append([]string(nil), entry.Tags...)
	slices.Sort(tags)
	return tags
}

func TestTag(t *testing.T) {
	ctx := context.Background()

	t.Run("should tag every placement of the item when one entry is tagged", func(t *testing.T) {
		s := openService(t, testOptions(t))
		first := mustIngest(t, s, "photos", "a.txt", "alpha")
		second := mustIngest(t, s, "photos", "copy/a.txt", "alpha")
		if first.Item != second.Item || first.EntryID == second.EntryID {
			t.Fatalf("fixture: %+v %+v", first, second)
		}
		result, err := s.Tag(ctx, "test", []TagChange{{IDs: []int64{first.EntryID}, Add: []string{"Holiday/Beach"}}})
		if err != nil {
			t.Fatal(err)
		}
		if result.Updated != 1 || len(result.Created) != 1 {
			t.Fatalf("result %+v", result)
		}
		for _, id := range []int64{first.EntryID, second.EntryID} {
			if tags := entryTags(t, s, id); len(tags) != 1 || tags[0] != "Holiday/Beach" {
				t.Fatalf("entry %d tags %v", id, tags)
			}
		}
	})

	t.Run("should return an undo that restores each item's exact prior tags", func(t *testing.T) {
		s := openService(t, testOptions(t))
		a := mustIngest(t, s, "photos", "a.txt", "alpha")
		b := mustIngest(t, s, "photos", "b.txt", "bravo")
		if _, err := s.Tag(ctx, "test", []TagChange{{IDs: []int64{a.EntryID}, Add: []string{"red", "blue"}}}); err != nil {
			t.Fatal(err)
		}
		result, err := s.Tag(ctx, "test", []TagChange{
			{IDs: []int64{a.EntryID, b.EntryID}, Add: []string{"green"}, Remove: []string{"red"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := entryTags(t, s, a.EntryID); !slices.Equal(got, []string{"blue", "green"}) {
			t.Fatalf("a after change: %v", got)
		}
		if got := entryTags(t, s, b.EntryID); !slices.Equal(got, []string{"green"}) {
			t.Fatalf("b after change: %v", got)
		}
		if result.Updated != 2 || len(result.Undo.Changes) != 2 {
			t.Fatalf("result %+v", result)
		}
		if _, err := s.Tag(ctx, "test", result.Undo.Changes); err != nil {
			t.Fatal(err)
		}
		if got := entryTags(t, s, a.EntryID); !slices.Equal(got, []string{"blue", "red"}) {
			t.Fatalf("a after undo: %v", got)
		}
		if got := entryTags(t, s, b.EntryID); len(got) != 0 {
			t.Fatalf("b after undo: %v", got)
		}
	})

	t.Run("should append nothing when the tag set does not change", func(t *testing.T) {
		s := openService(t, testOptions(t))
		a := mustIngest(t, s, "photos", "a.txt", "alpha")
		if _, err := s.Tag(ctx, "test", []TagChange{{IDs: []int64{a.EntryID}, Add: []string{"red"}}}); err != nil {
			t.Fatal(err)
		}
		before := s.core.j.LastSeq()
		result, err := s.Tag(ctx, "test", []TagChange{
			{IDs: []int64{a.EntryID}, Add: []string{"RED"}, Remove: []string{"absent"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Updated != 0 || len(result.Undo.Changes) != 0 || s.core.j.LastSeq() != before {
			t.Fatalf("result %+v seq %d -> %d", result, before, s.core.j.LastSeq())
		}
	})

	t.Run("should return ErrEntryNotFound and change nothing when an id is unknown", func(t *testing.T) {
		s := openService(t, testOptions(t))
		a := mustIngest(t, s, "photos", "a.txt", "alpha")
		before := s.core.j.LastSeq()
		_, err := s.Tag(ctx, "test", []TagChange{{IDs: []int64{a.EntryID, 999999}, Add: []string{"x"}}})
		if !errors.Is(err, ErrEntryNotFound) || s.core.j.LastSeq() != before {
			t.Fatalf("err = %v, seq %d -> %d", err, before, s.core.j.LastSeq())
		}
	})

	t.Run("should reject the whole request when a tag name is invalid", func(t *testing.T) {
		s := openService(t, testOptions(t))
		a := mustIngest(t, s, "photos", "a.txt", "alpha")
		before := s.core.j.LastSeq()
		_, err := s.Tag(ctx, "test", []TagChange{{IDs: []int64{a.EntryID}, Add: []string{"ok", "  "}}})
		if err == nil || s.core.j.LastSeq() != before {
			t.Fatalf("err = %v seq %d -> %d", err, before, s.core.j.LastSeq())
		}
	})
}

func TestTrash(t *testing.T) {
	ctx := context.Background()

	t.Run("should hide the entry from the live list and remove its view when trashed", func(t *testing.T) {
		opts := testOptions(t)
		s := openService(t, opts)
		a := mustIngest(t, s, "photos", "dir/a.txt", "alpha")
		view := filepath.Join(opts.Root, "views", "photos", "dir", "a.txt")
		if _, err := os.Stat(view); err != nil {
			t.Fatalf("view missing before trash: %v", err)
		}
		updated, err := s.Trash(ctx, "test", []int64{a.EntryID}, true)
		if err != nil || updated != 1 {
			t.Fatalf("updated %d err %v", updated, err)
		}
		live, err := s.Index().Entries(index.EntryQuery{Folder: "photos"})
		if err != nil || len(live) != 0 {
			t.Fatalf("live %d err %v", len(live), err)
		}
		if _, err := os.Stat(view); !os.IsNotExist(err) {
			t.Fatalf("view still present after trash: %v", err)
		}
		trashedOnly := true
		trashed, err := s.Index().Entries(index.EntryQuery{Folder: "photos", Trashed: &trashedOnly})
		if err != nil || len(trashed) != 1 {
			t.Fatalf("trashed %d err %v", len(trashed), err)
		}
		if again, err := s.Trash(ctx, "test", []int64{a.EntryID}, true); err != nil || again != 0 {
			t.Fatalf("trashing twice: %d %v", again, err)
		}
	})

	t.Run("should restore the view when untrashed", func(t *testing.T) {
		opts := testOptions(t)
		s := openService(t, opts)
		a := mustIngest(t, s, "photos", "a.txt", "alpha")
		if _, err := s.Trash(ctx, "test", []int64{a.EntryID}, true); err != nil {
			t.Fatal(err)
		}
		updated, err := s.Trash(ctx, "test", []int64{a.EntryID}, false)
		if err != nil || updated != 1 {
			t.Fatalf("updated %d err %v", updated, err)
		}
		if _, err := os.Stat(filepath.Join(opts.Root, "views", "photos", "a.txt")); err != nil {
			t.Fatalf("view not restored: %v", err)
		}
		live, err := s.Index().Entries(index.EntryQuery{Folder: "photos"})
		if err != nil || len(live) != 1 {
			t.Fatalf("live %d err %v", len(live), err)
		}
	})

	t.Run("should return ErrEntryNotFound when an id is unknown", func(t *testing.T) {
		s := openService(t, testOptions(t))
		if _, err := s.Trash(ctx, "test", []int64{12345}, true); !errors.Is(err, ErrEntryNotFound) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("should append one record per item when two placements of it are trashed together", func(t *testing.T) {
		s := openService(t, testOptions(t))
		a := mustIngest(t, s, "photos", "a.txt", "alpha")
		b := mustIngest(t, s, "photos", "b.txt", "alpha")
		before := s.core.j.LastSeq()
		updated, err := s.Trash(ctx, "test", []int64{a.EntryID, b.EntryID}, true)
		if err != nil || updated != 2 || s.core.j.LastSeq() != before+1 {
			t.Fatalf("updated %d err %v seq %d -> %d", updated, err, before, s.core.j.LastSeq())
		}
	})
}

func TestReceipt(t *testing.T) {
	ctx := context.Background()

	t.Run("should report held, verified and in_collection per hash when a receipt is requested", func(t *testing.T) {
		s := openService(t, testOptions(t))
		inPhotos := mustIngest(t, s, "photos", "a.txt", "alpha")
		inBooks := mustIngest(t, s, "books", "b.txt", "bravo")
		var unknown store.Hash
		unknown[0] = 7
		lines, err := s.Receipt(ctx, "photos", []store.Hash{inPhotos.Hash, inBooks.Hash, unknown})
		if err != nil || len(lines) != 3 {
			t.Fatalf("lines %+v err %v", lines, err)
		}
		want := []ReceiptLine{
			{Hash: inPhotos.Hash.String(), Held: true, Verified: true, InCollection: true, Size: 5, Status: ReceiptVerified},
			{Hash: inBooks.Hash.String(), Held: true, Verified: true, InCollection: false, Size: 5, Status: ReceiptVerified},
			{Hash: unknown.String(), Status: ReceiptNotHeld},
		}
		for i := range want {
			if lines[i] != want[i] {
				t.Errorf("line %d = %+v, want %+v", i, lines[i], want[i])
			}
		}
	})

	t.Run("should not count a trashed entry as in_collection", func(t *testing.T) {
		s := openService(t, testOptions(t))
		a := mustIngest(t, s, "photos", "a.txt", "alpha")
		if _, err := s.Trash(ctx, "test", []int64{a.EntryID}, true); err != nil {
			t.Fatal(err)
		}
		lines, err := s.Receipt(ctx, "photos", []store.Hash{a.Hash})
		if err != nil || !lines[0].Held || lines[0].InCollection {
			t.Fatalf("lines %+v err %v", lines, err)
		}
	})

	t.Run("should report verified false and mark the blob bad when a receipted blob is damaged", func(t *testing.T) {
		s := openService(t, testOptions(t))
		a := mustIngest(t, s, "photos", "a.txt", "alpha")
		blob := s.Store().Path(a.Hash)
		if err := os.Chmod(blob, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(blob, []byte("alphX"), 0o644); err != nil {
			t.Fatal(err)
		}
		lines, err := s.Receipt(ctx, "photos", []store.Hash{a.Hash})
		if err != nil {
			t.Fatal(err)
		}
		if !lines[0].Held || lines[0].Verified || lines[0].Status != ReceiptBad {
			t.Fatalf("line %+v", lines[0])
		}
		info, _, err := s.Index().Blob(a.Hash)
		if err != nil || !info.Bad {
			t.Fatalf("blob info %+v err %v", info, err)
		}
		events, err := s.Index().Events("", "blob_bad", 10)
		if err != nil || len(events) != 1 {
			t.Fatalf("events %+v err %v", events, err)
		}
	})

	t.Run("should return ErrBlobBad when OpenVerified is asked for a bad blob", func(t *testing.T) {
		s := openService(t, testOptions(t))
		a := mustIngest(t, s, "photos", "a.txt", "alpha")
		rc, size, err := s.OpenVerified(a.Hash)
		if err != nil || size != 5 {
			t.Fatalf("size %d err %v", size, err)
		}
		_ = rc.Close()
		if err := s.Index().BlobBadSet(a.Hash, true, "hash_mismatch"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.OpenVerified(a.Hash); !errors.Is(err, ErrBlobBad) {
			t.Fatalf("err = %v", err)
		}
		var unknown store.Hash
		if _, _, err := s.OpenVerified(unknown); !errors.Is(err, ErrBlobNotHeld) {
			t.Fatalf("unknown err = %v", err)
		}
	})

	t.Run("should return ErrCollectionUnknown when the collection is not declared", func(t *testing.T) {
		s := openService(t, testOptions(t))
		if _, err := s.Receipt(ctx, "music", nil); !errors.Is(err, ErrCollectionUnknown) {
			t.Fatalf("err = %v", err)
		}
	})
}
