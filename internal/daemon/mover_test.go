package daemon

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

// seedSyncedFile writes a real file into the folder and catalogues it with the
// metadata the scanner would have recorded, so the mover's source check sees a
// file that matches its row.
func seedSyncedFile(t *testing.T, cat *catalog.Catalog, syncDir, relPath string, size int) *catalog.FileRecord {
	t.Helper()
	full := filepath.Join(syncDir, relPath)
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
		FolderLabel:  "TestFolder",
		RelativePath: relPath,
		Filename:     filepath.Base(relPath),
		Extension:    filepath.Ext(relPath),
		SizeBytes:    int64(size),
		FileMtime:    info.ModTime().UTC(),
		FileAtime:    info.ModTime().UTC(),
		Status:       catalog.StatusSynced,
	}
	if _, err := cat.UpsertFile(rec, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	got, err := cat.GetFile("TestFolder", relPath)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func newTestMover(t *testing.T, cfg *config.Config, cat *catalog.Catalog) *Mover {
	t.Helper()
	declarePolicy(t, cfg, cat)
	m := NewMover(cfg, cat, testLogger())
	m.SetSpaceCheck(plentyOfSpace)
	return m
}

// This is the race D2 exists to lose gracefully: the daemon is 10 seconds into
// a 45-second copy of a 2 GB video when the owner sees it about to go and
// clicks pin. The pin must not be silently thrown away by the move that was
// already running, and the owner must be told what actually happened.
func TestPinShouldSurviveTheMoveWhenItArrivesWhileTheFileIsAlreadyBeingArchived(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "video.mp4", 2048)
	m := newTestMover(t, cfg, cat)

	moveStarted := make(chan struct{})
	releaseMove := make(chan struct{})
	realMoveOut := m.offloader.MoveOut
	m.SetMoveFuncs(func(src, dst string) error {
		close(moveStarted)
		<-releaseMove
		return realMoveOut(src, dst)
	}, nil)

	archiveDone := make(chan error, 1)
	go func() { _, err := m.ArchiveAutomatic(file.ID); archiveDone <- err }()
	<-moveStarted

	pinCommitted := make(chan struct{})
	go func() {
		defer close(pinCommitted)
		if _, err := m.SetPinned(file.ID, true); err != nil {
			t.Errorf("pinning during a move: %v", err)
		}
	}()

	// The pin must be BLOCKED, not applied against a row that is mid-move.
	select {
	case <-pinCommitted:
		t.Fatal("the pin committed while the move was still in flight; the mover mutex is not held")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseMove)
	if err := <-archiveDone; err != nil {
		t.Fatalf("archive: %v", err)
	}
	<-pinCommitted

	after, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != catalog.StatusArchived {
		t.Errorf("status = %q, want archived: the move had already committed", after.Status)
	}
	// MarkArchived must not clear the pin. The pin becomes a latent
	// instruction that takes effect the moment the file is restored.
	if !after.Pinned {
		t.Error("the pin was discarded by the move it arrived during")
	}
	if after.PinSource != catalog.PinSourceUser {
		t.Errorf("pin_source = %q, want %q", after.PinSource, catalog.PinSourceUser)
	}
}

func TestAutomaticOffloadShouldLeaveTheFileWhenAPinCommittedBeforeTheClaim(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "kept.jpg", 512)
	m := newTestMover(t, cfg, cat)

	if _, err := m.SetPinned(file.ID, true); err != nil {
		t.Fatal(err)
	}

	// The quota tick reaches this file with a candidate list built before the
	// pin landed. ClaimUnpinnedFile is the layer that catches it.
	_, err := m.ArchiveAutomatic(file.ID)
	if !errors.Is(err, ErrFileStateChanged) {
		t.Fatalf("err = %v, want ErrFileStateChanged", err)
	}

	after, _ := cat.GetFileByID(file.ID)
	if after.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", after.Status)
	}
	if _, err := os.Stat(filepath.Join(syncDir, "kept.jpg")); err != nil {
		t.Errorf("the pinned file must still be on disk: %v", err)
	}
}

func TestManualArchiveShouldClearThePinWhenTheOwnerAsksForItExplicitly(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "gone.jpg", 512)
	m := newTestMover(t, cfg, cat)

	if _, err := m.SetPinned(file.ID, true); err != nil {
		t.Fatal(err)
	}

	// The user who selects a pinned file and presses Archive means it. This is
	// the one path where an explicit action outranks the pin.
	after, err := m.ArchiveByID(file.ID)
	if err != nil {
		t.Fatalf("explicit archive of a pinned file: %v", err)
	}
	if after.Status != catalog.StatusArchived {
		t.Errorf("status = %q, want archived", after.Status)
	}
	if after.Pinned {
		t.Error("an explicit archive must clear the pin")
	}
	if after.PinSource != catalog.PinSourceNone {
		t.Errorf("pin_source = %q, want empty", after.PinSource)
	}
	if _, err := os.Stat(filepath.Join(coldDir, "archive", "TestFolder", "gone.jpg")); err != nil {
		t.Errorf("the file should be in cold storage: %v", err)
	}
}

