package daemon

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

// setupWedgeEnv is setupTestEnv plus the database path, which the event
// assertions need: the catalog has no list-events method until step 6 and the
// whole point of these tests is how many rows land in that table.
func setupWedgeEnv(t *testing.T) (syncDir string, cfg *config.Config, cat *catalog.Catalog, dbPath string) {
	t.Helper()

	syncDir = newSyncthingFolder(t, filepath.Join(t.TempDir(), "sync"))
	coldDir := filepath.Join(t.TempDir(), "cold")
	os.MkdirAll(filepath.Join(coldDir, "archive"), 0o755)

	cat, dbPath = openCatalog(t)
	cfg = &config.Config{
		Storage: config.StorageConfig{ColdStoragePath: coldDir},
		Daemon:  config.DaemonConfig{GracePeriodMinutes: 0},
		Folders: []config.FolderConfig{
			{Label: "TestFolder", Path: syncDir, BudgetBytes: 1000},
		},
	}
	return syncDir, cfg, cat, dbPath
}

func lastEventDetails(t *testing.T, dbPath, eventType string) string {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var details string
	err = db.QueryRow(`SELECT details FROM events WHERE event_type = ? ORDER BY id DESC LIMIT 1`,
		eventType).Scan(&details)
	if err != nil {
		t.Fatalf("no %s event: %v", eventType, err)
	}
	return details
}

func wedgeWarnedAt(t *testing.T, cat *catalog.Catalog, label string) *int64 {
	t.Helper()
	fc, err := cat.FolderConfigByLabel(label)
	if err != nil {
		t.Fatal(err)
	}
	return fc.WedgeWarnedAt
}

// The D3 behaviour in full: with pinned bytes alone over the budget the daemon
// cannot converge, so it offloads every unpinned eligible file -- it does not
// give up and leave them -- and then says so exactly once.
func TestEnforcementShouldOffloadEveryUnpinnedFileWhenPinnedBytesAloneExceedTheBudget(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	cfg.Folders[0].BudgetBytes = 1000

	pinned := seedSyncedFile(t, cat, syncDir, "pinned.jpg", 1500)
	loose := []*catalog.FileRecord{
		seedSyncedFile(t, cat, syncDir, "a.jpg", 400),
		seedSyncedFile(t, cat, syncDir, "b.jpg", 400),
		seedSyncedFile(t, cat, syncDir, "c.jpg", 400),
	}
	ageFiles(t, cat, syncDir, "pinned.jpg", "a.jpg", "b.jpg", "c.jpg")

	d := newTestDaemon(t, cfg, cat, testLogger())
	declarePolicy(t, cfg, cat)
	if _, err := d.mover.SetPinned(pinned.ID, true); err != nil {
		t.Fatal(err)
	}

	d.EnforceQuotas()

	for _, f := range loose {
		got, _ := cat.GetFileByID(f.ID)
		if got.Status != catalog.StatusArchived {
			t.Errorf("%s status = %q, want archived: a wedged folder still sheds everything it can",
				f.RelativePath, got.Status)
		}
	}
	got, _ := cat.GetFileByID(pinned.ID)
	if got.Status != catalog.StatusSynced {
		t.Errorf("pinned file status = %q, want synced", got.Status)
	}

	if n := countEvents(t, dbPath, "folder_wedged"); n != 1 {
		t.Errorf("folder_wedged events = %d, want 1", n)
	}
	details := lastEventDetails(t, dbPath, "folder_wedged")
	if !strings.Contains(details, "nothing left to offload") {
		t.Errorf("wedge event details = %q, want it to say what the owner can do about it", details)
	}
}

