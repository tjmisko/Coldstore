//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

// seedAgedFile writes a real file into the folder, backdates it well past any
// grace window, and catalogues it with the metadata a scan would have
// recorded. Both halves matter: enforcement reads the row, and the mover
// re-stats the file and refuses a move if the two disagree.
func seedAgedFile(t *testing.T, env *testEnv, relPath string, size int) *catalog.FileRecord {
	t.Helper()

	full := filepath.Join(env.syncDir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(full, old, old); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}

	rec := &catalog.FileRecord{
		FolderLabel:  "TestFolder",
		RelativePath: relPath,
		Filename:     filepath.Base(relPath),
		Extension:    filepath.Ext(relPath),
		SizeBytes:    int64(size),
		FileMtime:    info.ModTime().UTC(),
		FileAtime:    info.ModTime().UTC(),
		Status:       catalog.StatusSynced,
	}
	if _, err := env.cat.UpsertFile(rec, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	got, err := env.cat.GetFile("TestFolder", relPath)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// THE ACCEPTANCE TEST. This is the owner's original bug, in the owner's own
// words: a file they asked to get back was taken away again by the very next
// quota tick, roughly thirty seconds later. The restore has to commit its
// status and its pin in one statement (MarkRestored), or there is a window --
// and, before that, a permanent state -- in which the file is synced,
// unpinned, old, and the oldest candidate in an over-budget folder.
func TestRestoredFileShouldStillBeOnTheDeviceWhenTheNextQuotaTickRuns(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1000), withGracePeriod(0))

	// The folder is over budget with room for one of these two files.
	wanted := seedAgedFile(t, env, "wanted.jpg", 800)
	other := seedAgedFile(t, env, "other.jpg", 800)

	// Tick one: the oldest file goes to cold storage.
	env.daemon.EnforceQuotas()
	archived := reload(t, env, wanted.ID)
	if archived.Status != catalog.StatusArchived {
		// Whichever of the two went first is fine; drive the test off the one
		// that actually left.
		wanted, other = other, wanted
		archived = reload(t, env, wanted.ID)
	}
	if archived.Status != catalog.StatusArchived {
		t.Fatalf("the quota tick archived nothing; the fixture is not over budget")
	}

	// The owner presses Restore in the UI.
	req := httptest.NewRequest("POST",
		"/api/v1/files/"+strconv.FormatInt(wanted.ID, 10)+"/restore", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("restore status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	var restored catalog.FileRecord
	if err := json.NewDecoder(w.Body).Decode(&restored); err != nil {
		t.Fatal(err)
	}
	// t.Error, not t.Fatal: the pin is the mechanism, but the assertion that
	// matters to the owner is the one after the next tick, and the test should
	// report both.
	if !restored.Pinned {
		t.Error("the restore response must report the file as pinned")
	}
	if restored.PinSource != catalog.PinSourceRestore {
		t.Errorf("pin_source = %q, want %q", restored.PinSource, catalog.PinSourceRestore)
	}

	// The folder is over budget again, and the restored file is the oldest
	// unpinned-looking candidate in it. Only the pin can save it.
	env.daemon.EnforceQuotas()

	after := reload(t, env, wanted.ID)
	if after.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced: the quota tick undid the owner's restore", after.Status)
	}
	if !after.Pinned {
		t.Error("the pin should still be in place after the tick")
	}
	if _, err := os.Stat(filepath.Join(env.syncDir, wanted.RelativePath)); err != nil {
		t.Errorf("the restored file must still be on disk: %v", err)
	}
	// And the daemon did converge: it took the unpinned file instead.
	if got := reload(t, env, other.ID); got.Status != catalog.StatusArchived {
		t.Errorf("the other file's status = %q, want archived: the folder still had to fit", got.Status)
	}
}

func TestPinnedFileShouldSurviveWhenBudgetPressureEvictsEverythingAroundIt(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 500), withGracePeriod(0))

	// The pinned file is the OLDEST, so oldest-first eviction would take it
	// before anything else if the pin were not honoured.
	keep := seedAgedFile(t, env, "keep.jpg", 400)
	doomed := []*catalog.FileRecord{
		seedAgedFile(t, env, "a.jpg", 400),
		seedAgedFile(t, env, "b.jpg", 400),
		seedAgedFile(t, env, "c.jpg", 400),
	}

	pin(t, env, keep.ID)
	env.daemon.EnforceQuotas()

	if got := reload(t, env, keep.ID); got.Status != catalog.StatusSynced {
		t.Errorf("pinned file status = %q, want synced", got.Status)
	}
	if _, err := os.Stat(filepath.Join(env.syncDir, "keep.jpg")); err != nil {
		t.Errorf("the pinned file must still be on disk: %v", err)
	}
	for _, f := range doomed {
		if got := reload(t, env, f.ID); got.Status != catalog.StatusArchived {
			t.Errorf("%s status = %q, want archived", f.RelativePath, got.Status)
		}
	}
}

