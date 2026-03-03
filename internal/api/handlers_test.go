package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/daemon"
)

func setupTestServer(t *testing.T) (*Server, *catalog.Catalog, string) {
	t.Helper()

	syncDir := filepath.Join(t.TempDir(), "sync")
	coldDir := filepath.Join(t.TempDir(), "cold")
	os.MkdirAll(syncDir, 0755)
	os.MkdirAll(filepath.Join(coldDir, "archive"), 0755)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })

	cfg := &config.Config{
		Storage: config.StorageConfig{ColdStoragePath: coldDir},
		Folders: []config.FolderConfig{
			{Label: "TestFolder", Path: syncDir, BudgetBytes: 50000},
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	restorer := daemon.NewRestorer(cfg, cat, logger)
	scanner := daemon.NewScanner(cfg, cat, logger)
	srv := NewServer(cfg, cat, restorer, scanner, logger)

	return srv, cat, syncDir
}

func TestHandleListFilesEmpty(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/files", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var files []catalog.FileRecord
	json.NewDecoder(w.Body).Decode(&files)
	if len(files) != 0 {
		t.Errorf("expected empty list, got %d files", len(files))
	}
}

func TestHandleListFilesWithData(t *testing.T) {
	srv, cat, _ := setupTestServer(t)

	cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "a.jpg", Filename: "a.jpg",
		Extension: ".jpg", SizeBytes: 100, FileMtime: time.Now().UTC(), Status: catalog.StatusSynced,
	})
	cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "b.pdf", Filename: "b.pdf",
		Extension: ".pdf", SizeBytes: 200, FileMtime: time.Now().UTC(), Status: catalog.StatusArchived,
	})

	// List all
	req := httptest.NewRequest("GET", "/api/v1/files", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var files []catalog.FileRecord
	json.NewDecoder(w.Body).Decode(&files)
	if len(files) != 2 {
		t.Errorf("expected 2 files, got %d", len(files))
	}

	// Filter by status
	req = httptest.NewRequest("GET", "/api/v1/files?status=synced", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	json.NewDecoder(w.Body).Decode(&files)
	if len(files) != 1 {
		t.Errorf("expected 1 synced file, got %d", len(files))
	}

	// Filter by folder
	req = httptest.NewRequest("GET", "/api/v1/files?folder=TestFolder&ext=.pdf", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	json.NewDecoder(w.Body).Decode(&files)
	if len(files) != 1 || files[0].Filename != "b.pdf" {
		t.Errorf("expected b.pdf, got %v", files)
	}
}

func TestHandleDownloadSyncedFile(t *testing.T) {
	srv, cat, syncDir := setupTestServer(t)

	// Create a real file
	os.WriteFile(filepath.Join(syncDir, "test.txt"), []byte("hello world"), 0644)

	cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "test.txt", Filename: "test.txt",
		Extension: ".txt", SizeBytes: 11, FileMtime: time.Now().UTC(), Status: catalog.StatusSynced,
	})

	file, _ := cat.GetFile("TestFolder", "test.txt")

	req := httptest.NewRequest("GET", "/api/v1/files/"+itoa(file.ID)+"/download", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "hello world" {
		t.Errorf("body = %q, want 'hello world'", w.Body.String())
	}
}

func TestHandleStatus(t *testing.T) {
	srv, cat, _ := setupTestServer(t)

	cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "a.jpg", Filename: "a.jpg",
		Extension: ".jpg", SizeBytes: 1000, FileMtime: time.Now().UTC(), Status: catalog.StatusSynced,
	})

	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["total_files"].(float64) != 1 {
		t.Errorf("total_files = %v, want 1", resp["total_files"])
	}
	if resp["synced_files"].(float64) != 1 {
		t.Errorf("synced_files = %v, want 1", resp["synced_files"])
	}
}

func TestHandleFolderConfig(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	// List folders (empty)
	req := httptest.NewRequest("GET", "/api/v1/folders", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var configs []catalog.FolderConfigRecord
	json.NewDecoder(w.Body).Decode(&configs)
	if len(configs) != 0 {
		t.Errorf("expected 0 configs, got %d", len(configs))
	}

	// Create a folder config
	body := `{"syncthing_id":"abc","syncthing_path":"/tmp/test","budget_bytes":10000,"grace_minutes":30,"enabled":true}`
	req = httptest.NewRequest("PUT", "/api/v1/folders/Photos", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("PUT status = %d, want 200", w.Code)
	}

	// List again
	req = httptest.NewRequest("GET", "/api/v1/folders", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	json.NewDecoder(w.Body).Decode(&configs)
	if len(configs) != 1 || configs[0].FolderLabel != "Photos" {
		t.Errorf("expected 1 config for Photos, got %v", configs)
	}
}

func TestHandlePersistentPaths(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	// Add persistent path
	body := `{"pattern":"Starred/*"}`
	req := httptest.NewRequest("POST", "/api/v1/folders/Photos/persistent", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("POST status = %d, want 201", w.Code)
	}

	// List
	req = httptest.NewRequest("GET", "/api/v1/folders/Photos/persistent", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var paths []catalog.PersistentPath
	json.NewDecoder(w.Body).Decode(&paths)
	if len(paths) != 1 || paths[0].Pattern != "Starred/*" {
		t.Errorf("expected [Starred/*], got %v", paths)
	}

	// Delete
	req = httptest.NewRequest("DELETE", "/api/v1/folders/Photos/persistent/"+itoa(paths[0].ID), nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("DELETE status = %d, want 200", w.Code)
	}

	// Verify deleted
	req = httptest.NewRequest("GET", "/api/v1/folders/Photos/persistent", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	json.NewDecoder(w.Body).Decode(&paths)
	if len(paths) != 0 {
		t.Errorf("expected 0 paths after delete, got %d", len(paths))
	}
}

func TestHandleRestoreFile(t *testing.T) {
	srv, cat, syncDir := setupTestServer(t)

	// Set up an archived file in cold storage
	coldArchive := filepath.Join(srv.cfg.Storage.ColdStoragePath, "archive", "TestFolder")
	os.MkdirAll(coldArchive, 0755)
	os.WriteFile(filepath.Join(coldArchive, "old.txt"), []byte("archived content"), 0644)

	cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "old.txt", Filename: "old.txt",
		Extension: ".txt", SizeBytes: 16, FileMtime: time.Now().UTC(), Status: catalog.StatusSynced,
	})
	file, _ := cat.GetFile("TestFolder", "old.txt")
	cat.UpdateFileStatus(file.ID, catalog.StatusArchived)

	req := httptest.NewRequest("POST", "/api/v1/files/"+itoa(file.ID)+"/restore", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	// File should be back in sync dir
	content, err := os.ReadFile(filepath.Join(syncDir, "old.txt"))
	if err != nil {
		t.Fatal("restored file should exist:", err)
	}
	if string(content) != "archived content" {
		t.Errorf("content = %q, want 'archived content'", string(content))
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
