package catalog

import (
	"errors"
	"path"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type searchFileFixture struct {
	folder      string
	relative    string
	description string
	tags        []string
	captured    string
	lat         *float64
	lon         *float64
}

func addSearchFile(t *testing.T, cat *Catalog, fixture searchFileFixture) int64 {
	t.Helper()
	if fixture.folder == "" {
		fixture.folder = "Photos"
	}
	if _, err := cat.UpsertFile(&FileRecord{
		FolderLabel:  fixture.folder,
		RelativePath: fixture.relative,
		Filename:     path.Base(fixture.relative),
		Extension:    path.Ext(fixture.relative),
		SizeBytes:    1,
		FileMtime:    time.Unix(1, 0).UTC(),
		Status:       StatusSynced,
	}, 1); err != nil {
		t.Fatal(err)
	}
	file, err := cat.GetFile(fixture.folder, fixture.relative)
	if err != nil {
		t.Fatal(err)
	}
	edits := FileEdits{}
	if fixture.description != "" {
		edits.Description = &fixture.description
	}
	if fixture.captured != "" {
		edits.CapturedLocal = &fixture.captured
	}
	if fixture.lat != nil && fixture.lon != nil {
		edits.GPS = &GPSCoordinates{Lat: *fixture.lat, Lon: *fixture.lon}
	}
	if edits.Description != nil || edits.CapturedLocal != nil || edits.GPS != nil {
		if _, err := cat.UpdateFile(file.ID, edits); err != nil {
			t.Fatal(err)
		}
	}
	if len(fixture.tags) > 0 {
		if _, err := cat.ApplyTags([]int64{file.ID}, fixture.tags, nil, 2); err != nil {
			t.Fatal(err)
		}
	}
	return file.ID
}

func searchFilenames(files []FileRecord) []string {
	result := make([]string, len(files))
	for i := range files {
		result[i] = files[i].Filename
	}
	sort.Strings(result)
	return result
}

func TestExtendedSearchMatchesFilenamePathDescriptionAndTags(t *testing.T) {
	cat := setupTestCatalog(t)
	addSearchFile(t, cat, searchFileFixture{relative: "filename-needle.jpg"})
	addSearchFile(t, cat, searchFileFixture{relative: "needle-directory/plain.jpg"})
	addSearchFile(t, cat, searchFileFixture{relative: "caption.jpg", description: "A needle in this caption"})
	addSearchFile(t, cat, searchFileFixture{relative: "tagged.jpg", tags: []string{"Needle/Leaf"}})
	addSearchFile(t, cat, searchFileFixture{relative: "unicode.jpg", tags: []string{"ÍSLAND"}})
	addSearchFile(t, cat, searchFileFixture{relative: "unrelated.jpg", description: "nothing here"})
	statusOnly := addSearchFile(t, cat, searchFileFixture{folder: "NeedleFolder", relative: "status-only.jpg"})
	if err := cat.UpdateFileStatus(statusOnly, StatusArchived); err != nil {
		t.Fatal(err)
	}

	files, err := cat.ListFiles(FileQuery{Search: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := searchFilenames(files), []string{"caption.jpg", "filename-needle.jpg", "plain.jpg", "tagged.jpg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("search matches = %v, want %v", got, want)
	}

	files, err = cat.ListFiles(FileQuery{Search: "ÍS"})
	if err != nil {
		t.Fatal(err)
	}
	if got := searchFilenames(files); !reflect.DeepEqual(got, []string{"unicode.jpg"}) {
		t.Fatalf("Unicode folded tag search = %v", got)
	}

	for _, search := range []string{"NeedleFolder", "archived"} {
		files, err = cat.ListFiles(FileQuery{Search: search})
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 0 {
			t.Fatalf("q=%q matched folder/status-only row: %v", search, searchFilenames(files))
		}
	}
}

func TestExtendedSearchTreatsSQLWildcardsAsLiteralSubstringCharacters(t *testing.T) {
	cat := setupTestCatalog(t)
	addSearchFile(t, cat, searchFileFixture{relative: "literal.txt", description: `before 100%_ready after`})
	addSearchFile(t, cat, searchFileFixture{relative: "wildcard.txt", description: `before 100XYready after`})

	files, err := cat.ListFiles(FileQuery{Search: `100%_ready`})
	if err != nil {
		t.Fatal(err)
	}
	if got := searchFilenames(files); !reflect.DeepEqual(got, []string{"literal.txt"}) {
		t.Fatalf("literal substring matches = %v, want [literal.txt]", got)
	}
}

func TestTagFiltersNormalizeIncludeDescendantsAndComposeWithAND(t *testing.T) {
	cat := setupTestCatalog(t)
	addSearchFile(t, cat, searchFileFixture{relative: "parent.jpg", tags: []string{"Trips/Iceland"}})
	addSearchFile(t, cat, searchFileFixture{relative: "leaf.jpg", tags: []string{"Trips/Iceland/Reykjavik"}})
	addSearchFile(t, cat, searchFileFixture{relative: "both.jpg", tags: []string{"Trips/Iceland/Akureyri", "People/Alice"}})
	addSearchFile(t, cat, searchFileFixture{relative: "sibling.jpg", tags: []string{"Trips/Icelandic"}})
	addSearchFile(t, cat, searchFileFixture{relative: "people.jpg", tags: []string{"People/Alice"}})

	files, err := cat.ListFiles(FileQuery{Tags: []string{"  TRIPS / ICELAND  "}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := searchFilenames(files), []string{"both.jpg", "leaf.jpg", "parent.jpg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("subtree matches = %v, want %v", got, want)
	}

	files, err = cat.ListFiles(FileQuery{Tags: []string{"trips/iceland", " PEOPLE / ALICE "}})
	if err != nil {
		t.Fatal(err)
	}
	if got := searchFilenames(files); !reflect.DeepEqual(got, []string{"both.jpg"}) {
		t.Fatalf("AND matches = %v, want [both.jpg]", got)
	}
}

func TestTagFilterDoesNotInterpretWildcardCharacters(t *testing.T) {
	cat := setupTestCatalog(t)
	addSearchFile(t, cat, searchFileFixture{relative: "literal.jpg", tags: []string{`Special/100%_real`}})
	addSearchFile(t, cat, searchFileFixture{relative: "other.jpg", tags: []string{`Special/100XYreal`}})
	addSearchFile(t, cat, searchFileFixture{relative: "child.jpg", tags: []string{`Special/100%_real/Child`}})

	files, err := cat.ListFiles(FileQuery{Tags: []string{`special/100%_real`}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := searchFilenames(files), []string{"child.jpg", "literal.jpg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("escaped subtree matches = %v, want %v", got, want)
	}
}

func TestUntaggedAndDescribedFiltersAreTriStateAndComposable(t *testing.T) {
	cat := setupTestCatalog(t)
	addSearchFile(t, cat, searchFileFixture{relative: "neither.jpg"})
	addSearchFile(t, cat, searchFileFixture{relative: "tagged.jpg", tags: []string{"Keep"}})
	addSearchFile(t, cat, searchFileFixture{relative: "described.jpg", description: "caption"})
	addSearchFile(t, cat, searchFileFixture{relative: "both.jpg", description: "caption", tags: []string{"Keep"}})
	truth := true
	falsehood := false

	tests := []struct {
		name string
		q    FileQuery
		want []string
	}{
		{name: "untagged", q: FileQuery{Untagged: &truth}, want: []string{"described.jpg", "neither.jpg"}},
		{name: "tagged", q: FileQuery{Untagged: &falsehood}, want: []string{"both.jpg", "tagged.jpg"}},
		{name: "described", q: FileQuery{Described: &truth}, want: []string{"both.jpg", "described.jpg"}},
		{name: "not described", q: FileQuery{Described: &falsehood}, want: []string{"neither.jpg", "tagged.jpg"}},
		{name: "tagged and described", q: FileQuery{Untagged: &falsehood, Described: &truth}, want: []string{"both.jpg"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, err := cat.ListFiles(tt.q)
			if err != nil {
				t.Fatal(err)
			}
			if got := searchFilenames(files); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("matches = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSearchFiltersHaveRowsAndIDsParityAndPreserveExistingFilters(t *testing.T) {
	cat := setupTestCatalog(t)
	nearLat, nearLon := 0.1, 0.1
	wantID := addSearchFile(t, cat, searchFileFixture{
		folder: "Photos", relative: "inside.jpg", description: "travel diary",
		tags: []string{"Trips/Iceland", "People/Alice"}, captured: "2024-02-29 12:00:00",
		lat: &nearLat, lon: &nearLon,
	})
	farLat, farLon := 20.0, 20.0
	addSearchFile(t, cat, searchFileFixture{
		folder: "Photos", relative: "far.jpg", description: "travel diary",
		tags: []string{"Trips/Iceland", "People/Alice"}, captured: "2024-02-29 12:00:00",
		lat: &farLat, lon: &farLon,
	})
	addSearchFile(t, cat, searchFileFixture{
		folder: "Books", relative: "wrong-folder.jpg", description: "travel diary",
		tags: []string{"Trips/Iceland", "People/Alice"}, captured: "2024-02-29 12:00:00",
		lat: &nearLat, lon: &nearLon,
	})
	described := true
	q := FileQuery{
		Folder: "Photos", Search: "diary", Tags: []string{"Trips", "People"}, Described: &described,
		CapturedFrom: "2024-02-29", CapturedTo: "2024-02-29",
		Near: &NearFilter{Lat: 0, Lon: 0, RadiusKM: 100},
	}
	files, err := cat.ListFiles(q)
	if err != nil {
		t.Fatal(err)
	}
	ids, total, err := cat.ListFileIDs(q, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != wantID || !reflect.DeepEqual(ids, []int64{wantID}) || total != 1 {
		t.Fatalf("rows = %#v, ids = %v, total = %d, want id %d", searchFilenames(files), ids, total, wantID)
	}
}

func TestSearchFilterValidationAndIDsOverride(t *testing.T) {
	cat := setupTestCatalog(t)
	id := addSearchFile(t, cat, searchFileFixture{relative: "selected.jpg"})

	for _, tags := range [][]string{{"Trips//Iceland"}, {strings.Repeat("a", 65)}} {
		if _, err := cat.ListFiles(FileQuery{Tags: tags}); err == nil {
			t.Fatalf("ListFiles tags %q succeeded, want validation error", tags)
		}
		if _, _, err := cat.ListFileIDs(FileQuery{Tags: tags}, 100); err == nil {
			t.Fatalf("ListFileIDs tags %q succeeded, want validation error", tags)
		}
	}
	overCap := make([]string, MaxTagFilters+1)
	for i := range overCap {
		overCap[i] = string(rune('a' + i))
	}
	if _, err := cat.ListFiles(FileQuery{Tags: overCap}); !errors.Is(err, ErrTooManyTagFilters) {
		t.Fatalf("over-cap error = %v, want ErrTooManyTagFilters", err)
	}

	// IDs are the established reconciliation projection. They ignore all
	// filters, including invalid tag filters, rather than losing selected rows.
	files, err := cat.ListFiles(FileQuery{IDs: []int64{id}, Search: "no match", Tags: []string{"bad//tag"}})
	if err != nil {
		t.Fatal(err)
	}
	ids, total, err := cat.ListFileIDs(FileQuery{IDs: []int64{id}, Tags: overCap}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != id || !reflect.DeepEqual(ids, []int64{id}) || total != 1 {
		t.Fatalf("IDs override rows=%v ids=%v total=%d", searchFilenames(files), ids, total)
	}
}
