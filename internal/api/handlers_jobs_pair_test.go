package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func seedArchivedAPITestFile(t *testing.T, srv *Server, cat *catalog.Catalog,
	syncDir, rel string, size int) *catalog.FileRecord {
	t.Helper()
	source := filepath.Join(syncDir, rel)
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: rel, Filename: filepath.Base(rel),
		Extension: filepath.Ext(rel), SizeBytes: int64(size), FileMtime: info.ModTime(),
		FileAtime: info.ModTime(), Status: catalog.StatusSynced,
	}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	file, err := cat.GetFile("TestFolder", rel)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(srv.cfg.Storage.ColdStoragePath, "archive", "TestFolder", rel)
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, destination); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateFileStatus(file.ID, catalog.StatusArchived); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestRestoreJobPreflightAndTotalIncludeAnUnselectedPairMember(t *testing.T) {
	srv, cat, syncDir := setupTestServer(t)
	still := seedArchivedAPITestFile(t, srv, cat, syncDir, "IMG_1.heic", 80)
	movie := seedArchivedAPITestFile(t, srv, cat, syncDir, "IMG_1.mov", 120)
	if err := cat.SetPairAssignments([]catalog.PairAssignment{
		{ID: still.ID, Key: "cid:api", Source: "content_id"},
		{ID: movie.ID, Key: "cid:api", Source: "content_id"},
	}); err != nil {
		t.Fatal(err)
	}
	budget := int64(100)
	if found, err := cat.UpdateFolderPolicy("TestFolder", &budget, nil, nil); err != nil || !found {
		t.Fatalf("setting budget: found %v err %v", found, err)
	}

	body := []byte(`{"action":"restore","ids":[` + itoa(still.ID) + `]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("preflight status = %d, want 409: %s", w.Code, w.Body.String())
	}
	var conflict budgetPreflightError
	if err := json.NewDecoder(w.Body).Decode(&conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.OverageBytes != 100 {
		t.Errorf("overage = %d, want both pair members (200) against budget 100", conflict.OverageBytes)
	}

	forceBody := []byte(`{"action":"restore","ids":[` + itoa(still.ID) + `],"force":true}`)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(forceBody))
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("forced restore status = %d, want 202: %s", w.Code, w.Body.String())
	}
	var created jobCreated
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Total != 2 {
		t.Errorf("expanded job total = %d, want 2", created.Total)
	}
}
