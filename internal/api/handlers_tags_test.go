package api

import (
	"bytes"
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

func addAPITagTestFile(t *testing.T, cat *catalog.Catalog, folder, name string) int64 {
	t.Helper()
	if _, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: folder, RelativePath: name, Filename: name, Extension: ".jpg",
		SizeBytes: 1, FileMtime: time.Unix(1, 0).UTC(), Status: catalog.StatusSynced,
	}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	file, err := cat.GetFile(folder, name)
	if err != nil {
		t.Fatal(err)
	}
	return file.ID
}

func tagAPIRequest(t *testing.T, srv *Server, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func decodeTagAPIResponse[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var result T
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decoding response %q: %v", w.Body.String(), err)
	}
	return result
}

func TestHandleFileTagsAddsRemovesNormalizesAndHydrates(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	id := addAPITagTestFile(t, cat, "TestFolder", "one.jpg")

	w := tagAPIRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/files/%d/tags", id), fileTagsRequest{
		Add: []string{" Trips / Iceland ", "ÍSLAND"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", w.Code, w.Body.String())
	}
	response := decodeTagAPIResponse[fileTagsResponse](t, w)
	if want := []string{"Trips", "Trips/Iceland", "ÍSLAND"}; !reflect.DeepEqual(response.Created, want) {
		t.Fatalf("created = %v, want %v", response.Created, want)
	}
	if want := []string{"Trips/Iceland", "ÍSLAND"}; !reflect.DeepEqual(response.File.Tags, want) {
		t.Fatalf("file tags = %v, want %v", response.File.Tags, want)
	}

	// Whitespace and case variants resolve to the existing row.
	w = tagAPIRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/files/%d/tags", id), fileTagsRequest{
		Add: []string{" trips / ICELAND "},
	})
	response = decodeTagAPIResponse[fileTagsResponse](t, w)
	if w.Code != http.StatusOK || len(response.Created) != 0 || len(response.File.Tags) != 2 {
		t.Fatalf("duplicate POST status = %d, response = %#v", w.Code, response)
	}

	w = tagAPIRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/files/%d/tags", id), fileTagsRequest{
		Remove: []string{"TRIPS/ICELAND"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, body = %s", w.Code, w.Body.String())
	}
	response = decodeTagAPIResponse[fileTagsResponse](t, w)
	if !reflect.DeepEqual(response.File.Tags, []string{"ÍSLAND"}) || response.Created == nil {
		t.Fatalf("DELETE response = %#v", response)
	}

	w = tagAPIRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/files/%d/tags", id), fileTagsRequest{
		Add: []string{"not allowed"},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("DELETE with add status = %d, want 400", w.Code)
	}

	w = tagAPIRequest(t, srv, http.MethodPost, "/api/v1/files/999999/tags", fileTagsRequest{Add: []string{"Favorite"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing file status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestHandleFileTagsRejectsInvalidVocabulary(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	id := addAPITagTestFile(t, cat, "TestFolder", "one.jpg")
	tooLongPath := strings.Join([]string{
		strings.Repeat("a", 64), strings.Repeat("b", 64),
		strings.Repeat("c", 64), strings.Repeat("d", 64),
	}, "/")
	tests := []string{
		"", "/Trips", "Trips/", "Trips//Iceland", "Trips/\nIceland",
		strings.Repeat("界", 65), tooLongPath, "a/b/c/d/e/f/g",
	}
	for _, name := range tests {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			w := tagAPIRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/files/%d/tags", id),
				fileTagsRequest{Add: []string{name}})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			body := decodeTagAPIResponse[map[string]string](t, w)
			if !strings.Contains(body["error"], "invalid tag") {
				t.Fatalf("error = %q, want offending tag validation", body["error"])
			}
		})
	}
}

