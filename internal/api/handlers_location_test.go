package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func addAPILocationFile(t *testing.T, cat *catalog.Catalog, name string, lat, lon *float64) int64 {
	t.Helper()
	if _, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: name, Filename: name, Extension: ".jpg",
		Status: catalog.StatusSynced, FileMtime: time.Unix(1, 0).UTC(),
	}, 1); err != nil {
		t.Fatal(err)
	}
	file, err := cat.GetFile("TestFolder", name)
	if err != nil {
		t.Fatal(err)
	}
	if lat != nil && lon != nil {
		file, err = cat.UpdateFile(file.ID, catalog.FileEdits{GPS: &catalog.GPSCoordinates{Lat: *lat, Lon: *lon}})
		if err != nil {
			t.Fatal(err)
		}
	}
	return file.ID
}

func float64Pointer(value float64) *float64 { return &value }

func TestNearFilterIsSharedByFileRowsAndIDs(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	zero := addAPILocationFile(t, cat, "zero.jpg", float64Pointer(0), float64Pointer(0))
	inside := addAPILocationFile(t, cat, "inside.jpg", float64Pointer(0.2), float64Pointer(0.2))
	addAPILocationFile(t, cat, "box-only.jpg", float64Pointer(0.7), float64Pointer(0.7))
	addAPILocationFile(t, cat, "unknown.jpg", nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/files?near=0,0,100&limit=10&sort=name", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("files status = %d, body = %s", w.Code, w.Body.String())
	}
	var files []catalog.FileRecord
	if err := json.NewDecoder(w.Body).Decode(&files); err != nil {
		t.Fatal(err)
	}
	gotIDs := make([]int64, len(files))
	for i := range files {
		gotIDs[i] = files[i].ID
	}
	if want := []int64{inside, zero}; !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("file ids = %v, want %v", gotIDs, want)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/files/ids?near=0,0,100", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ids status = %d, body = %s", w.Code, w.Body.String())
	}
	var ids fileIDsResponse
	if err := json.NewDecoder(w.Body).Decode(&ids); err != nil {
		t.Fatal(err)
	}
	if want := []int64{zero, inside}; !reflect.DeepEqual(ids.IDs, want) || ids.Total != 2 || ids.Truncated {
		t.Fatalf("ids response = %#v, want ids %v, total 2", ids, want)
	}
}

func TestNearFilterValidation(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	tests := []string{
		"1,2", "one,2,3", "NaN,0,1", "+Inf,0,1", "91,0,1", "0,181,1",
		"0,0,0", "0,0,-1", "0,0,500.1",
	}
	for _, value := range tests {
		t.Run(fmt.Sprintf("near=%s", value), func(t *testing.T) {
			for _, path := range []string{"/api/v1/files", "/api/v1/files/ids"} {
				req := httptest.NewRequest(http.MethodGet, path+"?near="+value, nil)
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, req)
				if w.Code != http.StatusBadRequest {
					t.Errorf("%s status = %d, body = %s", path, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestValidCoordinatesRejectsNonFiniteValuesAndKeepsZero(t *testing.T) {
	if !validCoordinates(0, 0) {
		t.Fatal("0,0 is a valid coordinate")
	}
	for _, pair := range [][2]float64{
		{math.NaN(), 0}, {0, math.NaN()}, {math.Inf(1), 0}, {0, math.Inf(-1)},
		{90.1, 0}, {0, 180.1},
	} {
		if validCoordinates(pair[0], pair[1]) {
			t.Errorf("validCoordinates(%v, %v) = true", pair[0], pair[1])
		}
	}
}
