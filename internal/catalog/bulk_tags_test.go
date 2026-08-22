package catalog

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func tagsForFile(t *testing.T, cat *Catalog, id int64) []string {
	t.Helper()
	file, err := cat.GetFileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return file.Tags
}

func addManyTagTestFiles(t *testing.T, cat *Catalog, count int) []int64 {
	t.Helper()
	tx, err := cat.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.Prepare(`INSERT INTO files
		(folder_label, relative_path, filename, file_mtime) VALUES ('Photos', ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer statement.Close()
	ids := make([]int64, 0, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("bulk-%05d.jpg", i)
		result, err := statement.Exec(name, name, "1970-01-01 00:00:00")
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestApplyTagMutationsReturnsAnExactReplayableUndo(t *testing.T) {
	cat := setupTestCatalog(t)
	a := addTagTestFile(t, cat, "Photos", "a.jpg")
	b := addTagTestFile(t, cat, "Photos", "b.jpg")
	c := addTagTestFile(t, cat, "Photos", "c.jpg")
	if _, err := cat.ApplyTags([]int64{a}, []string{"Favorite", "Old"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ApplyTags([]int64{b}, []string{"Old"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ApplyTags([]int64{c}, []string{"Favorite"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	before := map[int64][]string{
		a: tagsForFile(t, cat, a),
		b: tagsForFile(t, cat, b),
		c: tagsForFile(t, cat, c),
	}

	result, err := cat.ApplyTagMutations([]TagFileMutation{{
		IDs: []int64{a, b, c}, Add: []string{"favorite"}, Remove: []string{"OLD"},
	}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 2 || len(result.Created) != 0 {
		t.Fatalf("result = %#v, want two changed files and no created tags", result)
	}
	wantUndo := []TagFileMutation{
		{IDs: []int64{b}, Add: []string{}, Remove: []string{"Favorite"}},
		{IDs: []int64{a, b}, Add: []string{"Old"}, Remove: []string{}},
	}
	if !reflect.DeepEqual(result.Undo.Changes, wantUndo) {
		t.Fatalf("undo = %#v, want %#v", result.Undo.Changes, wantUndo)
	}

	undone, err := cat.ApplyTagMutations(result.Undo.Changes, 3)
	if err != nil {
		t.Fatal(err)
	}
	if undone.Updated != 2 {
		t.Fatalf("undo updated = %d, want 2", undone.Updated)
	}
	for id, want := range before {
		if got := tagsForFile(t, cat, id); !reflect.DeepEqual(got, want) {
			t.Fatalf("file %d after undo = %v, want %v", id, got, want)
		}
	}
}

func TestApplyTagMutationsDeduplicatesIDsAndNormalizedNames(t *testing.T) {
	cat := setupTestCatalog(t)
	id := addTagTestFile(t, cat, "Photos", "one.jpg")
	result, err := cat.ApplyTagMutations([]TagFileMutation{
		{IDs: []int64{id, id, -1, 999999}, Add: []string{" Trips / Iceland ", "trips/ICELAND"}},
		{IDs: []int64{id}, Add: []string{"TRIPS/iceland"}},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 1 {
		t.Fatalf("updated = %d, want 1", result.Updated)
	}
	if want := []string{"Trips", "Trips/Iceland"}; !reflect.DeepEqual(result.Created, want) {
		t.Fatalf("created = %v, want %v", result.Created, want)
	}
	if want := []string{"Trips/Iceland"}; !reflect.DeepEqual(tagsForFile(t, cat, id), want) {
		t.Fatalf("file tags = %v, want %v", tagsForFile(t, cat, id), want)
	}
	if want := []TagFileMutation{{
		IDs: []int64{id}, Add: []string{}, Remove: []string{"Trips/Iceland"},
	}}; !reflect.DeepEqual(result.Undo.Changes, want) {
		t.Fatalf("undo = %#v, want %#v", result.Undo.Changes, want)
	}
	var ancestorLinks int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM file_tags ft JOIN tags t ON t.id = ft.tag_id
		WHERE t.name = 'Trips'`).Scan(&ancestorLinks); err != nil {
		t.Fatal(err)
	}
	if ancestorLinks != 0 {
		t.Fatalf("ancestor links = %d, want 0", ancestorLinks)
	}
}

