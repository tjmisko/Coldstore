//go:build e2e

package e2e

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

// seedOldFiles catalogs n files of size bytes each, all old enough to be past
// any grace period, and writes them to disk so the on-disk mtime re-check
// agrees with the catalog.
func seedOldFiles(t *testing.T, env *testEnv, folder, dir, prefix string, n, size int) {
	t.Helper()
	old := time.Now().Add(-24 * time.Hour)
	for i := 0; i < n; i++ {
		name := prefix + padInt(i, 2) + ".txt"
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		if _, err := env.cat.UpsertFile(&catalog.FileRecord{
			FolderLabel: folder, RelativePath: name, Filename: name,
			Extension: ".txt", SizeBytes: int64(size),
			FileMtime: old, FileAtime: old, Status: catalog.StatusSynced,
		}, time.Now().UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
}

func archivedCount(t *testing.T, env *testEnv, folder string) int {
	t.Helper()
	files, err := env.cat.ListFiles(catalog.FileQuery{
		Folder: folder, Status: catalog.StatusArchived, Limit: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	return len(files)
}

// The acceptance test for D1: a budget edited through the API changes what the
// daemon enforces on the very next tick, with no restart and no reload.
func TestEnforcementShouldUseTheNewBudgetWhenItIsEditedThroughTheAPI(t *testing.T) {
	env := newTestEnv(t, withGracePeriod(0))
	env.daemon.SetSpaceCheck(func(string) (float64, error) { return 50.0, nil })

	// 5 x 2000 = 10000 bytes against the TOML budget of 50000: nothing to do.
	seedOldFiles(t, env, "TestFolder", env.syncDir, "p", 5, 2000)

	env.daemon.EnforceQuotas()
	if got := archivedCount(t, env, "TestFolder"); got != 0 {
		t.Fatalf("archived %d files under the seeded 50000 budget, want 0", got)
	}

	// The owner tightens the budget in the UI. No restart happens.
	putFolder(t, env, "TestFolder", `{"budget_bytes":4000}`, http.StatusOK)

	env.daemon.EnforceQuotas()

	onDevice, _, err := env.cat.FolderSizes("TestFolder")
	if err != nil {
		t.Fatal(err)
	}
	if onDevice > 4000 {
		t.Errorf("on-device footprint = %d after the tick, want <= the new budget of 4000", onDevice)
	}
	if got := archivedCount(t, env, "TestFolder"); got != 3 {
		t.Errorf("archived %d files, want 3 (10000 -> 4000 at 2000 bytes each)", got)
	}
}

func TestEnforcementShouldSkipTheFolderWhenItIsDisabledThroughTheAPI(t *testing.T) {
	env := newTestEnv(t, withGracePeriod(0))
	env.daemon.SetSpaceCheck(func(string) (float64, error) { return 50.0, nil })

	seedOldFiles(t, env, "TestFolder", env.syncDir, "p", 5, 2000)
	putFolder(t, env, "TestFolder", `{"budget_bytes":1000,"enabled":false}`, http.StatusOK)

	env.daemon.EnforceQuotas()

	if got := archivedCount(t, env, "TestFolder"); got != 0 {
		t.Errorf("archived %d files, want 0: a disabled folder is not enforced", got)
	}
}

func TestEnforcementShouldSkipTheFolderWhenItHasDisappearedFromTOML(t *testing.T) {
	env := newTestEnv(t, withGracePeriod(0))
	env.daemon.SetSpaceCheck(func(string) (float64, error) { return 50.0, nil })

	seedOldFiles(t, env, "TestFolder", env.syncDir, "p", 5, 2000)
	setPolicy(t, env, "TestFolder", int64Ptr(1000), nil, nil)

	// The owner removed the folder from coldstore.toml and restarted.
	if err := env.cat.MarkFoldersAbsentFromTOML(nil); err != nil {
		t.Fatal(err)
	}

	env.daemon.EnforceQuotas()

	if got := archivedCount(t, env, "TestFolder"); got != 0 {
		t.Errorf("archived %d files, want 0: an orphaned folder is not enforced", got)
	}

	// It is still visible, and still flagged, rather than gone.
	folders := getFolders(t, env)
	if len(folders) != 1 || folders[0].PresentInTOML {
		t.Errorf("folders = %+v, want one row flagged present_in_toml=false", folders)
	}
}

func TestScanShouldSkipTheFolderWhenItIsDisabled(t *testing.T) {
	env := newTestEnv(t)

	if err := os.WriteFile(filepath.Join(env.syncDir, "new.txt"), make([]byte, 10), 0644); err != nil {
		t.Fatal(err)
	}
	enabled := false
	setPolicy(t, env, "TestFolder", nil, nil, &enabled)

	if err := env.scanner.ScanAll(); err != nil {
		t.Fatal(err)
	}

	count, err := env.cat.FileCount("")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("catalogued %d files, want 0: a disabled folder is not walked", count)
	}
}
