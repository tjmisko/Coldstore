package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func seedRenameAPITestFile(t *testing.T, cat *catalog.Catalog, syncDir, rel string) *catalog.FileRecord {
	t.Helper()
	full := filepath.Join(syncDir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: rel, Filename: filepath.Base(rel),
		Extension: filepath.Ext(rel), SizeBytes: info.Size(), FileMtime: info.ModTime(),
		FileAtime: info.ModTime(), Status: catalog.StatusSynced,
	}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	file, err := cat.GetFile("TestFolder", rel)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestRenameEndpointPreflightsAndEnqueuesDurablePayloadWithWarnings(t *testing.T) {
	srv, cat, syncDir := setupTestServer(t)
	seedRenameAPITestFile(t, cat, syncDir, "old/a.jpg")
	if err := cat.AddPersistentPath("TestFolder", "old/**"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/folders/TestFolder/rename",
		bytes.NewBufferString(`{"from":"old","to":"new"}`))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var response renameResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Action != catalog.JobActionRename || response.Total != 1 ||
		len(response.Warnings) != 1 || response.Warnings[0] != "old/**" {
		t.Fatalf("response = %+v", response)
	}
	job, _, err := cat.GetJob(response.JobID, 1)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := catalog.DecodeRenamePayload(job.Payload)
	if err != nil || payload.From != "old" || payload.To != "new" {
		t.Fatalf("payload = %+v, %v", payload, err)
	}
}

func TestRenameEndpointRejectsUnsafeAndOccupiedPathsWithoutAJob(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
		seed bool
	}{
		{name: "traversal", body: `{"from":"../old","to":"new"}`, want: http.StatusBadRequest},
		{name: "descendant", body: `{"from":"old","to":"old/new"}`, want: http.StatusBadRequest, seed: true},
		{name: "occupied", body: `{"from":"old","to":"new"}`, want: http.StatusConflict, seed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cat, syncDir := setupTestServer(t)
			if tc.seed {
				seedRenameAPITestFile(t, cat, syncDir, "old/a.jpg")
			}
			if tc.name == "occupied" {
				seedRenameAPITestFile(t, cat, syncDir, "new/b.jpg")
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/folders/TestFolder/rename",
				bytes.NewBufferString(tc.body))
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			jobs, err := cat.ListJobs(10)
			if err != nil {
				t.Fatal(err)
			}
			if len(jobs) != 0 {
				t.Fatalf("rejected request enqueued jobs: %+v", jobs)
			}
		})
	}
}

func TestUpdateFolderAcceptsCosmeticDisplayNameWithoutChangingStableIdentity(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/folders/TestFolder",
		bytes.NewBufferString(`{"display_name":"  Family Photos  "}`))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var folder catalog.FolderConfigRecord
	if err := json.NewDecoder(w.Body).Decode(&folder); err != nil {
		t.Fatal(err)
	}
	if folder.DisplayName != "Family Photos" || folder.FolderLabel != "TestFolder" {
		t.Fatalf("updated folder = %+v", folder)
	}
	stored, err := cat.FolderConfigByLabel("TestFolder")
	if err != nil {
		t.Fatal(err)
	}
	if stored.DisplayName != "Family Photos" || stored.SyncthingPath == "" {
		t.Fatalf("stored folder = %+v", stored)
	}
	status, err := srv.folderStatusForLabel("TestFolder")
	if err != nil {
		t.Fatal(err)
	}
	if status.DisplayName != "Family Photos" || status.Label != "TestFolder" {
		t.Fatalf("status folder = %+v", status)
	}
}

func TestUpdateFolderRejectsUnsafeOrOversizedDisplayNames(t *testing.T) {
	for _, name := range []string{"line\nbreak", "next\u0085line", strings.Repeat("x", 129)} {
		t.Run(name[:4], func(t *testing.T) {
			srv, cat, _ := setupTestServer(t)
			body, err := json.Marshal(map[string]string{"display_name": name})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPut, "/api/v1/folders/TestFolder", bytes.NewReader(body))
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			folder, err := cat.FolderConfigByLabel("TestFolder")
			if err != nil {
				t.Fatal(err)
			}
			if folder.DisplayName != "" {
				t.Fatalf("rejected display name persisted as %q", folder.DisplayName)
			}
		})
	}
}
