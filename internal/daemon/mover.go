package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

// minFreeSpacePercent is the cold-storage floor. Below it no file is moved in,
// whether by the quota tick, a manual archive or a job.
const minFreeSpacePercent = 5.0

// sourceChangedDedupeMinutes is how long one refused-because-the-source-moved
// report suppresses the next one for the same folder.
//
// The quota tick's pre-check re-stats the candidate but only compares its
// MTIME, so a row whose catalogued SIZE disagrees with the disk is picked as a
// candidate on every single tick and refused here every single time: one
// events row per tick, forever, until the next full walk rewrites the row.
// That is the cold_storage_low flood in a different costume.
//
// Suppression keys on (event_type, folder) and not on path, so this is
// deliberately coarse. One report per folder per hour is what a human reading
// the Activity list needs; the per-file detail is in the row that did get
// written, and the condition is by construction still true an hour later.
const sourceChangedDedupeMinutes = 60

var (
	// ErrFileStateChanged means the row was not in the status the move
	// required, either when it was re-read or when the compare-and-swap ran.
	// Something else -- the quota tick, another request, a pin -- got there
	// first. It is a 409, never a 500.
	ErrFileStateChanged = errors.New("file state changed before the move could start")

	// ErrColdStorageFull means cold storage is below minFreeSpacePercent.
	ErrColdStorageFull = errors.New("cold storage below 5% free")

	// ErrColdStorageUnmounted means cold_storage_path stopped being a mount
	// point while the daemon was running, and [storage] require_mountpoint is
	// set. Startup catches the boot case; this catches the unmount that
	// happens afterwards, which nothing else would.
	ErrColdStorageUnmounted = errors.New("cold storage is no longer a mounted volume")

	// ErrInvalidResolution means POST /files/{id}/resolve was given a `keep`
	// that is neither of the two legal values. It is a 400, not a 409.
	ErrInvalidResolution = errors.New(`keep must be "device" or "archive"`)
)

// The two directions a section 4.1b conflict can be resolved in.
const (
	ResolveKeepDevice  = "device"
	ResolveKeepArchive = "archive"
)

// Mover is the ONE place a file changes status, changes place, or changes its
// pin. Every entry point -- the quota tick, the restore endpoint, the archive
// endpoint, the pin endpoints, and later the job worker -- goes through it and
// takes its mutex.
//
// The mutex is what makes D2's promise true. A pin request that arrives while
// the daemon is already copying that file blocks until the move finishes and
// then commits against the settled row, so the owner is never shown a filled
// pin badge on a file that is about to leave. Without a single serialisation
// point the pin and the move race and the move wins silently.
//
// There must be exactly ONE Mover per process: main constructs it and injects
// the same pointer into the daemon and the API server. Two Movers over one
// catalog are two mutexes and no serialisation at all.
type Mover struct {
	mu sync.Mutex

	// scannerMu is the process scanner's existing exclusion lock. Rename holds
	// it across all filesystem actions and the catalog commit so a periodic walk
	// cannot publish the destination coordinate inside that deliberate window of
	// filesystem/catalog disagreement. Scanner never takes the mover lock, so
	// this lock order cannot cycle.
	scannerMu *sync.Mutex

	cat       *catalog.Catalog
	cfg       *config.Config
	logger    *slog.Logger
	offloader *Offloader
	restorer  *Restorer

	// Injectable seams. Tests replace them; production uses the defaults.
	spaceCheck func(string) (float64, error)
	moveOut    func(src, dst string) error
	moveIn     func(src, dst, archiveRoot string) error

	// adoptRow is catalog.AdoptDeviceCopy behind a seam. It exists so a test
	// can make the compare-and-swap LOSE -- the row leaving 'archived' while a
	// conflict is being resolved -- which is otherwise unreachable from
	// outside, because everything else that could move that row is queued
	// behind the mutex this method already holds.
	adoptRow func(id, sizeBytes int64, mtime, atime time.Time) (bool, error)

	// beforeRenameCommit is an adversarial-test seam at the last possible point
	// before the atomic no-replace syscall. Production leaves it nil.
	beforeRenameCommit func(renameFSAction) error
}

func (m *Mover) bindScanner(scanner *Scanner) {
	if scanner != nil {
		m.scannerMu = &scanner.mu
	}
}

func NewMover(cfg *config.Config, cat *catalog.Catalog, logger *slog.Logger) *Mover {
	offloader := NewOffloader(cfg, logger)
	restorer := NewRestorer(cfg, logger)
	return &Mover{
		cat:        cat,
		cfg:        cfg,
		logger:     logger,
		offloader:  offloader,
		restorer:   restorer,
		spaceCheck: AvailableSpacePercent,
		moveOut:    offloader.MoveOut,
		moveIn:     restorer.MoveIn,
		adoptRow:   cat.AdoptDeviceCopy,
	}
}

