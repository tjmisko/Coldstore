//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/thumbnail"
)

func TestThumbnailJPEG(t *testing.T) {
	env := newTestEnv(t)

	p := filepath.Join(env.syncDir, "photo.jpg")
	genImageFile(t, p, ".jpg")
	info, _ := os.Stat(p)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "photo.jpg", Filename: "photo.jpg",
		Extension: ".jpg", SizeBytes: info.Size(), FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())

	gen := thumbnail.NewGenerator(env.coldDir, env.cat, env.logger)
	files, _ := env.cat.FilesNeedingThumbnails([]string{".jpg", ".jpeg", ".png"}, 200)
	if len(files) == 0 {
		t.Fatal("expected files needing thumbnails")
	}

	sourcePathFn := func(f *catalog.FileRecord) string {
		return filepath.Join(env.syncDir, f.RelativePath)
	}
	gen.ProcessQueue(files, sourcePathFn, 1)

	file, _ := env.cat.GetFile("TestFolder", "photo.jpg")
	if file.ThumbnailPath == "" {
		t.Error("thumbnail_path should be set after generation")
	}

	thumbFull := filepath.Join(env.coldDir, "thumbnails", file.ThumbnailPath)
	if _, err := os.Stat(thumbFull); err != nil {
		t.Errorf("thumbnail file should exist at %s: %v", thumbFull, err)
	}
}

func TestThumbnailPNG(t *testing.T) {
	env := newTestEnv(t)

	p := filepath.Join(env.syncDir, "icon.png")
	genImageFile(t, p, ".png")
	info, _ := os.Stat(p)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "icon.png", Filename: "icon.png",
		Extension: ".png", SizeBytes: info.Size(), FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())

	gen := thumbnail.NewGenerator(env.coldDir, env.cat, env.logger)
	files, _ := env.cat.FilesNeedingThumbnails([]string{".jpg", ".jpeg", ".png"}, 200)
	sourcePathFn := func(f *catalog.FileRecord) string {
		return filepath.Join(env.syncDir, f.RelativePath)
	}
	gen.ProcessQueue(files, sourcePathFn, 1)

	file, _ := env.cat.GetFile("TestFolder", "icon.png")
	if file.ThumbnailPath == "" {
		t.Error("thumbnail_path should be set for PNG")
	}
}

func TestThumbnailVideo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available, skipping video thumbnail test")
	}

	env := newTestEnv(t)

	// Copy the minimal.mp4 fixture
	fixture, err := os.ReadFile("../../tests/fixtures/minimal.mp4")
	if err != nil {
		t.Skip("minimal.mp4 fixture not found:", err)
	}

	p := filepath.Join(env.syncDir, "clip.mp4")
	os.WriteFile(p, fixture, 0644)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "clip.mp4", Filename: "clip.mp4",
		Extension: ".mp4", SizeBytes: int64(len(fixture)), FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())

	gen := thumbnail.NewGenerator(env.coldDir, env.cat, env.logger)
	files, _ := env.cat.FilesNeedingThumbnails([]string{".mp4", ".mov"}, 200)
	if len(files) == 0 {
		t.Fatal("expected video file needing thumbnail")
	}

	sourcePathFn := func(f *catalog.FileRecord) string {
		return filepath.Join(env.syncDir, f.RelativePath)
	}
	gen.ProcessQueue(files, sourcePathFn, 1)

	file, _ := env.cat.GetFile("TestFolder", "clip.mp4")
	if file.ThumbnailPath == "" {
		t.Error("thumbnail_path should be set for video")
	}
}

func TestThumbnailArchivedFile(t *testing.T) {
	env := newTestEnv(t)

	// Put a JPEG directly in the archive
	archiveDir := filepath.Join(env.coldDir, "archive", "TestFolder")
	os.MkdirAll(archiveDir, 0755)
	p := filepath.Join(archiveDir, "archived.jpg")
	genImageFile(t, p, ".jpg")
	info, _ := os.Stat(p)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "archived.jpg", Filename: "archived.jpg",
		Extension: ".jpg", SizeBytes: info.Size(), FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())
	file, _ := env.cat.GetFile("TestFolder", "archived.jpg")
	env.cat.UpdateFileStatus(file.ID, catalog.StatusArchived)

	gen := thumbnail.NewGenerator(env.coldDir, env.cat, env.logger)
	files, _ := env.cat.FilesNeedingThumbnails([]string{".jpg", ".jpeg", ".png"}, 200)

	sourcePathFn := func(f *catalog.FileRecord) string {
		if f.Status == catalog.StatusArchived {
			return filepath.Join(env.coldDir, "archive", f.FolderLabel, f.RelativePath)
		}
		return filepath.Join(env.syncDir, f.RelativePath)
	}
	gen.ProcessQueue(files, sourcePathFn, 1)

	file, _ = env.cat.GetFile("TestFolder", "archived.jpg")
	if file.ThumbnailPath == "" {
		t.Error("archived file should get a thumbnail")
	}
}

func TestThumbnailIdempotent(t *testing.T) {
	env := newTestEnv(t)

	p := filepath.Join(env.syncDir, "idem.jpg")
	genImageFile(t, p, ".jpg")
	info, _ := os.Stat(p)

	env.cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "idem.jpg", Filename: "idem.jpg",
		Extension: ".jpg", SizeBytes: info.Size(), FileMtime: time.Now().UTC(),
		Status: catalog.StatusSynced,
	}, time.Now().UnixNano())

	gen := thumbnail.NewGenerator(env.coldDir, env.cat, env.logger)
	sourcePathFn := func(f *catalog.FileRecord) string {
		return filepath.Join(env.syncDir, f.RelativePath)
	}

	// Generate once
	files, _ := env.cat.FilesNeedingThumbnails([]string{".jpg", ".jpeg", ".png"}, 200)
	gen.ProcessQueue(files, sourcePathFn, 1)

	file, _ := env.cat.GetFile("TestFolder", "idem.jpg")
	firstThumb := file.ThumbnailPath

	// Second run: should find no files needing thumbnails
	files, _ = env.cat.FilesNeedingThumbnails([]string{".jpg", ".jpeg", ".png"}, 200)
	if len(files) != 0 {
		t.Errorf("expected 0 files needing thumbnails on second run, got %d", len(files))
	}

	file, _ = env.cat.GetFile("TestFolder", "idem.jpg")
	if file.ThumbnailPath != firstThumb {
		t.Error("thumbnail path should not change on re-run")
	}
}
