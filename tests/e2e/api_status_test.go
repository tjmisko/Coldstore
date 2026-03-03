//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func TestAPIStatusCountsAfterOffload(t *testing.T) {
	env := newTestEnv(t, withGracePeriod(0), withBudget("TestFolder", 3000))

	old := time.Now().Add(-2 * time.Hour)

	// Create 3 files, 2 will be offloaded (over 3000 budget)
	for i := 0; i < 3; i++ {
		name := "status" + padInt(i, 2) + ".txt"
		p := filepath.Join(env.syncDir, name)
		os.WriteFile(p, make([]byte, 2000), 0644)
		mtime := old.Add(-time.Duration(i) * time.Hour)
		os.Chtimes(p, mtime, mtime)

		env.cat.UpsertFile(&catalog.FileRecord{
			FolderLabel: "TestFolder", RelativePath: name, Filename: name,
			Extension: ".txt", SizeBytes: 2000, FileMtime: mtime,
			Status: catalog.StatusSynced,
		})
	}

	env.daemon.SetSpaceCheck(func(string) (float64, error) { return 50.0, nil })
	env.daemon.EnforceQuotas()

	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	archived := resp["archived_files"].(float64)
	if archived == 0 {
		t.Error("expected some archived files after offload")
	}

	total := resp["total_files"].(float64)
	if total != 3 {
		t.Errorf("total_files = %v, want 3", total)
	}
}

func TestAPIStatusFolderSyncedSize(t *testing.T) {
	env := newTestEnv(t)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "a.txt", Filename: "a.txt",
		Extension: ".txt", SizeBytes: 5000, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	})

	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	folders := resp["folders"].([]any)
	if len(folders) == 0 {
		t.Fatal("expected folder status")
	}
	folder := folders[0].(map[string]any)
	if folder["synced_size_bytes"].(float64) != 5000 {
		t.Errorf("synced_size = %v, want 5000", folder["synced_size_bytes"])
	}
}

func TestAPIAuthRequired(t *testing.T) {
	env := newTestEnv(t, withAuth("admin", "secret"))

	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (no auth provided)", w.Code)
	}
}

func TestAPIAuthSuccess(t *testing.T) {
	env := newTestEnv(t, withAuth("admin", "secret"))

	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	req.SetBasicAuth("admin", "secret")
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (correct auth)", w.Code)
	}
}

func TestAPIAuthWrongPassword(t *testing.T) {
	env := newTestEnv(t, withAuth("admin", "secret"))

	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	req.SetBasicAuth("admin", "wrong")
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (wrong password)", w.Code)
	}
}
