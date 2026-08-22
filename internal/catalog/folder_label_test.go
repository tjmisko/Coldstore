package catalog

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRenameFolderLabelShouldPreserveTheWholeCatalogIdentity(t *testing.T) {
	cat := setupTestCatalog(t)
	if _, err := cat.ReconcileFolderFromTOML(
		"Photos", "ph5wp-hswbp", "/home/tjmisko/Photos", 30_000, 45,
	); err != nil {
		t.Fatal(err)
	}
	displayName := "Old photos name"
	if found, err := cat.UpdateFolderSettings("Photos", nil, nil, nil, &displayName); err != nil || !found {
		t.Fatalf("setting display name = %v, %v", found, err)
	}

	file := &FileRecord{
		FolderLabel: "Photos", RelativePath: "2024/a.jpg", Filename: "a.jpg",
		Extension: ".jpg", SizeBytes: 123, FileMtime: time.Now().UTC(), Status: StatusArchived,
	}
	if _, err := cat.UpsertFile(file, 100); err != nil {
		t.Fatal(err)
	}
	stored, err := cat.GetFile("Photos", file.RelativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateThumbnailPath(stored.ID, "Photos/2024/a.jpg.thumb.jpg"); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetPinned(stored.ID, true, PinSourceUser, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	description := "keep this metadata"
	if _, err := cat.UpdateFile(stored.ID, FileEdits{Description: &description}); err != nil {
		t.Fatal(err)
	}
	if err := cat.AddPersistentPath("Photos", "Favorites/**"); err != nil {
		t.Fatal(err)
	}
	if err := cat.LogEvent("archive", "Photos", file.RelativePath, "old history"); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.Exec(`INSERT INTO jobs
		(action, folder_label, state, total, done, created_at, finished_at)
		VALUES ('scan', 'Photos', 'done', 1, 1, 10, 11)`); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.Exec(`INSERT INTO tags (name, name_fold, created_at)
		VALUES ('Family', 'family', 10)`); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.db.Exec(`INSERT INTO file_tags (file_id, tag_id, applied_at)
		SELECT ?, id, 10 FROM tags WHERE name_fold = 'family'`, stored.ID); err != nil {
		t.Fatal(err)
	}

	summary, err := cat.RenameFolderLabel(
		"Photos", "CameraRollTristan", "ph5wp-hswbp",
		"/home/tjmisko/Camera Roll (Tristan)", 2_000_000_000_000,
	)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Files != 1 || summary.PersistentPaths != 1 || summary.Events != 1 || summary.Jobs != 1 {
		t.Errorf("summary = %+v, want one existing row of each kind", summary)
	}
	if _, err := cat.FolderConfigByLabel("Photos"); !errors.Is(err, ErrNoSuchFolder) {
		t.Fatalf("old folder lookup error = %v, want ErrNoSuchFolder", err)
	}
	fc, err := cat.FolderConfigByLabel("CameraRollTristan")
	if err != nil {
		t.Fatal(err)
	}
	if fc.SyncthingID != "ph5wp-hswbp" ||
		fc.SyncthingPath != "/home/tjmisko/Camera Roll (Tristan)" ||
		fc.BudgetBytes != 2_000_000_000_000 || fc.GraceMinutes != 45 ||
		!fc.Enabled || !fc.PresentInTOML || fc.DisplayName != "" ||
		fc.LastEnforcedAt != nil || fc.WedgeWarnedAt != nil {
		t.Errorf("renamed folder config = %+v", fc)
	}

	renamed, err := cat.GetFile("CameraRollTristan", file.RelativePath)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID != stored.ID || renamed.ThumbnailPath != "CameraRollTristan/2024/a.jpg.thumb.jpg" ||
		!renamed.Pinned || renamed.Description != "keep this metadata" {
		t.Errorf("renamed file = %+v", renamed)
	}
	var tagLinks int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM file_tags WHERE file_id = ?`, renamed.ID).Scan(&tagLinks); err != nil {
		t.Fatal(err)
	}
	if tagLinks != 1 {
		t.Errorf("file tag links = %d, want 1", tagLinks)
	}
	paths, err := cat.ListPersistentPaths("CameraRollTristan")
	if err != nil || len(paths) != 1 || paths[0].Pattern != "Favorites/**" {
		t.Errorf("persistent paths = %+v, %v", paths, err)
	}
	events, err := cat.ListEvents("CameraRollTristan", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].EventType != "folder_label_migrated" ||
		events[1].EventType != "archive" {
		t.Errorf("renamed event history = %+v", events)
	}
	var jobLabel string
	if err := cat.db.QueryRow(`SELECT folder_label FROM jobs`).Scan(&jobLabel); err != nil {
		t.Fatal(err)
	}
	if jobLabel != "CameraRollTristan" {
		t.Errorf("job label = %q", jobLabel)
	}
}

func TestRenameFolderLabelShouldRefuseUnsafeCatalogStatesBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *Catalog)
		id    string
		want  string
	}{
		{
			name: "different Syncthing identity",
			id:   "other-id",
			want: "different Syncthing identities",
		},
		{
			name: "destination exists",
			id:   "ph5wp-hswbp",
			setup: func(t *testing.T, cat *Catalog) {
				if _, err := cat.ReconcileFolderFromTOML(
					"CameraRollTristan", "another-id", "/srv/other", 100, 30,
				); err != nil {
					t.Fatal(err)
				}
			},
			want: "already catalogued",
		},
		{
			name: "Syncthing identity has another owner",
			id:   "ph5wp-hswbp",
			setup: func(t *testing.T, cat *Catalog) {
				if _, err := cat.ReconcileFolderFromTOML(
					"Other", "ph5wp-hswbp", "/srv/other", 100, 30,
				); err != nil {
					t.Fatal(err)
				}
			},
			want: "also catalogued under folder",
		},
		{
			name: "in-flight file",
			id:   "ph5wp-hswbp",
			setup: func(t *testing.T, cat *Catalog) {
				if _, err := cat.UpsertFile(&FileRecord{
					FolderLabel: "Photos", RelativePath: "a.jpg", Filename: "a.jpg",
					FileMtime: time.Now().UTC(), Status: StatusArchiving,
				}, 1); err != nil {
					t.Fatal(err)
				}
			},
			want: "in-flight file move",
		},
		{
			name: "pending job",
			id:   "ph5wp-hswbp",
			setup: func(t *testing.T, cat *Catalog) {
				if _, err := cat.CreateJob(JobActionScan, "Photos", nil); err != nil {
					t.Fatal(err)
				}
			},
			want: "pending or running job",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat := setupTestCatalog(t)
			if _, err := cat.ReconcileFolderFromTOML(
				"Photos", "ph5wp-hswbp", "/home/tjmisko/Photos", 30_000, 30,
			); err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				tc.setup(t, cat)
			}

			_, err := cat.RenameFolderLabel(
				"Photos", "CameraRollTristan", tc.id,
				"/home/tjmisko/Camera Roll (Tristan)", 2_000_000_000_000,
			)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RenameFolderLabel error = %v, want %q", err, tc.want)
			}
			if _, err := cat.FolderConfigByLabel("Photos"); err != nil {
				t.Fatalf("source folder changed after refusal: %v", err)
			}
		})
	}
}
