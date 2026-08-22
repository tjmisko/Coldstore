package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

func TestMigrateFolderLabelShouldMoveArchiveAndThumbnailRoots(t *testing.T) {
	cat, _ := openCatalog(t)
	if _, err := cat.ReconcileFolderFromTOML(
		"Photos", "ph5wp-hswbp", "/home/tjmisko/Photos", 30_000, 30,
	); err != nil {
		t.Fatal(err)
	}
	file := &catalog.FileRecord{
		FolderLabel: "Photos", RelativePath: "2024/a.jpg", Filename: "a.jpg",
		Extension: ".jpg", FileMtime: time.Now().UTC(), Status: catalog.StatusArchived,
	}
	if _, err := cat.UpsertFile(file, 1); err != nil {
		t.Fatal(err)
	}
	stored, err := cat.GetFile("Photos", file.RelativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateThumbnailPath(stored.ID, "Photos/2024/a.jpg.thumb.jpg"); err != nil {
		t.Fatal(err)
	}

	coldRoot := t.TempDir()
	archiveFile := filepath.Join(coldRoot, "archive", "Photos", "2024", "a.jpg")
	thumbnailFile := filepath.Join(coldRoot, "thumbnails", "Photos", "2024", "a.jpg.thumb.jpg")
	for _, path := range []string{archiveFile, thumbnailFile} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{
		Storage: config.StorageConfig{ColdStoragePath: coldRoot},
		Folders: []config.FolderConfig{{
			Label: "CameraRollTristan", SyncthingID: "ph5wp-hswbp",
			Path: "/home/tjmisko/Camera Roll (Tristan)", BudgetBytes: 2_000_000_000_000,
		}},
	}
	result, err := MigrateFolderLabel(
		cfg, cat, "Photos", "CameraRollTristan", 2_000_000_000_000,
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.MovedRoots, ",") != "archive,thumbnails" {
		t.Errorf("moved roots = %v", result.MovedRoots)
	}
	for _, oldPath := range []string{archiveFile, thumbnailFile} {
		if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
			t.Errorf("old path %q still exists: %v", oldPath, err)
		}
	}
	for _, newPath := range []string{
		filepath.Join(coldRoot, "archive", "CameraRollTristan", "2024", "a.jpg"),
		filepath.Join(coldRoot, "thumbnails", "CameraRollTristan", "2024", "a.jpg.thumb.jpg"),
	} {
		if _, err := os.Stat(newPath); err != nil {
			t.Errorf("new path %q: %v", newPath, err)
		}
	}
	renamed, err := cat.GetFile("CameraRollTristan", file.RelativePath)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ThumbnailPath != "CameraRollTristan/2024/a.jpg.thumb.jpg" {
		t.Errorf("thumbnail path = %q", renamed.ThumbnailPath)
	}
}

func TestMigrateFolderLabelShouldRefuseDestinationRootWithoutChangingAnything(t *testing.T) {
	cat, _ := openCatalog(t)
	if _, err := cat.ReconcileFolderFromTOML(
		"Photos", "ph5wp-hswbp", "/home/tjmisko/Photos", 30_000, 30,
	); err != nil {
		t.Fatal(err)
	}
	coldRoot := t.TempDir()
	oldArchive := filepath.Join(coldRoot, "archive", "Photos")
	newArchive := filepath.Join(coldRoot, "archive", "CameraRollTristan")
	for _, path := range []string{oldArchive, newArchive} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		Storage: config.StorageConfig{ColdStoragePath: coldRoot},
		Folders: []config.FolderConfig{{
			Label: "CameraRollTristan", SyncthingID: "ph5wp-hswbp",
			Path: "/home/tjmisko/Camera Roll (Tristan)", BudgetBytes: 2_000_000_000_000,
		}},
	}

	_, err := MigrateFolderLabel(cfg, cat, "Photos", "CameraRollTristan", 2_000_000_000_000)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("MigrateFolderLabel error = %v, want destination conflict", err)
	}
	if _, err := os.Stat(oldArchive); err != nil {
		t.Errorf("old archive was moved after refusal: %v", err)
	}
	if _, err := cat.FolderConfigByLabel("Photos"); err != nil {
		t.Errorf("catalog changed after refusal: %v", err)
	}
}