func TestEnforcementShouldWarnOnceWhenTheFolderStaysWedgedAcrossTicks(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "huge.mp4", 5000)
	ageFiles(t, cat, syncDir, "huge.mp4")

	d := newTestDaemon(t, cfg, cat, testLogger())
	declarePolicy(t, cfg, cat)
	if _, err := d.mover.SetPinned(file.ID, true); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		d.EnforceQuotas()
	}

	if n := countEvents(t, dbPath, "folder_wedged"); n != 1 {
		t.Errorf("folder_wedged events = %d after three ticks, want 1", n)
	}

	// A folder that is STILL wedged a day later is a persistent problem and
	// should keep saying so, rather than leaving one row that scrolls away.
	stale := time.Now().Unix() - (25 * 60 * 60)
	if err := cat.SetWedgeWarnedAt("TestFolder", &stale); err != nil {
		t.Fatal(err)
	}
	d.EnforceQuotas()
	if n := countEvents(t, dbPath, "folder_wedged"); n != 2 {
		t.Errorf("folder_wedged events = %d after the 24h refresh, want 2", n)
	}
}

// An in-memory flag would reset here, and a systemd restart loop would refill
// the events table with the same warning. The latch is a column for exactly
// this reason.
func TestEnforcementShouldWarnOnlyOnceWhenTheDaemonIsRestartedWhileWedged(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "huge.mp4", 5000)
	ageFiles(t, cat, syncDir, "huge.mp4")

	first := newTestDaemon(t, cfg, cat, testLogger())
	declarePolicy(t, cfg, cat)
	if _, err := first.mover.SetPinned(file.ID, true); err != nil {
		t.Fatal(err)
	}
	first.EnforceQuotas()

	// "Restart": a brand new Daemon and a brand new Mover over the same
	// catalog, exactly what main builds on the next boot.
	second := newTestDaemon(t, cfg, cat, testLogger())
	second.EnforceQuotas()
	second.EnforceQuotas()

	if n := countEvents(t, dbPath, "folder_wedged"); n != 1 {
		t.Errorf("folder_wedged events = %d across a restart, want 1", n)
	}
}

func TestEnforcementShouldClearTheLatchWhenTheFolderIsNoLongerWedged(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "huge.mp4", 5000)
	ageFiles(t, cat, syncDir, "huge.mp4")

	d := newTestDaemon(t, cfg, cat, testLogger())
	declarePolicy(t, cfg, cat)
	if _, err := d.mover.SetPinned(file.ID, true); err != nil {
		t.Fatal(err)
	}
	d.EnforceQuotas()
	if wedgeWarnedAt(t, cat, "TestFolder") == nil {
		t.Fatal("the latch should be set while the folder is wedged")
	}

	// Raise the budget past the pinned bytes. The pass now takes the
	// within-budget exit -- which is the regression: with the epilogue at the
	// textual end of enforceFolder, that exit never clears the latch and the
	// next genuine wedge is swallowed.
	budget := int64(50000)
	if _, err := cat.UpdateFolderPolicy("TestFolder", &budget, nil, nil); err != nil {
		t.Fatal(err)
	}
	d.EnforceQuotas()

	if at := wedgeWarnedAt(t, cat, "TestFolder"); at != nil {
		t.Errorf("wedge_warned_at = %v, want nil once the folder is no longer wedged", *at)
	}
	if n := countEvents(t, dbPath, "folder_unwedged"); n != 1 {
		t.Errorf("folder_unwedged events = %d, want 1", n)
	}

	// And the next real wedge warns immediately rather than being suppressed.
	small := int64(1000)
	if _, err := cat.UpdateFolderPolicy("TestFolder", &small, nil, nil); err != nil {
		t.Fatal(err)
	}
	d.EnforceQuotas()
	if n := countEvents(t, dbPath, "folder_wedged"); n != 2 {
		t.Errorf("folder_wedged events = %d after a re-wedge, want 2", n)
	}
}

