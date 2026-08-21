package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
	metadatapkg "github.com/tjmisko/coldstore/internal/metadata"
)

func assignTestPair(t *testing.T, cat *catalog.Catalog, key string, files ...*catalog.FileRecord) {
	t.Helper()
	assignments := make([]catalog.PairAssignment, 0, len(files))
	for _, file := range files {
		assignments = append(assignments, catalog.PairAssignment{
			ID: file.ID, Key: key, Source: "content_id",
		})
	}
	if err := cat.SetPairAssignments(assignments); err != nil {
		t.Fatal(err)
	}
}

func TestMoverArchiveAndRestoreExpandTheWholePair(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_1.heic", 80)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_1.mov", 120)
	assignTestPair(t, cat, "cid:one", still, movie)
	mover := NewMover(cfg, cat, testLogger())
	mover.SetSpaceCheck(plentyOfSpace)

	if _, err := mover.ArchiveByID(still.ID); err != nil {
		t.Fatal(err)
	}
	if events, err := cat.ListEvents("TestFolder", "pair_split", "", 10); err != nil || len(events) != 0 {
		t.Fatalf("legitimate group archive wrote pair_split: events %+v err %v", events, err)
	}
	for _, file := range []*catalog.FileRecord{still, movie} {
		got, _ := cat.GetFileByID(file.ID)
		if got.Status != catalog.StatusArchived {
			t.Errorf("%s status after one-member archive = %q", file.RelativePath, got.Status)
		}
		if _, err := os.Stat(filepath.Join(coldDir, "archive", "TestFolder", file.RelativePath)); err != nil {
			t.Errorf("archive member %s: %v", file.RelativePath, err)
		}
	}

	if _, err := mover.RestoreByID(movie.ID); err != nil {
		t.Fatal(err)
	}
	for _, file := range []*catalog.FileRecord{still, movie} {
		got, _ := cat.GetFileByID(file.ID)
		if got.Status != catalog.StatusSynced || !got.Pinned || got.PinSource != catalog.PinSourceRestore {
			t.Errorf("%s after pair restore = status %q pin %v/%q",
				file.RelativePath, got.Status, got.Pinned, got.PinSource)
		}
		if _, err := os.Stat(filepath.Join(syncDir, file.RelativePath)); err != nil {
			t.Errorf("restored member %s: %v", file.RelativePath, err)
		}
	}
}

func TestMoverRefusesAWholePairWhenOneMemberIsMissing(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_2.heic", 80)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_2.mov", 120)
	assignTestPair(t, cat, "cid:two", still, movie)
	if err := cat.UpdateFileStatus(movie.ID, catalog.StatusMissing); err != nil {
		t.Fatal(err)
	}
	mover := NewMover(cfg, cat, testLogger())
	mover.SetSpaceCheck(plentyOfSpace)

	if _, err := mover.ArchiveByID(still.ID); !errors.Is(err, ErrFileStateChanged) {
		t.Fatalf("archive error = %v, want ErrFileStateChanged", err)
	}
	got, _ := cat.GetFileByID(still.ID)
	if got.Status != catalog.StatusSynced {
		t.Errorf("first member moved despite group preflight: %q", got.Status)
	}
}

