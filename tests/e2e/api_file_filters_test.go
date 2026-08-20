//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

// seedCatalogRows writes n catalog rows with no files behind them. These tests
// are about the query surface, not about moving bytes.
func seedCatalogRows(t *testing.T, env *testEnv, n int) []int64 {
	t.Helper()
	now := time.Now().UTC()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		name := "file" + padInt(i, 4) + ".jpg"
		if _, err := env.cat.UpsertFile(&catalog.FileRecord{
			FolderLabel: "TestFolder", RelativePath: name, Filename: name,
			Extension: ".jpg", SizeBytes: 100,
			FileMtime: now.Add(-time.Duration(i) * time.Minute),
			Status:    catalog.StatusSynced,
		}, time.Now().UnixNano()); err != nil {
			t.Fatal(err)
		}
		rec, err := env.cat.GetFile("TestFolder", name)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rec.ID)
	}
	return ids
}

func listFiles(t *testing.T, env *testEnv, query string) []catalog.FileRecord {
	t.Helper()
	w := do(t, env, "GET", "/api/v1/files"+query, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /files%s status = %d, body: %s", query, w.Code, w.Body.String())
	}
	var files []catalog.FileRecord
	if err := json.NewDecoder(w.Body).Decode(&files); err != nil {
		t.Fatal(err)
	}
	return files
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

// The ids branch of GET /files is what design section 7.3's refetchIds uses to
// repaint a selection after a bulk job. It used to fall through to the
// catalog's default LIMIT 50, so a 120-file bulk restore repainted 50 rows and
// the other 70 went on rendering as archived.
func TestFilesByIDsShouldReturnEveryRequestedRowWhenThereAreMoreThanOnePage(t *testing.T) {
	env := newTestEnv(t)
	ids := seedCatalogRows(t, env, 120)

	files := listFiles(t, env, "?ids="+joinIDs(ids))
	if len(files) != len(ids) {
		t.Fatalf("asked for %d ids, got %d records", len(ids), len(files))
	}
	for i := range files {
		if files[i].ID != ids[i] {
			t.Fatalf("record %d has id %d, want %d -- ids must come back in id order",
				i, files[i].ID, ids[i])
		}
	}
}

// ids overrides every other filter, and ids that no longer exist are simply
// absent rather than an error.
func TestFilesByIDsShouldIgnoreOtherFiltersAndSkipIdsThatNoLongerExist(t *testing.T) {
	env := newTestEnv(t)
	ids := seedCatalogRows(t, env, 3)

	query := fmt.Sprintf("?status=archived&folder=Nonexistent&ids=%s,999999", joinIDs(ids))
	files := listFiles(t, env, query)
	if len(files) != 3 {
		t.Fatalf("got %d records, want the 3 existing ids", len(files))
	}
}

func TestFilesByIDsShouldRejectASelectionLargerThanTheInlineCap(t *testing.T) {
	env := newTestEnv(t)

	ids := make([]int64, 501)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	w := do(t, env, "GET", "/api/v1/files?ids="+joinIDs(ids), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// One request must never be able to materialise the whole catalog.
func TestFilesShouldClampAnOversizedLimitAndRejectANonPositiveOne(t *testing.T) {
	env := newTestEnv(t)
	seedCatalogRows(t, env, 600)

	files := listFiles(t, env, "?limit=100000")
	if len(files) != 500 {
		t.Errorf("limit=100000 returned %d records, want the 500 ceiling", len(files))
	}

	for _, query := range []string{"?limit=0", "?limit=-5", "?limit=abc", "?offset=-1"} {
		w := do(t, env, "GET", "/api/v1/files"+query, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET /files%s status = %d, want 400", query, w.Code)
		}
	}
}

func TestFilesShouldPartitionOnThePinnedFilterWhenSomeFilesArePinned(t *testing.T) {
	env := newTestEnv(t)
	ids := seedCatalogRows(t, env, 5)
	pin(t, env, ids[0])
	pin(t, env, ids[1])

	pinned := listFiles(t, env, "?pinned=1&limit=500")
	if len(pinned) != 2 {
		t.Errorf("pinned=1 returned %d records, want 2", len(pinned))
	}
	unpinned := listFiles(t, env, "?pinned=0&limit=500")
	if len(unpinned) != 3 {
		t.Errorf("pinned=0 returned %d records, want 3", len(unpinned))
	}

	w := do(t, env, "GET", "/api/v1/files?pinned=yes", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("pinned=yes status = %d, want 400", w.Code)
	}
}

// pin_source is what makes "unpin the ones I did not choose" possible: a
// restore auto-pins, and that pin must stay distinguishable from one the owner
// pressed.
func TestFilesShouldSeparateRestorePinsFromUserPinsOnThePinSourceFilter(t *testing.T) {
	env := newTestEnv(t)
	archived := seedAgedFile(t, env, "archived.jpg", 1000)
	chosen := seedAgedFile(t, env, "chosen.jpg", 1000)

	env.archive(t, archived.ID)
	env.restore(t, archived.ID)
	pin(t, env, chosen.ID)

	byRestore := listFiles(t, env, "?pin_source=restore&limit=500")
	if len(byRestore) != 1 || byRestore[0].ID != archived.ID {
		t.Errorf("pin_source=restore returned %v, want just the restored file", byRestore)
	}
	byUser := listFiles(t, env, "?pin_source=user&limit=500")
	if len(byUser) != 1 || byUser[0].ID != chosen.ID {
		t.Errorf("pin_source=user returned %v, want just the pinned file", byUser)
	}
}

// GET /files/ids caps at maxJobIDs and must say so honestly, because the UI
// turns its answer straight into a job the owner believes covers everything.
func TestFileIDsShouldReportTruncationHonestlyWhenMoreRowsMatchThanFit(t *testing.T) {
	env := newTestEnv(t)
	ids := seedCatalogRows(t, env, 12)

	// Drive the cap through the catalog rather than seeding 20,001 rows: the
	// endpoint's contract is "cap, real total, truncated flag", and the cap is
	// the same code path at 5 as at 20000.
	got, total, err := env.cat.ListFileIDs(catalog.FileQuery{Folder: "TestFolder"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("returned %d ids, want the cap of 5", len(got))
	}
	if total != int64(len(ids)) {
		t.Errorf("total = %d, want the real match count %d", total, len(ids))
	}
	if total <= int64(len(got)) {
		t.Errorf("total %d must exceed the returned %d so truncated is true", total, len(got))
	}

	// And the endpoint itself, under the cap, reports truncated=false.
	w := do(t, env, "GET", "/api/v1/files/ids?folder=TestFolder", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp fileIDsJSON
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Truncated {
		t.Error("truncated = true with 12 rows and a 20000 cap")
	}
	if resp.Total != 12 || len(resp.IDs) != 12 {
		t.Errorf("total = %d, len(ids) = %d, want 12 and 12", resp.Total, len(resp.IDs))
	}
}

func TestFileIDsShouldApplyThePinnedFilterWhenAskedForPinsOnly(t *testing.T) {
	env := newTestEnv(t)
	ids := seedCatalogRows(t, env, 4)
	pin(t, env, ids[2])

	w := do(t, env, "GET", "/api/v1/files/ids?folder=TestFolder&pinned=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	var resp fileIDsJSON
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.IDs) != 1 || resp.IDs[0] != ids[2] {
		t.Errorf("ids = %v, want just %d", resp.IDs, ids[2])
	}
}
