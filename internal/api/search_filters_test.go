package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func addAPISearchFile(t *testing.T, cat *catalog.Catalog, name, description string, tags ...string) int64 {
	t.Helper()
	if _, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: name, Filename: name,
		Extension: ".jpg", Status: catalog.StatusSynced, FileMtime: time.Unix(1, 0).UTC(),
	}, 1); err != nil {
		t.Fatal(err)
	}
	file, err := cat.GetFile("TestFolder", name)
	if err != nil {
		t.Fatal(err)
	}
	if description != "" {
		if _, err := cat.UpdateFile(file.ID, catalog.FileEdits{Description: &description}); err != nil {
			t.Fatal(err)
		}
	}
	if len(tags) > 0 {
		if _, err := cat.ApplyTags([]int64{file.ID}, tags, nil, 2); err != nil {
			t.Fatal(err)
		}
	}
	return file.ID
}

func TestSearchFiltersAreSharedByFilesAndIDs(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	wantID := addAPISearchFile(t, cat, "target.jpg", "Arctic travel diary", "Trips/Iceland/Reykjavik", "People/Alice")
	addAPISearchFile(t, cat, "missing-person.jpg", "Arctic travel diary", "Trips/Iceland")
	addAPISearchFile(t, cat, "missing-description.jpg", "", "Trips/Iceland", "People/Alice")
	addAPISearchFile(t, cat, "untagged.jpg", "Arctic travel diary")

	values := url.Values{
		"folder":    {"TestFolder"},
		"q":         {"travel diary"},
		"tag":       {" trips / ICELAND ", "PEOPLE"},
		"untagged":  {"0"},
		"described": {"1"},
	}
	query := values.Encode()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/files?"+query, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("files status = %d, body = %s", w.Code, w.Body.String())
	}
	var files []catalog.FileRecord
	if err := json.NewDecoder(w.Body).Decode(&files); err != nil {
		t.Fatal(err)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/files/ids?"+query, nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ids status = %d, body = %s", w.Code, w.Body.String())
	}
	var ids fileIDsResponse
	if err := json.NewDecoder(w.Body).Decode(&ids); err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != wantID || !reflect.DeepEqual(ids.IDs, []int64{wantID}) || ids.Total != 1 || ids.Truncated {
		t.Fatalf("files = %#v, ids = %#v, want only %d", files, ids, wantID)
	}
}

func TestSearchFilterValidationIsSharedByFilesAndIDs(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	overCap := url.Values{}
	for i := 0; i <= catalog.MaxTagFilters; i++ {
		overCap.Add("tag", fmt.Sprintf("tag%d", i))
	}
	tests := []string{
		"tag=",
		"tag=Trips%2F%2FIceland",
		"tag=Trips%2FIce%0Aland",
		"tag=a%2Fb%2Fc%2Fd%2Fe%2Ff%2Fg",
		"tag=" + url.QueryEscape(strings.Repeat("界", 65)),
		"untagged=yes",
		"described=yes",
		overCap.Encode(),
	}
	for _, query := range tests {
		t.Run(query, func(t *testing.T) {
			for _, endpoint := range []string{"/api/v1/files", "/api/v1/files/ids"} {
				req := httptest.NewRequest(http.MethodGet, endpoint+"?"+query, nil)
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, req)
				if w.Code != http.StatusBadRequest {
					t.Errorf("%s?%s status = %d, want 400, body = %s", endpoint, query, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestFileIDsOverrideNewSearchFilters(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	id := addAPISearchFile(t, cat, "selected.jpg", "", "Keep")
	query := fmt.Sprintf("ids=%d&q=no-match&tag=Nope&untagged=1&described=1", id)

	for _, endpoint := range []string{"/api/v1/files", "/api/v1/files/ids"} {
		req := httptest.NewRequest(http.MethodGet, endpoint+"?"+query, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", endpoint, w.Code, w.Body.String())
		}
		if endpoint == "/api/v1/files" {
			var files []catalog.FileRecord
			if err := json.NewDecoder(w.Body).Decode(&files); err != nil {
				t.Fatal(err)
			}
			if len(files) != 1 || files[0].ID != id {
				t.Fatalf("files = %#v, want selected id %d", files, id)
			}
			continue
		}
		var ids fileIDsResponse
		if err := json.NewDecoder(w.Body).Decode(&ids); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ids.IDs, []int64{id}) || ids.Total != 1 {
			t.Fatalf("ids = %#v, want selected id %d", ids, id)
		}
	}
}
