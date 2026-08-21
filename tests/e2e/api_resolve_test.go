//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tjmisko/coldstore/internal/catalog"
)

// reoccupyArchivedPath puts a file back at the syncthing path of an archived
// row -- a photo recovered from the phone's Recently Deleted, a laptop
// reconnecting with an offline edit, a Receive Only folder's "Revert Local
// Changes" -- and rescans, which is how the conflict state is really reached.
func reoccupyArchivedPath(t *testing.T, env *testEnv, relPath, body string) *catalog.FileRecord {
	t.Helper()
	file := seedAgedFile(t, env, relPath, 1000)
	env.archive(t, file.ID)

	if err := os.WriteFile(filepath.Join(env.syncDir, relPath), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := env.scanner.ScanAll(); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	rec, err := env.cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != catalog.StatusArchived {
		t.Fatalf("status = %q after the rescan, want archived -- the row must not adopt the path", rec.Status)
	}
	return rec
}

func resolve(t *testing.T, env *testEnv, id int64, keep string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, env, "POST",
		"/api/v1/files/"+strconv.FormatInt(id, 10)+"/resolve", map[string]string{"keep": keep})
}

func TestResolveShouldAdoptTheDeviceCopyAndDeleteTheColdOneWhenTheOwnerKeepsTheDevice(t *testing.T) {
	env := newTestEnv(t)
	const body = "the copy the phone brought back"
	rec := reoccupyArchivedPath(t, env, "photo.jpg", body)

	w := resolve(t, env, rec.ID, "device")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	var got catalog.FileRecord
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", got.Status)
	}
	if got.SizeBytes != int64(len(body)) {
		t.Errorf("size_bytes = %d, want the on-disk %d", got.SizeBytes, len(body))
	}
	coldPath := filepath.Join(env.coldDir, "archive", "TestFolder", "photo.jpg")
	if _, err := os.Lstat(coldPath); !os.IsNotExist(err) {
		t.Errorf("the cold copy survived a keep=device resolution: %v", err)
	}
	onDisk, _ := os.ReadFile(filepath.Join(env.syncDir, "photo.jpg"))
	if string(onDisk) != body {
		t.Errorf("device file = %q, want %q", onDisk, body)
	}
}

func TestResolveShouldSetTheDeviceFileAsideWhenTheOwnerKeepsTheArchive(t *testing.T) {
	env := newTestEnv(t)
	rec := reoccupyArchivedPath(t, env, "photo.jpg", "the copy the phone brought back")

	w := resolve(t, env, rec.ID, "archive")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	var got catalog.FileRecord
	json.NewDecoder(w.Body).Decode(&got)
	if got.Status != catalog.StatusArchived {
		t.Errorf("status = %q, want archived", got.Status)
	}
	if _, err := os.Lstat(filepath.Join(env.syncDir, "photo.jpg")); !os.IsNotExist(err) {
		t.Error("the device file is still at the original name; it must be set aside")
	}

	entries, err := os.ReadDir(env.syncDir)
	if err != nil {
		t.Fatal(err)
	}
	var aside bool
	for _, e := range entries {
		if strings.Contains(e.Name(), ".coldstore-superseded") {
			aside = true
		}
	}
	if !aside {
		t.Errorf("nothing was set aside; folder holds %v", entries)
	}
	coldPath := filepath.Join(env.coldDir, "archive", "TestFolder", "photo.jpg")
	if _, err := os.Lstat(coldPath); err != nil {
		t.Errorf("the archived copy must be untouched: %v", err)
	}
}

func TestResolveShouldRefuseAnUnknownKeepValueAndANonConflictedRow(t *testing.T) {
	env := newTestEnv(t)

	rec := reoccupyArchivedPath(t, env, "photo.jpg", "brought back")
	if w := resolve(t, env, rec.ID, "whichever"); w.Code != http.StatusBadRequest {
		t.Errorf("keep=whichever status = %d, want 400", w.Code)
	}

	synced := seedAgedFile(t, env, "other.jpg", 500)
	if w := resolve(t, env, synced.ID, "device"); w.Code != http.StatusConflict {
		t.Errorf("resolving a synced row = %d, want 409", w.Code)
	}

	w := do(t, env, "POST", "/api/v1/files/999999/resolve", map[string]string{"keep": "device"})
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown id status = %d, want 404", w.Code)
	}
}

// An archived row with nothing at the syncthing path is not a conflict, and
// resolving it would leave the row pointing at a file that is not there.
func TestResolveShouldRefuseWhenNothingOccupiesTheSyncthingPath(t *testing.T) {
	env := newTestEnv(t)
	file := seedAgedFile(t, env, "photo.jpg", 1000)
	env.archive(t, file.ID)

	if w := resolve(t, env, file.ID, "device"); w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}
