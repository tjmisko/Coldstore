package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	metadatapkg "github.com/tjmisko/coldstore/internal/metadata"
)

func TestCleanRenamePathRejectsTraversalAndNonCanonicalForms(t *testing.T) {
	for _, unsafe := range []string{"", ".", "/absolute", "../escape", "a/../b", "a//b", "a/", `a\b`,
		"line\nbreak", ".stfolder", ".stfolder/index"} {
		if _, err := CleanRenamePath(unsafe); !errors.Is(err, ErrInvalidRenamePath) {
			t.Errorf("CleanRenamePath(%q) error = %v", unsafe, err)
		}
	}
	if got, err := CleanRenamePath("2024/Iceland"); err != nil || got != "2024/Iceland" {
		t.Fatalf("clean path = %q, %v", got, err)
	}
}

func TestDirectoryRenameMovesEveryTreeAndRewritesCatalogCoordinates(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	device := seedSyncedFile(t, cat, syncDir, "2024/01/a.jpg", 10)
	archived := seedSyncedFile(t, cat, syncDir, "2024/01/b.mov", 20)
	mover := NewMover(cfg, cat, testLogger())
	mover.SetSpaceCheck(plentyOfSpace)
	if _, err := mover.ArchiveByID(archived.ID); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetDegradedPair([]int64{device.ID, archived.ID}, "stem:2024/01/shared"); err != nil {
		t.Fatal(err)
	}
	thumbRel := "TestFolder/2024/01/a.jpg.thumb.jpg"
	thumb := filepath.Join(coldDir, "thumbnails", thumbRel)
	if err := os.MkdirAll(filepath.Dir(thumb), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thumb, []byte("thumb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateThumbnailPath(device.ID, thumbRel); err != nil {
		t.Fatal(err)
	}
	if err := cat.AddPersistentPath("TestFolder", "2024/01/**"); err != nil {
		t.Fatal(err)
	}

	warnings, err := mover.PreflightRename(context.Background(), "TestFolder", "2024/01", "2024/Iceland")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0] != "2024/01/**" {
		t.Fatalf("persistent warnings = %v", warnings)
	}
	if err := mover.RenamePath(context.Background(), "TestFolder", "2024/01", "2024/Iceland"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(syncDir, "2024/Iceland/a.jpg")); err != nil {
		t.Errorf("device tree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(coldDir, "archive/TestFolder/2024/Iceland/b.mov")); err != nil {
		t.Errorf("archive tree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(coldDir, "thumbnails/TestFolder/2024/Iceland/a.jpg.thumb.jpg")); err != nil {
		t.Errorf("thumbnail tree: %v", err)
	}
	for _, rel := range []string{"2024/Iceland/a.jpg", "2024/Iceland/b.mov"} {
		got, err := cat.GetFile("TestFolder", rel)
		if err != nil {
			t.Fatal(err)
		}
		if got.PairKey != "stem:2024/Iceland/shared" {
			t.Errorf("%s pair key = %q", rel, got.PairKey)
		}
	}
	got, _ := cat.GetFileByID(device.ID)
	if got.ThumbnailPath != "TestFolder/2024/Iceland/a.jpg.thumb.jpg" {
		t.Errorf("thumbnail_path = %q", got.ThumbnailPath)
	}
	events, err := cat.ListEvents("TestFolder", "folder_renamed", "", 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("rename events = %+v, %v", events, err)
	}
}

func TestDirectoryRenameTreatsSQLWildcardCharactersLiterally(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	seedSyncedFile(t, cat, syncDir, "literal_%/inside.jpg", 1)
	seedSyncedFile(t, cat, syncDir, "literal_AX/outside.jpg", 1)
	mover := NewMover(cfg, cat, testLogger())

	if err := mover.RenamePath(context.Background(), "TestFolder", "literal_%", "renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.GetFile("TestFolder", "renamed/inside.jpg"); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.GetFile("TestFolder", "literal_AX/outside.jpg"); err != nil {
		t.Fatalf("wildcard-looking sibling changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(syncDir, "literal_AX/outside.jpg")); err != nil {
		t.Fatalf("wildcard-looking sibling moved: %v", err)
	}
}

func TestDirectoryRenameUsesUnicodeCharacterOffsets(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	file := seedSyncedFile(t, cat, syncDir, "旅行/a.jpg", 1)
	if err := cat.SetDegradedPair([]int64{file.ID}, "stem:旅行/a"); err != nil {
		t.Fatal(err)
	}
	thumbRel := "TestFolder/旅行/a.jpg.thumb.jpg"
	thumb := filepath.Join(coldDir, "thumbnails", thumbRel)
	if err := os.MkdirAll(filepath.Dir(thumb), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thumb, []byte("thumb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateThumbnailPath(file.ID, thumbRel); err != nil {
		t.Fatal(err)
	}
	mover := NewMover(cfg, cat, testLogger())

	if err := mover.RenamePath(context.Background(), "TestFolder", "旅行", "Trips"); err != nil {
		t.Fatal(err)
	}
	got, err := cat.GetFile("TestFolder", "Trips/a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if got.ThumbnailPath != "TestFolder/Trips/a.jpg.thumb.jpg" || got.PairKey != "stem:Trips/a" {
		t.Fatalf("unicode coordinate rewrite = %+v", got)
	}
}

func TestDirectoryRenameRefusesOverwriteTransientDescendantAndSplitPair(t *testing.T) {
	t.Run("overwrite", func(t *testing.T) {
		syncDir, _, cfg, cat := setupTestEnv(t)
		declarePolicy(t, cfg, cat)
		seedSyncedFile(t, cat, syncDir, "old/a.jpg", 1)
		seedSyncedFile(t, cat, syncDir, "new/b.jpg", 1)
		mover := NewMover(cfg, cat, testLogger())
		if err := mover.RenamePath(context.Background(), "TestFolder", "old", "new"); !errors.Is(err, ErrRenameConflict) {
			t.Fatalf("overwrite error = %v", err)
		}
	})
	t.Run("catalog file is destination parent", func(t *testing.T) {
		syncDir, _, cfg, cat := setupTestEnv(t)
		declarePolicy(t, cfg, cat)
		seedSyncedFile(t, cat, syncDir, "old/a.jpg", 1)
		parent := seedSyncedFile(t, cat, syncDir, "new", 1)
		if err := os.Remove(filepath.Join(syncDir, parent.RelativePath)); err != nil {
			t.Fatal(err)
		}
		mover := NewMover(cfg, cat, testLogger())
		if err := mover.RenamePath(context.Background(), "TestFolder", "old", "new/sub"); !errors.Is(err, ErrRenameConflict) {
			t.Fatalf("catalog parent error = %v", err)
		}
		if _, err := os.Stat(filepath.Join(syncDir, "old/a.jpg")); err != nil {
			t.Fatalf("source moved before catalog-parent refusal: %v", err)
		}
	})
	t.Run("transient", func(t *testing.T) {
		syncDir, _, cfg, cat := setupTestEnv(t)
		declarePolicy(t, cfg, cat)
		file := seedSyncedFile(t, cat, syncDir, "old/a.jpg", 1)
		if err := cat.UpdateFileStatus(file.ID, catalog.StatusArchiving); err != nil {
			t.Fatal(err)
		}
		mover := NewMover(cfg, cat, testLogger())
		if err := mover.RenamePath(context.Background(), "TestFolder", "old", "new"); !errors.Is(err, ErrRenameTransient) {
			t.Fatalf("transient error = %v", err)
		}
	})
	t.Run("descendant", func(t *testing.T) {
		syncDir, _, cfg, cat := setupTestEnv(t)
		declarePolicy(t, cfg, cat)
		seedSyncedFile(t, cat, syncDir, "old/a.jpg", 1)
		mover := NewMover(cfg, cat, testLogger())
		if err := mover.RenamePath(context.Background(), "TestFolder", "old", "old/new"); !errors.Is(err, ErrInvalidRenamePath) {
			t.Fatalf("descendant error = %v", err)
		}
	})
	t.Run("pair outside directory", func(t *testing.T) {
		syncDir, _, cfg, cat := setupTestEnv(t)
		declarePolicy(t, cfg, cat)
		a := seedSyncedFile(t, cat, syncDir, "old/a.jpg", 1)
		b := seedSyncedFile(t, cat, syncDir, "elsewhere/a.mov", 1)
		assignTestPair(t, cat, "cid:spans-directories", a, b)
		mover := NewMover(cfg, cat, testLogger())
		if err := mover.RenamePath(context.Background(), "TestFolder", "old", "new"); !errors.Is(err, ErrRenamePair) {
			t.Fatalf("split-pair error = %v", err)
		}
	})
}

func TestFileRenameRefusesDestinationThroughASymlinkedParent(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	seedSyncedFile(t, cat, syncDir, "old.jpg", 1)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(syncDir, "outside-link")); err != nil {
		t.Fatal(err)
	}
	mover := NewMover(cfg, cat, testLogger())

	err := mover.RenamePath(context.Background(), "TestFolder", "old.jpg", "outside-link/new.jpg")
	if !errors.Is(err, ErrInvalidRenamePath) {
		t.Fatalf("symlink traversal error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new.jpg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rename escaped the folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(syncDir, "old.jpg")); err != nil {
		t.Fatalf("source moved after refused traversal: %v", err)
	}
}

func TestFileRenameRefusesDestinationsInEveryTreeBeforeMoving(t *testing.T) {
	for _, tree := range []string{"device", "archive", "thumbnail"} {
		t.Run(tree, func(t *testing.T) {
			syncDir, coldDir, cfg, cat := setupTestEnv(t)
			declarePolicy(t, cfg, cat)
			seedSyncedFile(t, cat, syncDir, "old.jpg", 1)
			var occupied string
			if tree == "device" {
				occupied = filepath.Join(syncDir, "new.jpg")
			} else if tree == "archive" {
				occupied = filepath.Join(coldDir, "archive/TestFolder/new.jpg")
			} else {
				occupied = filepath.Join(coldDir, "thumbnails/TestFolder/new.jpg.thumb.jpg")
			}
			if err := os.MkdirAll(filepath.Dir(occupied), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(occupied, []byte("unrelated"), 0o644); err != nil {
				t.Fatal(err)
			}
			mover := NewMover(cfg, cat, testLogger())
			if err := mover.RenamePath(context.Background(), "TestFolder", "old.jpg", "new.jpg"); !errors.Is(err, ErrRenameConflict) {
				t.Fatalf("destination error = %v", err)
			}
			if _, err := os.Stat(filepath.Join(syncDir, "old.jpg")); err != nil {
				t.Fatalf("source moved before full-tree preflight: %v", err)
			}
			contents, err := os.ReadFile(occupied)
			if err != nil || string(contents) != "unrelated" {
				t.Fatalf("occupied destination changed: %q, %v", contents, err)
			}
		})
	}
}

func TestSingleFileRenameExpandsADegradedPairAndKeepsItsStem(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "Trips/IMG_20.heic", 10)
	movie := seedSyncedFile(t, cat, syncDir, "Trips/IMG_20.mov", 20)
	aae := seedSyncedFile(t, cat, syncDir, "Trips/IMG_20.aae", 2)
	xmp := seedSyncedFile(t, cat, syncDir, "Trips/IMG_20.heic.xmp", 2)
	if err := cat.SetDegradedPair([]int64{still.ID, movie.ID, aae.ID, xmp.ID}, "stem:Trips/img_20"); err != nil {
		t.Fatal(err)
	}
	mover := NewMover(cfg, cat, testLogger())
	if err := mover.RenamePath(context.Background(), "TestFolder", still.RelativePath, "Trips/Holiday.jpg"); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"Trips/Holiday.jpg", "Trips/Holiday.mov", "Trips/Holiday.aae", "Trips/Holiday.jpg.xmp"} {
		got, err := cat.GetFile("TestFolder", rel)
		if err != nil {
			t.Fatal(err)
		}
		if got.PairKey != "stem:Trips/holiday" || got.PairSource != "basename" {
			t.Errorf("%s pairing = %q (%q)", rel, got.PairKey, got.PairSource)
		}
		if _, err := os.Stat(filepath.Join(syncDir, rel)); err != nil {
			t.Errorf("%s physical rename: %v", rel, err)
		}
	}
}

func TestSingleFileRenameKeepsContentIDPairAuthority(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_21.heic", 10)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_21.mov", 20)
	assignTestPair(t, cat, "cid:stable", still, movie)
	mover := NewMover(cfg, cat, testLogger())

	if err := mover.RenamePath(context.Background(), "TestFolder", still.RelativePath, "Holiday.heic"); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"Holiday.heic", "Holiday.mov"} {
		got, err := cat.GetFile("TestFolder", rel)
		if err != nil {
			t.Fatal(err)
		}
		if got.PairKey != "cid:stable" || got.PairSource != "content_id" {
			t.Fatalf("%s pairing = %q (%q)", rel, got.PairKey, got.PairSource)
		}
	}
}

func TestSingleFileRenameRefusesAPairWithAMissingMember(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_22.heic", 10)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_22.mov", 20)
	assignTestPair(t, cat, "cid:missing", still, movie)
	if err := cat.UpdateFileStatus(movie.ID, catalog.StatusMissing); err != nil {
		t.Fatal(err)
	}
	mover := NewMover(cfg, cat, testLogger())

	if err := mover.RenamePath(context.Background(), "TestFolder", still.RelativePath, "Holiday.heic"); !errors.Is(err, ErrRenamePair) {
		t.Fatalf("missing-pair error = %v", err)
	}
}

