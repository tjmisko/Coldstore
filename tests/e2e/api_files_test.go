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

func TestAPIFilesPagination(t *testing.T) {
	env := newTestEnv(t)

	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		name := "file" + padInt(i, 2) + ".txt"
		env.cat.UpsertFile(&catalog.FileRecord{
			FolderLabel: "TestFolder", RelativePath: name, Filename: name,
			Extension: ".txt", SizeBytes: 100, FileMtime: now.Add(-time.Duration(i) * time.Hour),
			Status: catalog.StatusSynced,
		})
	}

	// First page
	req := httptest.NewRequest("GET", "/api/v1/files?limit=3&offset=0", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	var page1 []catalog.FileRecord
	json.NewDecoder(w.Body).Decode(&page1)
	if len(page1) != 3 {
		t.Errorf("page 1 len = %d, want 3", len(page1))
	}

	// Second page
	req = httptest.NewRequest("GET", "/api/v1/files?limit=3&offset=3", nil)
	w = httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	var page2 []catalog.FileRecord
	json.NewDecoder(w.Body).Decode(&page2)
	if len(page2) != 3 {
		t.Errorf("page 2 len = %d, want 3", len(page2))
	}

	// Pages should not overlap
	if page1[0].ID == page2[0].ID {
		t.Error("pages should not overlap")
	}
}

func TestAPIFilesSortByMtime(t *testing.T) {
	env := newTestEnv(t)

	now := time.Now().UTC()
	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "old.txt", Filename: "old.txt",
		Extension: ".txt", SizeBytes: 100, FileMtime: now.Add(-24 * time.Hour),
		Status: catalog.StatusSynced,
	})
	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "new.txt", Filename: "new.txt",
		Extension: ".txt", SizeBytes: 100, FileMtime: now,
		Status: catalog.StatusSynced,
	})

	req := httptest.NewRequest("GET", "/api/v1/files?sort=mtime", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	var files []catalog.FileRecord
	json.NewDecoder(w.Body).Decode(&files)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	// Default mtime sort is DESC (newest first)
	if files[0].Filename != "new.txt" {
		t.Errorf("first file = %s, want new.txt (newest first)", files[0].Filename)
	}
}

func TestAPIFilesSortBySize(t *testing.T) {
	env := newTestEnv(t)

	now := time.Now().UTC()
	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "small.txt", Filename: "small.txt",
		Extension: ".txt", SizeBytes: 100, FileMtime: now, Status: catalog.StatusSynced,
	})
	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "big.txt", Filename: "big.txt",
		Extension: ".txt", SizeBytes: 10000, FileMtime: now, Status: catalog.StatusSynced,
	})

	req := httptest.NewRequest("GET", "/api/v1/files?sort=size", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	var files []catalog.FileRecord
	json.NewDecoder(w.Body).Decode(&files)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	// Size sort is DESC (biggest first)
	if files[0].Filename != "big.txt" {
		t.Errorf("first file = %s, want big.txt (biggest first)", files[0].Filename)
	}
}

func TestAPIFilesSortByName(t *testing.T) {
	env := newTestEnv(t)

	now := time.Now().UTC()
	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "zebra.txt", Filename: "zebra.txt",
		Extension: ".txt", SizeBytes: 100, FileMtime: now, Status: catalog.StatusSynced,
	})
	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "apple.txt", Filename: "apple.txt",
		Extension: ".txt", SizeBytes: 100, FileMtime: now, Status: catalog.StatusSynced,
	})

	req := httptest.NewRequest("GET", "/api/v1/files?sort=name", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	var files []catalog.FileRecord
	json.NewDecoder(w.Body).Decode(&files)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if files[0].Filename != "apple.txt" {
		t.Errorf("first file = %s, want apple.txt (A-Z)", files[0].Filename)
	}
}

func TestAPIDownloadArchivedFile(t *testing.T) {
	env := newTestEnv(t)

	content := []byte("archived download content")
	archiveDir := filepath.Join(env.coldDir, "archive", "TestFolder")
	os.MkdirAll(archiveDir, 0755)
	os.WriteFile(filepath.Join(archiveDir, "cold.txt"), content, 0644)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "cold.txt", Filename: "cold.txt",
		Extension: ".txt", SizeBytes: int64(len(content)), FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	})
	file, _ := env.cat.GetFile("TestFolder", "cold.txt")
	env.cat.UpdateFileStatus(file.ID, catalog.StatusArchived)

	req := httptest.NewRequest("GET", "/api/v1/files/"+strconv.FormatInt(file.ID, 10)+"/download", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != string(content) {
		t.Errorf("body = %q, want %q", w.Body.String(), string(content))
	}
}

func TestAPIDownloadSyncedFile(t *testing.T) {
	env := newTestEnv(t)

	content := []byte("synced download")
	os.WriteFile(filepath.Join(env.syncDir, "local.txt"), content, 0644)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "local.txt", Filename: "local.txt",
		Extension: ".txt", SizeBytes: int64(len(content)), FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	})
	file, _ := env.cat.GetFile("TestFolder", "local.txt")

	req := httptest.NewRequest("GET", "/api/v1/files/"+strconv.FormatInt(file.ID, 10)+"/download", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestAPIDownload404(t *testing.T) {
	env := newTestEnv(t)

	req := httptest.NewRequest("GET", "/api/v1/files/99999/download", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestAPIDownload409Restoring(t *testing.T) {
	env := newTestEnv(t)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "restoring.txt", Filename: "restoring.txt",
		Extension: ".txt", SizeBytes: 100, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	})
	file, _ := env.cat.GetFile("TestFolder", "restoring.txt")
	env.cat.UpdateFileStatus(file.ID, catalog.StatusRestoring)

	req := httptest.NewRequest("GET", "/api/v1/files/"+strconv.FormatInt(file.ID, 10)+"/download", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestAPIThumbnailServed(t *testing.T) {
	env := newTestEnv(t)

	// Create a thumbnail file
	thumbDir := filepath.Join(env.coldDir, "thumbnails", "TestFolder")
	os.MkdirAll(thumbDir, 0755)
	os.WriteFile(filepath.Join(thumbDir, "photo.thumb.jpg"), []byte("thumb data"), 0644)

	req := httptest.NewRequest("GET", "/api/v1/thumbnails/TestFolder/photo.thumb.jpg", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestAPIThumbnail404(t *testing.T) {
	env := newTestEnv(t)

	req := httptest.NewRequest("GET", "/api/v1/thumbnails/TestFolder/nonexistent.thumb.jpg", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