func TestMigrateFolderLabelShouldRollBackRootsWhenTheCatalogRefuses(t *testing.T) {
	cat, _ := openCatalog(t)
	if _, err := cat.ReconcileFolderFromTOML(
		"Photos", "ph5wp-hswbp", "/home/tjmisko/Photos", 30_000, 30,
	); err != nil {
		t.Fatal(err)
	}
	file := &catalog.FileRecord{
		FolderLabel: "Photos", RelativePath: "a.jpg", Filename: "a.jpg",
		FileMtime: time.Now().UTC(), Status: catalog.StatusSynced,
	}
	if _, err := cat.UpsertFile(file, 1); err != nil {
		t.Fatal(err)
	}
	stored, err := cat.GetFile("Photos", "a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	// This legacy-corrupt path is rejected only inside the catalog transaction,
	// after the filesystem roots have moved, exercising the rollback path.
	if err := cat.UpdateThumbnailPath(stored.ID, "Wrong/a.jpg.thumb.jpg"); err != nil {
		t.Fatal(err)
	}

	coldRoot := t.TempDir()
	for _, root := range []string{"archive", "thumbnails"} {
		if err := os.MkdirAll(filepath.Join(coldRoot, root, "Photos"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		Storage: config.StorageConfig{ColdStoragePath: coldRoot},
		Folders: []config.FolderConfig{{
			Label: "CameraRollTristan", SyncthingID: "ph5wp-hswbp",
			Path: "/home/tjmisko/Camera Roll (Tristan)", BudgetBytes: 100,
		}},
	}

	_, err = MigrateFolderLabel(cfg, cat, "Photos", "CameraRollTristan", 100)
	if err == nil || !strings.Contains(err.Error(), "thumbnail path") {
		t.Fatalf("MigrateFolderLabel error = %v, want thumbnail refusal", err)
	}
	for _, root := range []string{"archive", "thumbnails"} {
		if _, err := os.Stat(filepath.Join(coldRoot, root, "Photos")); err != nil {
			t.Errorf("%s source root was not rolled back: %v", root, err)
		}
		if _, err := os.Stat(filepath.Join(coldRoot, root, "CameraRollTristan")); !os.IsNotExist(err) {
			t.Errorf("%s destination remains after rollback: %v", root, err)
		}
	}
	if _, err := cat.FolderConfigByLabel("Photos"); err != nil {
		t.Errorf("catalog source changed after rollback: %v", err)
	}
}

func TestMigrateFolderLabelShouldRequireReplacementConfigShape(t *testing.T) {
	cat, _ := openCatalog(t)
	if _, err := cat.ReconcileFolderFromTOML(
		"Photos", "ph5wp-hswbp", "/home/tjmisko/Photos", 30_000, 30,
	); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		folders []config.FolderConfig
		want    string
	}{
		{
			name: "source remains declared",
			folders: []config.FolderConfig{
				{Label: "Photos", SyncthingID: "ph5wp-hswbp", Path: "/old"},
				{Label: "CameraRollTristan", SyncthingID: "ph5wp-hswbp", Path: "/new"},
			},
			want: "still declares source",
		},
		{
			name:    "destination absent",
			folders: []config.FolderConfig{{Label: "Books", SyncthingID: "books", Path: "/books"}},
			want:    "does not declare destination",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Storage: config.StorageConfig{ColdStoragePath: t.TempDir()},
				Folders: tc.folders,
			}
			_, err := MigrateFolderLabel(cfg, cat, "Photos", "CameraRollTristan", 100)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("MigrateFolderLabel error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestMigrateFolderLabelShouldRefuseAPathOwnedByAnotherCatalogFolder(t *testing.T) {
	cat, _ := openCatalog(t)
	if _, err := cat.ReconcileFolderFromTOML(
		"Photos", "ph5wp-hswbp", "/home/tjmisko/Photos", 30_000, 30,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ReconcileFolderFromTOML(
		"Retired", "retired-id", "/home/tjmisko/Camera Roll (Tristan)/", 30_000, 30,
	); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Storage: config.StorageConfig{ColdStoragePath: t.TempDir()},
		Folders: []config.FolderConfig{{
			Label: "CameraRollTristan", SyncthingID: "ph5wp-hswbp",
			Path: "/home/tjmisko/Camera Roll (Tristan)", BudgetBytes: 100,
		}},
	}

	_, err := MigrateFolderLabel(cfg, cat, "Photos", "CameraRollTristan", 100)
	if err == nil || !strings.Contains(err.Error(), "already catalogued under folder \"Retired\"") {
		t.Fatalf("MigrateFolderLabel error = %v, want cleaned destination path conflict", err)
	}
}
