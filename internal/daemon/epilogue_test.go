package daemon

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/tjmisko/coldstore/internal/catalog"
)

// lastEnforcedAt reads folder_configs.last_enforced_at straight out of SQLite.
// The point of these tests is whether the column moved at all, so nothing in
// between is allowed to interpret it.
func lastEnforcedAt(t *testing.T, dbPath, label string) *int64 {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var at sql.NullInt64
	if err := db.QueryRow(
		`SELECT last_enforced_at FROM folder_configs WHERE folder_label = ?`, label).Scan(&at); err != nil {
		t.Fatalf("reading last_enforced_at: %v", err)
	}
	if !at.Valid {
		return nil
	}
	return &at.Int64
}

// corruptBudgetColumn writes a non-numeric value into budget_bytes so the next
// FolderConfigByLabel fails its Scan. It stands in for any transient read error
// -- a locked database, a busy timeout -- which is otherwise impossible to
// provoke from a test without also breaking the write we want to observe.
func corruptBudgetColumn(t *testing.T, dbPath, label string) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(
		`UPDATE folder_configs SET budget_bytes = 'not a number' WHERE folder_label = ?`,
		label); err != nil {
		t.Fatal(err)
	}
}

// Defect 5. The epilogue re-reads the folder config precisely because the UI
// may have changed the policy while the pass was running -- and then judged
// the wedge against the budget the tick had STARTED with, so a budget lowered
// mid-pass produced no warning at all and the next one quoted a number that
// was no longer anybody's budget.
func TestEpilogueShouldJudgeTheWedgeAgainstTheBudgetWhenItIsLoweredDuringThePass(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	cfg.Folders[0].BudgetBytes = 5000

	pinned := seedSyncedFile(t, cat, syncDir, "pinned.jpg", 1500)
	seedSyncedFile(t, cat, syncDir, "loose.jpg", 4000)
	ageFiles(t, cat, syncDir, "pinned.jpg", "loose.jpg")

	d := newTestDaemon(t, cfg, cat, testLogger())
	declarePolicy(t, cfg, cat)
	if _, err := d.mover.SetPinned(pinned.ID, true); err != nil {
		t.Fatal(err)
	}

	// 5500 on device against a 5000 budget: the pass runs, and 1500 pinned is
	// comfortably inside it, so nothing is wedged when the tick starts.
	realMoveOut := d.mover.moveOut
	d.mover.SetMoveFuncs(func(src, dst string) error {
		// The owner drops the budget to 1000 in the UI while the copy runs.
		// 1500 pinned no longer fits, and there is nothing left to shed.
		lowered := int64(1000)
		if found, err := cat.UpdateFolderPolicy("TestFolder", &lowered, nil, nil); err != nil || !found {
			t.Errorf("lowering the budget mid-pass: found %v, err %v", found, err)
		}
		return realMoveOut(src, dst)
	}, nil)

	d.EnforceQuotas()

	if n := countEvents(t, dbPath, "folder_wedged"); n != 1 {
		t.Fatalf("folder_wedged events = %d, want 1: the epilogue must use the budget "+
			"that is in force when it runs, not the one the tick started with", n)
	}
	details := lastEventDetails(t, dbPath, "folder_wedged")
	if !strings.Contains(details, catalog.HumanSI(1000)+" budget") {
		t.Errorf("details = %q, want the current %s budget", details, catalog.HumanSI(1000))
	}
	if strings.Contains(details, "the "+catalog.HumanSI(5000)+" budget") {
		t.Errorf("details = %q, want no mention of the stale %s budget",
			details, catalog.HumanSI(5000))
	}
}

// The other direction, and the one that matters more: a budget RAISED mid-pass
// must not leave a wedge warning behind for a folder that is no longer wedged.
func TestEpilogueShouldNotWarnWhenTheBudgetIsRaisedAboveThePinnedBytesDuringThePass(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	cfg.Folders[0].BudgetBytes = 1000

	pinned := seedSyncedFile(t, cat, syncDir, "pinned.jpg", 1500)
	seedSyncedFile(t, cat, syncDir, "loose.jpg", 400)
	ageFiles(t, cat, syncDir, "pinned.jpg", "loose.jpg")

	d := newTestDaemon(t, cfg, cat, testLogger())
	declarePolicy(t, cfg, cat)
	if _, err := d.mover.SetPinned(pinned.ID, true); err != nil {
		t.Fatal(err)
	}

	realMoveOut := d.mover.moveOut
	d.mover.SetMoveFuncs(func(src, dst string) error {
		raised := int64(100000)
		if found, err := cat.UpdateFolderPolicy("TestFolder", &raised, nil, nil); err != nil || !found {
			t.Errorf("raising the budget mid-pass: found %v, err %v", found, err)
		}
		return realMoveOut(src, dst)
	}, nil)

	d.EnforceQuotas()

	if n := countEvents(t, dbPath, "folder_wedged"); n != 0 {
		t.Errorf("folder_wedged events = %d, want 0: the folder has room again", n)
	}
	if at := wedgeWarnedAt(t, cat, "TestFolder"); at != nil {
		t.Errorf("wedge_warned_at = %v, want unset", *at)
	}
}

