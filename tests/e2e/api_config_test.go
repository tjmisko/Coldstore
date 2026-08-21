//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/daemon"
)

// A fresh install declares its folders in coldstore.toml and nowhere else.
// GET /folders returning [] there is the bug that made the Settings panel
// unreachable: there was no way to get a folder into the policy table at all.
func TestAPIFoldersShouldListTheDeclaredFoldersWhenTheInstallIsFresh(t *testing.T) {
	env := newTestEnv(t)

	configs := getFolders(t, env)
	if len(configs) != 1 {
		t.Fatalf("GET /folders returned %d folders, want the 1 declared in coldstore.toml", len(configs))
	}
	fc := configs[0]
	if fc.FolderLabel != "TestFolder" {
		t.Errorf("folder_label = %q, want TestFolder", fc.FolderLabel)
	}
	if fc.SyncthingPath != env.syncDir {
		t.Errorf("syncthing_path = %q, want %q", fc.SyncthingPath, env.syncDir)
	}
	if fc.BudgetBytes != 50000 {
		t.Errorf("budget_bytes = %d, want the seeded 50000", fc.BudgetBytes)
	}
	if !fc.Enabled || !fc.PresentInTOML {
		t.Errorf("enabled = %v, present_in_toml = %v, want both true", fc.Enabled, fc.PresentInTOML)
	}
	if fc.LastEnforcedAt != nil {
		t.Errorf("last_enforced_at = %v, want null before the first tick", *fc.LastEnforcedAt)
	}
}

// The never-clobber rule, end to end and through the real handlers: TOML seeds
// a folder, the owner edits the budget in the UI, the daemon restarts and
// reconciles the same TOML again, and the edit is still there.
func TestAPIFolderBudgetShouldSurviveWhenTOMLIsReconciledAgain(t *testing.T) {
	env := newTestEnv(t)

	updated := putFolder(t, env, "TestFolder", `{"budget_bytes":777}`, http.StatusOK)
	if updated.BudgetBytes != 777 {
		t.Fatalf("PUT returned budget_bytes = %d, want 777", updated.BudgetBytes)
	}

	// A restart: same coldstore.toml, same seed budget of 50000.
	if err := daemon.ReconcileFolders(env.cfg, env.cat, env.logger); err != nil {
		t.Fatal(err)
	}

	after := getFolders(t, env)[0]
	if after.BudgetBytes != 777 {
		t.Errorf("budget_bytes = %d after re-reconciliation, want the user's 777: "+
			"coldstore.toml seeds a budget once and never again", after.BudgetBytes)
	}
	if after.SyncthingPath != env.syncDir {
		t.Errorf("syncthing_path = %q, want %q: infrastructure fields DO refresh from TOML",
			after.SyncthingPath, env.syncDir)
	}
}

func TestAPIFolderUpdateShouldRejectAnUnknownLabelWithoutCreatingIt(t *testing.T) {
	env := newTestEnv(t)

	putFolderRaw(t, env, "Bogus", `{"budget_bytes":1000}`, http.StatusNotFound)

	if got := len(getFolders(t, env)); got != 1 {
		t.Errorf("folder count = %d, want 1: only TOML reconciliation may create a folder row", got)
	}
}

func TestAPIFolderUpdateShouldLeaveInfrastructureFieldsAloneWhenTheyAreSent(t *testing.T) {
	env := newTestEnv(t)

	updated := putFolder(t, env,
		"TestFolder", `{"syncthing_id":"hijacked","syncthing_path":"/etc","budget_bytes":4242}`,
		http.StatusOK)

	if updated.SyncthingPath != env.syncDir {
		t.Errorf("syncthing_path = %q, want %q: the UI may not move a folder",
			updated.SyncthingPath, env.syncDir)
	}
	if updated.SyncthingID != "" {
		t.Errorf("syncthing_id = %q, want unchanged", updated.SyncthingID)
	}
	if updated.BudgetBytes != 4242 {
		t.Errorf("budget_bytes = %d, want 4242", updated.BudgetBytes)
	}
}

func TestAPIFolderUpdateShouldNotDisableTheFolderWhenOnlyTheBudgetIsSent(t *testing.T) {
	env := newTestEnv(t)

	updated := putFolder(t, env, "TestFolder", `{"budget_bytes":123456}`, http.StatusOK)

	if !updated.Enabled {
		t.Error("enabled = false after a budget-only PUT: omitted fields must be left alone")
	}
	if updated.GraceMinutes != 30 {
		t.Errorf("grace_minutes = %d, want the seeded 30", updated.GraceMinutes)
	}
}

func TestAPIFolderUpdateShouldRejectAnImpossiblePolicy(t *testing.T) {
	env := newTestEnv(t)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"zero budget", `{"budget_bytes":0}`, "budget_bytes must be positive"},
		{"negative budget", `{"budget_bytes":-1}`, "budget_bytes must be positive"},
		{"negative grace", `{"grace_minutes":-5}`, "grace_minutes cannot be negative"},
		{"malformed json", `{"budget_bytes":`, "invalid request body"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := putFolderRaw(t, env, "TestFolder", tc.body, http.StatusBadRequest)
			var errResp struct {
				Error string `json:"error"`
			}
			json.Unmarshal(body, &errResp)
			if errResp.Error != tc.want {
				t.Errorf("error = %q, want %q", errResp.Error, tc.want)
			}
		})
	}

	// Nothing was written by any of the rejected requests.
	fc := getFolders(t, env)[0]
	if fc.BudgetBytes != 50000 || fc.GraceMinutes != 30 {
		t.Errorf("policy = (%d, %d), want the untouched (50000, 30)", fc.BudgetBytes, fc.GraceMinutes)
	}
}

