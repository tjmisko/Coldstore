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

// These mirror the API contract's wire shapes (design section 6.13). They are
// declared here rather than reused from internal/api on purpose: the point of
// an e2e assertion is that the JSON the frontend will parse says what the
// contract promises, not that two Go structs agree with each other.
type folderStatusJSON struct {
	Label           string               `json:"label"`
	BudgetBytes     int64                `json:"budget_bytes"`
	SyncedSizeBytes int64                `json:"synced_size_bytes"`
	PinnedBytes     int64                `json:"pinned_bytes"`
	OverBudget      bool                 `json:"over_budget"`
	Wedged          bool                 `json:"wedged"`
	GraceMinutes    int                  `json:"grace_minutes"`
	Enabled         bool                 `json:"enabled"`
	PresentInTOML   bool                 `json:"present_in_toml"`
	LastEnforcedAt  *int64               `json:"last_enforced_at"`
	SyncthingType   string               `json:"syncthing_type"`
	Counts          catalog.FolderCounts `json:"counts"`
}

type pinResponseJSON struct {
	File    catalog.FileRecord `json:"file"`
	Folder  folderStatusJSON   `json:"folder"`
	Warning *string            `json:"warning"`
}

type bulkPinResponseJSON struct {
	Updated int64              `json:"updated"`
	Folders []folderStatusJSON `json:"folders"`
	Warning *string            `json:"warning"`
}

type statusResponseJSON struct {
	TotalFiles int64              `json:"total_files"`
	Folders    []folderStatusJSON `json:"folders"`
}

func do(t *testing.T, env *testEnv, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *httptest.ResponseRecorder = httptest.NewRecorder()
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req = httptest.NewRequest(method, path, bytes.NewReader(encoded))
	}
	env.server.Handler().ServeHTTP(r, req)
	return r
}

// pin presses the pin toggle the way the UI does, and fails the test if the
// API refuses.
func pin(t *testing.T, env *testEnv, id int64) pinResponseJSON {
	t.Helper()
	w := do(t, env, "POST", "/api/v1/files/"+strconv.FormatInt(id, 10)+"/pin", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("pin status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	var resp pinResponseJSON
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func statusFolder(t *testing.T, env *testEnv, label string) folderStatusJSON {
	t.Helper()
	w := do(t, env, "GET", "/api/v1/status", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	var resp statusResponseJSON
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	for _, f := range resp.Folders {
		if f.Label == label {
			return f
		}
	}
	t.Fatalf("no folder %q in /status", label)
	return folderStatusJSON{}
}

func TestAPIPinShouldMarkTheFileAndReturnTheFolderWhenTheOwnerPinsIt(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 10000))
	file := seedAgedFile(t, env, "keep.jpg", 400)

	resp := pin(t, env, file.ID)

	if !resp.File.Pinned {
		t.Error("the response must report the file as pinned")
	}
	if resp.File.PinSource != catalog.PinSourceUser {
		t.Errorf("pin_source = %q, want %q", resp.File.PinSource, catalog.PinSourceUser)
	}
	if resp.File.PinnedAt == nil {
		t.Error("pinned_at should be set")
	}
	// The folder block exists so the UI can redraw the budget bar without a
	// second round trip.
	if resp.Folder.Label != "TestFolder" || resp.Folder.PinnedBytes != 400 {
		t.Errorf("folder = %+v, want TestFolder with 400 pinned bytes", resp.Folder)
	}
	if resp.Warning != nil {
		t.Errorf("warning = %q, want null well inside the budget", *resp.Warning)
	}

	stored := reload(t, env, file.ID)
	if !stored.Pinned {
		t.Error("the pin should be in the database, not only in the response")
	}
}

func TestAPIPinShouldMoveNothingWhenTheFileIsPinnedInPlace(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 10000))
	file := seedAgedFile(t, env, "still-here.jpg", 400)

	pin(t, env, file.ID)

	after := reload(t, env, file.ID)
	if after.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced: pinning is not a move", after.Status)
	}
}

