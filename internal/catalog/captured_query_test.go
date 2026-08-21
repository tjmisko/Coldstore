package catalog

import (
	"reflect"
	"testing"
	"time"
)

func capturedFixture(t *testing.T) (*Catalog, map[string]int64) {
	t.Helper()
	cat := setupTestCatalog(t)
	names := []string{"early.jpg", "edge.jpg", "tie-a.jpg", "tie-b.jpg", "undated.jpg", "unscanned.jpg"}
	ids := make(map[string]int64, len(names))
	for _, name := range names {
		if _, err := cat.UpsertFile(&FileRecord{
			FolderLabel: "Photos", RelativePath: name, Filename: name,
			Extension: ".jpg", FileMtime: time.Now().UTC(), Status: StatusSynced,
		}, time.Now().UnixNano()); err != nil {
			t.Fatal(err)
		}
		row, err := cat.GetFile("Photos", name)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = row.ID
	}
	values := map[string]string{
		"early.jpg": "2024-01-01 23:59:59.999",
		"edge.jpg":  "2024-01-02 23:59:59.999",
		"tie-a.jpg": "2024-01-02 12:00:00.000",
		"tie-b.jpg": "2024-01-02 12:00:00.000",
	}
	for name, value := range values {
		if _, err := cat.db.Exec(`UPDATE files SET captured_local = ?, meta_scanned_at = 100 WHERE id = ?`, value, ids[name]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cat.db.Exec(`UPDATE files SET meta_scanned_at = 100 WHERE id = ?`, ids["undated.jpg"]); err != nil {
		t.Fatal(err)
	}
	return cat, ids
}

func fileNames(files []FileRecord) []string {
	names := make([]string, len(files))
	for i := range files {
		names[i] = files[i].Filename
	}
	return names
}

func TestCapturedFiltersAreInclusiveAndSharedByFilesAndIDs(t *testing.T) {
	cat, ids := capturedFixture(t)
	q := FileQuery{Folder: "Photos", CapturedFrom: "2024-01-02", CapturedTo: "2024-01-02", SortBy: "captured"}

	files, err := cat.ListFiles(q)
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"edge.jpg", "tie-a.jpg", "tie-b.jpg"}
	if got := fileNames(files); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("captured day files = %v, want %v", got, wantNames)
	}

	gotIDs, total, err := cat.ListFileIDs(q, 100)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []int64{ids["edge.jpg"], ids["tie-a.jpg"], ids["tie-b.jpg"]}
	if total != 3 || !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("captured day ids = %v total %d, want %v total 3", gotIDs, total, wantIDs)
	}
}

func TestUndatedMeansScannedWithoutADate(t *testing.T) {
	cat, ids := capturedFixture(t)

	files, err := cat.ListFiles(FileQuery{Folder: "Photos", Undated: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := fileNames(files); !reflect.DeepEqual(got, []string{"undated.jpg"}) {
		t.Fatalf("undated files = %v, want only scanned empty row", got)
	}
	gotIDs, total, err := cat.ListFileIDs(FileQuery{Folder: "Photos", Undated: true}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || !reflect.DeepEqual(gotIDs, []int64{ids["undated.jpg"]}) {
		t.Fatalf("undated ids = %v total %d", gotIDs, total)
	}
}

func TestCapturedSortIsTotalAndStableAcrossPages(t *testing.T) {
	cat, _ := capturedFixture(t)
	all, err := cat.ListFiles(FileQuery{Folder: "Photos", SortBy: "captured", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"edge.jpg", "tie-a.jpg", "tie-b.jpg", "early.jpg", "undated.jpg", "unscanned.jpg"}
	if got := fileNames(all); !reflect.DeepEqual(got, want) {
		t.Fatalf("captured order = %v, want %v", got, want)
	}
	page, err := cat.ListFiles(FileQuery{Folder: "Photos", SortBy: "captured", Limit: 2, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := fileNames(page); !reflect.DeepEqual(got, want[1:3]) {
		t.Fatalf("captured page = %v, want %v", got, want[1:3])
	}
}
