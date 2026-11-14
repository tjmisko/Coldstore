package ingest

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/index"
	"github.com/tjmisko/coldstore/internal/journal"
	"github.com/tjmisko/coldstore/internal/store"
)

func hashOf(s string) store.Hash { return store.Hash(sha256.Sum256([]byte(s))) }

// fakeState is an in-memory State: placements keyed by folder and path.
type fakeState struct {
	folders    map[string]bool
	placements map[[2]string]index.Placement
	current    map[journal.ItemID]store.Hash
	blobs      map[store.Hash]bool
	nextEntry  int64
}

func newFakeState(folders ...string) *fakeState {
	s := &fakeState{
		folders:    map[string]bool{},
		placements: map[[2]string]index.Placement{},
		current:    map[journal.ItemID]store.Hash{},
		blobs:      map[store.Hash]bool{},
	}
	for _, f := range folders {
		s.folders[f] = true
	}
	return s
}

func (s *fakeState) put(folder, path string, item journal.ItemID, h store.Hash, hot bool) index.Placement {
	s.nextEntry++
	p := index.Placement{EntryID: s.nextEntry, Folder: folder, Path: path, Item: item, Hot: hot}
	s.placements[[2]string{folder, path}] = p
	s.current[item] = h
	s.blobs[h] = true
	return p
}

func (s *fakeState) FolderByName(name string) (index.Folder, bool, error) {
	return index.Folder{Name: name, Kind: "collection"}, s.folders[name], nil
}

func (s *fakeState) PlacementAt(folder, path string) (index.Placement, bool, error) {
	p, ok := s.placements[[2]string{folder, path}]
	return p, ok, nil
}