func getFolders(t *testing.T, env *testEnv) []catalog.FolderConfigRecord {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/folders", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /folders status = %d, want 200", w.Code)
	}
	var configs []catalog.FolderConfigRecord
	if err := json.Unmarshal(w.Body.Bytes(), &configs); err != nil {
		t.Fatalf("decoding folders: %v (body %s)", err, w.Body.String())
	}
	return configs
}

func putFolderRaw(t *testing.T, env *testEnv, label, body string, wantStatus int) []byte {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/v1/folders/"+label, bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("PUT /folders/%s status = %d, want %d (body %s)", label, w.Code, wantStatus, w.Body.String())
	}
	return w.Body.Bytes()
}

func putFolder(t *testing.T, env *testEnv, label, body string, wantStatus int) catalog.FolderConfigRecord {
	t.Helper()
	var fc catalog.FolderConfigRecord
	if err := json.Unmarshal(putFolderRaw(t, env, label, body, wantStatus), &fc); err != nil {
		t.Fatalf("decoding folder: %v", err)
	}
	return fc
}

func TestAPIPersistentPathCRUD(t *testing.T) {
	env := newTestEnv(t)

	// Add
	body := `{"pattern":"Favorites/*"}`
	req := httptest.NewRequest("POST", "/api/v1/folders/TestFolder/persistent", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("POST status = %d, want 201", w.Code)
	}

	// List
	req = httptest.NewRequest("GET", "/api/v1/folders/TestFolder/persistent", nil)
	w = httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	var paths []catalog.PersistentPath
	json.NewDecoder(w.Body).Decode(&paths)
	if len(paths) != 1 {
		t.Fatalf("expected 1 persistent path, got %d", len(paths))
	}
	if paths[0].Pattern != "Favorites/*" {
		t.Errorf("pattern = %q, want Favorites/*", paths[0].Pattern)
	}

	// Delete
	req = httptest.NewRequest("DELETE", "/api/v1/folders/TestFolder/persistent/"+strconv.FormatInt(paths[0].ID, 10), nil)
	w = httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("DELETE status = %d, want 200", w.Code)
	}

	// Verify deleted
	req = httptest.NewRequest("GET", "/api/v1/folders/TestFolder/persistent", nil)
	w = httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	json.NewDecoder(w.Body).Decode(&paths)
	if len(paths) != 0 {
		t.Errorf("expected 0 paths after delete, got %d", len(paths))
	}
}

func TestAPIPersistentPathEmptyPattern(t *testing.T) {
	env := newTestEnv(t)

	body := `{"pattern":""}`
	req := httptest.NewRequest("POST", "/api/v1/folders/TestFolder/persistent", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (empty pattern)", w.Code)
	}
}

// The persistent-path collection was the one door left open through which a
// phantom folder could still be conjured: POST to a label with no
// folder_configs row returned 201 and created a glob for a folder the daemon
// would never enforce. PUT /folders/{label} was tightened for exactly this.
func TestPersistentPathsShouldRefuseAFolderThatWasNeverDeclaredInTOML(t *testing.T) {
	env := newTestEnv(t)

	w := do(t, env, "POST", "/api/v1/folders/NoSuchFolder/persistent",
		map[string]string{"pattern": "Foo/*"})
	if w.Code != http.StatusNotFound {
		t.Errorf("POST status = %d, want 404, body: %s", w.Code, w.Body.String())
	}

	w = do(t, env, "GET", "/api/v1/folders/NoSuchFolder/persistent", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("GET status = %d, want 404", w.Code)
	}

	w = do(t, env, "DELETE", "/api/v1/folders/NoSuchFolder/persistent/1", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("DELETE status = %d, want 404", w.Code)
	}
}

// A pattern that protects nothing looks identical to one that protects
// everything. match_count is what tells the owner which they just typed.
func TestPersistentPathsShouldReportHowManyFilesEachPatternProtects(t *testing.T) {
	env := newTestEnv(t)

	seedAgedFile(t, env, "Starred/keep1.jpg", 100)
	seedAgedFile(t, env, "Starred/keep2.jpg", 100)
	seedAgedFile(t, env, "Ordinary/other.jpg", 100)

	for _, pattern := range []string{"Starred/*", "NothingHere/*"} {
		w := do(t, env, "POST", "/api/v1/folders/TestFolder/persistent",
			map[string]string{"pattern": pattern})
		if w.Code != http.StatusCreated {
			t.Fatalf("POST %q status = %d", pattern, w.Code)
		}
	}

	w := do(t, env, "GET", "/api/v1/folders/TestFolder/persistent", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d", w.Code)
	}
	var paths []catalog.PersistentPath
	if err := json.NewDecoder(w.Body).Decode(&paths); err != nil {
		t.Fatal(err)
	}

	counts := map[string]int64{}
	for _, p := range paths {
		counts[p.Pattern] = p.MatchCount
	}
	if counts["Starred/*"] != 2 {
		t.Errorf("match_count for Starred/* = %d, want 2", counts["Starred/*"])
	}
	if counts["NothingHere/*"] != 0 {
		t.Errorf("match_count for NothingHere/* = %d, want 0", counts["NothingHere/*"])
	}
}
