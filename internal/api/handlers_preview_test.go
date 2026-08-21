package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/filetype"
)

// seedPreviewFile writes a file into the test folder and catalogues it with an
// explicit detected_type, the way the scanner's sniff pass would.
func seedPreviewFile(t *testing.T, cat *catalog.Catalog, syncDir, name, detected string, body []byte) *catalog.FileRecord {
	t.Helper()
	full := filepath.Join(syncDir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: name, Filename: filepath.Base(name),
		Extension: strings.ToLower(filepath.Ext(name)), SizeBytes: int64(len(body)),
		FileMtime: time.Now().UTC(), Status: catalog.StatusSynced,
	}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	rec, err := cat.GetFile("TestFolder", name)
	if err != nil {
		t.Fatal(err)
	}
	if detected != "" {
		if err := cat.SetDetectedType(rec.ID, detected); err != nil {
			t.Fatal(err)
		}
		rec, err = cat.GetFile("TestFolder", name)
		if err != nil {
			t.Fatal(err)
		}
	}
	return rec
}

func getPreview(t *testing.T, srv *Server, id int64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/files/"+itoa(id)+"/preview", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestPreviewShouldServeInlineWhenTheFileIsPreviewable(t *testing.T) {
	// The reason this route exists at all: the download route sets
	// Content-Disposition: attachment unconditionally, so pointing a reader at
	// it makes the browser save the file instead of showing it.
	srv, cat, syncDir := setupTestServer(t)
	rec := seedPreviewFile(t, cat, syncDir, "notes.txt", filetype.TypeText, []byte("hello\n"))

	w := getPreview(t, srv, rec.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
		t.Errorf("Content-Disposition = %q, want an inline disposition", got)
	}
}

func TestPreviewShouldStateTheContentTypeWhenTheFileHasNoExtension(t *testing.T) {
	// ServeFile guesses from the extension, and the files this route is most
	// for do not have one.
	srv, cat, syncDir := setupTestServer(t)
	rec := seedPreviewFile(t, cat, syncDir, "bookmarks", filetype.TypeText,
		[]byte("https://example.invalid/\n"))

	w := getPreview(t, srv, rec.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", got)
	}
}

func TestPreviewShouldStateTheContentTypeWhenTheFileIsAnEPUB(t *testing.T) {
	// .epub is not in Go's default MIME table, so ServeFile would answer
	// application/octet-stream and the browser would download it.
	srv, cat, syncDir := setupTestServer(t)
	rec := seedPreviewFile(t, cat, syncDir, "book.epub", filetype.TypeEPUB,
		[]byte("PK\x03\x04 not really, the type comes from the catalog"))

	w := getPreview(t, srv, rec.ID)

	if got := w.Header().Get("Content-Type"); got != "application/epub+zip" {
		t.Errorf("Content-Type = %q, want application/epub+zip", got)
	}
}

func TestPreviewShouldSetNosniffWhenServingAnyFile(t *testing.T) {
	// Stating the type is pointless if the browser is free to second-guess it:
	// sniffing is how a text file full of markup becomes a rendered HTML
	// document on the daemon's own origin.
	srv, cat, syncDir := setupTestServer(t)
	rec := seedPreviewFile(t, cat, syncDir, "notes.txt", filetype.TypeText,
		[]byte("<script>alert(1)</script>\n"))

	w := getPreview(t, srv, rec.ID)

	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestPreviewShouldCapTheBodyWhenTheTextFileIsHuge(t *testing.T) {
	// A 2 GB log must not stream the box to death to render a preview nobody
	// reads past the first screen.
	srv, cat, syncDir := setupTestServer(t)
	body := bytes.Repeat([]byte("a"), maxTextPreviewBytes+50_000)
	rec := seedPreviewFile(t, cat, syncDir, "huge.log", filetype.TypeText, body)

	w := getPreview(t, srv, rec.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body length %d", w.Code, w.Body.Len())
	}
	if w.Body.Len() != maxTextPreviewBytes {
		t.Errorf("body = %d bytes, want the %d-byte cap", w.Body.Len(), maxTextPreviewBytes)
	}
}

func TestPreviewShouldReportTruncationWhenTheTextFileIsCapped(t *testing.T) {
	srv, cat, syncDir := setupTestServer(t)
	body := bytes.Repeat([]byte("a"), maxTextPreviewBytes+1)
	rec := seedPreviewFile(t, cat, syncDir, "huge.log", filetype.TypeText, body)

	w := getPreview(t, srv, rec.ID)

	if got := w.Header().Get("X-Coldstore-Truncated"); got != "true" {
		t.Errorf("X-Coldstore-Truncated = %q, want true: a partial preview must say so", got)
	}
}

func TestPreviewShouldNotReportTruncationWhenTheTextFileFits(t *testing.T) {
	srv, cat, syncDir := setupTestServer(t)
	rec := seedPreviewFile(t, cat, syncDir, "small.txt", filetype.TypeText, []byte("short\n"))

	w := getPreview(t, srv, rec.ID)

	if got := w.Header().Get("X-Coldstore-Truncated"); got != "false" {
		t.Errorf("X-Coldstore-Truncated = %q, want false", got)
	}
	if w.Body.String() != "short\n" {
		t.Errorf("body = %q, want the whole file", w.Body.String())
	}
}

func TestPreviewShouldRefuseRangeRequestsWhenTheBodyIsCapped(t *testing.T) {
	// Accept-Ranges on a capped body would let a client read past the cap one
	// window at a time, which is the whole thing the cap exists to stop.
	srv, cat, syncDir := setupTestServer(t)
	body := bytes.Repeat([]byte("a"), maxTextPreviewBytes+10)
	rec := seedPreviewFile(t, cat, syncDir, "huge.log", filetype.TypeText, body)

	w := getPreview(t, srv, rec.ID)

	if got := w.Header().Get("Accept-Ranges"); got != "none" {
		t.Errorf("Accept-Ranges = %q, want none", got)
	}
}

func TestPreviewShouldRefuseWhenTheTypeHasNoInlineRendering(t *testing.T) {
	srv, cat, syncDir := setupTestServer(t)
	rec := seedPreviewFile(t, cat, syncDir, "firmware.bin", filetype.TypeUnknown,
		[]byte{0x00, 0x01, 0x02, 0x03})

	w := getPreview(t, srv, rec.ID)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415, body: %s", w.Code, w.Body.String())
	}
}