func TestMoverRestoreRefusesAStaleSyncedCompanionBeforeMovingAnything(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_11.heic", 80)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_11.mov", 120)
	assignTestPair(t, cat, "cid:eleven", still, movie)
	mover := NewMover(cfg, cat, testLogger())
	mover.SetSpaceCheck(plentyOfSpace)
	if _, err := mover.ArchiveByID(still.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := mover.RestoreByID(movie.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := mover.ArchiveByID(still.ID); err != nil {
		t.Fatal(err)
	}
	// Recreate a mixed group without moving through the group-aware entry
	// point, then remove the supposedly synced companion behind the catalog's
	// back. The archived member must remain untouched by restore preflight.
	if _, err := mover.restoreLocked(movie.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(syncDir, movie.RelativePath)); err != nil {
		t.Fatal(err)
	}

	if _, err := mover.RestoreByID(still.ID); !errors.Is(err, ErrFileStateChanged) {
		t.Fatalf("restore error = %v, want ErrFileStateChanged", err)
	}
	got, err := cat.GetFileByID(still.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != catalog.StatusArchived {
		t.Fatalf("archived member moved before companion preflight: %q", got.Status)
	}
}

func TestMoverArchiveRollsBackCompletedMembersAfterLaterMoveFailure(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_12.heic", 80)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_12.mov", 120)
	assignTestPair(t, cat, "cid:twelve", still, movie)
	const pinnedAt = int64(123456)
	if err := cat.SetPinned(still.ID, true, catalog.PinSourceUser, pinnedAt); err != nil {
		t.Fatal(err)
	}

	mover := NewMover(cfg, cat, testLogger())
	mover.SetSpaceCheck(plentyOfSpace)
	realMoveOut := mover.moveOut
	injected := errors.New("injected second archive failure")
	calls := 0
	mover.SetMoveFuncs(func(src, dst string) error {
		calls++
		if calls == 2 {
			return injected
		}
		return realMoveOut(src, dst)
	}, nil)

	if _, err := mover.ArchiveByID(still.ID); !errors.Is(err, injected) {
		t.Fatalf("archive error = %v, want injected cause", err)
	}
	for _, original := range []*catalog.FileRecord{still, movie} {
		got, err := cat.GetFileByID(original.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != catalog.StatusSynced {
			t.Errorf("%s status after rollback = %q", original.RelativePath, got.Status)
		}
		if _, err := os.Stat(filepath.Join(syncDir, original.RelativePath)); err != nil {
			t.Errorf("%s not restored to synced folder: %v", original.RelativePath, err)
		}
		if _, err := os.Stat(filepath.Join(coldDir, "archive", "TestFolder", original.RelativePath)); !os.IsNotExist(err) {
			t.Errorf("%s still visible in archive after rollback: %v", original.RelativePath, err)
		}
	}
	gotStill, _ := cat.GetFileByID(still.ID)
	if !gotStill.Pinned || gotStill.PinSource != catalog.PinSourceUser ||
		gotStill.PinnedAt == nil || *gotStill.PinnedAt != pinnedAt {
		t.Errorf("first member pin after rollback = %v/%q/%v", gotStill.Pinned, gotStill.PinSource, gotStill.PinnedAt)
	}
	gotMovie, _ := cat.GetFileByID(movie.ID)
	if gotMovie.Pinned || gotMovie.PinSource != "" || gotMovie.PinnedAt != nil {
		t.Errorf("failed member pin changed = %v/%q/%v", gotMovie.Pinned, gotMovie.PinSource, gotMovie.PinnedAt)
	}
	if events, err := cat.ListEvents("TestFolder", "pair_split", "", 10); err != nil || len(events) != 0 {
		t.Fatalf("successful rollback wrote pair_split: events %+v err %v", events, err)
	}
}

func TestMoverRestoreRollsBackCompletedMembersAndOriginalPinsAfterLaterFailure(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_13.heic", 80)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_13.mov", 120)
	assignTestPair(t, cat, "cid:thirteen", still, movie)
	mover := NewMover(cfg, cat, testLogger())
	mover.SetSpaceCheck(plentyOfSpace)
	if _, err := mover.ArchiveByID(still.ID); err != nil {
		t.Fatal(err)
	}
	const pinnedAt = int64(654321)
	if err := cat.SetPinned(still.ID, true, catalog.PinSourceUser, pinnedAt); err != nil {
		t.Fatal(err)
	}

	realMoveIn := mover.moveIn
	injected := errors.New("injected second restore failure")
	calls := 0
	mover.SetMoveFuncs(nil, func(src, dst, archiveRoot string) error {
		calls++
		if calls == 2 {
			return injected
		}
		return realMoveIn(src, dst, archiveRoot)
	})

	if _, err := mover.RestoreByID(still.ID); !errors.Is(err, injected) {
		t.Fatalf("restore error = %v, want injected cause", err)
	}
	for _, original := range []*catalog.FileRecord{still, movie} {
		got, err := cat.GetFileByID(original.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != catalog.StatusArchived {
			t.Errorf("%s status after rollback = %q", original.RelativePath, got.Status)
		}
		if _, err := os.Stat(filepath.Join(coldDir, "archive", "TestFolder", original.RelativePath)); err != nil {
			t.Errorf("%s not returned to archive: %v", original.RelativePath, err)
		}
		if _, err := os.Stat(filepath.Join(syncDir, original.RelativePath)); !os.IsNotExist(err) {
			t.Errorf("%s still visible in synced folder after rollback: %v", original.RelativePath, err)
		}
	}
	gotStill, _ := cat.GetFileByID(still.ID)
	if !gotStill.Pinned || gotStill.PinSource != catalog.PinSourceUser ||
		gotStill.PinnedAt == nil || *gotStill.PinnedAt != pinnedAt {
		t.Errorf("first member archived pin after rollback = %v/%q/%v", gotStill.Pinned, gotStill.PinSource, gotStill.PinnedAt)
	}
	gotMovie, _ := cat.GetFileByID(movie.ID)
	if gotMovie.Pinned || gotMovie.PinSource != "" || gotMovie.PinnedAt != nil {
		t.Errorf("failed member archived pin changed = %v/%q/%v", gotMovie.Pinned, gotMovie.PinSource, gotMovie.PinnedAt)
	}
	if events, err := cat.ListEvents("TestFolder", "pair_split", "", 10); err != nil || len(events) != 0 {
		t.Fatalf("successful rollback wrote pair_split: events %+v err %v", events, err)
	}
}

func TestMoverArchiveReportsOriginalAndRollbackFailuresAndLogsTheRemainingSplit(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_14.heic", 80)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_14.mov", 120)
	assignTestPair(t, cat, "cid:fourteen", still, movie)
	mover := NewMover(cfg, cat, testLogger())
	mover.SetSpaceCheck(plentyOfSpace)
	realMoveOut := mover.moveOut
	originalFailure := errors.New("injected later-member failure")
	rollbackFailure := errors.New("injected rollback failure")
	calls := 0
	mover.SetMoveFuncs(func(src, dst string) error {
		calls++
		if calls == 2 {
			return originalFailure
		}
		return realMoveOut(src, dst)
	}, func(string, string, string) error {
		return rollbackFailure
	})

	_, err := mover.ArchiveByID(still.ID)
	if !errors.Is(err, originalFailure) || !errors.Is(err, rollbackFailure) {
		t.Fatalf("archive error = %v, want original and rollback causes", err)
	}
	events, listErr := cat.ListEvents("TestFolder", "pair_split", "", 10)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(events) != 1 || !strings.Contains(events[0].Details, "rollback") {
		t.Fatalf("pair_split events = %+v, want failed rollback", events)
	}
}

func TestArchiveLockedWritesPairSplitOnlyWhenCoverageOmitsASibling(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_3.heic", 80)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_3.mov", 120)
	assignTestPair(t, cat, "cid:three", still, movie)
	mover := NewMover(cfg, cat, testLogger())
	mover.SetSpaceCheck(plentyOfSpace)

	mover.mu.Lock()
	_, err := mover.archiveLocked(still.ID, true, map[int64]struct{}{still.ID: {}})
	mover.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	events, err := cat.ListEvents("TestFolder", "pair_split", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !strings.Contains(events[0].Details, movie.RelativePath) {
		t.Fatalf("pair_split events = %+v, want omitted sibling", events)
	}
}

func TestQuotaArchivesBothMembersWhenOnlyOneMemberWouldCoverTheOverage(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	cfg.Daemon.GracePeriodMinutes = 0
	cfg.Folders[0].BudgetBytes = 100
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_4.heic", 100)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_4.mov", 100)
	assignTestPair(t, cat, "cid:four", still, movie)
	ageFiles(t, cat, syncDir, still.RelativePath, movie.RelativePath)
	d := newTestDaemon(t, cfg, cat, testLogger())
	d.SetSpaceCheck(plentyOfSpace)

	d.EnforceQuotas()
	for _, file := range []*catalog.FileRecord{still, movie} {
		got, _ := cat.GetFileByID(file.ID)
		if got.Status != catalog.StatusArchived {
			t.Errorf("%s status = %q, want whole pair archived", file.RelativePath, got.Status)
		}
	}
}

