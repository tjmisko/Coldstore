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

// The Conflicts tab is the only filter with no column behind it: it reads the
// events table, so the thing that must be proven is that a resolution takes a
// row OUT of it. A conflict that cannot be cleared is worse than no tab at
// all -- the owner resolves it, the badge stays, and they resolve it again.
func TestFilesShouldListOnlyUnresolvedConflictsOnTheConflictFilter(t *testing.T) {
	env := newTestEnv(t)
	stale := reoccupyArchivedPath(t, env, "resolved.jpg", "brought back")
	live := reoccupyArchivedPath(t, env, "outstanding.jpg", "brought back")

	both := listFiles(t, env, "?status=archived&conflict=1&limit=500")
	if len(both) != 2 {
		t.Fatalf("conflict=1 returned %d records before any resolution, want 2", len(both))
	}

	// keep=archive leaves the row archived, so ONLY the event log can tell the
	// two apart afterwards. keep=device would have moved the row to synced and
	// the status filter would have hidden it either way.
	if w := resolve(t, env, stale.ID, "archive"); w.Code != http.StatusOK {
		t.Fatalf("resolve status = %d, body: %s", w.Code, w.Body.String())
	}

	after := listFiles(t, env, "?status=archived&conflict=1&limit=500")
	if len(after) != 1 || after[0].ID != live.ID {
		t.Errorf("conflict=1 returned %v, want only the unresolved %d", after, live.ID)
	}

	w := do(t, env, "GET", "/api/v1/files/ids?status=archived&conflict=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("/files/ids status = %d", w.Code)
	}
	var resp fileIDsJSON
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 1 || len(resp.IDs) != 1 || resp.IDs[0] != live.ID {
		t.Errorf("/files/ids conflict=1 = %+v, want just %d", resp, live.ID)
	}

	if bad := do(t, env, "GET", "/api/v1/files?conflict=yes", nil); bad.Code != http.StatusBadRequest {
		t.Errorf("conflict=yes status = %d, want 400", bad.Code)
	}
}

// The wedge banner's remedy sends the owner to their oldest auto-pins, which
// is only a remedy if "oldest" is really what they get: the pins they have
// forgotten are at the far end of a list nobody scrolls.
func TestFilesShouldOrderOldestPinFirstOnThePinnedAtSort(t *testing.T) {
	env := newTestEnv(t)
	ids := seedCatalogRows(t, env, 3)

	// Explicit pin times, oldest last, so the seeding order cannot pass for a
	// correct sort.
	for i, id := range ids {
		at := int64(1_700_000_000 + (len(ids)-i)*3600)
		if err := env.cat.SetPinned(id, true, catalog.PinSourceRestore, at); err != nil {
			t.Fatal(err)
		}
	}

	got := listFiles(t, env, "?pinned=1&pin_source=restore&sort=pinned_at&limit=500")
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].PinnedAt == nil || got[i].PinnedAt == nil {
			t.Fatalf("record %d has no pinned_at", i)
		}
		if *got[i-1].PinnedAt > *got[i].PinnedAt {
			t.Errorf("pinned_at order broken at %d: %d before %d",
				i, *got[i-1].PinnedAt, *got[i].PinnedAt)
		}
	}
	if got[0].ID != ids[len(ids)-1] {
		t.Errorf("first record = %d, want the oldest pin %d", got[0].ID, ids[len(ids)-1])
	}
}

// Every page of an infinite scroll must be a slice of ONE total order. A
// camera roll copied off a card shares one mtime across hundreds of files, so
// a sort with no tiebreak repeats rows across pages and silently drops others.
func TestFilesShouldPageWithoutRepeatingRowsWhenTheSortKeyTies(t *testing.T) {
	env := newTestEnv(t)
	sameTime := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 10; i++ {
		name := "tied" + padInt(i, 4) + ".jpg"
		if _, err := env.cat.UpsertFile(&catalog.FileRecord{
			FolderLabel: "TestFolder", RelativePath: name, Filename: name,
			Extension: ".jpg", SizeBytes: 100, FileMtime: sameTime, FileAtime: sameTime,
			Status: catalog.StatusSynced,
		}, time.Now().UnixNano()); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[int64]bool{}
	for offset := 0; offset < 10; offset += 5 {
		page := listFiles(t, env, fmt.Sprintf("?limit=5&offset=%d", offset))
		if len(page) != 5 {
			t.Fatalf("page at offset %d returned %d records, want 5", offset, len(page))
		}
		for _, f := range page {
			if seen[f.ID] {
				t.Errorf("file %d appeared on two pages", f.ID)
			}
			seen[f.ID] = true
		}
	}
	if len(seen) != 10 {
		t.Errorf("saw %d distinct files across both pages, want 10", len(seen))
	}
}