func TestStatusShouldReportPinnedBytesAsASubsetOfTheFolderFootprint(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1000), withGracePeriod(0))

	pinned := seedAgedFile(t, env, "pinned.jpg", 300)
	seedAgedFile(t, env, "loose.jpg", 200)
	archived := seedAgedFile(t, env, "cold.jpg", 900)

	pin(t, env, pinned.ID)
	env.archive(t, archived.ID)
	// An archived file can be pinned too; those bytes are in cold storage and
	// must not show up in either figure.
	pin(t, env, archived.ID)

	folder := statusFolder(t, env, "TestFolder")

	if folder.SyncedSizeBytes != 500 {
		t.Errorf("synced_size_bytes = %d, want 500 (300 pinned + 200 loose, archived excluded)",
			folder.SyncedSizeBytes)
	}
	if folder.PinnedBytes != 300 {
		t.Errorf("pinned_bytes = %d, want 300", folder.PinnedBytes)
	}
	if folder.PinnedBytes > folder.SyncedSizeBytes {
		t.Error("pinned_bytes is a SUBSET of synced_size_bytes, never an addend")
	}
	if folder.OverBudget {
		t.Errorf("over_budget = true at %d bytes against a %d budget",
			folder.SyncedSizeBytes, folder.BudgetBytes)
	}
	if folder.Wedged {
		t.Error("wedged = true when pinned bytes are well inside the budget")
	}
	if folder.Counts.Pinned != 1 {
		t.Errorf("counts.pinned = %d, want 1 (the archived pin is not on the device)",
			folder.Counts.Pinned)
	}
}

func TestStatusShouldReportOverBudgetAndWedgedWhenPinnedBytesExceedTheBudget(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 500), withGracePeriod(0))

	big := seedAgedFile(t, env, "big.mp4", 900)
	pin(t, env, big.ID)

	folder := statusFolder(t, env, "TestFolder")
	if !folder.OverBudget {
		t.Error("over_budget should be true at 900 bytes against a 500 budget")
	}
	if !folder.Wedged {
		t.Error("wedged should be true: pinned bytes alone exceed the budget")
	}
}

// reload re-reads a file row, because every assertion here is about what the
// daemon did to it.
func reload(t *testing.T, env *testEnv, id int64) *catalog.FileRecord {
	t.Helper()
	got, err := env.cat.GetFileByID(id)
	if err != nil {
		t.Fatalf("reading file %d: %v", id, err)
	}
	return got
}

// grow simulates an in-flight Syncthing transfer: the file on disk gets bigger
// and newer while its catalog row still describes the old state.
func grow(t *testing.T, env *testEnv, relPath string, size int) {
	t.Helper()
	full := filepath.Join(env.syncDir, relPath)
	if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(full, now, now); err != nil {
		t.Fatal(err)
	}
}