// coldStorageReady refuses an offload into a cold_storage_path that is no
// longer a mounted volume. It is a no-op unless [storage] require_mountpoint is
// set, because a plain directory is a supported layout.
func (m *Mover) coldStorageReady() error {
	if !m.cfg.Storage.RequireMountpoint {
		return nil
	}
	path := m.cfg.Storage.ColdStoragePath
	state, err := config.DescribeMount(path)
	if err != nil {
		return fmt.Errorf("%w: cannot stat %s: %v", ErrColdStorageUnmounted, path, err)
	}
	if state.IsMountPoint {
		return nil
	}
	m.cat.LogEventDeduped("cold_storage_unmounted", "", "",
		fmt.Sprintf("offload refused: %s is not a mounted volume", path), 60)
	m.logger.Error("cold storage is not a mounted volume, refusing to archive",
		"path", path)
	return fmt.Errorf("%w: %s", ErrColdStorageUnmounted, path)
}

// SetSpaceCheck replaces the cold-storage free-space probe, for testing.
func (m *Mover) SetSpaceCheck(fn func(string) (float64, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spaceCheck = fn
}

// SetMoveFuncs replaces the two filesystem moves, for testing. A test that
// needs to observe what happens DURING a move -- a pin arriving mid-copy, say
// -- installs a move that blocks, and gets the real ordering rather than a
// simulated one.
func (m *Mover) SetMoveFuncs(out func(src, dst string) error, in func(src, dst, archiveRoot string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if out != nil {
		m.moveOut = out
	}
	if in != nil {
		m.moveIn = in
	}
}

// ---------------------------------------------------------------------------
// pins

// SetPinned writes one file's pin and returns the settled row.
//
// It takes the mover mutex, so it cannot commit while a move of that file is
// in flight; it waits for the move and then writes against whatever the move
// left behind. Pinning is legal in every status, including archived (where the
// pin is a latent instruction that takes effect on restore) and missing.
func (m *Mover) SetPinned(id int64, pinned bool) (*catalog.FileRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := m.cat.GetFileByID(id); err != nil {
		return nil, err
	}
	if err := m.cat.SetPinned(id, pinned, catalog.PinSourceUser, time.Now().Unix()); err != nil {
		return nil, err
	}
	return m.cat.GetFileByID(id)
}

// SetPinnedBulk is SetPinned over many ids under a single mutex acquisition.
// It returns the number of rows written; ids that match nothing are simply not
// counted.
func (m *Mover) SetPinnedBulk(ids []int64, pinned bool) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cat.SetPinnedBulk(ids, pinned, catalog.PinSourceUser, time.Now().Unix())
}

// ---------------------------------------------------------------------------
// moves

// ArchiveByID moves a synced file and every member of its pair group to cold
// storage on explicit instruction.
//
// It bypasses the grace period and the budget -- a manual or job-driven
// archive is something the owner asked for -- but it applies the free-space
// guard, and it CLEARS THE PIN first: the user who selects a pinned file and
// presses Archive means it (design section 4.6 rule 3).
func (m *Mover) ArchiveByID(id int64) (*catalog.FileRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.cat.GetFileByID(id); err != nil {
		return nil, err
	}
	if err := m.archiveGroupLocked([]int64{id}, true, false); err != nil {
		return nil, err
	}
	return m.cat.GetFileByID(id)
}

// ArchiveAutomatic is the quota tick's variant. It differs from ArchiveByID in
// exactly two ways, both of them load-bearing for D2: it never clears the pin,
// and it claims the row with ClaimUnpinnedFile, so a pin that commits between
// the candidate query and the claim makes the claim fail and the file stays.
// That is the third and authoritative layer of pin protection.
func (m *Mover) ArchiveAutomatic(id int64) (*catalog.FileRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.cat.GetFileByID(id); err != nil {
		return nil, err
	}
	if err := m.archiveGroupLocked([]int64{id}, false, false); err != nil {
		return nil, err
	}
	return m.cat.GetFileByID(id)
}

// ArchiveAutomaticGroup is the quota path. The supplied ids must already be a
// complete group whose pin/persistence/grace checks the daemon performed. If
// pairing changed since that snapshot, no bytes move and the next tick retries
// with a fresh group rather than archiving an unchecked companion.
func (m *Mover) ArchiveAutomaticGroup(ids []int64) ([]catalog.FileRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.archiveGroupLocked(ids, false, true); err != nil {
		return nil, err
	}
	expanded, err := m.cat.ExpandPairGroups(ids)
	if err != nil {
		return nil, err
	}
	return m.cat.FilesByIDs(expanded)
}

func (m *Mover) archiveGroupLocked(ids []int64, explicit, requireClosed bool) error {
	expanded, err := m.cat.ExpandPairGroups(ids)
	if err != nil {
		return err
	}
	if requireClosed && !sameIDs(ids, expanded) {
		return fmt.Errorf("%w: pair membership changed before automatic archive", ErrFileStateChanged)
	}
	files, err := m.cat.FilesByIDs(expanded)
	if err != nil {
		return err
	}
	if len(files) != len(expanded) {
		return fmt.Errorf("%w: pair group contains a file that no longer exists", ErrFileStateChanged)
	}
	covered := make(map[int64]struct{}, len(files))
	for i := range files {
		covered[files[i].ID] = struct{}{}
		switch files[i].Status {
		case catalog.StatusSynced:
			if !explicit && files[i].Pinned {
				return fmt.Errorf("%w: paired file %d became pinned", ErrFileStateChanged, files[i].ID)
			}
			fc, err := m.folderForMove(files[i].FolderLabel)
			if err != nil {
				return err
			}
			if err := m.sourceUnchanged(filepath.Join(fc.SyncthingPath, files[i].RelativePath), &files[i]); err != nil {
				return err
			}
		case catalog.StatusArchived:
			// Already in the requested state: an idempotent no-op.
		default:
			return fmt.Errorf("%w: paired file %d is %s", ErrFileStateChanged,
				files[i].ID, files[i].Status)
		}
	}

	// A filesystem has no transaction spanning several paths. Keep the mover
	// lock for the complete attempt and compensate synchronously if a later
	// member fails, but be honest about the remaining boundary: SIGKILL, power
	// loss, or another process observing the folder between two renames can still
	// see an intermediate split. Startup recovery and pair_split remain the
	// backstops for those unavoidable crash/visibility windows.
	completed := make([]catalog.FileRecord, 0, len(files))
	for i := range files {
		if files[i].Status == catalog.StatusArchived {
			continue
		}
		if _, err := m.archiveLocked(files[i].ID, explicit, covered); err != nil {
			original := fmt.Errorf("archiving pair member %d: %w", files[i].ID, err)
			rollbackErrs := m.rollbackArchivedMembers(completed)
			if verifyErr := m.verifyOriginalGroupState(files); verifyErr != nil {
				rollbackErrs = append(rollbackErrs, verifyErr)
			}
			if len(rollbackErrs) == 0 {
				return original
			}
			m.logSplitGroups(expanded, "archive group rollback could not rejoin every member")
			return errors.Join(append([]error{original}, rollbackErrs...)...)
		}
		completed = append(completed, files[i])
	}
	return nil
}

func (m *Mover) rollbackArchivedMembers(completed []catalog.FileRecord) []error {
	var rollbackErrs []error
	for i := len(completed) - 1; i >= 0; i-- {
		original := &completed[i]
		if _, err := m.restoreLocked(original.ID); err != nil {
			rollbackErrs = append(rollbackErrs,
				fmt.Errorf("rolling archived member %d back to synced: %w", original.ID, err))
		}
		if err := m.restorePinState(original); err != nil {
			rollbackErrs = append(rollbackErrs,
				fmt.Errorf("restoring original pin for member %d: %w", original.ID, err))
		}
	}
	return rollbackErrs
}

func (m *Mover) archiveLocked(id int64, explicit bool, covered map[int64]struct{}) (*catalog.FileRecord, error) {
	// Re-read inside the lock; never trust what the caller was holding.
	rec, err := m.cat.GetFileByID(id)
	if err != nil {
		return nil, err
	}
	if rec.Status == catalog.StatusArchived {
		return rec, nil // invariant 12: already there, no-op success
	}
	if rec.Status != catalog.StatusSynced {
		return nil, fmt.Errorf("%w: file %d is %s", ErrFileStateChanged, id, rec.Status)
	}
	m.warnPairSplit(rec, covered)

	fc, err := m.folderForMove(rec.FolderLabel)
	if err != nil {
		return nil, err
	}

	// Before the space check, because an unmounted volume makes the space
	// check answer about the ROOT filesystem and pass. This is FolderReady's
	// job done for the other side of the move: the offload direction writes
	// the only surviving copy of a file, so a cold_storage_path that has
	// silently become a stub directory on root is a file destroyed, not a file
	// misplaced. Restore needs no equivalent -- its source is in cold storage,
	// so an unmounted volume fails the open and nothing is lost.
	if err := m.coldStorageReady(); err != nil {
		return nil, err
	}

	free, err := m.spaceCheck(m.cfg.Storage.ColdStoragePath)
	if err != nil {
		return nil, fmt.Errorf("checking cold storage space: %w", err)
	}
	if free < minFreeSpacePercent {
		m.cat.LogEventDeduped("cold_storage_low", rec.FolderLabel, "",
			fmt.Sprintf("offload paused: cold storage at %.1f%% free", free), 60)
		m.logger.Warn("cold storage below 5% free, refusing to archive",
			"folder", rec.FolderLabel, "free_percent", free)
		return nil, ErrColdStorageFull
	}

	src := filepath.Join(fc.SyncthingPath, rec.RelativePath)
	dst := m.offloader.ArchivePath(rec.FolderLabel, rec.RelativePath)

	// The guard against archiving an in-flight Syncthing transfer. A manual
	// archive skips the grace period by design, so this stat inside the lock
	// is the only thing between a bulk Archive and a file Syncthing is still
	// writing: renaming an active download into cold storage makes Syncthing's
	// pull fail with ENOENT and restart the transfer from block zero, and
	// leaves a fragment in the archive that looks like a restorable file
	// (design section 5.9).
	if err := m.sourceUnchanged(src, rec); err != nil {
		return nil, err
	}

	// Design section 4.6 rule 3: an explicit archive clears the pin before it
	// claims, because the user who selects a pinned file and presses Archive
	// means it. But the clear must be UNDONE on every failure path. The caller
	// gets a 409 or a 500 and reasonably concludes nothing happened, and if
	// the pin stayed cleared the next quota tick would take the file the owner
	// had pinned -- the one piece of state D2 promises is held until they
	// unpin it.
	restorePin := func() {}
	if explicit {
		restorePin = m.pinRestorer(rec)
		if err := m.cat.SetPinned(id, false, "", 0); err != nil {
			return nil, fmt.Errorf("clearing pin before archive: %w", err)
		}
	}

	claimed, err := m.claimForArchive(id, explicit)
	if err != nil {
		restorePin()
		return nil, err
	}
	if !claimed {
		restorePin()
		return nil, fmt.Errorf("%w: file %d could not be claimed for archiving", ErrFileStateChanged, id)
	}

	if err := m.moveOut(src, dst); err != nil {
		// ClaimFileOnDevice, not ClaimFile: the file is still sitting in the
		// folder, and a walk that started before this revert would otherwise
		// sweep the row to missing on its way out.
		if _, rerr := m.cat.ClaimFileOnDevice(id, catalog.StatusArchiving, catalog.StatusSynced); rerr != nil {
			m.logger.Error("cannot revert a failed archive to synced",
				"id", id, "path", rec.RelativePath, "error", rerr)
		}
		restorePin()
		if errors.Is(err, ErrSourceChanged) {
			m.cat.LogEvent("source_changed", rec.FolderLabel, rec.RelativePath,
				"the file was rewritten while it was being archived; both copies kept")
		}
		return nil, err
	}

	ok, err := m.cat.MarkArchived(id, time.Now())
	if err != nil {
		return nil, fmt.Errorf("recording archive: %w", err)
	}
	if !ok {
		// Invariant 11. The bytes moved but the row says something else.
		m.logger.Error("MarkArchived affected 0 rows after a completed move",
			"id", id, "path", rec.RelativePath, "dst", dst)
		m.cat.LogEvent("archive_unconfirmed", rec.FolderLabel, rec.RelativePath,
			"file moved to cold storage but the catalog row was not in 'archiving'")
		return nil, fmt.Errorf("archive of %d completed but could not be recorded", id)
	}

	m.cat.LogEvent("archive", rec.FolderLabel, rec.RelativePath,
		fmt.Sprintf("offloaded %d bytes", rec.SizeBytes))
	m.logger.Info("offloaded file",
		"folder", rec.FolderLabel, "path", rec.RelativePath, "size", rec.SizeBytes)

	return m.cat.GetFileByID(id)
}

// pinRestorer captures a file's pin as it was before an explicit archive
// cleared it, and returns a function that puts it back. An unpinned file
// yields a no-op, so callers can invoke the result unconditionally.
func (m *Mover) pinRestorer(rec *catalog.FileRecord) func() {
	return func() {
		if !rec.Pinned {
			return
		}
		if err := m.restorePinState(rec); err != nil {
			m.logger.Error("cannot restore the pin an abandoned archive cleared",
				"id", rec.ID, "path", rec.RelativePath, "error", err)
		}
	}
}

// restorePinState puts a row's pin back exactly as it was in a group snapshot.
// It is stricter than pinRestorer: compensating moves also have to clear the
// automatic restore pin when the original row was unpinned.
func (m *Mover) restorePinState(rec *catalog.FileRecord) error {
	if !rec.Pinned {
		return m.cat.SetPinned(rec.ID, false, "", 0)
	}
	source := rec.PinSource
	if source != catalog.PinSourceUser && source != catalog.PinSourceRestore {
		source = catalog.PinSourceUser
	}
	at := time.Now().Unix()
	if rec.PinnedAt != nil {
		at = *rec.PinnedAt
	}
	return m.cat.SetPinned(rec.ID, true, source, at)
}

func (m *Mover) claimForArchive(id int64, explicit bool) (bool, error) {
	if explicit {
		return m.cat.ClaimFile(id, catalog.StatusSynced, catalog.StatusArchiving)
	}
	return m.cat.ClaimUnpinnedFile(id)
}

// RestoreByID moves an archived file and every member of its pair group back
// to the Syncthing folder and pins each member that was restored.
func (m *Mover) RestoreByID(id int64) (*catalog.FileRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.cat.GetFileByID(id); err != nil {
		return nil, err
	}
	if err := m.restoreGroupLocked([]int64{id}); err != nil {
		return nil, err
	}
	return m.cat.GetFileByID(id)
}

func (m *Mover) restoreGroupLocked(ids []int64) error {
	expanded, err := m.cat.ExpandPairGroups(ids)
	if err != nil {
		return err
	}
	files, err := m.cat.FilesByIDs(expanded)
	if err != nil {
		return err
	}
	if len(files) != len(expanded) {
		return fmt.Errorf("%w: pair group contains a file that no longer exists", ErrFileStateChanged)
	}
	hasArchived := false
	for i := range files {
		hasArchived = hasArchived || files[i].Status == catalog.StatusArchived
	}
	for i := range files {
		switch files[i].Status {
		case catalog.StatusArchived:
			if _, err := m.folderForMove(files[i].FolderLabel); err != nil {
				return err
			}
			src := m.restorer.ColdPath(files[i].FolderLabel, files[i].RelativePath)
			if _, err := os.Lstat(src); err != nil {
				return fmt.Errorf("%w: cannot stat %s: %v", ErrFileStateChanged, src, err)
			}
		case catalog.StatusSynced:
			if !hasArchived {
				continue // the complete selection is already restored
			}
			// Already in the requested state, but it is still a required group
			// member. Refuse the restore if the catalog is stale and its bytes
			// have disappeared or changed; otherwise restoring the archived
			// members would leave a physical split behind.
			fc, err := m.folderForMove(files[i].FolderLabel)
			if err != nil {
				return err
			}
			if err := m.sourceUnchanged(
				filepath.Join(fc.SyncthingPath, files[i].RelativePath), &files[i]); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: paired file %d is %s", ErrFileStateChanged,
				files[i].ID, files[i].Status)
		}
	}
	completed := make([]catalog.FileRecord, 0, len(files))
	covered := make(map[int64]struct{}, len(files))
	for i := range files {
		covered[files[i].ID] = struct{}{}
	}
	for i := range files {
		if files[i].Status == catalog.StatusArchived {
			if _, err := m.restoreLocked(files[i].ID); err != nil {
				original := fmt.Errorf("restoring pair member %d: %w", files[i].ID, err)
				rollbackErrs := m.rollbackRestoredMembers(completed, covered)
				if verifyErr := m.verifyOriginalGroupState(files); verifyErr != nil {
					rollbackErrs = append(rollbackErrs, verifyErr)
				}
				if len(rollbackErrs) == 0 {
					return original
				}
				m.logSplitGroups(expanded, "restore group rollback could not rejoin every member")
				return errors.Join(append([]error{original}, rollbackErrs...)...)
			}
			completed = append(completed, files[i])
		}
	}
	return nil
}

