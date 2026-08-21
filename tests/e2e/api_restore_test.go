//go:build e2e

package e2e

import (
	"encoding/json"
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
	}, time.Now().UnixNano())
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

// The old contract answered this with a 500 and a raw Go error string. A file
// that is already where the owner asked for it is not a server failure: it is
// the outcome they wanted (invariant 12, design section 6.6).
func TestAPIRestoreShouldReturnTheRecordWhenTheFileIsAlreadySynced(t *testing.T) {
	env := newTestEnv(t)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "synced.txt", Filename: "synced.txt",
		Extension: ".txt", SizeBytes: 100, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := env.cat.GetFile("TestFolder", "synced.txt")

	req := httptest.NewRequest("POST", "/api/v1/files/"+strconv.FormatInt(file.ID, 10)+"/restore", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	var got catalog.FileRecord
	json.NewDecoder(w.Body).Decode(&got)
	if got.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", got.Status)
	}
}

func TestAPIRestoreShouldReturn409WhenTheFileIsMidMove(t *testing.T) {
	env := newTestEnv(t)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "moving.txt", Filename: "moving.txt",
		Extension: ".txt", SizeBytes: 100, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := env.cat.GetFile("TestFolder", "moving.txt")
	env.cat.UpdateFileStatus(file.ID, catalog.StatusArchiving)

	req := httptest.NewRequest("POST", "/api/v1/files/"+strconv.FormatInt(file.ID, 10)+"/restore", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (a transient state is the caller's problem, not a 500)", w.Code)
	}
}
