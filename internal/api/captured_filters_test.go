package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func TestCapturedFiltersApplyToFilesAndIDs(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	for _, name := range []string{"inside.jpg", "outside.jpg"} {
		if _, err := cat.UpsertFile(&catalog.FileRecord{
			FolderLabel: "TestFolder", RelativePath: name, Filename: name,
			Extension: ".jpg", FileMtime: time.Now(), Status: catalog.StatusSynced,
		}, time.Now().UnixNano()); err != nil {
			t.Fatal(err)
		}
		file, err := cat.GetFile("TestFolder", name)
		if err != nil {
			t.Fatal(err)
		}
		date := "2024-02-29 12:00:00"
		if name == "outside.jpg" {
			date = "2024-03-01 00:00:00"
		}
		if _, err := cat.UpdateFile(file.ID, catalog.FileEdits{CapturedLocal: &date}); err != nil {
			t.Fatal(err)
		}
	}

	for _, path := range []string{
		"/api/v1/files?folder=TestFolder&captured_from=2024-02-29&captured_to=2024-02-29&sort=captured",
		"/api/v1/files/ids?folder=TestFolder&captured_from=2024-02-29&captured_to=2024-02-29",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %s", path, w.Code, w.Body.String())
		}
		var body any
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if path == "/api/v1/files?folder=TestFolder&captured_from=2024-02-29&captured_to=2024-02-29&sort=captured" {
			rows := body.([]any)
			if len(rows) != 1 || rows[0].(map[string]any)["filename"] != "inside.jpg" {
				t.Fatalf("files response = %#v", body)
			}
		} else if body.(map[string]any)["total"] != float64(1) {
			t.Fatalf("ids response = %#v", body)
		}
	}
}

func TestCapturedFilterValidation(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	for _, path := range []string{
		"/api/v1/files?captured_from=2024-02-30",
		"/api/v1/files?captured_to=2024-2-01",
		"/api/v1/files?undated=yes",
		"/api/v1/files/ids?captured_from=not-a-date",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400, body = %s", path, w.Code, w.Body.String())
		}
	}
}