func TestQuotaDoesNotArchiveEitherMemberWhenOneIsPinned(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	cfg.Daemon.GracePeriodMinutes = 0
	cfg.Folders[0].BudgetBytes = 100
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_8.heic", 100)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_8.mov", 100)
	assignTestPair(t, cat, "cid:eight", still, movie)
	if err := cat.SetPinned(movie.ID, true, catalog.PinSourceUser, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	ageFiles(t, cat, syncDir, still.RelativePath, movie.RelativePath)
	d := newTestDaemon(t, cfg, cat, testLogger())
	d.SetSpaceCheck(plentyOfSpace)

	d.EnforceQuotas()
	for _, file := range []*catalog.FileRecord{still, movie} {
		got, _ := cat.GetFileByID(file.ID)
		if got.Status != catalog.StatusSynced {
			t.Errorf("%s status = %q, pinned sibling must protect whole pair", file.RelativePath, got.Status)
		}
	}
}

type livePhotoExtractor struct {
	movieID      string
	stillID      string
	probeErr     error
	probeStarted chan struct{}
	probeRelease chan struct{}
}

type mappedLivePhotoExtractor struct {
	bulk  map[string]metadatapkg.Extracted
	probe map[string]metadatapkg.Extracted
}

func (e *mappedLivePhotoExtractor) Extract(_ context.Context, filePath, _ string) (metadatapkg.Extracted, error) {
	return e.bulk[filePath], nil
}

