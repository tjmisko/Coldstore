package index

import (
	"fmt"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/journal"
	"github.com/tjmisko/coldstore/internal/store"
)

func names(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
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

// gallery holds four items in Photos (one trashed) and one in Docs.
func gallery(t *testing.T) *Index {
	t.Helper()
	ix := openTemp(t)
	s := &script{}
	s.folder("Photos", journal.FolderKindCollection).folder("Docs", journal.FolderKindCollection)
	for i, name := range []string{"one", "two", "three", "four", "five"} {
		s.blob(hashOf(name), int64(100*(i+1)))
		s.item("I-"+name, hashOf(name))
	}
	day := func(n int) time.Time { return baseTime.AddDate(0, 0, n) }
	s.placeAt("Photos", "2020/Beach.JPG", "I-one", day(1))
	s.placeAt("Photos", "2020/hill_100%.png", "I-two", day(3))
	s.placeAt("Photos", "2021/city.jpg", "I-three", day(2))
	s.placeAt("Photos", "2021/old.jpg", "I-four", day(4))
	s.placeAt("Docs", "notes.txt", "I-five", day(5))
	s.tag("I-one", []string{"Trip/Paris"}, nil).
		tag("I-two", []string{"Trip"}, nil).
		tag("I-three", []string{"Tripod"}, nil).
		tag("I-five", []string{"Trip/Paris"}, nil).
		trash("I-four")
	mustApplyAll(t, ix, s)
	return ix
}

func TestEntries(t *testing.T) {
	yes, no := true, false

	t.Run("should return only live entries when Trashed is nil", func(t *testing.T) {
		got, err := gallery(t).Entries(EntryQuery{Folder: "Photos"})
		must(t, err)
		if len(got) != 3 {
			t.Fatalf("got %v", names(got))
		}
		for _, e := range got {
			if e.TrashedAt != nil {
				t.Fatalf("trashed entry %s returned", e.Path)
			}
		}
	})

	t.Run("should return only trashed entries when Trashed is true", func(t *testing.T) {
		got, err := gallery(t).Entries(EntryQuery{Folder: "Photos", Trashed: &yes})
		must(t, err)
		if !equalStrings(names(got), []string{"2021/old.jpg"}) || got[0].TrashedAt == nil {
			t.Fatalf("got %v", names(got))
		}
	})

	t.Run("should return live entries when Trashed is false", func(t *testing.T) {
		got, _ := gallery(t).Entries(EntryQuery{Folder: "Photos", Trashed: &no})
		if len(got) != 3 {
			t.Fatalf("got %v", names(got))
		}
	})

	t.Run("should return an error when neither a folder nor ids are given", func(t *testing.T) {
		if _, err := gallery(t).Entries(EntryQuery{}); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("should sort newest mtime first by default", func(t *testing.T) {
		got, _ := gallery(t).Entries(EntryQuery{Folder: "Photos"})
		want := []string{"2020/hill_100%.png", "2021/city.jpg", "2020/Beach.JPG"}
		if !equalStrings(names(got), want) {
			t.Fatalf("got %v, want %v", names(got), want)
		}
	})

	t.Run("should sort largest first when SortBy is size", func(t *testing.T) {
		got, _ := gallery(t).Entries(EntryQuery{Folder: "Photos", SortBy: "size"})
		want := []string{"2021/city.jpg", "2020/hill_100%.png", "2020/Beach.JPG"}
		if !equalStrings(names(got), want) {
			t.Fatalf("got %v, want %v", names(got), want)
		}
	})

	t.Run("should sort by path ignoring case when SortBy is name", func(t *testing.T) {
		got, _ := gallery(t).Entries(EntryQuery{Folder: "Photos", SortBy: "name"})
		want := []string{"2020/Beach.JPG", "2020/hill_100%.png", "2021/city.jpg"}
		if !equalStrings(names(got), want) {
			t.Fatalf("got %v, want %v", names(got), want)
		}
	})

	t.Run("should return an error when SortBy is unknown", func(t *testing.T) {
		if _, err := gallery(t).Entries(EntryQuery{Folder: "Photos", SortBy: "captured"}); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("should cap the page at Limit", func(t *testing.T) {
		got, _ := gallery(t).Entries(EntryQuery{Folder: "Photos", Limit: 2})
		if len(got) != 2 {
			t.Fatalf("got %d entries", len(got))
		}
	})

	t.Run("should skip Offset entries when paging", func(t *testing.T) {
		got, _ := gallery(t).Entries(EntryQuery{Folder: "Photos", Limit: 2, Offset: 2})
		if !equalStrings(names(got), []string{"2020/Beach.JPG"}) {
			t.Fatalf("got %v", names(got))
		}
	})

	t.Run("should cap the page at MaxPageLimit when Limit is larger", func(t *testing.T) {
		ix := openTemp(t)
		s := &script{}
		s.folder("P", "collection").blob(hashOf("x"), 1).item("I", hashOf("x"))
		for i := 0; i < MaxPageLimit+20; i++ {
			s.place("P", fmt.Sprintf("f%04d", i), "I")
		}
		mustApplyAll(t, ix, s)
		got, err := ix.Entries(EntryQuery{Folder: "P", Limit: 100000})
		must(t, err)
		if len(got) != MaxPageLimit {
			t.Fatalf("got %d entries, want %d", len(got), MaxPageLimit)
		}
	})

	t.Run("should match the extension case-insensitively with or without the dot", func(t *testing.T) {
		ix := gallery(t)
		for _, ext := range []string{"jpg", ".JPG"} {
			got, _ := ix.Entries(EntryQuery{Folder: "Photos", Extension: ext})
			if !equalStrings(names(got), []string{"2021/city.jpg", "2020/Beach.JPG"}) {
				t.Fatalf("Extension %q: got %v", ext, names(got))
			}
		}
	})

	t.Run("should search path case-insensitively when Search is set", func(t *testing.T) {
		got, _ := gallery(t).Entries(EntryQuery{Folder: "Photos", Search: "BEACH"})
		if !equalStrings(names(got), []string{"2020/Beach.JPG"}) {
			t.Fatalf("got %v", names(got))
		}
	})

	t.Run("should treat percent and underscore literally when searching", func(t *testing.T) {
		ix := gallery(t)
		for search, want := range map[string][]string{
			"100%": {"2020/hill_100%.png"},
			"l_1":  {"2020/hill_100%.png"},
			"%":    {"2020/hill_100%.png"},
			"_":    {"2020/hill_100%.png"},
			"c_ty": nil,
		} {
			got, _ := ix.Entries(EntryQuery{Folder: "Photos", Search: search})
			if !equalStrings(names(got), want) {
				t.Fatalf("Search %q: got %v, want %v", search, names(got), want)
			}
		}
	})

	t.Run("should match descendants when a tag filter names a parent", func(t *testing.T) {
		got, _ := gallery(t).Entries(EntryQuery{Folder: "Photos", Tags: []string{"trip"}})
		want := []string{"2020/hill_100%.png", "2020/Beach.JPG"}
		if !equalStrings(names(got), want) {
			t.Fatalf("got %v, want %v (Tripod must not match Trip)", names(got), want)
		}
	})

	t.Run("should match only the child when a tag filter names a child", func(t *testing.T) {
		got, _ := gallery(t).Entries(EntryQuery{Folder: "Photos", Tags: []string{"Trip/Paris"}})
		if !equalStrings(names(got), []string{"2020/Beach.JPG"}) {
			t.Fatalf("got %v", names(got))
		}
	})

	t.Run("should require every tag when several tag filters are given", func(t *testing.T) {
		got, _ := gallery(t).Entries(EntryQuery{Folder: "Photos", Tags: []string{"Trip", "Tripod"}})
		if len(got) != 0 {
			t.Fatalf("got %v", names(got))
		}
	})

	t.Run("should return an error when a tag filter is invalid", func(t *testing.T) {
		if _, err := gallery(t).Entries(EntryQuery{Folder: "Photos", Tags: []string{"/bad"}}); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("should select untagged entries when Untagged is true", func(t *testing.T) {
		ix := gallery(t)
		got, _ := ix.Entries(EntryQuery{Folder: "Photos", Untagged: &yes})
		if len(got) != 0 {
			t.Fatalf("got %v", names(got))
		}
		got, _ = ix.Entries(EntryQuery{Folder: "Photos", Untagged: &no})
		if len(got) != 3 {
			t.Fatalf("got %v", names(got))
		}
	})

	t.Run("should ignore every other filter when IDs is set", func(t *testing.T) {
		ix := gallery(t)
		all, _ := ix.Entries(EntryQuery{Folder: "Docs"})
		trashed, _ := ix.Entries(EntryQuery{Folder: "Photos", Trashed: &yes})
		ids := []int64{all[0].EntryID, trashed[0].EntryID}
		got, err := ix.Entries(EntryQuery{IDs: ids, Folder: "Photos", Search: "zzz", Extension: "nope", Tags: []string{"none"}})
		must(t, err)
		if len(got) != 2 {
			t.Fatalf("got %v", names(got))
		}
	})

	t.Run("should return the item's tags in sorted order on each entry", func(t *testing.T) {
		got, _ := gallery(t).Entries(EntryQuery{Folder: "Photos", Search: "beach"})
		if len(got) != 1 || !equalStrings(got[0].Tags, []string{"Trip/Paris"}) {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("should join size hash and operational state onto the entry", func(t *testing.T) {
		ix := gallery(t)
		must(t, ix.BlobDerivedSet(hashOf("one"), "image/jpeg", "failed", "boom"))
		must(t, ix.BlobBadSet(hashOf("one"), true, "missing"))
		got, _ := ix.Entries(EntryQuery{Folder: "Photos", Search: "beach"})
		e := got[0]
		if e.Size != 100 || e.Hash != hashOf("one") || e.DetectedType != "image/jpeg" ||
			e.ThumbState != "failed" || e.ThumbError != "boom" || !e.BlobBad {
			t.Fatalf("entry = %+v", e)
		}
		if !e.FirstSeen.Equal(baseTime.Add(time.Duration(e.EntryID) * time.Second)) {
			t.Fatalf("FirstSeen = %v", e.FirstSeen)
		}
	})
}

func TestEntryIDs(t *testing.T) {
	t.Run("should return ids in query order and the full total when limited", func(t *testing.T) {
		ix := gallery(t)
		ids, total, err := ix.EntryIDs(EntryQuery{Folder: "Photos"}, 2)
		must(t, err)
		if total != 3 || len(ids) != 2 {
			t.Fatalf("ids = %v, total = %d", ids, total)
		}
		page, _ := ix.Entries(EntryQuery{Folder: "Photos", Limit: 2})
		if page[0].EntryID != ids[0] || page[1].EntryID != ids[1] {
			t.Fatalf("ids %v do not match entries %v", ids, names(page))
		}
	})
}

func TestQueries(t *testing.T) {
	t.Run("should list placements of one hash in one folder when PlacementsByCurrentHash is called", func(t *testing.T) {
		ix := openTemp(t)
		a := hashOf("a")
		s := (&script{}).folder("P", "collection").folder("Q", "collection").blob(a, 1).item("I", a).
			place("P", "x", "I").place("P", "y", "I").place("Q", "z", "I")
		mustApplyAll(t, ix, s)
		got, err := ix.PlacementsByCurrentHash("P", a)
		must(t, err)
		if len(got) != 2 || got[0].Path != "x" || got[1].Path != "y" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("should count live and trashed entries when FolderStats is called", func(t *testing.T) {
		stats, err := gallery(t).FolderStats("Photos")
		must(t, err)
		if stats.Live != 3 || stats.Trashed != 1 || stats.StoredBytes != 100+200+300+400 {
			t.Fatalf("stats = %+v", stats)
		}
	})

	t.Run("should count blobs and bad blobs when ArchiveStats is called", func(t *testing.T) {
		ix := gallery(t)
		must(t, ix.BlobBadSet(hashOf("one"), true, "missing"))
		stats, err := ix.ArchiveStats()
		must(t, err)
		if stats.BlobCount != 5 || stats.BlobBytes != 1500 || stats.BadBlobCount != 1 {
			t.Fatalf("stats = %+v", stats)
		}
	})

	t.Run("should count exact and descendant entries when Tags is called", func(t *testing.T) {
		got, err := gallery(t).Tags("Photos", "", 100)
		must(t, err)
		want := []TagCount{
			{Name: "Trip", Count: 1, DescendantCount: 2},
			{Name: "Trip/Paris", Count: 1, DescendantCount: 1},
			{Name: "Tripod", Count: 1, DescendantCount: 1},
		}
		if len(got) != len(want) {
			t.Fatalf("got %+v", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("tag %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("should filter by folded prefix when Tags is given a prefix", func(t *testing.T) {
		got, _ := gallery(t).Tags("Photos", "TRIP/", 100)
		if len(got) != 1 || got[0].Name != "Trip/Paris" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("should list every blob in hash order when BlobHashes is called", func(t *testing.T) {
		count := 0
		prev := ""
		err := gallery(t).BlobHashes(func(h store.Hash, size int64) error {
			s := fmt.Sprintf("%x", h)
			if s <= prev {
				t.Fatalf("out of order: %s after %s", s, prev)
			}
			prev = s
			count++
			return nil
		})
		must(t, err)
		if count != 5 {
			t.Fatalf("count = %d", count)
		}
	})

	t.Run("should report live links only when an item is trashed", func(t *testing.T) {
		ix := gallery(t)
		var got []string
		must(t, ix.LiveLinks(func(folder, path string, _ store.Hash) error {
			got = append(got, folder+"/"+path)
			return nil
		}))
		if len(got) != 4 {
			t.Fatalf("got %v", got)
		}
		if _, ok, _ := ix.LiveLinkAt("Photos", "2021/old.jpg"); ok {
			t.Fatal("trashed placement reported as a live link")
		}
	})
}

func TestOperational(t *testing.T) {
	t.Run("should offer a blob again until three attempts when derive records no final state", func(t *testing.T) {
		ix := gallery(t)
		h := hashOf("one")
		for attempt := 0; attempt < MaxThumbAttempts; attempt++ {
			todo, _ := ix.BlobsToDerive(100)
			if !containsHash(todo, h) {
				t.Fatalf("attempt %d: blob not offered", attempt)
			}
			must(t, ix.BlobDerivedSet(h, "", "", "transient"))
		}
		todo, _ := ix.BlobsToDerive(100)
		if containsHash(todo, h) {
			t.Fatal("blob offered after three attempts")
		}
	})

	t.Run("should stop offering a blob when it has a final thumb state", func(t *testing.T) {
		ix := gallery(t)
		must(t, ix.BlobDerivedSet(hashOf("one"), "image/jpeg", "done", ""))
		todo, _ := ix.BlobsToDerive(100)
		if containsHash(todo, hashOf("one")) {
			t.Fatal("derived blob offered again")
		}
	})

	t.Run("should list never-verified blobs before the oldest verified when BlobsByVerifiedAge is called", func(t *testing.T) {
		ix := gallery(t)
		must(t, ix.BlobVerifiedSet(hashOf("one"), baseTime.Add(2*time.Hour)))
		must(t, ix.BlobVerifiedSet(hashOf("two"), baseTime.Add(time.Hour)))
		got, _ := ix.BlobsByVerifiedAge(100)
		n := len(got)
		if n != 5 || got[3] != hashOf("two") || got[4] != hashOf("one") {
			t.Fatalf("order = %x", got)
		}
	})

	t.Run("should list events newest first and filter by folder and type", func(t *testing.T) {
		ix := openTemp(t)
		must(t, ix.EventRecord(baseTime, "ingest_failed", "P", "a", "one"))
		must(t, ix.EventRecord(baseTime, "blob_bad", "", "", "two"))
		must(t, ix.EventRecord(baseTime, "ingest_failed", "Q", "b", "three"))
		all, _ := ix.Events("", "", 10)
		if len(all) != 3 || all[0].Details != "three" {
			t.Fatalf("all = %+v", all)
		}
		onlyP, _ := ix.Events("P", "", 10)
		if len(onlyP) != 1 || onlyP[0].Details != "one" {
			t.Fatalf("onlyP = %+v", onlyP)
		}
		bad, _ := ix.Events("", "blob_bad", 10)
		if len(bad) != 1 || bad[0].Details != "two" {
			t.Fatalf("bad = %+v", bad)
		}
	})

	t.Run("should clear the reason when a blob is marked good again", func(t *testing.T) {
		ix := gallery(t)
		h := hashOf("one")
		must(t, ix.BlobBadSet(h, true, "missing"))
		must(t, ix.BlobBadSet(h, false, "ignored"))
		info, _, _ := ix.Blob(h)
		if info.Bad || info.BadReason != "" {
			t.Fatalf("info = %+v", info)
		}
	})
}

func containsHash(list []store.Hash, h store.Hash) bool {
	for _, x := range list {
		if x == h {
			return true
		}
	}
	return false
}