func TestManualArchiveShouldIgnoreTheGracePeriodAndTheBudgetWhenTheOwnerAsksForIt(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	// Brand new file, folder far under its budget: nothing the quota tick
	// would ever touch.
	cfg.Folders[0].BudgetBytes = 1 << 30
	cfg.Daemon.GracePeriodMinutes = 600
	file := seedSyncedFile(t, cat, syncDir, "fresh.jpg", 128)
	m := newTestMover(t, cfg, cat)

	after, err := m.ArchiveByID(file.ID)
	if err != nil {
		t.Fatalf("manual archive: %v", err)
	}
	if after.Status != catalog.StatusArchived {
		t.Errorf("status = %q, want archived", after.Status)
	}
}

// A bulk Archive over "select all in folder" can reach a file Syncthing is
// still writing. Renaming an active download into cold storage makes the pull
// fail with ENOENT and restart from block zero, and leaves a fragment in the
// archive that looks like a restorable file.
func TestArchiveShouldRefuseWhenTheSourceIsStillBeingWritten(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "incoming.mp4", 1024)
	m := newTestMover(t, cfg, cat)

	// The transfer advances: more bytes, newer mtime.
	full := filepath.Join(syncDir, "incoming.mp4")
	if err := os.WriteFile(full, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	os.Chtimes(full, later, later)

	_, err := m.ArchiveByID(file.ID)
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("err = %v, want ErrSourceChanged", err)
	}

	after, _ := cat.GetFileByID(file.ID)
	if after.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced: nothing was moved", after.Status)
	}
	if _, err := os.Stat(full); err != nil {
		t.Errorf("the in-flight transfer must be left alone: %v", err)
	}
}

func TestArchiveShouldRefuseWhenColdStorageIsNearlyFull(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "big.mp4", 1024)
	m := newTestMover(t, cfg, cat)
	m.SetSpaceCheck(func(string) (float64, error) { return 4.2, nil })

	_, err := m.ArchiveByID(file.ID)
	if !errors.Is(err, ErrColdStorageFull) {
		t.Fatalf("err = %v, want ErrColdStorageFull", err)
	}
	after, _ := cat.GetFileByID(file.ID)
	if after.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", after.Status)
	}
}

func TestArchiveShouldRefuseWhenTheFolderRootIsNotMounted(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "photo.jpg", 128)
	m := newTestMover(t, cfg, cat)

	// An unmounted volume looks exactly like an empty folder except for this.
	if err := os.Remove(filepath.Join(syncDir, ".stfolder")); err != nil {
		t.Fatal(err)
	}

	if _, err := m.ArchiveByID(file.ID); !errors.Is(err, ErrFolderUnavailable) {
		t.Fatalf("err = %v, want ErrFolderUnavailable", err)
	}
}

func TestRestoreShouldAdoptTheDeviceCopyWhenSomethingAlreadyOccupiesTheDestination(t *testing.T) {
	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "shared.jpg", 128)
	m := newTestMover(t, cfg, cat)

	if _, err := m.ArchiveByID(file.ID); err != nil {
		t.Fatal(err)
	}

	// A device that edited the file while offline reconnects, and Syncthing's
	// version vector outranks coldstore's deletion: the path is occupied again
	// while the row still says archived.
	newer := []byte("the version from the laptop")
	if err := os.WriteFile(filepath.Join(syncDir, "shared.jpg"), newer, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := m.RestoreByID(file.ID)
	if !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("err = %v, want ErrDestinationExists", err)
	}

	// Nothing is destroyed: the device copy stands, the archived copy is set
	// aside, and the row now describes the file that is actually there.
	got, err := os.ReadFile(filepath.Join(syncDir, "shared.jpg"))
	if err != nil || string(got) != string(newer) {
		t.Errorf("the newer device copy must not be overwritten (read %q, %v)", got, err)
	}
	aside := filepath.Join(coldDir, "archive", "TestFolder", "shared.jpg"+supersededSuffix)
	if _, err := os.Stat(aside); err != nil {
		t.Errorf("the archived copy should be set aside, not deleted: %v", err)
	}
	after, _ := cat.GetFileByID(file.ID)
	if after.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced: coldstore adopted the on-device copy", after.Status)
	}
}

