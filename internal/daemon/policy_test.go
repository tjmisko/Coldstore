package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

// seedAgedFile writes a file to disk and catalogs it with the given age, so
// that both the catalog record and the on-disk mtime re-check agree.
func seedAgedFile(t *testing.T, cat *catalog.Catalog, dir, name string, size int, age time.Duration) {
	t.Helper()
	at := time.Now().Add(-age)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: name, Filename: name,
		Extension: ".txt", SizeBytes: int64(size),
		FileMtime: at, FileAtime: at, Status: catalog.StatusSynced,
	}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
}

func TestEnforcementShouldUseTheFoldersOwnGracePeriodWhenItDiffersFromTheTOMLDefault(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	cfg.Folders[0].BudgetBytes = 1000
	cfg.Daemon.GracePeriodMinutes = 0
	declarePolicy(t, cfg, cat)

	// The owner raises grace to two hours in the UI. No restart.
	grace := 120
	if found, err := cat.UpdateFolderPolicy("TestFolder", nil, &grace, nil); err != nil || !found {
		t.Fatalf("UpdateFolderPolicy = %v, %v", found, err)
	}

	seedAgedFile(t, cat, syncDir, "recent.txt", 5000, 30*time.Minute)

	d := newTestDaemon(t, cfg, cat, testLogger())
	d.SetSpaceCheck(func(string) (float64, error) { return 90.0, nil })
	d.EnforceQuotas()

	f, err := cat.GetFile("TestFolder", "recent.txt")
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced: the folder's 120-minute grace comes from the database",
			f.Status)
	}
}

func TestEnforcementShouldRecordTheEnforcementTimeWhenTheFolderIsAlreadyWithinBudget(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	cfg.Folders[0].BudgetBytes = 1_000_000
	declarePolicy(t, cfg, cat)

	seedAgedFile(t, cat, syncDir, "small.txt", 10, 24*time.Hour)

	before, err := cat.FolderConfigByLabel("TestFolder")
	if err != nil {
		t.Fatal(err)
	}
	if before.LastEnforcedAt != nil {
		t.Fatalf("last_enforced_at = %v before any tick, want null", *before.LastEnforcedAt)
	}

	d := newTestDaemon(t, cfg, cat, testLogger())
	d.SetSpaceCheck(func(string) (float64, error) { return 90.0, nil })
	d.EnforceQuotas()

	after, err := cat.FolderConfigByLabel("TestFolder")
	if err != nil {
		t.Fatal(err)
	}
	if after.LastEnforcedAt == nil {
		t.Error("last_enforced_at is still null: a folder that needed nothing was still checked")
	}
}

func TestEnforcementShouldOffloadNothingWhenTheBudgetIsUnset(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	cfg.Folders[0].BudgetBytes = 1000
	cfg.Daemon.GracePeriodMinutes = 0
	declarePolicy(t, cfg, cat)

	budget := int64(0)
	if found, err := cat.UpdateFolderPolicy("TestFolder", &budget, nil, nil); err != nil || !found {
		t.Fatalf("UpdateFolderPolicy = %v, %v", found, err)
	}

	seedAgedFile(t, cat, syncDir, "old.txt", 5000, 24*time.Hour)

	d := newTestDaemon(t, cfg, cat, testLogger())
	d.SetSpaceCheck(func(string) (float64, error) { return 90.0, nil })
	d.EnforceQuotas()

	f, err := cat.GetFile("TestFolder", "old.txt")
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced: a zero budget means unset, not 'archive everything'", f.Status)
	}

	fc, err := cat.FolderConfigByLabel("TestFolder")
	if err != nil {
		t.Fatal(err)
	}
	if fc.LastEnforcedAt == nil {
		t.Error("last_enforced_at should be stamped even for a folder that was skipped")
	}
}

func TestEnforcementShouldIgnoreTheTOMLBudgetWhenTheDatabaseDisagrees(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	cfg.Folders[0].BudgetBytes = 50_000
	cfg.Daemon.GracePeriodMinutes = 0
	declarePolicy(t, cfg, cat)

	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		seedAgedFile(t, cat, syncDir, name, 2000, 24*time.Hour)
	}

	budget := int64(2500)
	if found, err := cat.UpdateFolderPolicy("TestFolder", &budget, nil, nil); err != nil || !found {
		t.Fatalf("UpdateFolderPolicy = %v, %v", found, err)
	}

	d := newTestDaemon(t, cfg, cat, testLogger())
	d.SetSpaceCheck(func(string) (float64, error) { return 90.0, nil })
	d.EnforceQuotas()

	onDevice, _, err := cat.FolderSizes("TestFolder")
	if err != nil {
		t.Fatal(err)
	}
	if onDevice > 2500 {
		t.Errorf("on-device footprint = %d, want <= 2500: enforcement reads the database, not cfg", onDevice)
	}
}

func TestEnforcementShouldSkipTheFolderWhenItIsNoLongerDeclaredInTOML(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	cfg.Folders[0].BudgetBytes = 1000
	cfg.Daemon.GracePeriodMinutes = 0
	declarePolicy(t, cfg, cat)

	seedAgedFile(t, cat, syncDir, "old.txt", 5000, 24*time.Hour)

	if err := cat.MarkFoldersAbsentFromTOML(nil); err != nil {
		t.Fatal(err)
	}

	d := newTestDaemon(t, cfg, cat, testLogger())
	d.SetSpaceCheck(func(string) (float64, error) { return 90.0, nil })
	d.EnforceQuotas()

	f, err := cat.GetFile("TestFolder", "old.txt")
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced: an orphaned folder is not enforced", f.Status)
	}
}

func TestEnforcementShouldFollowTheFolderWhenTOMLMovesItToANewPath(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	cfg.Folders[0].BudgetBytes = 1000
	cfg.Daemon.GracePeriodMinutes = 0

	moved := newSyncthingFolder(t, filepath.Join(t.TempDir(), "moved"))
	seedAgedFile(t, cat, moved, "old.txt", 5000, 24*time.Hour)

	// The old location still holds a stale copy; enforcement must not use it.
	if err := os.WriteFile(filepath.Join(syncDir, "old.txt"), make([]byte, 5000), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg.Folders[0].Path = moved
	declarePolicy(t, cfg, cat)

	d := newTestDaemon(t, cfg, cat, testLogger())
	d.SetSpaceCheck(func(string) (float64, error) { return 90.0, nil })
	d.EnforceQuotas()

	f, err := cat.GetFile("TestFolder", "old.txt")
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != catalog.StatusArchived {
		t.Fatalf("status = %q, want archived", f.Status)
	}
	if _, err := os.Stat(filepath.Join(moved, "old.txt")); !os.IsNotExist(err) {
		t.Error("the file at the folder's current path should have been moved out")
	}
	if _, err := os.Stat(filepath.Join(syncDir, "old.txt")); err != nil {
		t.Error("the stale copy at the old path should not have been touched")
	}
}
