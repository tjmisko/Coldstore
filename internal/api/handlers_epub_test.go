package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/filetype"
)

const testOPF = `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>The Pocket Guide</dc:title></metadata>
  <manifest>
    <item id="c" href="cover.png" media-type="image/png" properties="cover-image"/>
    <item id="ch1" href="text/ch1.xhtml" media-type="application/xhtml+xml"/>
    <item id="style" href="css/book.css" media-type="text/css"/>
    <item id="tool" href="run.sh" media-type="application/x-sh"/>
  </manifest>
  <spine><itemref idref="ch1"/></spine>
</package>`

// seedEPUBFile writes a small book into the test folder and catalogues it.
func seedEPUBFile(t *testing.T, cat *catalog.Catalog, syncDir, name string) *catalog.FileRecord {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("application/epub+zip"))

	entries := map[string]string{
		"META-INF/container.xml": `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`,
		"OEBPS/content.opf":    testOPF,
		"OEBPS/text/ch1.xhtml": `<html><body><h1>One</h1><img src="../cover.png"/></body></html>`,
		"OEBPS/css/book.css":   `body { color: #eee; }`,
		"OEBPS/cover.png":      "pretend png",
		"OEBPS/run.sh":         "#!/bin/sh\nrm -rf /\n",
	}
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return seedPreviewFile(t, cat, syncDir, name, filetype.TypeEPUB, buf.Bytes())
}

func get(t *testing.T, srv *Server, url string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", url, nil))
	return w
}