func TestRestoreShouldPinTheFileWhenTheMoveCommits(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "wanted.jpg", 256)
	m := newTestMover(t, cfg, cat)

	if _, err := m.ArchiveByID(file.ID); err != nil {
		t.Fatal(err)
	}
	after, err := m.RestoreByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}

	if after.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", after.Status)
	}
	if !after.Pinned {
		t.Fatal("a restored file must be pinned, or the next quota tick takes it straight back")
	}
	if after.PinSource != catalog.PinSourceRestore {
		t.Errorf("pin_source = %q, want %q", after.PinSource, catalog.PinSourceRestore)
	}
	if after.PinnedAt == nil {
		t.Error("pinned_at should be set")
	}
}

func TestPinShouldBeAcceptedWhenTheFileIsInColdStorage(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "away.jpg", 256)
	m := newTestMover(t, cfg, cat)

	if _, err := m.ArchiveByID(file.ID); err != nil {
		t.Fatal(err)
	}

	// Pinning an archived file is legal: it is a latent instruction that takes
	// effect on restore.
	after, err := m.SetPinned(file.ID, true)
	if err != nil {
		t.Fatalf("pinning an archived file: %v", err)
	}
	if !after.Pinned || after.Status != catalog.StatusArchived {
		t.Errorf("got pinned=%v status=%q, want pinned on an archived row", after.Pinned, after.Status)
	}
}

func TestPinShouldReportNoSuchFileWhenTheIDIsUnknown(t *testing.T) {
	_, _, cfg, cat := setupTestEnv(t)
	m := newTestMover(t, cfg, cat)

	if _, err := m.SetPinned(999999, true); !errors.Is(err, catalog.ErrNoSuchFile) {
		t.Fatalf("err = %v, want catalog.ErrNoSuchFile", err)
	}
}

func TestFolderReadyShouldRefuseWhenTheMarkerIsMissing(t *testing.T) {
	dir := t.TempDir()

	err := FolderReady(dir)
	if !errors.Is(err, ErrFolderUnavailable) {
		t.Fatalf("err = %v, want ErrFolderUnavailable for a directory with no .stfolder", err)
	}
	if !strings.Contains(err.Error(), ".stfolder") {
		t.Errorf("the error should name the marker so the owner can act on it: %v", err)
	}

	newSyncthingFolder(t, dir)
	if err := FolderReady(dir); err != nil {
		t.Errorf("a real syncthing folder should be ready: %v", err)
	}
}

// The production shape of the invariant-10 hole, driven through RestoreByID
// with the cross-device branch forced -- which is the branch every real
// deployment takes, the Syncthing folder and the cold pool being different
// filesystems.
//
// A reviewer reproduced this end to end: restore reported success, the folder
// held the archived copy, and the file Syncthing delivered mid-restore was
// gone with no .coldstore-superseded sibling anywhere. Syncthing then
// propagates that overwrite to every device as an ordinary local edit, so no
// conflict file appears either.
func TestRestoreShouldNotOverwriteAFileSyncthingDeliversWhileTheRestoreIsCopying(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "photo.jpg", 64)
	m := newTestMover(t, cfg, cat)

	if _, err := m.ArchiveByID(file.ID); err != nil {
		t.Fatalf("archiving: %v", err)
	}

	dst := filepath.Join(syncDir, "photo.jpg")
	const delivered = "the newer copy the phone had all along"

	// Force the cross-device path and drop the interloper in mid-copy.
	m.SetMoveFuncs(nil, func(src, moveDst, archiveRoot string) error {
		return crossDeviceMove(src, moveDst, testLogger())
	})
	withDestWriter(t, func(f *os.File) io.Writer {
		return writerFunc(func(p []byte) (int, error) {
			if _, err := os.Lstat(dst); os.IsNotExist(err) {
				os.WriteFile(dst, []byte(delivered), 0o644)
			}
			return f.Write(p)
		})
	})

	_, err := m.RestoreByID(file.ID)
	if !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("RestoreByID error = %v, want ErrDestinationExists", err)
	}

	got, readErr := os.ReadFile(dst)
	if readErr != nil || string(got) != delivered {
		t.Fatalf("DATA LOSS: the folder holds %q (%v), want the delivered copy", got, readErr)
	}

	// The row adopted the device copy, and the archived bytes were kept beside
	// the cold path rather than deleted.
	rec, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced (the device copy is adopted)", rec.Status)
	}
	coldDir := filepath.Join(cfg.Storage.ColdStoragePath, "archive", "TestFolder")
	entries, _ := os.ReadDir(coldDir)
	var kept bool
	for _, e := range entries {
		if strings.Contains(e.Name(), supersededSuffix) {
			kept = true
		}
	}
	if !kept {
		t.Errorf("the archived copy was not kept aside; cold dir holds %v", entries)
	}
}

