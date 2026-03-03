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
	})
	file, _ := env.cat.GetFile("TestFolder", "restore_me.txt")

	// Offload
	if err := env.offloader.Offload(file, env.syncDir); err != nil {
		t.Fatal(err)
	}

	// Verify source gone
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("source should be gone after offload")
	}

	// Restore
	file, _ = env.cat.GetFile("TestFolder", "restore_me.txt")
	if err := env.restorer.Restore(file); err != nil {
		t.Fatal(err)
	}

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
	})
	file, _ := env.cat.GetFile("TestFolder", "x/y/only.txt")

	env.offloader.Offload(file, env.syncDir)

	// Restore
	file, _ = env.cat.GetFile("TestFolder", "x/y/only.txt")
	env.restorer.Restore(file)

	// Archive subdirectories should be cleaned up
	archiveSubdir := filepath.Join(env.coldDir, "archive", "TestFolder", "x", "y")
	if _, err := os.Stat(archiveSubdir); !os.IsNotExist(err) {
		t.Error("empty archive subdirectories should be cleaned up")
	}
}

func TestRestoreRejectsNonArchived(t *testing.T) {
	env := newTestEnv(t)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "synced.txt", Filename: "synced.txt",
		Extension: ".txt", SizeBytes: 100, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	})
	file, _ := env.cat.GetFile("TestFolder", "synced.txt")

	err := env.restorer.Restore(file)
	if err == nil {
		t.Error("restoring a synced file should fail")
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
	})
	file, _ := env.cat.GetFile("TestFolder", "api_restore.txt")

	// Offload first
	env.offloader.Offload(file, env.syncDir)

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