func (m *Mover) rollbackRestoredMembers(completed []catalog.FileRecord,
	covered map[int64]struct{}) []error {

	var rollbackErrs []error
	for i := len(completed) - 1; i >= 0; i-- {
		original := &completed[i]
		if _, err := m.archiveLocked(original.ID, true, covered); err != nil {
			rollbackErrs = append(rollbackErrs,
				fmt.Errorf("rolling restored member %d back to archived: %w", original.ID, err))
		}
		if err := m.restorePinState(original); err != nil {
			rollbackErrs = append(rollbackErrs,
				fmt.Errorf("restoring original pin for member %d: %w", original.ID, err))
		}
	}
	return rollbackErrs
}

func (m *Mover) verifyOriginalGroupState(originals []catalog.FileRecord) error {
	ids := make([]int64, len(originals))
	for i := range originals {
		ids[i] = originals[i].ID
	}
	current, err := m.cat.FilesByIDs(ids)
	if err != nil {
		return fmt.Errorf("verifying pair rollback: %w", err)
	}
	byID := make(map[int64]catalog.FileRecord, len(current))
	for i := range current {
		byID[current[i].ID] = current[i]
	}
	for i := range originals {
		before := &originals[i]
		after, ok := byID[before.ID]
		if !ok {
			return fmt.Errorf("pair rollback lost catalog member %d", before.ID)
		}
		if after.Status != before.Status || after.Pinned != before.Pinned ||
			after.PinSource != before.PinSource || !sameOptionalInt64(after.PinnedAt, before.PinnedAt) {
			return fmt.Errorf("pair rollback left member %d as %s pin=%v/%q; want %s pin=%v/%q",
				before.ID, after.Status, after.Pinned, after.PinSource,
				before.Status, before.Pinned, before.PinSource)
		}
	}
	return nil
}

