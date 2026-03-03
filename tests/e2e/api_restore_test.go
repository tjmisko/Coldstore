//go:build e2e

package e2e

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func TestAPIRestoreRoundTrip(t *testing.T) {
	env := newTestEnv(t)

	content := []byte("api restore round-trip")
	archiveDir := filepath.Join(env.coldDir, "archive", "TestFolder")
	os.MkdirAll(archiveDir, 0755)
	os.WriteFile(filepath.Join(archiveDir, "archived.txt"), content, 0644)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "archived.txt", Filename: "archived.txt",
		Extension: ".txt", SizeBytes: int64(len(content)), FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	})
	file, _ := env.cat.GetFile("TestFolder", "archived.txt")
	env.cat.UpdateFileStatus(file.ID, catalog.StatusArchived)

	req := httptest.NewRequest("POST", "/api/v1/files/"+strconv.FormatInt(file.ID, 10)+"/restore", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	// File should be in sync dir
	got, err := os.ReadFile(filepath.Join(env.syncDir, "archived.txt"))
	if err != nil {
		t.Fatal("file should be restored:", err)
	}
	if string(got) != string(content) {
		t.Error("content mismatch after API restore")
	}
}

func TestAPIRestore404(t *testing.T) {
	env := newTestEnv(t)

	req := httptest.NewRequest("POST", "/api/v1/files/99999/restore", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestAPIRestoreWrongStatus(t *testing.T) {
	env := newTestEnv(t)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "synced.txt", Filename: "synced.txt",
		Extension: ".txt", SizeBytes: 100, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	})
	file, _ := env.cat.GetFile("TestFolder", "synced.txt")

	req := httptest.NewRequest("POST", "/api/v1/files/"+strconv.FormatInt(file.ID, 10)+"/restore", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (cannot restore non-archived file)", w.Code)
	}
}
