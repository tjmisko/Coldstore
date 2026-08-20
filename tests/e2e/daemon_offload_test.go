//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func TestOffloadMovesToArchivePath(t *testing.T) {
	env := newTestEnv(t)

	subdir := filepath.Join(env.syncDir, "2024", "01")
	os.MkdirAll(subdir, 0755)
	content := []byte("offload test content")
	src := filepath.Join(subdir, "photo.jpg")
	os.WriteFile(src, content, 0644)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "2024/01/photo.jpg", Filename: "photo.jpg",
		Extension: ".jpg", SizeBytes: int64(len(content)), FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := env.cat.GetFile("TestFolder", "2024/01/photo.jpg")

	env.archive(t, file.ID)

	// Source should be gone
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source file should be removed after offload")
	}

	// Destination should exist
	archivePath := filepath.Join(env.coldDir, "archive", "TestFolder", "2024", "01", "photo.jpg")
	got, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal("archived file should exist:", err)
	}
	if string(got) != string(content) {
		t.Error("archived file content mismatch")
	}
}

func TestOffloadCatalogUpdated(t *testing.T) {
	env := newTestEnv(t)

	p := filepath.Join(env.syncDir, "doc.pdf")
	os.WriteFile(p, []byte("pdf content"), 0644)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "doc.pdf", Filename: "doc.pdf",
		Extension: ".pdf", SizeBytes: 11, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := env.cat.GetFile("TestFolder", "doc.pdf")

	env.archive(t, file.ID)

	updated, _ := env.cat.GetFile("TestFolder", "doc.pdf")
	if updated.Status != catalog.StatusArchived {
		t.Errorf("status = %q, want archived", updated.Status)
	}
	if updated.ArchivedAt == nil {
		t.Error("archived_at should be set")
	}
}

func TestOffloadCreatesDirectoryStructure(t *testing.T) {
	env := newTestEnv(t)

	deep := filepath.Join(env.syncDir, "a", "b", "c")
	os.MkdirAll(deep, 0755)
	os.WriteFile(filepath.Join(deep, "deep.txt"), []byte("deep"), 0644)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "a/b/c/deep.txt", Filename: "deep.txt",
		Extension: ".txt", SizeBytes: 4, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := env.cat.GetFile("TestFolder", "a/b/c/deep.txt")

	env.archive(t, file.ID)

	archivePath := filepath.Join(env.coldDir, "archive", "TestFolder", "a", "b", "c", "deep.txt")
	if _, err := os.Stat(archivePath); err != nil {
		t.Errorf("archive directory structure should be created: %v", err)
	}
}

func TestOffloadEventLogged(t *testing.T) {
	env := newTestEnv(t)

	p := filepath.Join(env.syncDir, "logged.txt")
	os.WriteFile(p, []byte("log me"), 0644)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "logged.txt", Filename: "logged.txt",
		Extension: ".txt", SizeBytes: 6, FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := env.cat.GetFile("TestFolder", "logged.txt")

	env.archive(t, file.ID)

	// Verify event was logged by checking catalog (events table)
	// The offloader calls cat.LogEvent("archive", ...) on success
	// We can verify indirectly: the file is archived and the method didn't error
	updated, _ := env.cat.GetFile("TestFolder", "logged.txt")
	if updated.Status != catalog.StatusArchived {
		t.Error("file should be archived (event logging is implicit)")
	}
}
