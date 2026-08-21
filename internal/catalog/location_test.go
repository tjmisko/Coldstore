package catalog

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func addLocationTestFile(t *testing.T, cat *Catalog, name string, coordinates *GPSCoordinates) int64 {
	t.Helper()
	if _, err := cat.UpsertFile(&FileRecord{
		FolderLabel: "Photos", RelativePath: name, Filename: name, Extension: ".jpg",
		Status: StatusSynced, FileMtime: time.Unix(1, 0).UTC(),
	}, 1); err != nil {
		t.Fatal(err)
	}
	file, err := cat.GetFile("Photos", name)
	if err != nil {
		t.Fatal(err)
	}
	if coordinates != nil {
		file, err = cat.UpdateFile(file.ID, FileEdits{GPS: coordinates})
		if err != nil {
			t.Fatal(err)
		}
	}
	return file.ID
}

func fileIDs(files []FileRecord) []int64 {
	ids := make([]int64, len(files))
	for i := range files {
		ids[i] = files[i].ID
	}
	return ids
}

func TestListFilesNearUsesExactCircleBeforePagination(t *testing.T) {
	cat := setupTestCatalog(t)
	insideA := addLocationTestFile(t, cat, "a-inside.jpg", &GPSCoordinates{Lat: 0.1, Lon: 0.1})
	addLocationTestFile(t, cat, "b-box-only.jpg", &GPSCoordinates{Lat: 0.7, Lon: 0.7})
	insideC := addLocationTestFile(t, cat, "c-inside.jpg", &GPSCoordinates{Lat: 0.2, Lon: 0.2})
	addLocationTestFile(t, cat, "d-outside.jpg", &GPSCoordinates{Lat: 2, Lon: 2})
	addLocationTestFile(t, cat, "e-unknown.jpg", nil)

	near := &NearFilter{Lat: 0, Lon: 0, RadiusKM: 100}
	files, err := cat.ListFiles(FileQuery{Folder: "Photos", Near: near, SortBy: "name", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fileIDs(files), []int64{insideA, insideC}; !reflect.DeepEqual(got, want) {
		t.Fatalf("near page ids = %v, want %v", got, want)
	}

	second, err := cat.ListFiles(FileQuery{Folder: "Photos", Near: near, SortBy: "name", Offset: 1, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fileIDs(second), []int64{insideC}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second page ids = %v, want %v", got, want)
	}
}

func TestListFileIDsNearUsesSameExactFilterAndTotal(t *testing.T) {
	cat := setupTestCatalog(t)
	insideA := addLocationTestFile(t, cat, "a.jpg", &GPSCoordinates{Lat: 0.1, Lon: 0.1})
	addLocationTestFile(t, cat, "box-only.jpg", &GPSCoordinates{Lat: 0.7, Lon: 0.7})
	insideB := addLocationTestFile(t, cat, "b.jpg", &GPSCoordinates{Lat: -0.2, Lon: -0.2})

	near := &NearFilter{Lat: 0, Lon: 0, RadiusKM: 100}
	ids, total, err := cat.ListFileIDs(FileQuery{Near: near}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []int64{insideA}) || total != 2 {
		t.Fatalf("capped ids = %v, total = %d; want [%d], 2 (other exact id %d)", ids, total, insideA, insideB)
	}

	ids, total, err = cat.ListFileIDs(FileQuery{Near: near}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{insideA, insideB}; !reflect.DeepEqual(ids, want) || total != 2 {
		t.Fatalf("ids = %v, total = %d; want %v, 2", ids, total, want)
	}
}

func TestNearFilterHandlesDatelinePolesAndZeroCoordinates(t *testing.T) {
	cat := setupTestCatalog(t)
	zero := addLocationTestFile(t, cat, "zero.jpg", &GPSCoordinates{Lat: 0, Lon: 0})
	dateline := addLocationTestFile(t, cat, "dateline.jpg", &GPSCoordinates{Lat: 0, Lon: -179.8})
	pole := addLocationTestFile(t, cat, "pole.jpg", &GPSCoordinates{Lat: 89.9, Lon: 120})

	files, err := cat.ListFiles(FileQuery{Near: &NearFilter{Lat: 0, Lon: 0, RadiusKM: 1}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := fileIDs(files); !reflect.DeepEqual(got, []int64{zero}) {
		t.Fatalf("0,0 query ids = %v, want [%d]", got, zero)
	}

	files, err = cat.ListFiles(FileQuery{Near: &NearFilter{Lat: 0, Lon: 179.8, RadiusKM: 50}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := fileIDs(files); !reflect.DeepEqual(got, []int64{dateline}) {
		t.Fatalf("dateline query ids = %v, want [%d]", got, dateline)
	}

	files, err = cat.ListFiles(FileQuery{Near: &NearFilter{Lat: 89.8, Lon: 0, RadiusKM: 50}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := fileIDs(files); !reflect.DeepEqual(got, []int64{pole}) {
		t.Fatalf("pole query ids = %v, want [%d]", got, pole)
	}
}

func TestNearFilterRejectsNonFiniteAndOutOfRangeValues(t *testing.T) {
	cat := setupTestCatalog(t)
	tests := []NearFilter{
		{Lat: math.NaN(), Lon: 0, RadiusKM: 1},
		{Lat: 0, Lon: math.Inf(1), RadiusKM: 1},
		{Lat: 0, Lon: 0, RadiusKM: math.Inf(1)},
		{Lat: -91, Lon: 0, RadiusKM: 1},
		{Lat: 0, Lon: 181, RadiusKM: 1},
		{Lat: 0, Lon: 0, RadiusKM: 0},
		{Lat: 0, Lon: 0, RadiusKM: 500.1},
	}
	for _, near := range tests {
		if _, err := cat.ListFiles(FileQuery{Near: &near}); !errors.Is(err, ErrInvalidNear) {
			t.Errorf("ListFiles(%+v) error = %v, want ErrInvalidNear", near, err)
		}
		if _, _, err := cat.ListFileIDs(FileQuery{Near: &near}, 10); !errors.Is(err, ErrInvalidNear) {
			t.Errorf("ListFileIDs(%+v) error = %v, want ErrInvalidNear", near, err)
		}
	}
}

func TestNearFilterBuildsNormalDatelineAndPoleBoundingBoxesInSQL(t *testing.T) {
	tests := []struct {
		name        string
		near        NearFilter
		wantSQL     string
		dontWantSQL string
	}{
		{name: "ordinary", near: NearFilter{Lat: 0, Lon: 0, RadiusKM: 10}, wantSQL: "gps_lon BETWEEN ? AND ?"},
		{name: "dateline", near: NearFilter{Lat: 0, Lon: 179.9, RadiusKM: 50}, wantSQL: "gps_lon >= ? OR gps_lon <= ?"},
		{name: "pole", near: NearFilter{Lat: 89.9, Lon: 0, RadiusKM: 50}, dontWantSQL: "gps_lon BETWEEN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			where, _ := (FileQuery{Near: &tt.near}).filter()
			if !strings.Contains(where, "gps_lat BETWEEN ? AND ?") {
				t.Fatalf("SQL lacks latitude bounding box: %s", where)
			}
			if tt.wantSQL != "" && !strings.Contains(where, tt.wantSQL) {
				t.Fatalf("SQL = %s, want %q", where, tt.wantSQL)
			}
			if tt.dontWantSQL != "" && strings.Contains(where, tt.dontWantSQL) {
				t.Fatalf("SQL = %s, must not contain %q", where, tt.dontWantSQL)
			}
		})
	}
}