func sameOptionalInt64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (m *Mover) restoreLocked(id int64) (*catalog.FileRecord, error) {
	rec, err := m.cat.GetFileByID(id)
	if err != nil {
		return nil, err
	}
	if rec.Status == catalog.StatusSynced {
		return rec, nil // invariant 12: already here, no-op success
	}
	if rec.Status != catalog.StatusArchived {
		return nil, fmt.Errorf("%w: file %d is %s", ErrFileStateChanged, id, rec.Status)
	}

	fc, err := m.folderForMove(rec.FolderLabel)
	if err != nil {
		return nil, err
	}

	src := m.restorer.ColdPath(rec.FolderLabel, rec.RelativePath)
	dst := filepath.Join(fc.SyncthingPath, rec.RelativePath)

	// A file can legitimately reappear at the destination while the row still
	// says archived: that is what Syncthing does when a device that edited it
	// offline reconnects with a version vector that outranks coldstore's
	// deletion. Restoring over it would push September's bytes to every
	// device as an ordinary local edit, producing no conflict file anywhere.
	// So: adopt what is on the device, keep the archived copy beside it, and
	// report the conflict (design section 5.2).
	if _, err := os.Lstat(dst); err == nil {
		return nil, m.adoptDeviceCopy(rec, catalog.StatusArchived, src, dst)
	}

	ok, err := m.cat.ClaimFile(id, catalog.StatusArchived, catalog.StatusRestoring)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: file %d could not be claimed for restoring", ErrFileStateChanged, id)
	}

	if err := m.moveIn(src, dst, m.restorer.ArchiveRoot(rec.FolderLabel)); err != nil {
		// The destination was clear when we looked and was taken while the
		// copy ran -- the same reconnecting-device case as above, only later.
		// crossDeviceMove refused to overwrite it and left the cold copy
		// intact, so the resolution is identical.
		if errors.Is(err, ErrDestinationExists) {
			return nil, m.adoptDeviceCopy(rec, catalog.StatusRestoring, src, dst)
		}
		if _, rerr := m.cat.ClaimFile(id, catalog.StatusRestoring, catalog.StatusArchived); rerr != nil {
			m.logger.Error("cannot revert a failed restore to archived",
				"id", id, "path", rec.RelativePath, "error", rerr)
		}
		return nil, err
	}

	// MarkRestored, not UpdateFileStatus(synced): it commits the status and the
	// pin in ONE statement, so a crash can never leave a restored file unpinned
	// and immediately re-archivable by the next quota tick (D2).
	ok, err = m.cat.MarkRestored(id, time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("recording restore: %w", err)
	}
	if !ok {
		m.logger.Error("MarkRestored affected 0 rows after a completed move",
			"id", id, "path", rec.RelativePath, "dst", dst)
		m.cat.LogEvent("archive_unconfirmed", rec.FolderLabel, rec.RelativePath,
			"file moved back to the folder but the catalog row was not in 'restoring'")
		return nil, fmt.Errorf("restore of %d completed but could not be recorded", id)
	}

	m.cat.LogEvent("restore", rec.FolderLabel, rec.RelativePath,
		fmt.Sprintf("restored %d bytes", rec.SizeBytes))
	m.logger.Info("restored file",
		"folder", rec.FolderLabel, "path", rec.RelativePath, "size", rec.SizeBytes)

	return m.cat.GetFileByID(id)
}