func TestHandleListTagsScopesCountsAndPrefix(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	id1 := addAPITagTestFile(t, cat, "TestFolder", "one.jpg")
	id2 := addAPITagTestFile(t, cat, "TestFolder", "two.jpg")
	id3 := addAPITagTestFile(t, cat, "Other", "other.jpg")
	if _, err := cat.ApplyTags([]int64{id1}, []string{"Trips/Iceland"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ApplyTags([]int64{id2}, []string{"Trips/Iceland/Reykjavik", "ÍSLAND"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ApplyTags([]int64{id3}, []string{"Trips/Iceland"}, nil, 1); err != nil {
		t.Fatal(err)
	}

	w := tagAPIRequest(t, srv, http.MethodGet, "/api/v1/tags?folder=TestFolder&prefix=TRIPS&limit=2", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET tags status = %d, body = %s", w.Code, w.Body.String())
	}
	tags := decodeTagAPIResponse[[]catalog.Tag](t, w)
	if len(tags) != 2 || tags[0].Name != "Trips/Iceland" || tags[0].Count != 1 || tags[0].DescendantCount != 2 {
		t.Fatalf("tags = %#v", tags)
	}

	w = tagAPIRequest(t, srv, http.MethodGet, "/api/v1/tags?folder=TestFolder&prefix="+url.QueryEscape("ís"), nil)
	tags = decodeTagAPIResponse[[]catalog.Tag](t, w)
	if w.Code != http.StatusOK || len(tags) != 1 || tags[0].Name != "ÍSLAND" {
		t.Fatalf("unicode prefix status = %d, tags = %#v", w.Code, tags)
	}
	w = tagAPIRequest(t, srv, http.MethodGet, "/api/v1/tags?limit=0", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status = %d, want 400", w.Code)
	}
}

func TestHandleTagLifecycleMergesAndRequiresDeleteCascade(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	id1 := addAPITagTestFile(t, cat, "TestFolder", "one.jpg")
	id2 := addAPITagTestFile(t, cat, "TestFolder", "two.jpg")
	if _, err := cat.ApplyTags([]int64{id1}, []string{"Trips/Icland/Reykjavik"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ApplyTags([]int64{id2}, []string{"Trips/Iceland/Reykjavik"}, nil, 1); err != nil {
		t.Fatal(err)
	}

	w := tagAPIRequest(t, srv, http.MethodPut, "/api/v1/tags/Trips%2FIcland", renameTagRequest{Name: "Trips/Iceland"})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", w.Code, w.Body.String())
	}
	rename := decodeTagAPIResponse[catalog.TagRenameResult](t, w)
	if !rename.Merged || rename.FilesAffected != 1 {
		t.Fatalf("rename = %#v", rename)
	}

	w = tagAPIRequest(t, srv, http.MethodDelete, "/api/v1/tags/Trips", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("DELETE without cascade status = %d, body = %s", w.Code, w.Body.String())
	}
	errorBody := decodeTagAPIResponse[map[string]string](t, w)
	if !strings.Contains(errorBody["error"], "Trips/Iceland") {
		t.Fatalf("descendant error = %q", errorBody["error"])
	}

	w = tagAPIRequest(t, srv, http.MethodDelete, "/api/v1/tags/Trips?cascade=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("cascade DELETE status = %d, body = %s", w.Code, w.Body.String())
	}
	deleted := decodeTagAPIResponse[catalog.TagDeleteResult](t, w)
	if deleted.Deleted != 3 || deleted.FilesAffected != 2 {
		t.Fatalf("delete = %#v", deleted)
	}
	for _, id := range []int64{id1, id2} {
		file, err := cat.GetFileByID(id)
		if err != nil || file.Tags == nil || len(file.Tags) != 0 {
			t.Fatalf("file %d after delete = %#v, %v", id, file, err)
		}
	}

	w = tagAPIRequest(t, srv, http.MethodDelete, "/api/v1/tags/Missing", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing tag status = %d, want 404", w.Code)
	}
	w = tagAPIRequest(t, srv, http.MethodDelete, "/api/v1/tags/Missing?cascade=yes", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid cascade status = %d, want 400", w.Code)
	}
}
