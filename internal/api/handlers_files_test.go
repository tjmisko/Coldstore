package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

// 404 is a diagnosis, not a shrug: it tells the owner the file is gone from
// the catalog, which is the one answer that makes them stop looking. A
// database that cannot be read has to say so, or a corrupt catalog presents as
// an empty one and the UI helpfully reports every file as deleted.
func TestShouldAnswerServerErrorWhenTheCatalogCannotBeReadForADownload(t *testing.T) {
	srv, cat, _ := setupTestServer(t)

	cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "a.jpg", Filename: "a.jpg",
		Extension: ".jpg", SizeBytes: 100, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, err := cat.GetFile("TestFolder", "a.jpg")
	if err != nil {
		t.Fatal(err)
	}

	// The cheapest real database failure there is. Every query from here on
	// returns "sql: database is closed", which is emphatically not ErrNoSuchFile.
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/v1/files/"+itoa(file.ID)+"/download", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code == http.StatusNotFound {
		t.Fatalf("status = 404: a broken catalog is being reported as a missing file, body: %s",
			w.Body.String())
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500, body: %s", w.Code, w.Body.String())
	}
}

// The other half of the same distinction: an id that genuinely is not there
// must still be a 404, so the fix above cannot be "return 500 for everything".
func TestShouldAnswerNotFoundWhenTheDownloadedFileIdDoesNotExist(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/files/99999/download", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body: %s", w.Code, w.Body.String())
	}
}

// Same audit, the pin handler: it reads the row before the write now, and that
// read must not turn a database failure into "your file is gone".
func TestShouldAnswerServerErrorWhenTheCatalogCannotBeReadForAPin(t *testing.T) {
	srv, cat, _ := setupTestServer(t)

	cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "b.jpg", Filename: "b.jpg",
		Extension: ".jpg", SizeBytes: 100, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, err := cat.GetFile("TestFolder", "b.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/api/v1/files/"+itoa(file.ID)+"/pin", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500, body: %s", w.Code, w.Body.String())
	}
}