// WithPairLock runs pairing-sensitive catalog work under the same lock as every
// move. Metadata uses it while publishing a candidate cluster; job creation
// uses it from expansion through restore preflight and item insertion. The
// callback must not call another Mover method.
func (m *Mover) WithPairLock(ctx context.Context, fn func(context.Context) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(ctx)
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[int64]struct{}, len(a))
	for _, id := range a {
		seen[id] = struct{}{}
	}
	if len(seen) != len(b) {
		return false
	}
	for _, id := range b {
		if _, ok := seen[id]; !ok {
			return false
		}
	}
	return true
}

func (m *Mover) warnPairSplit(rec *catalog.FileRecord, covered map[int64]struct{}) {
	if rec.PairKey == "" {
		return
	}
	ids, err := m.cat.ExpandPairGroups([]int64{rec.ID})
	if err != nil {
		m.logger.Error("cannot check pair before archive", "id", rec.ID, "error", err)
		return
	}
	files, err := m.cat.FilesByIDs(ids)
	if err != nil {
		m.logger.Error("cannot read pair before archive", "id", rec.ID, "error", err)
		return
	}
	for i := range files {
		if files[i].ID == rec.ID || files[i].Status != catalog.StatusSynced {
			continue
		}
		if _, ok := covered[files[i].ID]; ok {
			continue
		}
		details := fmt.Sprintf("archive of %s omitted synced pair member %s (%s)",
			rec.RelativePath, files[i].RelativePath, rec.PairKey)
		m.cat.LogEvent("pair_split", rec.FolderLabel, rec.RelativePath, details)
		m.logger.Warn("archive would split a pair", "folder", rec.FolderLabel,
			"path", rec.RelativePath, "sibling", files[i].RelativePath, "pair_key", rec.PairKey)
		return
	}
}