func TestAPIUnpinShouldClearTheAttributionAndWarnAboutNothing(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 100))
	file := seedAgedFile(t, env, "big.mp4", 900)
	pin(t, env, file.ID)

	w := do(t, env, "DELETE", "/api/v1/files/"+strconv.FormatInt(file.ID, 10)+"/pin", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("unpin status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	var resp pinResponseJSON
	json.NewDecoder(w.Body).Decode(&resp)

	if resp.File.Pinned {
		t.Error("the file should be unpinned")
	}
	if resp.File.PinnedAt != nil || resp.File.PinSource != catalog.PinSourceNone {
		t.Errorf("pinned_at = %v, pin_source = %q, want both cleared",
			resp.File.PinnedAt, resp.File.PinSource)
	}
	// DELETE always returns a null warning, even for a folder that is still
	// over budget: nothing the owner just did made anything worse.
	if resp.Warning != nil {
		t.Errorf("warning = %q, want null on unpin", *resp.Warning)
	}
}

// The warning is a STATE test, not a transition test. Pinning one more photo
// into an already-wedged folder must keep saying so, or the second pin looks
// routine and the owner never learns what they have done.
func TestAPIPinShouldWarnEveryTimeWhenTheFolderIsAlreadyWedged(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1000))
	first := seedAgedFile(t, env, "one.mp4", 900)
	second := seedAgedFile(t, env, "two.mp4", 900)

	firstResp := pin(t, env, first.ID)
	if firstResp.Warning != nil {
		t.Errorf("warning = %q, want null at 900 pinned against a 1000 budget", *firstResp.Warning)
	}

	secondResp := pin(t, env, second.ID)
	if secondResp.Warning == nil {
		t.Fatal("pinning past the budget must warn")
	}
	if !secondResp.Folder.Wedged {
		t.Error("the folder block should report wedged")
	}

	// And the pin is applied anyway: the owner may knowingly wedge their own
	// folder (D3).
	if !secondResp.File.Pinned {
		t.Error("the pin must be applied despite the warning")
	}

	// A THIRD pin into the same wedged folder still warns.
	third := seedAgedFile(t, env, "three.mp4", 100)
	thirdResp := pin(t, env, third.ID)
	if thirdResp.Warning == nil {
		t.Error("a state test warns every time the folder is wedged, not only on the transition")
	}
}

func TestAPIPinShouldExplainWhenTheFileHasAlreadyFinishedArchiving(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 10000))
	file := seedAgedFile(t, env, "left.jpg", 400)
	env.archive(t, file.ID)

	resp := pin(t, env, file.ID)

	if resp.Warning == nil {
		t.Fatal("pinning an archived file must explain that the pin is latent")
	}
	if !contains(*resp.Warning, "restore it") {
		t.Errorf("warning = %q, want it to say the pin takes effect on restore", *resp.Warning)
	}
	if !resp.File.Pinned {
		t.Error("the pin is still recorded on the archived row")
	}
}

func TestAPIPinShouldReturn404WhenTheFileDoesNotExist(t *testing.T) {
	env := newTestEnv(t)

	w := do(t, env, "POST", "/api/v1/files/99999/pin", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestAPIPinShouldReturn400WhenTheIDIsNotANumber(t *testing.T) {
	env := newTestEnv(t)

	w := do(t, env, "POST", "/api/v1/files/abc/pin", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestAPIBulkPinShouldPinEveryFileInOneRequestWhenManyAreSelected(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 10000))

	var ids []int64
	for _, name := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		ids = append(ids, seedAgedFile(t, env, name, 100).ID)
	}

	w := do(t, env, "POST", "/api/v1/files/pin",
		map[string]any{"ids": ids, "pinned": true})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	var resp bulkPinResponseJSON
	json.NewDecoder(w.Body).Decode(&resp)

	if resp.Updated != 3 {
		t.Errorf("updated = %d, want 3", resp.Updated)
	}
	if len(resp.Folders) != 1 || resp.Folders[0].Label != "TestFolder" {
		t.Fatalf("folders = %+v, want one entry for TestFolder", resp.Folders)
	}
	if resp.Folders[0].PinnedBytes != 300 {
		t.Errorf("pinned_bytes = %d, want 300", resp.Folders[0].PinnedBytes)
	}
	if resp.Warning != nil {
		t.Errorf("warning = %q, want null", *resp.Warning)
	}
	for _, id := range ids {
		if !reload(t, env, id).Pinned {
			t.Errorf("file %d should be pinned", id)
		}
	}
}

func TestAPIBulkPinShouldWarnWhenTheSelectionWedgesTheFolder(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 500))

	var ids []int64
	for _, name := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		ids = append(ids, seedAgedFile(t, env, name, 300).ID)
	}

	w := do(t, env, "POST", "/api/v1/files/pin",
		map[string]any{"ids": ids, "pinned": true})
	var resp bulkPinResponseJSON
	json.NewDecoder(w.Body).Decode(&resp)

	if resp.Warning == nil {
		t.Fatal("900 pinned bytes against a 500 budget must warn")
	}
	if !resp.Folders[0].Wedged {
		t.Error("the folder block should report wedged")
	}
}

