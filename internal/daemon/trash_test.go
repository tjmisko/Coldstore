package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func TestTrashShouldRestoreSyncedFileToItsDeviceFolder(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "Trips/photo.jpg", 128)
	mover := newTestMover(t, cfg, cat)

	trashed, err := mover.TrashByID(file.ID)
	if err != nil {
		t.Fatalf("TrashByID: %v", err)
	}
	if trashed.Status != catalog.StatusTrashed || trashed.TrashedFrom != catalog.StatusSynced || trashed.TrashedAt == nil {
		t.Fatalf("trashed record = status %q, from %q, at %v", trashed.Status, trashed.TrashedFrom, trashed.TrashedAt)
	}
	if _, err := os.Stat(filepath.Join(syncDir, "Trips", "photo.jpg")); !os.IsNotExist(err) {
		t.Errorf("device copy still exists after trash: %v", err)
	}
	trashPath := filepath.Join(coldDir, "trash", "TestFolder", "Trips", "photo.jpg")
	if _, err := os.Stat(trashPath); err != nil {
		t.Fatalf("trash copy: %v", err)
	}

	restored, err := mover.RestoreTrashByID(file.ID)
	if err != nil {
		t.Fatalf("RestoreTrashByID: %v", err)
	}
	if restored.Status != catalog.StatusSynced || restored.TrashedFrom != "" || restored.TrashedAt != nil {
		t.Fatalf("restored record = status %q, from %q, at %v", restored.Status, restored.TrashedFrom, restored.TrashedAt)
	}
	if restored.Pinned {
		t.Error("restoring from trash must not add the archive-restore pin")
	}
	if _, err := os.Stat(filepath.Join(syncDir, "Trips", "photo.jpg")); err != nil {
		t.Errorf("restored device copy: %v", err)
	}
	if _, err := os.Stat(trashPath); !os.IsNotExist(err) {
		t.Errorf("trash copy still exists after restore: %v", err)
	}
}

func TestTrashShouldRestoreArchivedFileBackToTheArchive(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "Trips/old.jpg", 96)
	mover := newTestMover(t, cfg, cat)
	if _, err := mover.ArchiveByID(file.ID); err != nil {
		t.Fatalf("ArchiveByID: %v", err)
	}

	trashed, err := mover.TrashByID(file.ID)
	if err != nil {
		t.Fatalf("TrashByID: %v", err)
	}
	if trashed.TrashedFrom != catalog.StatusArchived {
		t.Fatalf("trashed_from = %q, want archived", trashed.TrashedFrom)
	}
	archivePath := filepath.Join(coldDir, "archive", "TestFolder", "Trips", "old.jpg")
	trashPath := filepath.Join(coldDir, "trash", "TestFolder", "Trips", "old.jpg")
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Errorf("archive copy still exists after trash: %v", err)
	}
	if _, err := os.Stat(trashPath); err != nil {
		t.Fatalf("trash copy: %v", err)
	}

	restored, err := mover.RestoreTrashByID(file.ID)
	if err != nil {
		t.Fatalf("RestoreTrashByID: %v", err)
	}
	if restored.Status != catalog.StatusArchived {
		t.Fatalf("status = %q, want archived", restored.Status)
	}
	if _, err := os.Stat(archivePath); err != nil {
		t.Errorf("restored archive copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(syncDir, "Trips", "old.jpg")); !os.IsNotExist(err) {
		t.Errorf("archived trash restore unexpectedly recreated a device copy: %v", err)
	}
}

func TestPermanentDeleteShouldOnlyAcceptTrashAndRemoveItsCatalogRow(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "photo.jpg", 64)
	mover := newTestMover(t, cfg, cat)

	if err := mover.DeleteByID(file.ID); !errors.Is(err, ErrFileStateChanged) {
		t.Fatalf("DeleteByID(active) error = %v, want ErrFileStateChanged", err)
	}
	if _, err := os.Stat(filepath.Join(syncDir, "photo.jpg")); err != nil {
		t.Fatalf("rejected delete removed active file: %v", err)
	}

	thumbRel := filepath.Join("TestFolder", "photo.jpg.webp")
	thumbPath := filepath.Join(coldDir, "thumbnails", thumbRel)
	if err := os.MkdirAll(filepath.Dir(thumbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thumbPath, []byte("thumb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateThumbnailPath(file.ID, thumbRel); err != nil {
		t.Fatal(err)
	}
	if _, err := mover.TrashByID(file.ID); err != nil {
		t.Fatalf("TrashByID: %v", err)
	}
	trashPath := filepath.Join(coldDir, "trash", "TestFolder", "photo.jpg")

	if err := mover.DeleteByID(file.ID); err != nil {
		t.Fatalf("DeleteByID(trashed): %v", err)
	}
	if _, err := cat.GetFileByID(file.ID); !errors.Is(err, catalog.ErrNoSuchFile) {
		t.Fatalf("GetFileByID error = %v, want ErrNoSuchFile", err)
	}
	if _, err := os.Stat(trashPath); !os.IsNotExist(err) {
		t.Errorf("trash bytes still exist after permanent delete: %v", err)
	}
	if _, err := os.Stat(thumbPath); !os.IsNotExist(err) {
		t.Errorf("thumbnail still exists after permanent delete: %v", err)
	}
}