func (m *Mover) logSplitGroups(ids []int64, reason string) {
	files, err := m.cat.FilesByIDs(ids)
	if err != nil {
		return
	}
	byKey := make(map[string][]catalog.FileRecord)
	for _, file := range files {
		if file.PairKey != "" {
			byKey[file.FolderLabel+"\x00"+file.PairKey] = append(
				byKey[file.FolderLabel+"\x00"+file.PairKey], file)
		}
	}
	for _, group := range byKey {
		archived, synced := false, false
		for _, file := range group {
			archived = archived || file.Status == catalog.StatusArchived
			synced = synced || file.Status == catalog.StatusSynced
		}
		if archived && synced {
			m.cat.LogEvent("pair_split", group[0].FolderLabel, group[0].RelativePath,
				reason+": "+group[0].PairKey)
		}
	}
}

// adoptDeviceCopy is the restore-conflict resolution: a file has reappeared at
// the syncthing path under a row coldstore believed was archived.
//
// Restoring over it would push the archived bytes to every device as an
// ordinary local edit, producing no conflict file anywhere. So the on-device
// copy wins, the archived copy is kept beside it under .coldstore-superseded,
// and the caller gets ErrDestinationExists, which the API renders as a 409.
//
// from is whatever status the row is actually in when this runs: 'archived' on
// the pre-move check, 'restoring' when the collision happened mid-copy.
func (m *Mover) adoptDeviceCopy(rec *catalog.FileRecord, from catalog.FileStatus, src, dst string) error {
	if _, err := os.Lstat(src); err == nil {
		aside := asidePath(src)
		if rerr := os.Rename(src, aside); rerr != nil {
			m.logger.Error("cannot set the archived copy aside",
				"src", src, "aside", aside, "error", rerr)
		}
	}
	// ClaimFileOnDevice: the file IS on the device now, so last_seen has to
	// say so or a walk already in flight sweeps the row straight to missing.
	if _, cerr := m.cat.ClaimFileOnDevice(rec.ID, from, catalog.StatusSynced); cerr != nil {
		m.logger.Error("cannot adopt the on-device copy", "id", rec.ID, "error", cerr)
	}
	m.cat.LogEvent("restore_conflict", rec.FolderLabel, rec.RelativePath,
		"a copy already existed in the folder; coldstore adopted it and set the "+
			"archived copy aside as "+supersededSuffix)
	return fmt.Errorf("%w: %s", ErrDestinationExists, dst)
}

