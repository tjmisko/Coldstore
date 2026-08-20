//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func TestQuotaOffloadsOldestFirst(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 5000), withGracePeriod(0))

	// 3 files totaling 9000 bytes, budget is 5000
	times := []time.Duration{-3 * time.Hour, -2 * time.Hour, -1 * time.Hour}
	names := []string{"oldest.txt", "middle.txt", "newest.txt"}

	for i, name := range names {
		p := filepath.Join(env.syncDir, name)
		os.WriteFile(p, make([]byte, 3000), 0644)
		mtime := time.Now().Add(times[i])
		os.Chtimes(p, mtime, mtime)

		env.cat.UpsertFile(&catalog.FileRecord{
			FolderLabel: "TestFolder", RelativePath: name, Filename: name,
			Extension: ".txt", SizeBytes: 3000, FileMtime: mtime, Status: catalog.StatusSynced,
		}, time.Now().UnixNano())
	}

	env.daemon.SetSpaceCheck(func(string) (float64, error) { return 50.0, nil })
	env.daemon.EnforceQuotas()

	f1, _ := env.cat.GetFile("TestFolder", "oldest.txt")
	if f1.Status != catalog.StatusArchived {
		t.Errorf("oldest.txt status = %q, want archived", f1.Status)
	}

	f2, _ := env.cat.GetFile("TestFolder", "middle.txt")
	if f2.Status != catalog.StatusArchived {
		t.Errorf("middle.txt status = %q, want archived", f2.Status)
	}

	f3, _ := env.cat.GetFile("TestFolder", "newest.txt")
	if f3.Status != catalog.StatusSynced {
		t.Errorf("newest.txt status = %q, want synced", f3.Status)
	}
}

func TestQuotaRespectsGracePeriod(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1000), withGracePeriod(60))

	p := filepath.Join(env.syncDir, "recent.txt")
	os.WriteFile(p, make([]byte, 5000), 0644)
	// mtime is now — within grace period

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "recent.txt", Filename: "recent.txt",
		Extension: ".txt", SizeBytes: 5000, FileMtime: time.Now().UTC(), Status: catalog.StatusSynced,
	}, time.Now().UnixNano())

	env.daemon.SetSpaceCheck(func(string) (float64, error) { return 50.0, nil })
	env.daemon.EnforceQuotas()

	f, _ := env.cat.GetFile("TestFolder", "recent.txt")
	if f.Status != catalog.StatusSynced {
		t.Errorf("recent file status = %q, want synced (grace period)", f.Status)
	}
}

func TestQuotaRespectsPersistentPatterns(t *testing.T) {
	env := newTestEnv(t,
		withBudget("TestFolder", 1000),
		withGracePeriod(0),
		withPersistent("TestFolder", "Starred/*"),
	)

	starredDir := filepath.Join(env.syncDir, "Starred")
	os.MkdirAll(starredDir, 0755)
	old := time.Now().Add(-24 * time.Hour)

	p := filepath.Join(starredDir, "fav.jpg")
	os.WriteFile(p, make([]byte, 5000), 0644)
	os.Chtimes(p, old, old)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "Starred/fav.jpg", Filename: "fav.jpg",
		Extension: ".jpg", SizeBytes: 5000, FileMtime: old, Status: catalog.StatusSynced,
	}, time.Now().UnixNano())

	env.daemon.SetSpaceCheck(func(string) (float64, error) { return 50.0, nil })
	env.daemon.EnforceQuotas()

	f, _ := env.cat.GetFile("TestFolder", "Starred/fav.jpg")
	if f.Status != catalog.StatusSynced {
		t.Errorf("persistent file status = %q, want synced", f.Status)
	}
}

func TestQuotaMaxMtimeAtimeOrdering(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 3000), withGracePeriod(0))

	// File A: old mtime but recent atime — should be kept
	// File B: recent mtime but same age overall — should be kept
	// File C: old everything — should be offloaded

	oldTime := time.Now().Add(-24 * time.Hour)
	recentTime := time.Now().Add(-5 * time.Minute)

	files := []struct {
		name  string
		mtime time.Time
		atime time.Time
	}{
		{"old_mtime_recent_atime.txt", oldTime, recentTime},
		{"recent_mtime.txt", recentTime, recentTime},
		{"old_everything.txt", oldTime, oldTime},
	}

	for _, f := range files {
		p := filepath.Join(env.syncDir, f.name)
		os.WriteFile(p, make([]byte, 2000), 0644)
		os.Chtimes(p, f.atime, f.mtime)

		env.cat.UpsertFile(&catalog.FileRecord{
			FolderLabel: "TestFolder", RelativePath: f.name, Filename: f.name,
			Extension: ".txt", SizeBytes: 2000, FileMtime: f.mtime, FileAtime: f.atime,
			Status: catalog.StatusSynced,
		}, time.Now().UnixNano())
	}

	env.daemon.SetSpaceCheck(func(string) (float64, error) { return 50.0, nil })
	env.daemon.EnforceQuotas()

	// old_everything.txt should be offloaded first (lowest max(mtime, atime))
	f, _ := env.cat.GetFile("TestFolder", "old_everything.txt")
	if f.Status != catalog.StatusArchived {
		t.Errorf("old_everything.txt status = %q, want archived", f.Status)
	}
}