// Two conditions must never share a deduped event type: if they did, the wedge
// row written at 09:00 would make the 09:20 "cold storage at 4.2% free" row
// vanish, and offloading would stop with nothing anywhere saying why.
func TestEnforcementShouldStillWarnAboutColdStorageWhenTheFolderIsAlsoWedged(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	pinned := seedSyncedFile(t, cat, syncDir, "huge.mp4", 5000)
	ageFiles(t, cat, syncDir, "huge.mp4")

	d := newTestDaemon(t, cfg, cat, testLogger())
	declarePolicy(t, cfg, cat)
	if _, err := d.mover.SetPinned(pinned.ID, true); err != nil {
		t.Fatal(err)
	}
	d.EnforceQuotas()

	// A new photo lands, and by the time the next tick runs the pool is full.
	seedSyncedFile(t, cat, syncDir, "loose.jpg", 400)
	ageFiles(t, cat, syncDir, "loose.jpg")
	d.SetSpaceCheck(func(string) (float64, error) { return 4.2, nil })
	d.EnforceQuotas()

	if n := countEvents(t, dbPath, "folder_wedged"); n != 1 {
		t.Errorf("folder_wedged events = %d, want 1", n)
	}
	if n := countEvents(t, dbPath, "cold_storage_low"); n != 1 {
		t.Errorf("cold_storage_low events = %d, want 1: it must not be suppressed by the wedge warning", n)
	}
}

func TestEnforcementShouldWriteOneUnavailableEventWhenTheFolderRootIsNotMounted(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	seedSyncedFile(t, cat, syncDir, "photo.jpg", 5000)

	d := newTestDaemon(t, cfg, cat, testLogger())
	declarePolicy(t, cfg, cat)
	if err := os.Remove(filepath.Join(syncDir, ".stfolder")); err != nil {
		t.Fatal(err)
	}

	d.EnforceQuotas()
	d.EnforceQuotas()

	if n := countEvents(t, dbPath, "folder_unavailable"); n != 1 {
		t.Errorf("folder_unavailable events = %d across two ticks, want 1 (deduped at an hour)", n)
	}
	// The daemon still LOOKED at the folder, and the UI's "last checked"
	// must not freeze just because the drive is out.
	fc, _ := cat.FolderConfigByLabel("TestFolder")
	if fc.LastEnforcedAt == nil {
		t.Error("last_enforced_at should be stamped even for an unavailable folder")
	}
}

// ageFiles backdates both the file on disk and its catalog row past any grace
// window, so enforcement sees an eligible candidate.
func ageFiles(t *testing.T, cat *catalog.Catalog, syncDir string, relPaths ...string) {
	t.Helper()
	old := time.Now().Add(-24 * time.Hour)
	for _, rel := range relPaths {
		full := filepath.Join(syncDir, rel)
		if err := os.Chtimes(full, old, old); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(full)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := cat.GetFile("TestFolder", rel)
		if err != nil {
			t.Fatal(err)
		}
		rec.FileMtime = info.ModTime().UTC()
		rec.FileAtime = info.ModTime().UTC()
		if _, err := cat.UpsertFile(rec, time.Now().UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
}

// The cold-storage warning is deduped for a reason: the pool stays full for
// hours, enforcement runs every tick, and an undeduped write puts one row per
// folder per tick into the same events table the UI reads. Asserting the event
// merely EXISTS cannot tell LogEventDeduped from LogEvent, so this asserts the
// count stays at one across repeated ticks.
func TestEnforcementShouldWriteOneColdStorageWarningWhenThePoolStaysFullAcrossTicks(t *testing.T) {
	syncDir, cfg, cat, dbPath := setupWedgeEnv(t)
	seedSyncedFile(t, cat, syncDir, "a.jpg", 4000)
	seedSyncedFile(t, cat, syncDir, "b.jpg", 4000)
	ageFiles(t, cat, syncDir, "a.jpg", "b.jpg")

	d := newTestDaemon(t, cfg, cat, testLogger())
	declarePolicy(t, cfg, cat)
	d.SetSpaceCheck(func(string) (float64, error) { return 1.0, nil })

	for range 5 {
		d.EnforceQuotas()
	}

	if n := countEvents(t, dbPath, "cold_storage_low"); n != 1 {
		t.Errorf("cold_storage_low events = %d after 5 ticks with a full pool, want 1: the warning must be deduped, not written per tick", n)
	}
}
