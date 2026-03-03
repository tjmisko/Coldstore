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
)

func TestAPIFolderCRUD(t *testing.T) {
	env := newTestEnv(t)

	// List (empty)
	req := httptest.NewRequest("GET", "/api/v1/folders", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	var configs []catalog.FolderConfigRecord
	json.NewDecoder(w.Body).Decode(&configs)
	if len(configs) != 0 {
		t.Errorf("expected 0 folder configs, got %d", len(configs))
	}

	// Create
	body := `{"syncthing_id":"abc-123","syncthing_path":"/mnt/photos","budget_bytes":20000000000,"grace_minutes":60,"enabled":true}`
	req = httptest.NewRequest("PUT", "/api/v1/folders/Photos", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("PUT status = %d, want 200", w.Code)
	}

	// Verify created
	req = httptest.NewRequest("GET", "/api/v1/folders", nil)
	w = httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	json.NewDecoder(w.Body).Decode(&configs)
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	if configs[0].FolderLabel != "Photos" {
		t.Errorf("label = %q, want Photos", configs[0].FolderLabel)
	}
	if configs[0].BudgetBytes != 20000000000 {
		t.Errorf("budget = %d, want 20000000000", configs[0].BudgetBytes)
	}
}

func TestAPIFolderUpdateOverwrites(t *testing.T) {
	env := newTestEnv(t)

	// Create initial
	body := `{"syncthing_id":"v1","syncthing_path":"/old","budget_bytes":1000,"grace_minutes":10,"enabled":true}`
	req := httptest.NewRequest("PUT", "/api/v1/folders/Data", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	// Update
	body = `{"syncthing_id":"v2","syncthing_path":"/new","budget_bytes":9999,"grace_minutes":99,"enabled":false}`
	req = httptest.NewRequest("PUT", "/api/v1/folders/Data", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	// Verify updated
	req = httptest.NewRequest("GET", "/api/v1/folders", nil)
	w = httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	var configs []catalog.FolderConfigRecord
	json.NewDecoder(w.Body).Decode(&configs)
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	if configs[0].SyncthingID != "v2" {
		t.Errorf("syncthing_id = %q, want v2", configs[0].SyncthingID)
	}
	if configs[0].BudgetBytes != 9999 {
		t.Errorf("budget = %d, want 9999", configs[0].BudgetBytes)
	}
}

func TestAPIPersistentPathCRUD(t *testing.T) {
	env := newTestEnv(t)

	// Add
	body := `{"pattern":"Favorites/*"}`
	req := httptest.NewRequest("POST", "/api/v1/folders/Photos/persistent", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("POST status = %d, want 201", w.Code)
	}

	// List
	req = httptest.NewRequest("GET", "/api/v1/folders/Photos/persistent", nil)
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
	req = httptest.NewRequest("DELETE", "/api/v1/folders/Photos/persistent/"+strconv.FormatInt(paths[0].ID, 10), nil)
	w = httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("DELETE status = %d, want 200", w.Code)
	}

	// Verify deleted
	req = httptest.NewRequest("GET", "/api/v1/folders/Photos/persistent", nil)
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
	req := httptest.NewRequest("POST", "/api/v1/folders/Photos/persistent", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (empty pattern)", w.Code)
	}
}