// A zero budget is an UNSET budget, not "archive everything". The old
// behaviour emptied a folder the moment its budget column was left at the
// default, which is the most destructive possible reading of a missing value.
func TestQuotaShouldOffloadNothingWhenTheBudgetIsUnset(t *testing.T) {
	env := newTestEnv(t, withGracePeriod(0))
	setPolicy(t, env, "TestFolder", int64Ptr(0), nil, nil)

	old := time.Now().Add(-2 * time.Hour)
	for i := 0; i < 3; i++ {
		name := "file" + padInt(i, 2) + ".txt"
		p := filepath.Join(env.syncDir, name)
		os.WriteFile(p, make([]byte, 1000), 0644)
		os.Chtimes(p, old, old)

		env.cat.UpsertFile(&catalog.FileRecord{
			FolderLabel: "TestFolder", RelativePath: name, Filename: name,
			Extension: ".txt", SizeBytes: 1000, FileMtime: old, Status: catalog.StatusSynced,
		}, time.Now().UnixNano())
	}

	env.daemon.SetSpaceCheck(func(string) (float64, error) { return 50.0, nil })
	env.daemon.EnforceQuotas()

	synced, _ := env.cat.ListFiles(catalog.FileQuery{Folder: "TestFolder", Status: catalog.StatusSynced, Limit: 100})
	if len(synced) != 3 {
		t.Errorf("with an unset budget, expected all 3 files still synced, got %d", len(synced))
	}

	// The folder was still looked at, so the UI does not report a dead daemon.
	fc, err := env.cat.FolderConfigByLabel("TestFolder")
	if err != nil {
		t.Fatal(err)
	}
	if fc.LastEnforcedAt == nil {
		t.Error("last_enforced_at should be stamped even when the budget is unset")
	}
}

func TestQuotaMultiFolderIndependence(t *testing.T) {
	env := newTestEnv(t, withMultiFolders(2), withGracePeriod(0))

	// Set different budgets, in the database, the way the UI would.
	setPolicy(t, env, "FolderA", int64Ptr(3000), nil, nil)
	setPolicy(t, env, "FolderB", int64Ptr(100000), nil, nil)

	old := time.Now().Add(-2 * time.Hour)

	// FolderA: over budget
	for i := 0; i < 3; i++ {
		name := "a" + padInt(i, 2) + ".txt"
		p := filepath.Join(env.cfg.Folders[0].Path, name)
		os.WriteFile(p, make([]byte, 2000), 0644)
		os.Chtimes(p, old, old)

		env.cat.UpsertFile(&catalog.FileRecord{
			FolderLabel: "FolderA", RelativePath: name, Filename: name,
			Extension: ".txt", SizeBytes: 2000, FileMtime: old, Status: catalog.StatusSynced,
		}, time.Now().UnixNano())
	}

	// FolderB: under budget
	for i := 0; i < 3; i++ {
		name := "b" + padInt(i, 2) + ".txt"
		p := filepath.Join(env.cfg.Folders[1].Path, name)
		os.WriteFile(p, make([]byte, 2000), 0644)
		os.Chtimes(p, old, old)

		env.cat.UpsertFile(&catalog.FileRecord{
			FolderLabel: "FolderB", RelativePath: name, Filename: name,
			Extension: ".txt", SizeBytes: 2000, FileMtime: old, Status: catalog.StatusSynced,
		}, time.Now().UnixNano())
	}

	env.daemon.SetSpaceCheck(func(string) (float64, error) { return 50.0, nil })
	env.daemon.EnforceQuotas()

	// FolderA should have offloaded some
	archivedA, _ := env.cat.ListFiles(catalog.FileQuery{Folder: "FolderA", Status: catalog.StatusArchived, Limit: 100})
	if len(archivedA) == 0 {
		t.Error("FolderA should have offloaded files")
	}

	// FolderB should have none offloaded
	archivedB, _ := env.cat.ListFiles(catalog.FileQuery{Folder: "FolderB", Status: catalog.StatusArchived, Limit: 100})
	if len(archivedB) != 0 {
		t.Errorf("FolderB should have 0 archived, got %d", len(archivedB))
	}
}
