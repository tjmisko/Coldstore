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
		}, time.Now().UnixNano())
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

func TestAPIStatusShouldDescribeTheFolderFromTheDatabaseWhenAskedForStatus(t *testing.T) {
	env := newTestEnv(t)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "a.txt", Filename: "a.txt",
		Extension: ".txt", SizeBytes: 5000, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())

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

	// The on-device footprint, and the policy it is measured against, both
	// come from folder_configs rather than from coldstore.toml.
	if folder["synced_size_bytes"].(float64) != 5000 {
		t.Errorf("synced_size_bytes = %v, want 5000", folder["synced_size_bytes"])
	}
	if folder["budget_bytes"].(float64) != 50000 {
		t.Errorf("budget_bytes = %v, want the seeded 50000", folder["budget_bytes"])
	}
	if folder["grace_minutes"].(float64) != 30 {
		t.Errorf("grace_minutes = %v, want the seeded 30", folder["grace_minutes"])
	}
	if folder["enabled"] != true || folder["present_in_toml"] != true {
		t.Errorf("enabled = %v, present_in_toml = %v, want both true",
			folder["enabled"], folder["present_in_toml"])
	}
	if folder["last_enforced_at"] != nil {
		t.Errorf("last_enforced_at = %v, want null before the first tick", folder["last_enforced_at"])
	}
	if folder["over_budget"] != false || folder["wedged"] != false {
		t.Errorf("over_budget = %v, wedged = %v, want both false at 5000 of 50000",
			folder["over_budget"], folder["wedged"])
	}
	// pinned_bytes is a subset of the footprint, so an unpinned folder is 0.
	if folder["pinned_bytes"].(float64) != 0 {
		t.Errorf("pinned_bytes = %v, want 0", folder["pinned_bytes"])
	}
	if _, ok := folder["syncthing_type"]; !ok {
		t.Error("syncthing_type must always be present, even when unverified")
	}
	counts := folder["counts"].(map[string]any)
	if counts["total"].(float64) != 1 || counts["synced"].(float64) != 1 {
		t.Errorf("counts = %v, want one synced file", counts)
	}
}

func TestAPIStatusShouldListADisabledFolderWhenItIsSwitchedOff(t *testing.T) {
	env := newTestEnv(t)

	disabled := false
	setPolicy(t, env, "TestFolder", nil, nil, &disabled)

	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	folders := resp["folders"].([]any)
	if len(folders) != 1 {
		t.Fatalf("folders = %v, want the disabled folder listed and flagged", folders)
	}
	if folders[0].(map[string]any)["enabled"] != false {
		t.Error("enabled = true, want false: a disabled folder is flagged, not hidden")
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