func (e *mappedLivePhotoExtractor) ProbeStillCandidate(_ context.Context, filePath, _ string) (metadatapkg.Extracted, error) {
	return e.probe[filePath], nil
}

func seedSyncedFileInFolder(t *testing.T, cat *catalog.Catalog, folder, root, relPath string,
	size int) *catalog.FileRecord {

	t.Helper()
	full := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	rec := &catalog.FileRecord{
		FolderLabel: folder, RelativePath: relPath, Filename: filepath.Base(relPath),
		Extension: filepath.Ext(relPath), SizeBytes: int64(size),
		FileMtime: info.ModTime().UTC(), FileAtime: info.ModTime().UTC(),
		Status: catalog.StatusSynced,
	}
	if _, err := cat.UpsertFile(rec, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	got, err := cat.GetFile(folder, relPath)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (e *livePhotoExtractor) Extract(_ context.Context, _ string, ext string) (metadatapkg.Extracted, error) {
	if isPairVideo(ext) {
		return metadatapkg.Extracted{ContentID: e.movieID}, nil
	}
	return metadatapkg.Extracted{}, nil
}

func (e *livePhotoExtractor) ProbeStillCandidate(_ context.Context, _ string, _ string) (metadatapkg.Extracted, error) {
	if e.probeStarted != nil {
		close(e.probeStarted)
	}
	if e.probeRelease != nil {
		<-e.probeRelease
	}
	if e.probeErr != nil {
		return metadatapkg.Extracted{}, e.probeErr
	}
	return metadatapkg.Extracted{ContentID: e.stillID}, nil
}

func TestPairAssignmentAndArchiveShareOneLockBoundary(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_9.heic", 10)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_9.mov", 10)
	started := make(chan struct{})
	release := make(chan struct{})
	d := newTestDaemon(t, cfg, cat, testLogger())
	d.SetSpaceCheck(plentyOfSpace)
	d.metadataExtractor = &livePhotoExtractor{
		movieID: "locked-id", stillID: "locked-id",
		probeStarted: started, probeRelease: release,
	}

	metadataDone := make(chan struct{})
	go func() {
		defer close(metadataDone)
		d.processMetadataQueue(context.Background(), []catalog.FileRecord{*movie},
			func(file *catalog.FileRecord) string {
				return filepath.Join(syncDir, file.RelativePath)
			}, 1)
	}()
	<-started // the MOV CID is written, but the candidate still is not yet assigned

	archiveDone := make(chan error, 1)
	go func() {
		_, err := d.mover.ArchiveByID(movie.ID)
		archiveDone <- err
	}()
	select {
	case err := <-archiveDone:
		t.Fatalf("archive escaped the pairing lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-metadataDone
	if err := <-archiveDone; err != nil {
		t.Fatal(err)
	}
	for _, file := range []*catalog.FileRecord{still, movie} {
		got, _ := cat.GetFileByID(file.ID)
		if got.Status != catalog.StatusArchived || got.PairKey != "cid:locked-id" {
			t.Errorf("%s after serialized pair/archive = status %q key %q",
				file.RelativePath, got.Status, got.PairKey)
		}
	}
}

func TestMetadataPairsMatchingContentIDsAndAttachesSidecars(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "Trips/IMG_5.heic", 10)
	movie := seedSyncedFile(t, cat, syncDir, "Trips/IMG_5.mov", 10)
	aae := seedSyncedFile(t, cat, syncDir, "Trips/IMG_5.aae", 2)
	xmp := seedSyncedFile(t, cat, syncDir, "Trips/IMG_5.heic.xmp", 2)
	d := newTestDaemon(t, cfg, cat, testLogger())
	d.metadataExtractor = &livePhotoExtractor{movieID: " SAME-ID ", stillID: "same-id"}

	d.extractMetadata(context.Background())
	for _, file := range []*catalog.FileRecord{still, movie, aae, xmp} {
		got, _ := cat.GetFileByID(file.ID)
		if got.PairKey != "cid:same-id" || got.PairSource != "content_id" {
			t.Errorf("%s pairing = %q (%q)", file.RelativePath, got.PairKey, got.PairSource)
		}
	}
}

func TestMetadataDisambiguatesImportRenumberCollisionByContentID(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "Imports/IMG_1234.heic", 10)
	trueMovie := seedSyncedFile(t, cat, syncDir, "Imports/IMG_1234(2).mov", 10)
	unrelated := seedSyncedFile(t, cat, syncDir, "Imports/IMG_1234.mov", 10)
	d := newTestDaemon(t, cfg, cat, testLogger())
	d.metadataExtractor = &mappedLivePhotoExtractor{
		bulk: map[string]metadatapkg.Extracted{
			filepath.Join(syncDir, trueMovie.RelativePath): {ContentID: "true-pair"},
			filepath.Join(syncDir, unrelated.RelativePath): {ContentID: "stranger"},
		},
		probe: map[string]metadatapkg.Extracted{
			filepath.Join(syncDir, still.RelativePath): {ContentID: "true-pair"},
		},
	}

	d.extractMetadata(context.Background())
	gotStill, _ := cat.GetFileByID(still.ID)
	gotTrueMovie, _ := cat.GetFileByID(trueMovie.ID)
	gotUnrelated, _ := cat.GetFileByID(unrelated.ID)
	if gotStill.PairKey != "cid:true-pair" || gotTrueMovie.PairKey != "cid:true-pair" {
		t.Fatalf("true renumbered pair = still %q movie %q", gotStill.PairKey, gotTrueMovie.PairKey)
	}
	if gotUnrelated.PairKey != "cid:stranger" {
		t.Fatalf("unrelated same-stem movie joined wrong pair: %q", gotUnrelated.PairKey)
	}
	expanded, err := cat.ExpandPairGroups([]int64{still.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(expanded) != 2 || expanded[0] != still.ID || expanded[1] != trueMovie.ID {
		t.Fatalf("renumbered pair expansion = %v", expanded)
	}
}

func TestMetadataRenumberCandidateStillRequiresMatchingContentID(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_2000.heic", 10)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_2000(3).mov", 10)
	d := newTestDaemon(t, cfg, cat, testLogger())
	d.metadataExtractor = &mappedLivePhotoExtractor{
		bulk: map[string]metadatapkg.Extracted{
			filepath.Join(syncDir, movie.RelativePath): {ContentID: "movie-only"},
		},
		probe: map[string]metadatapkg.Extracted{
			filepath.Join(syncDir, still.RelativePath): {ContentID: "still-only"},
		},
	}

	d.extractMetadata(context.Background())
	gotStill, _ := cat.GetFileByID(still.ID)
	gotMovie, _ := cat.GetFileByID(movie.ID)
	if gotStill.PairKey != "cid:still-only" || gotMovie.PairKey != "cid:movie-only" {
		t.Fatalf("mismatched renumber candidate = still %q movie %q", gotStill.PairKey, gotMovie.PairKey)
	}
}

func TestMetadataRenumberCandidateHonorsCaptureTimeWindow(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_3000.heic", 10)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_3000(2).mov", 10)
	now := time.Now().Unix()
	if err := cat.SetExtractedMetadata(still.ID, metadatapkg.Extracted{
		CapturedLocal: "2025-01-01 12:00:00", CapturedSource: "exif",
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetExtractedMetadata(movie.ID, metadatapkg.Extracted{
		CapturedLocal: "2025-01-01 12:01:01", CapturedSource: "quicktime", ContentID: "too-late",
	}, now); err != nil {
		t.Fatal(err)
	}
	d := newTestDaemon(t, cfg, cat, testLogger())
	d.metadataExtractor = &mappedLivePhotoExtractor{
		probe: map[string]metadatapkg.Extracted{
			filepath.Join(syncDir, still.RelativePath): {ContentID: "too-late"},
		},
	}

	d.extractMetadata(context.Background())
	gotStill, _ := cat.GetFileByID(still.ID)
	gotMovie, _ := cat.GetFileByID(movie.ID)
	if gotStill.PairKey != "" || gotMovie.PairKey != "cid:too-late" {
		t.Fatalf("out-of-window renumber candidate paired: still %q movie %q", gotStill.PairKey, gotMovie.PairKey)
	}
}

func TestMetadataPairCandidatesNeverCrossFolders(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	otherDir := newSyncthingFolder(t, filepath.Join(t.TempDir(), "other"))
	cfg.Folders = append(cfg.Folders, config.FolderConfig{
		Label: "OtherFolder", Path: otherDir, BudgetBytes: 5000,
	})
	declarePolicy(t, cfg, cat)
	firstStill := seedSyncedFileInFolder(t, cat, "TestFolder", syncDir, "IMG_4000.heic", 10)
	firstMovie := seedSyncedFileInFolder(t, cat, "TestFolder", syncDir, "IMG_4000(2).mov", 10)
	secondStill := seedSyncedFileInFolder(t, cat, "OtherFolder", otherDir, "IMG_4000.heic", 10)
	secondMovie := seedSyncedFileInFolder(t, cat, "OtherFolder", otherDir, "IMG_4000(2).mov", 10)
	d := newTestDaemon(t, cfg, cat, testLogger())
	d.metadataExtractor = &mappedLivePhotoExtractor{
		bulk: map[string]metadatapkg.Extracted{
			filepath.Join(syncDir, firstMovie.RelativePath):   {ContentID: "folder-one"},
			filepath.Join(otherDir, secondMovie.RelativePath): {ContentID: "folder-two"},
		},
		probe: map[string]metadatapkg.Extracted{
			filepath.Join(syncDir, firstStill.RelativePath):   {ContentID: "folder-one"},
			filepath.Join(otherDir, secondStill.RelativePath): {ContentID: "folder-two"},
		},
	}

	d.extractMetadata(context.Background())
	for _, tc := range []struct {
		file *catalog.FileRecord
		key  string
	}{
		{firstStill, "cid:folder-one"}, {firstMovie, "cid:folder-one"},
		{secondStill, "cid:folder-two"}, {secondMovie, "cid:folder-two"},
	} {
		got, _ := cat.GetFileByID(tc.file.ID)
		if got.PairKey != tc.key {
			t.Errorf("%s/%s key = %q, want %q", got.FolderLabel, got.RelativePath, got.PairKey, tc.key)
		}
	}
}

func TestMetadataBackfillsPairingForAlreadyScannedCandidates(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "Imports/IMG_10.heic", 10)
	movie := seedSyncedFile(t, cat, syncDir, "Imports/IMG_10.mov", 10)
	aae := seedSyncedFile(t, cat, syncDir, "Imports/IMG_10.aae", 2)
	now := time.Now().Unix()
	if err := cat.SetExtractedMetadata(still.ID, metadatapkg.Extracted{}, now); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetExtractedMetadata(movie.ID, metadatapkg.Extracted{ContentID: "backfill-id"}, now); err != nil {
		t.Fatal(err)
	}

	d := newTestDaemon(t, cfg, cat, testLogger())
	d.metadataExtractor = &livePhotoExtractor{stillID: "backfill-id"}
	d.extractMetadata(context.Background())

	for _, file := range []*catalog.FileRecord{still, movie, aae} {
		got, err := cat.GetFileByID(file.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.PairKey != "cid:backfill-id" || got.PairSource != "content_id" {
			t.Errorf("%s backfilled pairing = %q (%q)",
				file.RelativePath, got.PairKey, got.PairSource)
		}
	}
}

func TestMetadataPersistsMismatchedStillCIDWithoutPairingTheSidecar(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_6.heic", 10)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_6.mov", 10)
	aae := seedSyncedFile(t, cat, syncDir, "IMG_6.aae", 2)
	d := newTestDaemon(t, cfg, cat, testLogger())
	d.metadataExtractor = &livePhotoExtractor{movieID: "movie-id", stillID: "still-id"}

	d.extractMetadata(context.Background())
	gotStill, _ := cat.GetFileByID(still.ID)
	gotMovie, _ := cat.GetFileByID(movie.ID)
	gotAAE, _ := cat.GetFileByID(aae.ID)
	if gotStill.PairKey != "cid:still-id" || gotMovie.PairKey != "cid:movie-id" {
		t.Fatalf("mismatch was not preserved: still %q movie %q", gotStill.PairKey, gotMovie.PairKey)
	}
	if gotAAE.PairKey != "" {
		t.Errorf("ambiguous sidecar was paired as %q", gotAAE.PairKey)
	}
}

func TestMetadataUsesCanonicalDegradedStemOnlyWhenExifToolIsMissing(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "Edits/IMG_E1234.heic", 10)
	movie := seedSyncedFile(t, cat, syncDir, "Edits/IMG_1234.mov", 10)
	aae := seedSyncedFile(t, cat, syncDir, "Edits/IMG_1234.aae", 2)
	d := newTestDaemon(t, cfg, cat, testLogger())
	d.metadataExtractor = &livePhotoExtractor{
		movieID:  "known-only-on-video",
		probeErr: &exec.Error{Name: "exiftool", Err: exec.ErrNotFound},
	}

	d.extractMetadata(context.Background())
	for _, file := range []*catalog.FileRecord{still, movie, aae} {
		got, _ := cat.GetFileByID(file.ID)
		if got.PairKey != "stem:Edits/img_1234" || got.PairSource != "basename" {
			t.Errorf("%s degraded pairing = %q (%q)", file.RelativePath, got.PairKey, got.PairSource)
		}
	}
}

func TestMetadataDoesNotDegradeOnProbeTimeout(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	declarePolicy(t, cfg, cat)
	still := seedSyncedFile(t, cat, syncDir, "IMG_7.heic", 10)
	movie := seedSyncedFile(t, cat, syncDir, "IMG_7.mov", 10)
	d := newTestDaemon(t, cfg, cat, testLogger())
	d.metadataExtractor = &livePhotoExtractor{movieID: "movie-id", probeErr: context.DeadlineExceeded}

	d.extractMetadata(context.Background())
	gotStill, _ := cat.GetFileByID(still.ID)
	gotMovie, _ := cat.GetFileByID(movie.ID)
	if gotStill.PairKey != "" || gotStill.PairSource != "" {
		t.Errorf("timeout degraded still to %q (%q)", gotStill.PairKey, gotStill.PairSource)
	}
	if gotMovie.PairSource != "content_id" {
		t.Errorf("timeout replaced MOV authority: %q (%q)", gotMovie.PairKey, gotMovie.PairSource)
	}
	if gotStill.MetaScannedAt != nil || gotStill.MetaAttempts == 0 {
		t.Errorf("timeout did not leave a retryable still: %+v", gotStill)
	}
}

func TestCaptureTimesMoreThanAMinuteApartAreNotCandidates(t *testing.T) {
	now := time.Now().UTC()
	a := catalog.FileRecord{CapturedLocal: now.Format("2006-01-02 15:04:05")}
	b := catalog.FileRecord{CapturedLocal: now.Add(61 * time.Second).Format("2006-01-02 15:04:05")}
	if captureTimesClose(a, b) {
		t.Fatal("capture times 61 seconds apart were accepted")
	}
}
