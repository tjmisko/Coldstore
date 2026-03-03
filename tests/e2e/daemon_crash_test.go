//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func TestCrashRecoveryArchivingWithArchiveExists(t *testing.T) {
	env := newTestEnv(t)

	// Simulate crash: file is in "archiving" state and exists in archive
	archivePath := filepath.Join(env.coldDir, "archive", "TestFolder")
	os.MkdirAll(archivePath, 0755)
	os.WriteFile(filepath.Join(archivePath, "crashed.txt"), []byte("archived"), 0644)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "crashed.txt", Filename: "crashed.txt",
		Extension: ".txt", SizeBytes: 8, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	})
	file, _ := env.cat.GetFile("TestFolder", "crashed.txt")
	env.cat.UpdateFileStatus(file.ID, catalog.StatusArchiving)

	// ScanAll runs recoverArchiving
	env.scanner.ScanAll()

	file, _ = env.cat.GetFile("TestFolder", "crashed.txt")
	if file.Status != catalog.StatusArchived {
		t.Errorf("status = %q, want archived (archive exists)", file.Status)
	}
}

func TestCrashRecoveryArchivingNoArchive(t *testing.T) {
	env := newTestEnv(t)

	// Simulate crash: file is in "archiving" state but NOT in archive
	// (the move never completed) — and the source still exists
	os.WriteFile(filepath.Join(env.syncDir, "incomplete.txt"), []byte("still here"), 0644)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "incomplete.txt", Filename: "incomplete.txt",
		Extension: ".txt", SizeBytes: 10, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	})
	file, _ := env.cat.GetFile("TestFolder", "incomplete.txt")
	env.cat.UpdateFileStatus(file.ID, catalog.StatusArchiving)

	env.scanner.ScanAll()

	file, _ = env.cat.GetFile("TestFolder", "incomplete.txt")
	if file.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced (archive doesn't exist)", file.Status)
	}
}

func TestCrashRecoveryMultipleArchivingFiles(t *testing.T) {
	env := newTestEnv(t)

	archivePath := filepath.Join(env.coldDir, "archive", "TestFolder")
	os.MkdirAll(archivePath, 0755)

	// File 1: archive exists — should become archived
	os.WriteFile(filepath.Join(archivePath, "a.txt"), []byte("a"), 0644)
	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "a.txt", Filename: "a.txt",
		Extension: ".txt", SizeBytes: 1, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	})
	fa, _ := env.cat.GetFile("TestFolder", "a.txt")
	env.cat.UpdateFileStatus(fa.ID, catalog.StatusArchiving)

	// File 2: no archive — should revert to synced
	os.WriteFile(filepath.Join(env.syncDir, "b.txt"), []byte("b"), 0644)
	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "b.txt", Filename: "b.txt",
		Extension: ".txt", SizeBytes: 1, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	})
	fb, _ := env.cat.GetFile("TestFolder", "b.txt")
	env.cat.UpdateFileStatus(fb.ID, catalog.StatusArchiving)

	env.scanner.ScanAll()

	fa, _ = env.cat.GetFile("TestFolder", "a.txt")
	if fa.Status != catalog.StatusArchived {
		t.Errorf("a.txt status = %q, want archived", fa.Status)
	}

	fb, _ = env.cat.GetFile("TestFolder", "b.txt")
	if fb.Status != catalog.StatusSynced {
		t.Errorf("b.txt status = %q, want synced", fb.Status)
	}
}
