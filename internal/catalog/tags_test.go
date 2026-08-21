package catalog

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func addTagTestFile(t *testing.T, cat *Catalog, folder, name string) int64 {
	t.Helper()
	record := &FileRecord{
		FolderLabel: folder, RelativePath: name, Filename: name,
		Extension: ".jpg", SizeBytes: 1, FileMtime: time.Unix(1, 0).UTC(), Status: StatusSynced,
	}
	if _, err := cat.UpsertFile(record, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	got, err := cat.GetFile(folder, name)
	if err != nil {
		t.Fatal(err)
	}
	return got.ID
}

func tagNames(tags []Tag) []string {
	result := make([]string, len(tags))
	for i := range tags {
		result[i] = tags[i].Name
	}
	return result
}

func TestNormalizeTagName(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want normalizedTag
	}{
		{name: "plain", raw: "People", want: normalizedTag{name: "People", fold: "people"}},
		{name: "segments and whitespace", raw: "  Trips  / New\u00a0\u2003York ", want: normalizedTag{name: "Trips/New York", fold: "trips/new york"}},
		{name: "unicode lower", raw: "ÍSLAND", want: normalizedTag{name: "ÍSLAND", fold: "ísland"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeTagName(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("normalizeTagName(%q) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestNormalizeTagNameRejectsInvalidNamesAndLimits(t *testing.T) {
	tooLongPath := strings.Join([]string{
		strings.Repeat("a", 64), strings.Repeat("b", 64),
		strings.Repeat("c", 64), strings.Repeat("d", 64),
	}, "/")
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "whitespace", raw: "   "},
		{name: "leading slash", raw: "/Trips"},
		{name: "trailing slash", raw: "Trips/"},
		{name: "empty segment", raw: "Trips//Iceland"},
		{name: "whitespace segment", raw: "Trips/  /Iceland"},
		{name: "newline control", raw: "Trips/Ice\nland"},
		{name: "nul control", raw: "Trips/\x00Iceland"},
		{name: "segment over rune limit", raw: strings.Repeat("界", 65)},
		{name: "path over rune limit", raw: tooLongPath},
		{name: "depth over limit", raw: "a/b/c/d/e/f/g"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := normalizeTagName(tt.raw)
			var invalid *InvalidTagNameError
			if !errors.As(err, &invalid) {
				t.Fatalf("normalizeTagName(%q) error = %v, want InvalidTagNameError", tt.raw, err)
			}
			if invalid.Name != tt.raw {
				t.Fatalf("error names %q, want %q", invalid.Name, tt.raw)
			}
		})
	}
}

func TestResolveOrCreateTagsMaterializesAncestorsAndReusesFoldedNames(t *testing.T) {
	cat := setupTestCatalog(t)
	tags, created, err := cat.ResolveOrCreateTags([]string{" Trips / Iceland ", "trips/iceland/Reykjavik", "TRIPS/ICELAND"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tagNames(tags), []string{"Trips/Iceland", "Trips/Iceland/Reykjavik"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("leaves = %v, want %v", got, want)
	}
	if want := []string{"Trips", "Trips/Iceland", "Trips/Iceland/Reykjavik"}; !reflect.DeepEqual(created, want) {
		t.Fatalf("created = %v, want %v", created, want)
	}

	_, created, err = cat.ResolveOrCreateTags([]string{"trips/iceland/reykjavik"}, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Fatalf("case-only duplicate created %v", created)
	}
	var count int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("tag row count = %d, want 3", count)
	}
}

func TestApplyTagsHydratesFilesAndLeavesAncestorsUnapplied(t *testing.T) {
	cat := setupTestCatalog(t)
	id1 := addTagTestFile(t, cat, "Photos", "one.jpg")
	id2 := addTagTestFile(t, cat, "Photos", "two.jpg")

	result, err := cat.ApplyTags([]int64{id1, id1, id2, -1, 999999},
		[]string{" Trips / Iceland ", "Favorite"}, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 2 {
		t.Fatalf("updated = %d, want 2", result.Updated)
	}
	if want := []string{"Trips", "Trips/Iceland", "Favorite"}; !reflect.DeepEqual(result.Created, want) {
		t.Fatalf("created = %v, want %v", result.Created, want)
	}
	file, err := cat.GetFileByID(id1)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Favorite", "Trips/Iceland"}; !reflect.DeepEqual(file.Tags, want) {
		t.Fatalf("tags = %v, want %v", file.Tags, want)
	}
	var ancestorLinks int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM file_tags ft JOIN tags t ON t.id = ft.tag_id WHERE t.name = 'Trips'`).Scan(&ancestorLinks); err != nil {
		t.Fatal(err)
	}
	if ancestorLinks != 0 {
		t.Fatalf("ancestor links = %d, want 0", ancestorLinks)
	}

	result, err = cat.ApplyTags([]int64{id1, id2}, []string{"favorite"}, []string{"TRIPS/ICELAND"}, 200)
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 2 || len(result.Created) != 0 {
		t.Fatalf("second mutation = %#v", result)
	}
	files, err := cat.ListFiles(FileQuery{IDs: []int64{id1, id2}})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if !reflect.DeepEqual(file.Tags, []string{"Favorite"}) {
			t.Fatalf("file %d tags = %v", file.ID, file.Tags)
		}
	}

	untagged := addTagTestFile(t, cat, "Photos", "untagged.jpg")
	plain, err := cat.GetFileByID(untagged)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Tags == nil || len(plain.Tags) != 0 {
		t.Fatalf("untagged tags = %#v, want non-nil empty slice", plain.Tags)
	}
}

func TestApplyTagsChunksLargeSelections(t *testing.T) {
	cat := setupTestCatalog(t)
	ids := make([]int64, 0, 501)
	for i := 0; i < 501; i++ {
		ids = append(ids, addTagTestFile(t, cat, "Photos", fmt.Sprintf("%03d.jpg", i)))
	}
	result, err := cat.ApplyTags(ids, []string{"Batch"}, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 501 {
		t.Fatalf("result = %#v, want 501 updates", result)
	}
	var count int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM file_tags`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 501 {
		t.Fatalf("file_tags count = %d, want 501", count)
	}
}

func TestApplyTagsRejectsOverlappingAddAndRemove(t *testing.T) {
	cat := setupTestCatalog(t)
	id := addTagTestFile(t, cat, "Photos", "one.jpg")
	_, err := cat.ApplyTags([]int64{id}, []string{" Iceland "}, []string{"ICELAND"}, 100)
	var invalid *InvalidTagNameError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want InvalidTagNameError", err)
	}
	var count int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("tag rows = %d after rejected request", count)
	}
}

func TestListTagsCountsSubtreesPrefixesAndFolders(t *testing.T) {
	cat := setupTestCatalog(t)
	p1 := addTagTestFile(t, cat, "Photos", "one.jpg")
	p2 := addTagTestFile(t, cat, "Photos", "two.jpg")
	b1 := addTagTestFile(t, cat, "Books", "book.jpg")
	if _, err := cat.ApplyTags([]int64{p1}, []string{"Trips/Iceland"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ApplyTags([]int64{p2}, []string{"Trips/Iceland/Reykjavik", "ÍSLAND"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ApplyTags([]int64{b1}, []string{"Trips/Iceland"}, nil, 1); err != nil {
		t.Fatal(err)
	}

	tags, err := cat.ListTags("", "trips", 50)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tagNames(tags), []string{"Trips/Iceland", "Trips/Iceland/Reykjavik", "Trips"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered names = %v, want %v", got, want)
	}
	counts := make(map[string][2]int64)
	for _, tag := range tags {
		counts[tag.Name] = [2]int64{tag.Count, tag.DescendantCount}
	}
	if counts["Trips"] != [2]int64{0, 3} || counts["Trips/Iceland"] != [2]int64{2, 3} ||
		counts["Trips/Iceland/Reykjavik"] != [2]int64{1, 1} {
		t.Fatalf("counts = %v", counts)
	}

	tags, err = cat.ListTags("Photos", "TRIPS", 50)
	if err != nil {
		t.Fatal(err)
	}
	counts = make(map[string][2]int64)
	for _, tag := range tags {
		counts[tag.Name] = [2]int64{tag.Count, tag.DescendantCount}
	}
	if counts["Trips"] != [2]int64{0, 2} || counts["Trips/Iceland"] != [2]int64{1, 2} {
		t.Fatalf("photo counts = %v", counts)
	}

	tags, err = cat.ListTags("Photos", "ís", 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := tagNames(tags); !reflect.DeepEqual(got, []string{"ÍSLAND"}) {
		t.Fatalf("unicode prefix = %v", got)
	}
	tags, err = cat.ListTags("Books", "ís", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 0 {
		t.Fatalf("folder-scoped unused tags = %v, want none", tagNames(tags))
	}
}

func TestRenameTagRenamesSubtreeAndCaseOnlyDisplay(t *testing.T) {
	cat := setupTestCatalog(t)
	id := addTagTestFile(t, cat, "Photos", "one.jpg")
	if _, err := cat.ApplyTags([]int64{id}, []string{"Trips/Icland/Reykjavik"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	result, err := cat.RenameTag(" trips / icland ", "Trips/Iceland", 2)
	if err != nil {
		t.Fatal(err)
	}
	if result.Merged || result.FilesAffected != 1 {
		t.Fatalf("rename result = %#v", result)
	}
	file, err := cat.GetFileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(file.Tags, []string{"Trips/Iceland/Reykjavik"}) {
		t.Fatalf("renamed tags = %v", file.Tags)
	}
	if _, err := cat.RenameTag("Trips/Iceland", "Trips/ICELAND", 3); err != nil {
		t.Fatal(err)
	}
	file, err = cat.GetFileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(file.Tags, []string{"Trips/ICELAND/Reykjavik"}) {
		t.Fatalf("case-renamed tags = %v", file.Tags)
	}
}

func TestRenameTagMergesSubtreeWithoutLosingAssociations(t *testing.T) {
	cat := setupTestCatalog(t)
	id1 := addTagTestFile(t, cat, "Photos", "one.jpg")
	id2 := addTagTestFile(t, cat, "Photos", "two.jpg")
	id3 := addTagTestFile(t, cat, "Photos", "three.jpg")
	if _, err := cat.ApplyTags([]int64{id1, id3}, []string{"Trips/Icland/Reykjavik"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ApplyTags([]int64{id2, id3}, []string{"Trips/Iceland/Reykjavik"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	result, err := cat.RenameTag("Trips/Icland", "Trips/Iceland", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Merged || result.FilesAffected != 2 {
		t.Fatalf("merge result = %#v", result)
	}
	for _, id := range []int64{id1, id2, id3} {
		file, err := cat.GetFileByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(file.Tags, []string{"Trips/Iceland/Reykjavik"}) {
			t.Fatalf("file %d tags = %v", id, file.Tags)
		}
	}
	var oldRows, links int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM tags WHERE name_fold LIKE 'trips/icland%'`).Scan(&oldRows); err != nil {
		t.Fatal(err)
	}
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM file_tags`).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if oldRows != 0 || links != 3 {
		t.Fatalf("old rows = %d, links = %d; want 0, 3", oldRows, links)
	}
}

func TestRenameTagRejectsMoveIntoOwnSubtree(t *testing.T) {
	cat := setupTestCatalog(t)
	if _, _, err := cat.ResolveOrCreateTags([]string{"Trips/Iceland"}, 1); err != nil {
		t.Fatal(err)
	}
	_, err := cat.RenameTag("Trips", "Trips/Iceland/New", 2)
	var invalid *InvalidTagNameError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want InvalidTagNameError", err)
	}
}

func TestDeleteTagRequiresCascadeAndPreservesFiles(t *testing.T) {
	cat := setupTestCatalog(t)
	id1 := addTagTestFile(t, cat, "Photos", "one.jpg")
	id2 := addTagTestFile(t, cat, "Photos", "two.jpg")
	if _, err := cat.ApplyTags([]int64{id1}, []string{"Trips/Iceland/Reykjavik"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ApplyTags([]int64{id2}, []string{"Trips/Iceland"}, nil, 1); err != nil {
		t.Fatal(err)
	}

	_, err := cat.DeleteTag("Trips", false)
	var descendants *TagHasDescendantsError
	if !errors.As(err, &descendants) {
		t.Fatalf("error = %v, want TagHasDescendantsError", err)
	}
	if want := []string{"Trips/Iceland", "Trips/Iceland/Reykjavik"}; !reflect.DeepEqual(descendants.Descendants, want) {
		t.Fatalf("descendants = %v, want %v", descendants.Descendants, want)
	}

	result, err := cat.DeleteTag("trips", true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 3 || result.FilesAffected != 2 {
		t.Fatalf("delete result = %#v", result)
	}
	for _, id := range []int64{id1, id2} {
		file, err := cat.GetFileByID(id)
		if err != nil {
			t.Fatalf("file %d was deleted: %v", id, err)
		}
		if file.Tags == nil || len(file.Tags) != 0 {
			t.Fatalf("file %d tags = %#v", id, file.Tags)
		}
	}
	if _, err := cat.DeleteTag("Trips", true); !errors.Is(err, ErrNoSuchTag) {
		t.Fatalf("second delete error = %v, want ErrNoSuchTag", err)
	}
}
