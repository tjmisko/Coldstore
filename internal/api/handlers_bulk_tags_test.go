package api

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func TestHandleBulkFileTagsReturnsAndAcceptsExactUndo(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	a := addAPITagTestFile(t, cat, "TestFolder", "a.jpg")
	b := addAPITagTestFile(t, cat, "TestFolder", "b.jpg")
	c := addAPITagTestFile(t, cat, "TestFolder", "c.jpg")
	if _, err := cat.ApplyTags([]int64{a}, []string{"Favorite", "Old"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ApplyTags([]int64{b}, []string{"Old"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ApplyTags([]int64{c}, []string{"Favorite"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	before := map[int64][]string{}
	for _, id := range []int64{a, b, c} {
		file, err := cat.GetFileByID(id)
		if err != nil {
			t.Fatal(err)
		}
		before[id] = file.Tags
	}

	w := tagAPIRequest(t, srv, http.MethodPost, "/api/v1/files/tags", bulkFileTagsRequest{
		IDs: []int64{a, b, c}, Add: []string{"favorite"}, Remove: []string{"old"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("bulk status = %d, body = %s", w.Code, w.Body.String())
	}
	rawResponse := w.Body.String()
	result := decodeTagAPIResponse[catalog.TagMutationResult](t, w)
	if result.Updated != 2 || len(result.Undo.Changes) != 2 {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(rawResponse, `"created":[]`) || !strings.Contains(rawResponse, `"add":[]`) {
		t.Fatalf("response arrays must be non-null: %s", rawResponse)
	}

	w = tagAPIRequest(t, srv, http.MethodPost, "/api/v1/files/tags", bulkFileTagsRequest{
		Changes: result.Undo.Changes,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("undo status = %d, body = %s", w.Code, w.Body.String())
	}
	undoResult := decodeTagAPIResponse[catalog.TagMutationResult](t, w)
	if undoResult.Updated != 2 {
		t.Fatalf("undo result = %#v", undoResult)
	}
	for id, want := range before {
		file, err := cat.GetFileByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(file.Tags, want) {
			t.Fatalf("file %d after API undo = %v, want %v", id, file.Tags, want)
		}
	}
}

func TestHandleBulkFileTagsValidatesRequestShapesAndOverlap(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	id := addAPITagTestFile(t, cat, "TestFolder", "one.jpg")
	tests := []struct {
		name string
		body any
	}{
		{name: "empty", body: bulkFileTagsRequest{}},
		{name: "ids only", body: bulkFileTagsRequest{IDs: []int64{id}}},
		{name: "explicit empty changes", body: bulkFileTagsRequest{Changes: []catalog.TagFileMutation{}}},
		{name: "change without ids", body: bulkFileTagsRequest{Changes: []catalog.TagFileMutation{{Add: []string{"A"}}}}},
		{name: "change without tags", body: bulkFileTagsRequest{Changes: []catalog.TagFileMutation{{IDs: []int64{id}}}}},
		{name: "mixed forms", body: bulkFileTagsRequest{
			IDs: []int64{id}, Add: []string{"A"},
			Changes: []catalog.TagFileMutation{{IDs: []int64{id}, Add: []string{"B"}}},
		}},
		{name: "uniform overlap", body: bulkFileTagsRequest{
			IDs: []int64{id}, Add: []string{" Flag "}, Remove: []string{"FLAG"},
		}},
		{name: "heterogeneous same-file overlap", body: bulkFileTagsRequest{Changes: []catalog.TagFileMutation{
			{IDs: []int64{id}, Add: []string{"Flag"}},
			{IDs: []int64{id}, Remove: []string{"flag"}},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := tagAPIRequest(t, srv, http.MethodPost, "/api/v1/files/tags", tt.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleBulkFileTagsDeduplicatesBeforeCapsAndIgnoresMissingIDs(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	id := addAPITagTestFile(t, cat, "TestFolder", "one.jpg")
	ids := make([]int64, catalog.MaxBulkTagIDs+100)
	for i := range ids {
		ids[i] = id
	}
	names := make([]string, catalog.MaxBulkTagNames+20)
	for i := range names {
		names[i] = " Trips / Iceland "
	}
	ids = append(ids, 999999)
	w := tagAPIRequest(t, srv, http.MethodPost, "/api/v1/files/tags", bulkFileTagsRequest{
		IDs: ids, Add: names,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("deduplicated request status = %d, body = %s", w.Code, w.Body.String())
	}
	result := decodeTagAPIResponse[catalog.TagMutationResult](t, w)
	if result.Updated != 1 || len(result.Undo.Changes) != 1 ||
		!reflect.DeepEqual(result.Undo.Changes[0].IDs, []int64{id}) {
		t.Fatalf("deduplicated result = %#v", result)
	}

	// Repeating an already-applied association is a legal no-op, not an error.
	w = tagAPIRequest(t, srv, http.MethodPost, "/api/v1/files/tags", bulkFileTagsRequest{
		IDs: []int64{id, 999998}, Add: []string{"trips/iceland"}, Remove: []string{"Absent"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("no-op status = %d, body = %s", w.Code, w.Body.String())
	}
	result = decodeTagAPIResponse[catalog.TagMutationResult](t, w)
	if result.Updated != 0 || len(result.Created) != 0 || len(result.Undo.Changes) != 0 {
		t.Fatalf("no-op result = %#v", result)
	}

	// An all-missing request does not manufacture unused vocabulary.
	w = tagAPIRequest(t, srv, http.MethodPost, "/api/v1/files/tags", bulkFileTagsRequest{
		IDs: []int64{999997}, Add: []string{"Missing/Only"},
	})
	result = decodeTagAPIResponse[catalog.TagMutationResult](t, w)
	if w.Code != http.StatusOK || result.Updated != 0 || len(result.Created) != 0 {
		t.Fatalf("missing-only status = %d, result = %#v", w.Code, result)
	}
}

func TestHandleBulkFileTagsEnforcesExistingIDAndNormalizedNameCaps(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	id := addAPITagTestFile(t, cat, "TestFolder", "one.jpg")
	tooManyIDs := make([]int64, catalog.MaxBulkTagIDs+1)
	for i := range tooManyIDs {
		tooManyIDs[i] = int64(i + 100000)
	}
	tooManyIDs[0] = id
	w := tagAPIRequest(t, srv, http.MethodPost, "/api/v1/files/tags", bulkFileTagsRequest{
		IDs: tooManyIDs, Add: []string{"A"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("only existing ids count toward the cap: status = %d, body = %s", w.Code, w.Body.String())
	}

	tooManyNames := make([]string, catalog.MaxBulkTagNames+1)
	for i := range tooManyNames {
		tooManyNames[i] = fmt.Sprintf("tag-%02d", i)
	}
	w = tagAPIRequest(t, srv, http.MethodPost, "/api/v1/files/tags", bulkFileTagsRequest{
		IDs: []int64{id}, Add: tooManyNames,
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "16") {
		t.Fatalf("tag cap status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestSingleFileTagEndpointKeepsItsPreBulkNameLimitBehavior(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	id := addAPITagTestFile(t, cat, "TestFolder", "one.jpg")
	names := make([]string, catalog.MaxBulkTagNames+1)
	for i := range names {
		names[i] = fmt.Sprintf("single-%02d", i)
	}
	w := tagAPIRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/files/%d/tags", id),
		fileTagsRequest{Add: names})
	if w.Code != http.StatusOK {
		t.Fatalf("single-file status = %d, body = %s", w.Code, w.Body.String())
	}
	response := decodeTagAPIResponse[fileTagsResponse](t, w)
	if len(response.File.Tags) != len(names) {
		t.Fatalf("single-file tags = %d, want %d", len(response.File.Tags), len(names))
	}
}