// ResolveConflict settles a section 4.1b conflict -- an archived row whose path
// has reappeared in the Syncthing folder -- in whichever direction the owner
// chose. It runs through the mover for the same reason every other transition
// does: nothing may change this row's status or place while a move of it is in
// flight.
//
//   - keep == "device": delete the cold-storage copy and adopt the file on
//     disk, taking its real size and timestamps.
//   - keep == "archive": rename the device file aside as
//     .coldstore-superseded and leave the row archived, so the next restore
//     puts the archived copy back.
//
// Nothing is ever deleted in the "archive" direction: the device copy is set
// aside, not removed. In the "device" direction the cold copy IS deleted,
// because that is exactly what the owner asked for and the bytes they are
// keeping are the ones already in the folder.
func (m *Mover) ResolveConflict(id int64, keep string) (*catalog.FileRecord, error) {
	if keep != ResolveKeepDevice && keep != ResolveKeepArchive {
		return nil, fmt.Errorf("%w: %q", ErrInvalidResolution, keep)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := m.cat.GetFileByID(id)
	if err != nil {
		return nil, err
	}
	if rec.Status != catalog.StatusArchived {
		return nil, fmt.Errorf("%w: file %d is %s, not archived", ErrFileStateChanged, id, rec.Status)
	}

	fc, err := m.folderForMove(rec.FolderLabel)
	if err != nil {
		return nil, err
	}

	devicePath := filepath.Join(fc.SyncthingPath, rec.RelativePath)
	coldPath := m.restorer.ColdPath(rec.FolderLabel, rec.RelativePath)

	info, err := os.Lstat(devicePath)
	if err != nil {
		return nil, fmt.Errorf("%w: nothing at %s to resolve against", ErrFileStateChanged, devicePath)
	}

	if keep == ResolveKeepArchive {
		aside := asidePath(devicePath)
		if err := os.Rename(devicePath, aside); err != nil {
			return nil, fmt.Errorf("setting the device copy aside: %w", err)
		}
		// conflict_resolved, not conflict: the Conflicts filter (FileQuery.
		// Conflicted) reads this log to decide what is still outstanding, and
		// a resolution logged under the same type as the collision would keep
		// the row in the owner's Conflicts tab permanently.
		m.cat.LogEvent("conflict_resolved", rec.FolderLabel, rec.RelativePath,
			"resolved in favour of the archived copy; the device copy was kept as "+
				filepath.Base(aside))
		return m.cat.GetFileByID(id)
	}

	// keep == device. The cold copy is set aside FIRST and unlinked only after
	// the row has stopped pointing at it, so a failure in between leaves bytes
	// rather than losing them.
	//
	// A bare unlink here is not merely untidy, it is unrecoverable. If the
	// adopt below fails -- a database error, or 0 rows because the row left
	// 'archived' while this ran -- the row still says archived and still names
	// the archive path, and the bytes at that path are already gone: a file
	// that can never be restored, that the scanner re-reports as the same
	// conflict on every single walk, and that no endpoint can repair. Renaming
	// makes that window undoable, and it is what the paragraph above always
	// claimed the code did.
	aside := ""
	if _, err := os.Lstat(coldPath); err == nil {
		aside = asidePath(coldPath)
		if err := os.Rename(coldPath, aside); err != nil {
			return nil, fmt.Errorf("setting the cold copy aside: %w", err)
		}
	}
	putColdBack := func() {
		if aside == "" {
			return
		}
		if err := os.Rename(aside, coldPath); err != nil {
			m.logger.Error("cannot put the cold copy back after an abandoned resolution",
				"aside", aside, "cold", coldPath, "error", err)
		}
	}

	atime := info.ModTime()
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		atime = time.Unix(st.Atim.Sec, st.Atim.Nsec)
	}
	adopted, err := m.adoptRow(id, info.Size(), info.ModTime(), atime)
	if err != nil {
		putColdBack()
		return nil, fmt.Errorf("adopting the device copy: %w", err)
	}
	if !adopted {
		putColdBack()
		return nil, fmt.Errorf("%w: file %d left 'archived' while it was being resolved",
			ErrFileStateChanged, id)
	}

	// The row is synced and no longer references the archive path, so now --
	// and only now -- the copy the owner chose to discard goes. Failing here
	// costs one stray file in cold storage, never a broken row.
	//
	// Nothing reports that stray yet. Design section 5.11's stray-archive sweep
	// is NOT implemented: the scanner skips the suffix, no tick walks cold
	// storage looking for unreferenced files, and the Warn below is the only
	// trace the owner ever gets. Do not write a comment here promising a sweep
	// until one exists.
	if aside != "" {
		if err := os.Remove(aside); err != nil && !os.IsNotExist(err) {
			m.logger.Warn("cannot delete the cold copy after resolving in the device's favour",
				"path", aside, "error", err)
		}
	}
	m.cat.LogEvent("conflict_resolved", rec.FolderLabel, rec.RelativePath,
		"resolved in favour of the device copy; the cold-storage copy was deleted")
	return m.cat.GetFileByID(id)
}