// D2 says a pin is held until the user unpins. An explicit archive clears it
// on purpose (design section 4.6 rule 3), but a FAILED archive returns an
// error the caller reads as "nothing happened" -- and if the pin stayed
// cleared, the next quota tick takes the file the owner had pinned.
func TestArchiveShouldRestoreThePinWhenTheMoveFails(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "pinned.jpg", 128)
	m := newTestMover(t, cfg, cat)

	if _, err := m.SetPinned(file.ID, true); err != nil {
		t.Fatalf("pinning: %v", err)
	}
	before, _ := cat.GetFileByID(file.ID)

	m.SetMoveFuncs(func(src, dst string) error {
		return errors.New("simulated cold storage failure")
	}, nil)

	if _, err := m.ArchiveByID(file.ID); err == nil {
		t.Fatal("expected the archive to fail")
	}

	after, err := cat.GetFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Pinned {
		t.Fatal("the pin was cleared by an archive that failed")
	}
	if after.PinSource != before.PinSource {
		t.Errorf("pin_source = %q, want %q", after.PinSource, before.PinSource)
	}
	if after.PinnedAt == nil || before.PinnedAt == nil || *after.PinnedAt != *before.PinnedAt {
		t.Errorf("pinned_at changed: %v -> %v", before.PinnedAt, after.PinnedAt)
	}
	if after.Status != catalog.StatusSynced {
		t.Errorf("status = %q, want synced", after.Status)
	}
}

// The same rule for the claim that never happens: a file another actor has
// already moved out from under us must not lose its pin either.
func TestArchiveShouldRestoreThePinWhenTheClaimIsLost(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "contested.jpg", 128)
	m := newTestMover(t, cfg, cat)

	if _, err := m.SetPinned(file.ID, true); err != nil {
		t.Fatalf("pinning: %v", err)
	}

	// The free-space probe runs after the status re-read and before the claim,
	// so it is the seam that puts another actor inside that exact window --
	// which is the only way to reach the "claimed == false" branch and see
	// whether it puts the pin back.
	m.SetMoveFuncs(func(src, dst string) error { return errors.New("unreachable") }, nil)
	m.SetSpaceCheck(func(string) (float64, error) {
		if _, err := cat.ClaimFile(file.ID, catalog.StatusSynced, catalog.StatusRestoring); err != nil {
			t.Errorf("stealing the row: %v", err)
		}
		return 90.0, nil
	})

	if _, err := m.ArchiveByID(file.ID); !errors.Is(err, ErrFileStateChanged) {
		t.Fatalf("error = %v, want ErrFileStateChanged", err)
	}
	after, _ := cat.GetFileByID(file.ID)
	if !after.Pinned {
		t.Error("the pin was cleared by an archive that never started")
	}
}

// The other half of the mountpoint guard. Startup catches the boot case; this
// catches the volume that goes away afterwards, which nothing else would.
//
// The offload direction is where it matters: it writes the only surviving copy
// of the file and then deletes the source, so an unmounted cold_storage_path is
// a file destroyed rather than a file misplaced. And the 5%-free floor cannot
// stand in for this -- statfs on the stub answers about the ROOT filesystem,
// which on a real box has plenty of room.
func TestArchiveShouldRefuseWhenColdStorageStoppedBeingAMountPoint(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	cfg.Storage.RequireMountpoint = true
	file := seedSyncedFile(t, cat, syncDir, "photo.jpg", 2048)
	m := newTestMover(t, cfg, cat)

	_, err := m.ArchiveAutomatic(file.ID)
	if !errors.Is(err, ErrColdStorageUnmounted) {
		t.Fatalf("archive error = %v, want ErrColdStorageUnmounted", err)
	}

	after, err := cat.GetFile("TestFolder", "photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != catalog.StatusSynced {
		t.Errorf("status = %s, want synced: a refused archive must leave the row alone",
			after.Status)
	}
	if _, err := os.Stat(filepath.Join(syncDir, "photo.jpg")); err != nil {
		t.Errorf("the source file was touched by a refused archive: %v", err)
	}
}

func TestArchiveShouldProceedOnAPlainColdStorageDirectoryByDefault(t *testing.T) {
	syncDir, _, cfg, cat := setupTestEnv(t)
	file := seedSyncedFile(t, cat, syncDir, "photo.jpg", 2048)
	m := newTestMover(t, cfg, cat)

	if _, err := m.ArchiveAutomatic(file.ID); err != nil {
		t.Fatalf("require_mountpoint is off, so a plain directory must archive: %v", err)
	}
}