func (s *fakeState) PlacementsByCurrentHash(folder string, h store.Hash) ([]index.Placement, error) {
	var out []index.Placement
	for _, p := range s.placements {
		if p.Folder == folder && s.current[p.Item] == h {
			out = append(out, p)
		}
	}
	// deterministic order
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].Path < out[i].Path {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func (s *fakeState) PlacementPathConflict(folder, path string) (string, bool, error) {
	for key := range s.placements {
		if key[0] != folder {
			continue
		}
		if strings.HasPrefix(path, key[1]+"/") || strings.HasPrefix(key[1], path+"/") {
			return key[1], true, nil
		}
	}
	return "", false, nil
}

func (s *fakeState) Entry(id int64) (index.Entry, bool, error) {
	for _, p := range s.placements {
		if p.EntryID == id {
			return index.Entry{Placement: p, Hash: s.current[p.Item]}, true, nil
		}
	}
	return index.Entry{}, false, nil
}

func (s *fakeState) Blob(h store.Hash) (index.BlobInfo, bool, error) {
	return index.BlobInfo{}, s.blobs[h], nil
}

func fixedID(id string) func() ItemID { return func() ItemID { return ItemID(id) } }

func never(string, string) bool { return false }

func recordTypes(recs []journal.Record) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Type)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestResolve(t *testing.T) {
	arrival := func(folder, path string) Arrival {
		return Arrival{Folder: folder, Path: path, Mtime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	}

	t.Run("should emit nothing when the path already means these bytes", func(t *testing.T) {
		st := newFakeState("photos")
		h := hashOf("a")
		placed := st.put("photos", "a.txt", "ITEM1", h, false)
		recs, res, err := Resolve(st, arrival("photos", "a.txt"), h, 1, "import", fixedID("NEW"), never)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 0 || res.Outcome != OutcomeUnchanged || res.Item != "ITEM1" || res.EntryID != placed.EntryID {
			t.Fatalf("recs=%v res=%+v", recs, res)
		}
	})

	t.Run("should add a version to the same item when a hot path changes", func(t *testing.T) {
		st := newFakeState("photos")
		st.put("photos", "a.txt", "ITEM1", hashOf("old"), true)
		recs, res, err := Resolve(st, arrival("photos", "a.txt"), hashOf("new"), 3, "import", fixedID("NEW"), never)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{journal.TypeBlobAdded, journal.TypeItemVersioned, journal.TypePlacementSet}
		if !equalStrings(recordTypes(recs), want) || res.Outcome != OutcomeVersioned || res.Item != "ITEM1" {
			t.Fatalf("types=%v res=%+v", recordTypes(recs), res)
		}
		if recs[1].Item != "ITEM1" || recs[2].Item != "ITEM1" {
			t.Fatalf("records name another item: %+v", recs)
		}
	})

	t.Run("should create a new item beside the first when a collection path arrives with different bytes", func(t *testing.T) {
		st := newFakeState("photos")
		st.put("photos", "dir/a.txt", "ITEM1", hashOf("old"), false)
		h := hashOf("new")
		recs, res, err := Resolve(st, arrival("photos", "dir/a.txt"), h, 3, "import", fixedID("ITEM2"), never)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{journal.TypeBlobAdded, journal.TypeItemCreated, journal.TypePlacementSet}
		if !equalStrings(recordTypes(recs), want) || res.Outcome != OutcomePlacedBeside {
			t.Fatalf("types=%v res=%+v", recordTypes(recs), res)
		}
		beside := BesidePath("dir/a.txt", h)
		if res.Path != beside || recs[2].Path != beside || recs[2].Asked != "dir/a.txt" || recs[2].Item != "ITEM2" {
			t.Fatalf("placement_set=%+v res=%+v", recs[2], res)
		}
		if !strings.HasPrefix(beside, "dir/a.sha256-"+h.String()[:12]) || !strings.HasSuffix(beside, ".txt") {
			t.Fatalf("beside path %q", beside)
		}
	})

	t.Run("should be unchanged when the beside path already holds these bytes", func(t *testing.T) {
		st := newFakeState("photos")
		st.put("photos", "a.txt", "ITEM1", hashOf("old"), false)
		h := hashOf("new")
		besidePlaced := st.put("photos", BesidePath("a.txt", h), "ITEM2", h, false)
		recs, res, err := Resolve(st, arrival("photos", "a.txt"), h, 3, "import", fixedID("ITEM3"), never)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 0 || res.Outcome != OutcomeUnchanged || res.Item != "ITEM2" || res.Path != BesidePath("a.txt", h) || res.EntryID != besidePlaced.EntryID {
			t.Fatalf("recs=%v res=%+v", recs, res)
		}
	})

	t.Run("should move the placement and keep the item when the old path has gone", func(t *testing.T) {
		st := newFakeState("photos")
		h := hashOf("a")
		st.put("photos", "old.txt", "ITEM1", h, false)
		gone := func(folder, path string) bool { return path == "old.txt" }
		recs, res, err := Resolve(st, arrival("photos", "new.txt"), h, 1, "sync", fixedID("NEW"), gone)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{journal.TypePlacementCleared, journal.TypePlacementSet}
		if !equalStrings(recordTypes(recs), want) || res.Outcome != OutcomeRenamed || res.Item != "ITEM1" {
			t.Fatalf("types=%v res=%+v", recordTypes(recs), res)
		}
		if recs[0].Path != "old.txt" || recs[1].Path != "new.txt" || recs[1].Item != "ITEM1" {
			t.Fatalf("records %+v", recs)
		}
	})

	t.Run("should add a second placement to the same item when the old path is still there", func(t *testing.T) {
		st := newFakeState("photos")
		h := hashOf("a")
		st.put("photos", "old.txt", "ITEM1", h, false)
		recs, res, err := Resolve(st, arrival("photos", "copy.txt"), h, 1, "import", fixedID("NEW"), never)
		if err != nil {
			t.Fatal(err)
		}
		if !equalStrings(recordTypes(recs), []string{journal.TypePlacementSet}) || res.Outcome != OutcomeCopied || res.Item != "ITEM1" {
			t.Fatalf("types=%v res=%+v", recordTypes(recs), res)
		}
	})

	t.Run("should create a new item when the bytes are new to the folder", func(t *testing.T) {
		st := newFakeState("photos")
		h := hashOf("fresh")
		recs, res, err := Resolve(st, arrival("photos", "a.txt"), h, 5, "import", fixedID("ITEM9"), never)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{journal.TypeBlobAdded, journal.TypeItemCreated, journal.TypePlacementSet}
		if !equalStrings(recordTypes(recs), want) || res.Outcome != OutcomeCreated || res.Item != "ITEM9" {
			t.Fatalf("types=%v res=%+v", recordTypes(recs), res)
		}
		if recs[0].Hash != h.String() || recs[0].Size != 5 || recs[0].By != "import" {
			t.Fatalf("blob_added %+v", recs[0])
		}
		if recs[2].Mtime == nil || !recs[2].Mtime.Equal(arrival("", "").Mtime) {
			t.Fatalf("placement_set mtime %v", recs[2].Mtime)
		}
	})

	t.Run("should not emit blob_added when the index already names the blob", func(t *testing.T) {
		st := newFakeState("photos")
		h := hashOf("known")
		st.blobs[h] = true
		recs, _, err := Resolve(st, arrival("photos", "a.txt"), h, 5, "import", fixedID("ITEM9"), never)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{journal.TypeItemCreated, journal.TypePlacementSet}
		if !equalStrings(recordTypes(recs), want) {
			t.Fatalf("types=%v", recordTypes(recs))
		}
	})

	t.Run("should create a separate item when the same bytes arrive in a different collection", func(t *testing.T) {
		st := newFakeState("photos", "books")
		h := hashOf("a")
		st.put("photos", "a.txt", "ITEM1", h, false)
		recs, res, err := Resolve(st, arrival("books", "a.txt"), h, 1, "import", fixedID("ITEM2"), never)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeCreated || res.Item != "ITEM2" {
			t.Fatalf("res=%+v", res)
		}
		want := []string{journal.TypeItemCreated, journal.TypePlacementSet}
		if !equalStrings(recordTypes(recs), want) {
			t.Fatalf("types = %v, want %v", recordTypes(recs), want)
		}
	})

	t.Run("should return ErrCollectionUnknown when the folder is not declared", func(t *testing.T) {
		st := newFakeState("photos")
		_, _, err := Resolve(st, arrival("nowhere", "a.txt"), hashOf("a"), 1, "import", fixedID("X"), never)
		if !errors.Is(err, ErrCollectionUnknown) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("should leave the mtime unset when the arrival has none", func(t *testing.T) {
		st := newFakeState("photos")
		a := Arrival{Folder: "photos", Path: "a.txt"}
		recs, _, err := Resolve(st, a, hashOf("z"), 1, "upload", fixedID("X"), never)
		if err != nil {
			t.Fatal(err)
		}
		if recs[len(recs)-1].Mtime != nil {
			t.Fatalf("mtime = %v", recs[len(recs)-1].Mtime)
		}
	})
}

func TestCleanPath(t *testing.T) {
	t.Run("should reject a path when a segment is ..", func(t *testing.T) {
		for _, p := range []string{"..", "a/../b", "a/.."} {
			if _, err := CleanPath(p); !errors.Is(err, ErrBadPath) {
				t.Errorf("%q: err = %v", p, err)
			}
		}
	})
	t.Run("should reject an absolute path", func(t *testing.T) {
		if _, err := CleanPath("/etc/passwd"); !errors.Is(err, ErrBadPath) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("should reject empty, dot, doubled and trailing slash forms", func(t *testing.T) {
		for _, p := range []string{"", ".", "a//b", "a/", "./a"} {
			if _, err := CleanPath(p); !errors.Is(err, ErrBadPath) {
				t.Errorf("%q: err = %v", p, err)
			}
		}
	})
	t.Run("should reject a path containing NUL", func(t *testing.T) {
		if _, err := CleanPath("a\x00b"); !errors.Is(err, ErrBadPath) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("should reject a path that is not valid UTF-8", func(t *testing.T) {
		if _, err := CleanPath("a\xffb"); !errors.Is(err, ErrBadPath) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("should reject a segment longer than 255 bytes", func(t *testing.T) {
		if _, err := CleanPath(strings.Repeat("a", 256)); !errors.Is(err, ErrBadPath) {
			t.Fatalf("err = %v", err)
		}
		if _, err := CleanPath(strings.Repeat("a", 255)); err != nil {
			t.Fatalf("255 bytes must pass: %v", err)
		}
	})
	t.Run("should reject a path longer than 4096 bytes", func(t *testing.T) {
		p := strings.Repeat(strings.Repeat("a", 100)+"/", 41) + "b"
		if _, err := CleanPath(p); !errors.Is(err, ErrBadPath) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("should keep a backslash as an ordinary character", func(t *testing.T) {
		got, err := CleanPath(`dir\sub/file\name.txt`)
		if err != nil || got != `dir\sub/file\name.txt` {
			t.Fatalf("got %q, err %v", got, err)
		}
	})
}

func TestBesidePath(t *testing.T) {
	h := hashOf("x")
	marker := ".sha256-" + h.String()[:12]

	t.Run("should insert the hash before the extension when the file has one", func(t *testing.T) {
		if got := BesidePath("d/e/photo.jpg", h); got != "d/e/photo"+marker+".jpg" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("should append the hash when the file has no extension", func(t *testing.T) {
		if got := BesidePath("d/README", h); got != "d/README"+marker {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("should treat a leading dot as part of the stem", func(t *testing.T) {
		if got := BesidePath(".bashrc", h); got != ".bashrc"+marker {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("should keep the name within 255 bytes when the stem is already at the limit", func(t *testing.T) {
		path := "d/" + strings.Repeat("é", 100) + strings.Repeat("a", 51) + ".jpg" // 255 bytes
		if len(path)-2 != 255 {
			t.Fatalf("fixture is %d bytes", len(path)-2)
		}
		got := BesidePath(path, h)
		name := got[strings.LastIndexByte(got, '/')+1:]
		if len(name) > 255 {
			t.Fatalf("name is %d bytes", len(name))
		}
		if !strings.HasSuffix(name, marker+".jpg") {
			t.Fatalf("name %q lost the marker or extension", name)
		}
		if _, err := CleanPath(got); err != nil {
			t.Fatalf("beside path is not a valid path: %v", err)
		}
		if again := BesidePath(path, h); again != got {
			t.Fatal("BesidePath is not deterministic")
		}
	})
}

func TestNewItemID(t *testing.T) {
	t.Run("should return 26 Crockford characters that sort by time", func(t *testing.T) {
		earlier := NewItemID(time.UnixMilli(1_700_000_000_000))
		later := NewItemID(time.UnixMilli(1_700_000_001_000))
		if len(earlier) != 26 || len(later) != 26 {
			t.Fatalf("lengths %d %d", len(earlier), len(later))
		}
		if earlier >= later {
			t.Fatalf("%s should sort before %s", earlier, later)
		}
		for _, c := range string(earlier) {
			if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", c) {
				t.Fatalf("character %q is not Crockford", c)
			}
		}
	})
	t.Run("should not repeat when called twice at the same instant", func(t *testing.T) {
		now := time.Now()
		if NewItemID(now) == NewItemID(now) {
			t.Fatal("two ids collided")
		}
	})
}

func TestResolvePathConflict(t *testing.T) {
	h := hashOf("x")
	arrival := func(folder, path string) Arrival { return Arrival{Folder: folder, Path: path} }
	t.Run("should reject with ErrPathConflict when the path is nested under a placed file", func(t *testing.T) {
		st := newFakeState("photos")
		st.put("photos", "a", "I1", hashOf("old"), true)
		recs, _, err := Resolve(st, arrival("photos", "a/b"), h, 1, "import", fixedID("NEW"), never)
		if !errors.Is(err, ErrPathConflict) || len(recs) != 0 {
			t.Fatalf("err = %v, recs = %v", err, recs)
		}
	})
	t.Run("should reject with ErrPathConflict when the path is a directory prefix of a placed file", func(t *testing.T) {
		st := newFakeState("photos")
		st.put("photos", "a/b", "I1", hashOf("old"), true)
		_, _, err := Resolve(st, arrival("photos", "a"), h, 1, "import", fixedID("NEW"), never)
		if !errors.Is(err, ErrPathConflict) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("should reject with ErrPathConflict when a copy arrives at a conflicting path", func(t *testing.T) {
		st := newFakeState("photos")
		st.put("photos", "a", "I1", h, true)
		_, _, err := Resolve(st, arrival("photos", "a/copy"), h, 1, "import", fixedID("NEW"), never)
		if !errors.Is(err, ErrPathConflict) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("should accept when the conflicting path is in another folder", func(t *testing.T) {
		st := newFakeState("photos", "books")
		st.put("books", "a", "I1", hashOf("old"), true)
		_, _, err := Resolve(st, arrival("photos", "a/b"), h, 1, "import", fixedID("NEW"), never)
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("should accept when only a sibling shares the name prefix", func(t *testing.T) {
		st := newFakeState("photos")
		st.put("photos", "ab", "I1", hashOf("old"), true)
		_, _, err := Resolve(st, arrival("photos", "a"), h, 1, "import", fixedID("NEW"), never)
		if err != nil {
			t.Fatal(err)
		}
	})
}