func TestApplyTagMutationsIgnoresUnknownIDsWithoutCreatingVocabulary(t *testing.T) {
	cat := setupTestCatalog(t)
	result, err := cat.ApplyTagMutations([]TagFileMutation{{
		IDs: []int64{999998, 999999}, Add: []string{"Does/Not/Exist"},
	}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 0 || len(result.Created) != 0 || len(result.Undo.Changes) != 0 {
		t.Fatalf("result = %#v, want a non-null empty no-op", result)
	}
	var tags int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&tags); err != nil {
		t.Fatal(err)
	}
	if tags != 0 {
		t.Fatalf("tag rows = %d after unknown-id request", tags)
	}
}

func TestApplyTagMutationsRejectsOnlyPerFileDirectionOverlap(t *testing.T) {
	cat := setupTestCatalog(t)
	a := addTagTestFile(t, cat, "Photos", "a.jpg")
	b := addTagTestFile(t, cat, "Photos", "b.jpg")
	if _, err := cat.ApplyTags([]int64{b}, []string{"Flag"}, nil, 1); err != nil {
		t.Fatal(err)
	}

	// The same tag may move in opposite directions on different files. Exact
	// undo depends on this being representable in one request.
	if _, err := cat.ApplyTagMutations([]TagFileMutation{
		{IDs: []int64{a}, Add: []string{"Flag"}},
		{IDs: []int64{b}, Remove: []string{"flag"}},
	}, 2); err != nil {
		t.Fatalf("disjoint opposite changes: %v", err)
	}

	_, err := cat.ApplyTagMutations([]TagFileMutation{
		{IDs: []int64{a}, Add: []string{"Flag"}},
		{IDs: []int64{a}, Remove: []string{" flag "}},
	}, 3)
	var invalid *InvalidTagNameError
	if !errors.As(err, &invalid) {
		t.Fatalf("overlap error = %v, want InvalidTagNameError", err)
	}
}

func TestApplyTagMutationsRollsBackAssignmentsAndCreatedAncestors(t *testing.T) {
	cat := setupTestCatalog(t)
	a := addTagTestFile(t, cat, "Photos", "a.jpg")
	b := addTagTestFile(t, cat, "Photos", "b.jpg")
	if _, err := cat.db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_second_tag
		BEFORE INSERT ON file_tags WHEN NEW.file_id = %d
		BEGIN SELECT RAISE(ABORT, 'injected bulk tag failure'); END`, b)); err != nil {
		t.Fatal(err)
	}

	if _, err := cat.ApplyTagMutations([]TagFileMutation{{
		IDs: []int64{a, b}, Add: []string{"Trips/Iceland"},
	}}, 1); err == nil {
		t.Fatal("injected assignment failure returned nil")
	}
	for table, want := range map[string]int{"tags": 0, "file_tags": 0} {
		var got int
		if err := cat.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s rows after rollback = %d, want %d", table, got, want)
		}
	}
}

func TestApplyTagMutationsChunksSelectionsOverFiveHundredIDs(t *testing.T) {
	cat := setupTestCatalog(t)
	ids := addManyTagTestFiles(t, cat, idChunkSize+1)
	result, err := cat.ApplyTagMutations([]TagFileMutation{{
		IDs: ids, Add: []string{"Batch"},
	}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != int64(len(ids)) || len(result.Undo.Changes) != 1 ||
		len(result.Undo.Changes[0].IDs) != len(ids) {
		t.Fatalf("chunked result = %#v", result)
	}
	var assignments int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM file_tags`).Scan(&assignments); err != nil {
		t.Fatal(err)
	}
	if assignments != len(ids) {
		t.Fatalf("assignments = %d, want %d", assignments, len(ids))
	}
}

func TestApplyTagMutationsEnforcesNormalizedCapsAndAcceptsLegalNoOps(t *testing.T) {
	cat := setupTestCatalog(t)
	ids := make([]int64, MaxBulkTagIDs)
	for i := range ids {
		ids[i] = int64(100000 + i)
	}
	result, err := cat.ApplyTagMutations([]TagFileMutation{{
		IDs: ids, Add: []string{"Missing IDs do not create this"},
	}}, 1)
	if err != nil || result.Updated != 0 {
		t.Fatalf("legal cap no-op = %#v, %v", result, err)
	}
	ids = append(ids, 999999)
	result, err = cat.ApplyTagMutations([]TagFileMutation{{IDs: ids, Add: []string{"Still missing"}}}, 1)
	if err != nil || result.Updated != 0 {
		t.Fatalf("more than 5000 unknown ids should still be a no-op: %#v, %v", result, err)
	}

	existing := addManyTagTestFiles(t, cat, MaxBulkTagIDs+1)
	_, err = cat.ApplyTagMutations([]TagFileMutation{{IDs: existing, Add: []string{"Too many"}}}, 1)
	var limit *TagMutationLimitError
	if !errors.As(err, &limit) || limit.Field != "file ids" {
		t.Fatalf("id cap error = %v", err)
	}

	id := addTagTestFile(t, cat, "Photos", "one.jpg")
	names := make([]string, MaxBulkTagNames+1)
	for i := range names {
		names[i] = fmt.Sprintf("tag-%02d", i)
	}
	_, err = cat.ApplyTagMutations([]TagFileMutation{{IDs: []int64{id}, Add: names}}, 1)
	if !errors.As(err, &limit) || limit.Field != "tag names" {
		t.Fatalf("tag cap error = %v", err)
	}

	duplicates := make([]string, MaxBulkTagNames+20)
	for i := range duplicates {
		duplicates[i] = " Duplicate "
	}
	if _, err := cat.ApplyTagMutations([]TagFileMutation{{
		IDs: []int64{id, id}, Add: duplicates,
	}}, 2); err != nil {
		t.Fatalf("normalized duplicates should count once: %v", err)
	}
	noOp, err := cat.ApplyTagMutations([]TagFileMutation{{
		IDs: []int64{id}, Add: []string{"duplicate"}, Remove: []string{"absent"},
	}}, 3)
	if err != nil || noOp.Updated != 0 || len(noOp.Undo.Changes) != 0 {
		t.Fatalf("legal no-op = %#v, %v", noOp, err)
	}
}