func TestEPUBSpineShouldListTheReadingOrderWhenTheFileIsABook(t *testing.T) {
	srv, cat, syncDir := setupTestServer(t)
	rec := seedEPUBFile(t, cat, syncDir, "guide.epub")

	w := get(t, srv, "/api/v1/files/"+itoa(rec.ID)+"/epub")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	var body struct {
		Title    string `json:"title"`
		Sections []struct {
			Path string `json:"path"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Title != "The Pocket Guide" {
		t.Errorf("title = %q", body.Title)
	}
	if len(body.Sections) != 1 || body.Sections[0].Path != "OEBPS/text/ch1.xhtml" {
		t.Errorf("sections = %+v, want the one chapter resolved against the OPF's directory", body.Sections)
	}
}

func TestEPUBResourceShouldServeAChapterAsHTMLWhenRequested(t *testing.T) {
	srv, cat, syncDir := setupTestServer(t)
	rec := seedEPUBFile(t, cat, syncDir, "guide.epub")

	w := get(t, srv, "/api/v1/files/"+itoa(rec.ID)+"/epub/OEBPS/text/ch1.xhtml")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	if !strings.Contains(w.Body.String(), "<h1>One</h1>") {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestEPUBResourceShouldServeAStylesheetWhenTheChapterNeedsIt(t *testing.T) {
	// The chapter's own relative URLs resolve against this route, so the
	// stylesheet has to be reachable at the path the markup names.
	srv, cat, syncDir := setupTestServer(t)
	rec := seedEPUBFile(t, cat, syncDir, "guide.epub")

	w := get(t, srv, "/api/v1/files/"+itoa(rec.ID)+"/epub/OEBPS/css/book.css")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/css") {
		t.Errorf("Content-Type = %q, want text/css", got)
	}
}

func TestEPUBResourceShouldSetNosniffAndACSPWhenServingAChapter(t *testing.T) {
	// The chapter is untrusted markup from a file that arrived over Syncthing.
	// It renders in a sandboxed iframe; this is the matching server-side
	// statement.
	srv, cat, syncDir := setupTestServer(t)
	rec := seedEPUBFile(t, cat, syncDir, "guide.epub")

	w := get(t, srv, "/api/v1/files/"+itoa(rec.ID)+"/epub/OEBPS/text/ch1.xhtml")

	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy on untrusted book markup")
	}
	for _, want := range []string{"default-src 'none'", "frame-ancestors 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q is missing %q", csp, want)
		}
	}
}

func TestEPUBResourceShouldRefuseWhenTheEntryTypeIsNotOneABookUses(t *testing.T) {
	// The archive really does contain run.sh. A preview route that will serve
	// an arbitrary type out of a file synced from a phone is a file-hosting
	// hole.
	srv, cat, syncDir := setupTestServer(t)
	rec := seedEPUBFile(t, cat, syncDir, "guide.epub")

	w := get(t, srv, "/api/v1/files/"+itoa(rec.ID)+"/epub/OEBPS/run.sh")

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415, body: %s", w.Code, w.Body.String())
	}
}

func TestEPUBResourceShouldRefuseWhenTheEntryNameEscapesTheArchive(t *testing.T) {
	// The untrusted component of the route. The mux cleans a URL and knows
	// nothing about what is inside a zip, so the check has to be explicit.
	srv, cat, syncDir := setupTestServer(t)
	rec := seedEPUBFile(t, cat, syncDir, "guide.epub")
	base := "/api/v1/files/" + itoa(rec.ID) + "/epub/"

	// A secret next to the archive, which a successful traversal would read.
	if err := os.WriteFile(filepath.Join(syncDir, "secret.css"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"../secret.css",
		"OEBPS/../../secret.css",
		"..%2F..%2Fsecret.css",
		"%2e%2e%2fsecret.css",
		"OEBPS/..%2f..%2fsecret.css",
	} {
		w := get(t, srv, base+name)
		if w.Code == http.StatusOK {
			t.Errorf("%q returned 200: %s", name, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "SECRET") {
			t.Errorf("%q read a file outside the archive", name)
		}
	}
}

func TestEPUBResourceShouldAnswerNotFoundWhenTheEntryIsNotInTheArchive(t *testing.T) {
	srv, cat, syncDir := setupTestServer(t)
	rec := seedEPUBFile(t, cat, syncDir, "guide.epub")

	w := get(t, srv, "/api/v1/files/"+itoa(rec.ID)+"/epub/OEBPS/text/nope.xhtml")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body: %s", w.Code, w.Body.String())
	}
}

func TestEPUBSpineShouldRefuseWhenTheFileIsNotAnEPUB(t *testing.T) {
	// Otherwise this route is a general-purpose archive extractor pointed at
	// every file in every synced folder.
	srv, cat, syncDir := setupTestServer(t)
	rec := seedPreviewFile(t, cat, syncDir, "notes.txt", filetype.TypeText, []byte("hello\n"))

	w := get(t, srv, "/api/v1/files/"+itoa(rec.ID)+"/epub")

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415, body: %s", w.Code, w.Body.String())
	}
}

func TestEPUBSpineShouldRefuseWhenTheArchiveIsNotABook(t *testing.T) {
	// A plain zip renamed .epub. It opens as a zip and has no container.xml.
	srv, cat, syncDir := setupTestServer(t)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("readme.txt")
	w.Write([]byte("not a book"))
	zw.Close()

	rec := seedPreviewFile(t, cat, syncDir, "fake.epub", filetype.TypeEPUB, buf.Bytes())

	res := get(t, srv, "/api/v1/files/"+itoa(rec.ID)+"/epub")

	if res.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422, body: %s", res.Code, res.Body.String())
	}
}

func TestEPUBSpineShouldServeAnArchivedBookWithoutRestoringItWhenRequested(t *testing.T) {
	srv, cat, syncDir := setupTestServer(t)
	rec := seedEPUBFile(t, cat, syncDir, "cold.epub")

	body, err := os.ReadFile(filepath.Join(syncDir, "cold.epub"))
	if err != nil {
		t.Fatal(err)
	}
	coldPath := filepath.Join(srv.cfg.Storage.ColdStoragePath, "archive", "TestFolder", "cold.epub")
	if err := os.MkdirAll(filepath.Dir(coldPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(coldPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(syncDir, "cold.epub")); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateFileStatus(rec.ID, catalog.StatusArchived); err != nil {
		t.Fatal(err)
	}

	w := get(t, srv, "/api/v1/files/"+itoa(rec.ID)+"/epub")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	after, err := cat.GetFileByID(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != catalog.StatusArchived {
		t.Errorf("status = %q after a preview, want archived", after.Status)
	}
}

func TestEPUBSpineShouldAnswerNotFoundWhenTheFileIdDoesNotExist(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	w := get(t, srv, "/api/v1/files/99999/epub")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body: %s", w.Code, w.Body.String())
	}
}