func TestAPIBulkPinShouldUnpinEveryFileWhenPinnedIsFalse(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 10000))

	var ids []int64
	for _, name := range []string{"a.jpg", "b.jpg"} {
		id := seedAgedFile(t, env, name, 100).ID
		ids = append(ids, id)
		pin(t, env, id)
	}

	w := do(t, env, "POST", "/api/v1/files/pin",
		map[string]any{"ids": ids, "pinned": false})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp bulkPinResponseJSON
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Warning != nil {
		t.Errorf("warning = %q, want null on a bulk unpin", *resp.Warning)
	}
	for _, id := range ids {
		if reload(t, env, id).Pinned {
			t.Errorf("file %d should be unpinned", id)
		}
	}
}

func TestAPIBulkPinShouldRejectAnEmptySelection(t *testing.T) {
	env := newTestEnv(t)

	w := do(t, env, "POST", "/api/v1/files/pin", map[string]any{"ids": []int64{}, "pinned": true})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an empty selection", w.Code)
	}

	w = do(t, env, "POST", "/api/v1/files/pin", map[string]any{"pinned": true})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 when ids is absent", w.Code)
	}
}

func TestAPIBulkPinShouldRejectASelectionLargerThanTheCap(t *testing.T) {
	env := newTestEnv(t)

	ids := make([]int64, 5001)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	w := do(t, env, "POST", "/api/v1/files/pin", map[string]any{"ids": ids, "pinned": true})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for more than 5000 ids", w.Code)
	}
}

func TestAPIArchiveShouldMoveTheFileWhenTheOwnerAsksForIt(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1000000))
	file := seedAgedFile(t, env, "away.jpg", 400)
	pin(t, env, file.ID)

	w := do(t, env, "POST", "/api/v1/files/"+strconv.FormatInt(file.ID, 10)+"/archive", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	var got catalog.FileRecord
	json.NewDecoder(w.Body).Decode(&got)

	if got.Status != catalog.StatusArchived {
		t.Errorf("status = %q, want archived", got.Status)
	}
	if got.Pinned {
		t.Error("an explicit archive clears the pin")
	}
}

func TestAPIArchiveShouldReturnTheRecordWhenTheFileIsAlreadyArchived(t *testing.T) {
	env := newTestEnv(t)
	file := seedAgedFile(t, env, "cold.jpg", 400)
	env.archive(t, file.ID)

	w := do(t, env, "POST", "/api/v1/files/"+strconv.FormatInt(file.ID, 10)+"/archive", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (invariant 12), body: %s", w.Code, w.Body.String())
	}
}

func TestAPIArchiveShouldReturn507WhenColdStorageIsNearlyFull(t *testing.T) {
	env := newTestEnv(t)
	file := seedAgedFile(t, env, "big.mp4", 400)
	env.mover.SetSpaceCheck(func(string) (float64, error) { return 3.0, nil })

	w := do(t, env, "POST", "/api/v1/files/"+strconv.FormatInt(file.ID, 10)+"/archive", nil)
	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507, body: %s", w.Code, w.Body.String())
	}
	if reload(t, env, file.ID).Status != catalog.StatusSynced {
		t.Error("nothing should have moved")
	}
}

func TestAPIArchiveShouldReturn409WhenTheSourceIsStillBeingWritten(t *testing.T) {
	env := newTestEnv(t)
	file := seedAgedFile(t, env, "incoming.mp4", 400)

	// Syncthing is still pulling: the file on disk no longer matches its row.
	grow(t, env, "incoming.mp4", 4096)

	w := do(t, env, "POST", "/api/v1/files/"+strconv.FormatInt(file.ID, 10)+"/archive", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body: %s", w.Code, w.Body.String())
	}
	if reload(t, env, file.ID).Status != catalog.StatusSynced {
		t.Error("the in-flight transfer must be left alone")
	}
}

func TestAPIArchiveShouldReturn404WhenTheFileDoesNotExist(t *testing.T) {
	env := newTestEnv(t)

	if w := do(t, env, "POST", "/api/v1/files/99999/archive", nil); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && bytes.Contains([]byte(haystack), []byte(needle))
}