// Defect 6. last_enforced_at is what the UI's "last checked N minutes ago"
// reads. The epilogue's own reads can fail, and when they did it returned
// before the stamp -- freezing the timestamp at the moment the trouble started
// and reporting a live daemon as a stopped one, which is the exact symptom the
// stamp exists to expose.
func TestEpilogueShouldStillRecordTheEnforcementTimeWhenItsOwnReadFails(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	cfg.Folders[0].BudgetBytes = 1000

	seedSyncedFile(t, cat, syncDir, "loose.jpg", 4000)
	ageFiles(t, cat, syncDir, "loose.jpg")

	d := newTestDaemon(t, cfg, cat, testLogger())
	declarePolicy(t, cfg, cat)

	if at := lastEnforcedAt(t, dbPath, "TestFolder"); at != nil {
		t.Fatalf("last_enforced_at = %v before the first tick, want unset", *at)
	}

	// The read breaks after the tick has already selected its folders, so the
	// pass runs to completion and only the epilogue trips over it.
	realMoveOut := d.mover.moveOut
	d.mover.SetMoveFuncs(func(src, dst string) error {
		corruptBudgetColumn(t, dbPath, "TestFolder")
		return realMoveOut(src, dst)
	}, nil)

	d.EnforceQuotas()

	if _, err := cat.FolderConfigByLabel("TestFolder"); err == nil {
		t.Fatal("the folder config read was supposed to fail; the test proves nothing")
	}
	if at := lastEnforcedAt(t, dbPath, "TestFolder"); at == nil {
		t.Error("last_enforced_at is still unset: a transient read error must not " +
			"freeze the timestamp the UI reports as liveness")
	}
}

// Defect 9. "More than 10% of the folder's file count" with no floor makes an
// ordinary tick on a small folder a mass eviction: one file out of six is 16%.
func TestBulkEvictionShouldStaySilentWhenASmallFolderShedsASingleFile(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	cfg.Folders[0].BudgetBytes = 1 << 40

	names := make([]string, 0, 6)
	for i := range 6 {
		name := fmt.Sprintf("clip%d.mp4", i)
		seedSyncedFile(t, cat, syncDir, name, 100)
		names = append(names, name)
	}
	declarePolicy(t, cfg, cat)
	d := newTestDaemon(t, cfg, cat, testLogger())
	ageFiles(t, cat, syncDir, names...)

	// 600 bytes on device, 500 of budget: exactly one file has to go.
	budget := int64(500)
	if found, err := cat.UpdateFolderPolicy("TestFolder", &budget, nil, nil); err != nil || !found {
		t.Fatalf("setting the budget: found %v, err %v", found, err)
	}

	d.EnforceQuotas()

	counts, err := cat.FolderCounts("TestFolder")
	if err != nil {
		t.Fatal(err)
	}
	if counts.Archived != 1 {
		t.Fatalf("archived %d files, want exactly 1", counts.Archived)
	}
	if n := countEvents(t, dbPath, "bulk_eviction"); n != 0 {
		t.Errorf("bulk_eviction events = %d, want 0: shedding one file out of six is "+
			"an ordinary tick, and a warning that fires on the normal case is not a warning", n)
	}
}

// The floor must not swallow a real mass eviction. Ten files is where the
// percentage arm starts being allowed to speak.
func TestBulkEvictionShouldStillWarnWhenThePassReachesTheFloor(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	cfg.Folders[0].BudgetBytes = 1 << 40

	names := make([]string, 0, 40)
	for i := range 40 {
		name := fmt.Sprintf("photo%02d.jpg", i)
		seedSyncedFile(t, cat, syncDir, name, 100)
		names = append(names, name)
	}
	declarePolicy(t, cfg, cat)
	d := newTestDaemon(t, cfg, cat, testLogger())
	ageFiles(t, cat, syncDir, names...)

	// 4000 bytes on device, 3000 of budget: ten of the forty go, which is 25%.
	budget := int64(3000)
	if found, err := cat.UpdateFolderPolicy("TestFolder", &budget, nil, nil); err != nil || !found {
		t.Fatalf("setting the budget: found %v, err %v", found, err)
	}

	d.EnforceQuotas()

	counts, err := cat.FolderCounts("TestFolder")
	if err != nil {
		t.Fatal(err)
	}
	if counts.Archived != bulkEvictionMinFiles {
		t.Fatalf("archived %d files, want exactly %d", counts.Archived, bulkEvictionMinFiles)
	}
	if n := countEvents(t, dbPath, "bulk_eviction"); n != 1 {
		t.Errorf("bulk_eviction events = %d, want 1", n)
	}
}
