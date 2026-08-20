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

func TestRestoreRoundTrip(t *testing.T) {
	env := newTestEnv(t)

	content := []byte("restore round-trip content")
	p := filepath.Join(env.syncDir, "restore_me.txt")
	os.WriteFile(p, content, 0644)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "restore_me.txt", Filename: "restore_me.txt",
		Extension: ".txt", SizeBytes: int64(len(content)), FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := env.cat.GetFile("TestFolder", "restore_me.txt")

	// Offload
	env.archive(t, file.ID)

	// Verify source gone
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("source should be gone after offload")
	}

	// Restore
	file, _ = env.cat.GetFile("TestFolder", "restore_me.txt")
	env.restore(t, file.ID)

	// Verify content
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal("restored file should exist:", err)
	}
	if string(got) != string(content) {
		t.Error("restored file content mismatch")
	}

	// Verify catalog
	file, _ = env.cat.GetFile("TestFolder", "restore_me.txt")
	if file.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", file.Status)
	}
}

func TestRestoreCleansEmptyArchiveDirs(t *testing.T) {
	env := newTestEnv(t)

	deep := filepath.Join(env.syncDir, "x", "y")
	os.MkdirAll(deep, 0755)
	os.WriteFile(filepath.Join(deep, "only.txt"), []byte("only file"), 0644)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "x/y/only.txt", Filename: "only.txt",
		Extension: ".txt", SizeBytes: 9, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := env.cat.GetFile("TestFolder", "x/y/only.txt")

	env.archive(t, file.ID)

	// Restore
	file, _ = env.cat.GetFile("TestFolder", "x/y/only.txt")
	env.restore(t, file.ID)

	// Archive subdirectories should be cleaned up
	archiveSubdir := filepath.Join(env.coldDir, "archive", "TestFolder", "x", "y")
	if _, err := os.Stat(archiveSubdir); !os.IsNotExist(err) {
		t.Error("empty archive subdirectories should be cleaned up")
	}
}

func TestRestoreShouldMoveNothingWhenTheFileIsAlreadySynced(t *testing.T) {
	env := newTestEnv(t)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "synced.txt", Filename: "synced.txt",
		Extension: ".txt", SizeBytes: 100, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := env.cat.GetFile("TestFolder", "synced.txt")

	// Invariant 12: a restore of a file that is already where the owner wants
	// it is a no-op SUCCESS, not an error. There is nothing to report and
	// nothing to undo.
	got, err := env.mover.RestoreByID(file.ID)
	if err != nil {
		t.Fatalf("restoring an already-synced file should succeed: %v", err)
	}
	if got.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", got.Status)
	}
	if got.Pinned {
		t.Error("a no-op restore must not pin: nothing was restored")
	}
}

func TestRestoreAPIRoundTrip(t *testing.T) {
	env := newTestEnv(t)

	content := []byte("api restore content")
	p := filepath.Join(env.syncDir, "api_restore.txt")
	os.WriteFile(p, content, 0644)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "api_restore.txt", Filename: "api_restore.txt",
		Extension: ".txt", SizeBytes: int64(len(content)), FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := env.cat.GetFile("TestFolder", "api_restore.txt")

	// Offload first
	env.archive(t, file.ID)

	// Restore via API
	file, _ = env.cat.GetFile("TestFolder", "api_restore.txt")
	req := httptest.NewRequest("POST", "/api/v1/files/"+strconv.FormatInt(file.ID, 10)+"/restore", nil)
	w := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	// Verify file is back
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal("file should be restored:", err)
	}
	if string(got) != string(content) {
		t.Error("restored content mismatch")
	}

	// Verify API response shows synced
	var resp catalog.FileRecord
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != catalog.StatusSynced {
		t.Errorf("API response status = %q, want synced", resp.Status)
	}
}

func TestRestoredFileShouldStayOnDeviceWhenTheNextQuotaTickRuns(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 10), withGracePeriod(0))

	content := make([]byte, 2000)
	p := filepath.Join(env.syncDir, "wanted.txt")
	os.WriteFile(p, content, 0644)
	old := time.Now().Add(-24 * time.Hour)
	os.Chtimes(p, old, old)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "wanted.txt", Filename: "wanted.txt",
		Extension: ".txt", SizeBytes: int64(len(content)), FileMtime: old, FileAtime: old,
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := env.cat.GetFile("TestFolder", "wanted.txt")

	env.archive(t, file.ID)
	file, _ = env.cat.GetFile("TestFolder", "wanted.txt")
	env.restore(t, file.ID)

	file, _ = env.cat.GetFile("TestFolder", "wanted.txt")
	if !file.Pinned {
		t.Fatal("a restored file must be pinned, or the daemon takes it straight back")
	}
	if file.PinSource != catalog.PinSourceRestore {
		t.Errorf("pin_source = %q, want %q", file.PinSource, catalog.PinSourceRestore)
	}

	// The folder is far over budget and the file is well outside the grace
	// window, so only the pin can save it.
	env.daemon.SetSpaceCheck(func(string) (float64, error) { return 90.0, nil })
	env.daemon.EnforceQuotas()

	file, _ = env.cat.GetFile("TestFolder", "wanted.txt")
	if file.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced: the restore was undone by the next tick", file.Status)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("the restored file must still be on disk: %v", err)
	}
}