// folderForMove resolves a folder and refuses the move unless its root is
// really there. An orphaned folder is refused too: coldstore stops moving
// files for a folder the owner has removed from coldstore.toml, while keeping
// what it already knows browsable.
func (m *Mover) folderForMove(label string) (*catalog.FolderConfigRecord, error) {
	fc, err := m.cat.FolderConfigByLabel(label)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrFolderUnavailable, label)
	}
	if !fc.PresentInTOML {
		return nil, fmt.Errorf("%w: %s is no longer declared in coldstore.toml",
			ErrFolderUnavailable, label)
	}
	if err := FolderReady(fc.SyncthingPath); err != nil {
		return nil, err
	}
	return fc, nil
}

// sourceUnchanged refuses a move whose source no longer matches the catalog
// row: a different size, or an mtime that has moved FORWARD past the one the
// catalog recorded.
//
// Forward, not merely different, and whole seconds, not nanoseconds. The row
// is written after the file is read, so a catalogued timestamp a fraction of a
// second ahead of the disk is ordinary and says nothing; only an mtime that
// has advanced past it means somebody has touched the file since. A file
// Syncthing is actively writing fails both halves of this test.
func (m *Mover) sourceUnchanged(src string, rec *catalog.FileRecord) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("%w: cannot stat %s: %v", ErrFileStateChanged, src, err)
	}
	if info.Size() != rec.SizeBytes || info.ModTime().Unix() > rec.FileMtime.Unix() {
		// Deduped, unlike the mid-move report in archiveLocked: that one fires
		// once per move that actually started, while this one fires once per
		// tick for as long as the disagreement lasts. See
		// sourceChangedDedupeMinutes.
		m.cat.LogEventDeduped("source_changed", rec.FolderLabel, rec.RelativePath,
			fmt.Sprintf("on disk %d bytes at %s, catalogued %d bytes at %s",
				info.Size(), info.ModTime().UTC().Format(time.RFC3339),
				rec.SizeBytes, rec.FileMtime.UTC().Format(time.RFC3339)),
			sourceChangedDedupeMinutes)
		return fmt.Errorf("%w: %s", ErrSourceChanged, src)
	}
	return nil
}