func TestSingleFileRenameRefusesAPairMemberMissingFromItsCataloguedTree(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_23.heic", 10)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_23.mov", 20)
	assignTestPair(t, cat, "cid:physically-missing", still, movie)
	if err := os.Remove(filepath.Join(syncDir, movie.RelativePath)); err != nil {
		t.Fatal(err)
	}
	mover := NewMover(cfg, cat, testLogger())

	if err := mover.RenamePath(context.Background(), "TestFolder", still.RelativePath, "Holiday.heic"); !errors.Is(err, ErrRenamePair) {
		t.Fatalf("physically missing pair error = %v", err)
	}
}

func TestSingleFileRenameRefusesAnAmbiguousEditedPair(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	original := seedSyncedFile(t, cat, syncDir, "IMG_30.heic", 10)
	edited := seedSyncedFile(t, cat, syncDir, "IMG_E30.heic", 10)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_30.mov", 10)
	assignTestPair(t, cat, "cid:edited", original, edited, movie)
	mover := NewMover(cfg, cat, testLogger())
	if err := mover.RenamePath(context.Background(), "TestFolder", original.RelativePath, "Holiday.heic"); !errors.Is(err, ErrRenamePair) {
		t.Fatalf("edited-pair error = %v", err)
	}
}

func TestArchivedFileRenameChangesExtensionAndResetsDerivedState(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	file := seedSyncedFile(t, cat, syncDir, "old.jpg", 10)
	mover := NewMover(cfg, cat, testLogger())
	mover.SetSpaceCheck(plentyOfSpace)
	if _, err := mover.ArchiveByID(file.ID); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetExtractedMetadata(file.ID, metadatapkg.Extracted{
		CapturedLocal: "2024-01-02 03:04:05", CapturedSource: "exif",
	}, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := cat.RecordThumbnailFailure(file.ID, "old failure", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	thumbRel := "TestFolder/old.jpg.thumb.jpg"
	thumb := filepath.Join(coldDir, "thumbnails", thumbRel)
	if err := os.MkdirAll(filepath.Dir(thumb), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thumb, []byte("thumb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateThumbnailPath(file.ID, thumbRel); err != nil {
		t.Fatal(err)
	}

	if err := mover.RenamePath(context.Background(), "TestFolder", "old.jpg", "renamed.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(coldDir, "archive/TestFolder/renamed.png")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(thumb); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old thumbnail still exists: %v", err)
	}
	got, err := cat.GetFile("TestFolder", "renamed.png")
	if err != nil {
		t.Fatal(err)
	}
	if got.Filename != "renamed.png" || got.Extension != ".png" || got.ThumbnailPath != "" ||
		got.ThumbAttempts != 0 || got.ThumbLastAttempt != nil || got.ThumbError != "" ||
		got.MetaScannedAt != nil || got.MetaAttempts != 0 || got.MetaLastAttempt != nil ||
		got.MetaError != "" || got.CapturedLocal != "" || got.CapturedSource != "" {
		t.Fatalf("renamed derived state = %+v", got)
	}
}

func TestRenameResumeFinishesAfterFilesystemStepsAndThenNoOps(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	seedSyncedFile(t, cat, syncDir, "old/a.jpg", 1)
	mover := NewMover(cfg, cat, testLogger())
	plan, err := mover.planRename("TestFolder", "old", "new", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range plan.actions {
		if err := performRenameAction(action); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cat.GetFile("TestFolder", "old/a.jpg"); err != nil {
		t.Fatalf("test did not stop before catalog commit: %v", err)
	}
	if err := mover.RenamePath(context.Background(), "TestFolder", "old", "new"); err != nil {
		t.Fatal(err)
	}
	if err := mover.RenamePath(context.Background(), "TestFolder", "old", "new"); err != nil {
		t.Fatalf("completed rename was not idempotent: %v", err)
	}
	if _, err := cat.GetFile("TestFolder", "new/a.jpg"); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyDirectoryRenameIsIdempotentAfterTheFilesystemMove(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	if err := os.MkdirAll(filepath.Join(syncDir, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	mover := NewMover(cfg, cat, testLogger())
	plan, err := mover.planRename("TestFolder", "old", "new", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range plan.actions {
		if err := performRenameAction(action); err != nil {
			t.Fatal(err)
		}
	}
	if err := mover.RenamePath(context.Background(), "TestFolder", "old", "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(syncDir, "new")); err != nil {
		t.Fatal(err)
	}
}

func TestRenameJobResumesAfterTheOperationCommittedBeforeBookkeeping(t *testing.T) {
	env := newWorkerEnv(t)
	seedSyncedFile(t, env.cat, env.syncDir, "old/a.jpg", 1)
	jobID, err := env.cat.CreateRenameJob("TestFolder", "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.cat.MarkJobRunning(jobID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := env.mover.RenamePath(context.Background(), "TestFolder", "old", "new"); err != nil {
		t.Fatal(err)
	}
	items, jobs, err := env.cat.ResumeInterruptedJobs()
	if err != nil {
		t.Fatal(err)
	}
	if items != 0 || jobs != 1 {
		t.Fatalf("resumed items/jobs = %d/%d, want 0/1", items, jobs)
	}
	fresh := NewWorker(env.cat, env.mover, env.scanner, testLogger())
	fresh.RunOnce(context.Background())
	job := env.job(t, jobID)
	assertTerminalCounters(t, job)
	if job.State != catalog.JobStateDone || job.Done != 1 || job.FailedCount != 0 {
		t.Fatalf("resumed rename job = %+v", job)
	}
}

func TestRenamePreflightAndMoveShareTheMoverMutex(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	file := seedSyncedFile(t, cat, syncDir, "old/a.jpg", 1)
	mover := NewMover(cfg, cat, testLogger())
	mover.SetSpaceCheck(plentyOfSpace)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- mover.WithRenamePreflight(context.Background(), "TestFolder", "old", "new",
			func([]string) error {
				close(started)
				<-release
				return nil
			})
	}()
	<-started
	archiveDone := make(chan error, 1)
	go func() {
		_, err := mover.ArchiveByID(file.ID)
		archiveDone <- err
	}()
	select {
	case err := <-archiveDone:
		t.Fatalf("archive escaped rename preflight lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-archiveDone; err != nil {
		t.Fatal(err)
	}
}

func TestRenameAtomicCommitRefusesDestinationCreatedAfterCheck(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	seedSyncedFile(t, cat, syncDir, "old.jpg", 1)
	mover := NewMover(cfg, cat, testLogger())
	destination := filepath.Join(syncDir, "new.jpg")
	mover.beforeRenameCommit = func(action renameFSAction) error {
		if action.destination != destination {
			return nil
		}
		return os.WriteFile(destination, []byte("concurrent Syncthing delivery"), 0o644)
	}

	err := mover.RenamePath(context.Background(), "TestFolder", "old.jpg", "new.jpg")
	if !errors.Is(err, ErrRenameConflict) || !errors.Is(err, errRenameIncomplete) {
		t.Fatalf("rename error = %v, want retryable conflict", err)
	}
	if contents, err := os.ReadFile(destination); err != nil || string(contents) != "concurrent Syncthing delivery" {
		t.Fatalf("concurrent destination changed: %q, %v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(syncDir, "old.jpg")); err != nil {
		t.Fatalf("source disappeared after no-replace refusal: %v", err)
	}
}

func TestRenameJobRetriesAPartialThreeTreeMoveInsteadOfTerminalizingIt(t *testing.T) {
	env := newWorkerEnv(t)
	device := seedSyncedFile(t, env.cat, env.syncDir, "old/device.jpg", 1)
	archived := seedSyncedFile(t, env.cat, env.syncDir, "old/archive.jpg", 1)
	env.mover.SetSpaceCheck(plentyOfSpace)
	if _, err := env.mover.ArchiveByID(archived.ID); err != nil {
		t.Fatal(err)
	}
	jobID, err := env.cat.CreateRenameJob("TestFolder", "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := env.cat.GetJob(jobID, 1)
	if err != nil {
		t.Fatal(err)
	}
	archiveRoot := env.mover.restorer.ArchiveRoot("TestFolder")
	injected := false
	env.mover.beforeRenameCommit = func(action renameFSAction) error {
		if !injected && action.root == archiveRoot {
			injected = true
			return syscall.EIO
		}
		return nil
	}

	if err := env.worker.runJob(context.Background(), job); !errors.Is(err, errRenameIncomplete) {
		t.Fatalf("partial run error = %v", err)
	}
	partial := env.job(t, jobID)
	if partial.State != catalog.JobStateRunning || partial.Done != 0 || partial.FailedCount != 0 {
		t.Fatalf("partial rename was terminalized: %+v", partial)
	}
	if _, err := os.Stat(filepath.Join(env.syncDir, "new/device.jpg")); err != nil {
		t.Fatalf("device step did not complete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archiveRoot, "old/archive.jpg")); err != nil {
		t.Fatalf("archive source should remain for retry: %v", err)
	}
	if _, err := env.cat.GetFileByID(device.ID); err != nil {
		t.Fatalf("catalog row disappeared during partial move: %v", err)
	}

	env.mover.beforeRenameCommit = nil
	items, jobs, err := env.cat.ResumeInterruptedJobs()
	if err != nil {
		t.Fatal(err)
	}
	if items != 0 || jobs != 1 {
		t.Fatalf("startup recovery resumed items/jobs = %d/%d, want 0/1", items, jobs)
	}
	fresh := NewWorker(env.cat, env.mover, env.scanner, testLogger())
	fresh.RunOnce(context.Background())
	finished := env.job(t, jobID)
	if finished.State != catalog.JobStateDone || finished.Done != 1 || finished.FailedCount != 0 {
		t.Fatalf("retried rename job = %+v", finished)
	}
	for _, filename := range []string{
		filepath.Join(env.syncDir, "new/device.jpg"),
		filepath.Join(archiveRoot, "new/archive.jpg"),
	} {
		if _, err := os.Stat(filename); err != nil {
			t.Errorf("resumed tree member %s: %v", filename, err)
		}
	}
	for _, rel := range []string{"new/device.jpg", "new/archive.jpg"} {
		if _, err := env.cat.GetFile("TestFolder", rel); err != nil {
			t.Errorf("resumed catalog coordinate %s: %v", rel, err)
		}
	}
}

func TestRenameExcludesScannerAcrossFilesystemAndCatalogCommit(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	seedSyncedFile(t, cat, syncDir, "old/a.jpg", 1)
	mover := NewMover(cfg, cat, testLogger())
	scanner := NewScanner(cfg, cat, testLogger())
	_ = NewWorker(cat, mover, scanner, testLogger()) // binds the shared scanner lock
	folder, err := cat.FolderConfigByLabel("TestFolder")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	mover.beforeRenameCommit = func(action renameFSAction) error {
		if action.root == syncDir {
			close(started)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- mover.RenamePath(context.Background(), "TestFolder", "old", "new")
	}()
	<-started
	if err := scanner.ScanFolderContext(context.Background(), *folder); !errors.Is(err, ErrScanInProgress) {
		close(release)
		t.Fatalf("scan during rename error = %v, want ErrScanInProgress", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := cat.GetFile("TestFolder", "new/a.jpg"); err != nil {
		t.Fatal(err)
	}
}

func TestRenameRejectsScannerIgnoredAndSyncthingControlDestinations(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		target    string
		directory bool
	}{
		{name: "hidden parent", source: "old.jpg", target: ".hidden/new.jpg"},
		{name: "hidden directory", source: "old/a.jpg", target: ".hidden", directory: true},
		{name: "syncthing ignore file", source: "old.jpg", target: ".stignore"},
		{name: "syncthing temporary", source: "old.jpg", target: ".syncthing.new.tmp"},
		{name: "temporary suffix", source: "old.jpg", target: "new.tmp"},
		{name: "coldstore scratch", source: "old.jpg", target: "new.coldstore-tmp"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			syncDir, _, cfg, cat := setupTestEnv(t)
			declarePolicy(t, cfg, cat)
			seedSyncedFile(t, cat, syncDir, test.source, 1)
			mover := NewMover(cfg, cat, testLogger())
			from := test.source
			if test.directory {
				from = filepath.Dir(test.source)
			}
			if _, err := mover.PreflightRename(context.Background(), "TestFolder", from, test.target); !errors.Is(err, ErrInvalidRenamePath) {
				t.Fatalf("preflight error = %v", err)
			}
			if _, err := os.Stat(filepath.Join(syncDir, test.source)); err != nil {
				t.Fatalf("source changed after refused destination: %v", err)
			}
		})
	}
}

func TestRenameResumeRejectsWrongDestinationKind(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	seedSyncedFile(t, cat, syncDir, "old/a.jpg", 1)
	mover := NewMover(cfg, cat, testLogger())
	plan, err := mover.planRename("TestFolder", "old", "new", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range plan.actions {
		if action.root == syncDir {
			if err := performRenameAction(action); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Remove(filepath.Join(syncDir, "new/a.jpg")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(syncDir, "new")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(syncDir, "new"), []byte("wrong kind"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = mover.RenamePath(context.Background(), "TestFolder", "old", "new")
	if !errors.Is(err, ErrRenameConflict) {
		t.Fatalf("wrong-kind resume error = %v", err)
	}
	if _, err := cat.GetFile("TestFolder", "old/a.jpg"); err != nil {
		t.Fatalf("catalog committed onto wrong-kind destination: %v", err)
	}
}

func TestRenameWarnsForEveryTextualPersistentPrefix(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	seedSyncedFile(t, cat, syncDir, "old/a.jpg", 1)
	for _, pattern := range []string{"old/**", "old*.jpg", "older/**", "unrelated/**"} {
		if err := cat.AddPersistentPath("TestFolder", pattern); err != nil {
			t.Fatal(err)
		}
	}
	mover := NewMover(cfg, cat, testLogger())
	warnings, err := mover.PreflightRename(context.Background(), "TestFolder", "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"old*.jpg", "old/**", "older/**"}
	if len(warnings) != len(want) {
		t.Fatalf("warnings = %v, want %v", warnings, want)
	}
	for i := range want {
		if warnings[i] != want[i] {
			t.Fatalf("warnings = %v, want %v", warnings, want)
		}
	}
}

func TestRenameExecutionRejectsParentSymlinkInsertedAfterPlanning(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	seedSyncedFile(t, cat, syncDir, "old.jpg", 1)
	mover := NewMover(cfg, cat, testLogger())
	plan, err := mover.planRename("TestFolder", "old.jpg", "swapped/new.jpg", true)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(syncDir, "swapped")); err != nil {
		t.Fatal(err)
	}
	for _, action := range plan.actions {
		if action.root != syncDir {
			continue
		}
		if err := performRenameAction(action); err == nil {
			t.Fatal("rename followed a parent symlink inserted after planning")
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "new.jpg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rename escaped through substituted symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(syncDir, "old.jpg")); err != nil {
		t.Fatalf("source moved through substituted symlink: %v", err)
	}
}