func TestPreviewShouldFallBackToTheExtensionWhenTheRowHasNotBeenSniffed(t *testing.T) {
	// An archived row catalogued before migration 6 has an empty
	// detected_type. Refusing to preview it would be a regression for every
	// file already in the catalog.
	srv, cat, syncDir := setupTestServer(t)
	rec := seedPreviewFile(t, cat, syncDir, "legacy.pdf", "", []byte("%PDF-1.4\n"))

	w := getPreview(t, srv, rec.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/pdf" {
		t.Errorf("Content-Type = %q, want application/pdf", got)
	}
}

func TestPreviewShouldSniffTheFileWhenNeitherTheRowNorTheNameSaysAnything(t *testing.T) {
	srv, cat, syncDir := setupTestServer(t)
	rec := seedPreviewFile(t, cat, syncDir, "mystery", "", []byte("%PDF-1.4\nbody\n"))

	w := getPreview(t, srv, rec.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/pdf" {
		t.Errorf("Content-Type = %q, want application/pdf", got)
	}
}

func TestPreviewShouldServeAnArchivedFileWithoutRestoringItWhenRequested(t *testing.T) {
	// The owner's explicit call: previewing a cold file streams it out of cold
	// storage and leaves it archived. Browsing the archive must not un-archive
	// it.
	srv, cat, syncDir := setupTestServer(t)
	rec := seedPreviewFile(t, cat, syncDir, "cold.txt", filetype.TypeText, []byte("cold bytes\n"))

	// Move the bytes to cold storage and mark the row archived, the way the
	// mover would.
	coldPath := filepath.Join(srv.cfg.Storage.ColdStoragePath, "archive", "TestFolder", "cold.txt")
	if err := os.MkdirAll(filepath.Dir(coldPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(coldPath, []byte("cold bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(syncDir, "cold.txt")); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateFileStatus(rec.ID, catalog.StatusArchived); err != nil {
		t.Fatal(err)
	}

	w := getPreview(t, srv, rec.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "cold bytes\n" {
		t.Errorf("body = %q, want the cold-storage bytes", w.Body.String())
	}

	after, err := cat.GetFileByID(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != catalog.StatusArchived {
		t.Errorf("status = %q after a preview, want archived: previewing must not restore", after.Status)
	}
}

func TestPreviewShouldAnswerNotFoundWhenTheFileIdDoesNotExist(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	w := getPreview(t, srv, 99999)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body: %s", w.Code, w.Body.String())
	}
}

func TestSanitizeFilenameShouldStripTheCharactersThatBreakAHeaderWhenPresent(t *testing.T) {
	// A Syncthing folder holds whatever names the owner's devices produced. A
	// header value with a CR in it is a response-splitting bug, not a cosmetic
	// one.
	tests := []struct{ in, want string }{
		{`holiday.jpg`, `holiday.jpg`},
		{"bad\r\nInjected: yes", "bad__Injected: yes"},
		{`quo"te.txt`, `quo_te.txt`},
		{`back\slash.txt`, `back_slash.txt`},
		{`../../etc/passwd`, `passwd`},
		{``, `download`},
	}
	for _, tt := range tests {
		if got := sanitizeFilename(tt.in); got != tt.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
