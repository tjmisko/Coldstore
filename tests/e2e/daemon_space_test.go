//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func TestLowDiskPausesOffload(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1000), withGracePeriod(0))

	old := time.Now().Add(-2 * time.Hour)
	p := filepath.Join(env.syncDir, "big.txt")
	os.WriteFile(p, make([]byte, 5000), 0644)
	os.Chtimes(p, old, old)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "big.txt", Filename: "big.txt",
		Extension: ".txt", SizeBytes: 5000, FileMtime: old, Status: catalog.StatusSynced,
	}, time.Now().UnixNano())

	// Inject low disk space
	env.daemon.SetSpaceCheck(func(string) (float64, error) {
		return 3.0, nil // below 5% threshold
	})

	env.daemon.EnforceQuotas()

	file, _ := env.cat.GetFile("TestFolder", "big.txt")
	if file.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced (low disk should pause offload)", file.Status)
	}
}

func TestOffloadResumesWhenSpaceAvailable(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1000), withGracePeriod(0))

	old := time.Now().Add(-2 * time.Hour)
	p := filepath.Join(env.syncDir, "resume.txt")
	os.WriteFile(p, make([]byte, 5000), 0644)
	os.Chtimes(p, old, old)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "resume.txt", Filename: "resume.txt",
		Extension: ".txt", SizeBytes: 5000, FileMtime: old, Status: catalog.StatusSynced,
	}, time.Now().UnixNano())

	// First: low disk — should not offload
	env.daemon.SetSpaceCheck(func(string) (float64, error) {
		return 2.0, nil
	})
	env.daemon.EnforceQuotas()

	file, _ := env.cat.GetFile("TestFolder", "resume.txt")
	if file.Status != catalog.StatusSynced {
		t.Fatal("should not offload with low disk")
	}

	// Second: space available — should offload
	env.daemon.SetSpaceCheck(func(string) (float64, error) {
		return 50.0, nil
	})
	env.daemon.EnforceQuotas()

	file, _ = env.cat.GetFile("TestFolder", "resume.txt")
	if file.Status != catalog.StatusArchived {
		t.Errorf("status = %q, want archived (space available)", file.Status)
	}
}
