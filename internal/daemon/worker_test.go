package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

// workerEnv is the whole job stack over one temp folder: a real catalog, a real
// mover, a real scanner, and the worker that drives them.
type workerEnv struct {
	syncDir string
	coldDir string
	cfg     *config.Config
	cat     *catalog.Catalog
	mover   *Mover
	scanner *Scanner
	worker  *Worker
}

func newWorkerEnv(t *testing.T) *workerEnv {
	t.Helper()

	syncDir, coldDir, cfg, cat := setupTestEnv(t)
	cfg.Folders[0].BudgetBytes = 1 << 40 // enforcement is not what these tests are about
	mover := newTestMover(t, cfg, cat)
	scanner := NewScanner(cfg, cat, testLogger())

	return &workerEnv{
		syncDir: syncDir,
		coldDir: coldDir,
		cfg:     cfg,
		cat:     cat,
		mover:   mover,
		scanner: scanner,
		worker:  NewWorker(cat, mover, scanner, testLogger()),
	}
}

// seedMany writes n synced files and returns their ids.
func (env *workerEnv) seedMany(t *testing.T, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		rec := seedSyncedFile(t, env.cat, env.syncDir, fmt.Sprintf("photo%02d.jpg", i), 100)
		ids = append(ids, rec.ID)
	}
	return ids
}

func (env *workerEnv) job(t *testing.T, id int64) *catalog.Job {
	t.Helper()
	job, _, err := env.cat.GetJob(id, 100)
	if err != nil {
		t.Fatalf("reading job %d: %v", id, err)
	}
	return job
}

func (env *workerEnv) status(t *testing.T, id int64) catalog.FileStatus {
	t.Helper()
	rec, err := env.cat.GetFileByID(id)
	if err != nil {
		t.Fatalf("reading file %d: %v", id, err)
	}
	return rec.Status
}

// assertTerminalCounters is the invariant four catalog methods must preserve
// between them: a terminal job has accounted for every one of its items.
func assertTerminalCounters(t *testing.T, job *catalog.Job) {
	t.Helper()
	if job.State != catalog.JobStateDone && job.State != catalog.JobStateFailed {
		t.Fatalf("job %d state = %q, want a terminal state", job.ID, job.State)
	}
	if job.Done != job.Total {
		t.Errorf("job %d done = %d, want total = %d", job.ID, job.Done, job.Total)
	}
	if job.FailedCount > job.Done {
		t.Errorf("job %d failed = %d exceeds done = %d", job.ID, job.FailedCount, job.Done)
	}
	if job.FinishedAt == nil {
		t.Errorf("job %d has no finished_at", job.ID)
	}
}

func TestJobWorkerShouldArchiveEveryFileAndFinishDoneWhenAllItemsSucceed(t *testing.T) {
	env := newWorkerEnv(t)
	ids := env.seedMany(t, 5)

	jobID, err := env.cat.CreateJob(catalog.JobActionArchive, "", ids)
	if err != nil {
		t.Fatal(err)
	}

	env.worker.RunOnce(context.Background())

	job := env.job(t, jobID)
	assertTerminalCounters(t, job)
	if job.State != catalog.JobStateDone {
		t.Errorf("state = %q, want done", job.State)
	}
	if job.FailedCount != 0 {
		t.Errorf("failed = %d, want 0", job.FailedCount)
	}
	if job.StartedAt == nil {
		t.Error("started_at was never stamped")
	}
	for _, id := range ids {
		if got := env.status(t, id); got != catalog.StatusArchived {
			t.Errorf("file %d status = %q, want archived", id, got)
		}
	}
}

func TestJobWorkerShouldPinEveryFileWhenABulkRestoreCompletes(t *testing.T) {
	env := newWorkerEnv(t)
	ids := env.seedMany(t, 6)
	for _, id := range ids {
		if _, err := env.mover.ArchiveByID(id); err != nil {
			t.Fatal(err)
		}
	}

	jobID, err := env.cat.CreateJob(catalog.JobActionRestore, "", ids)
	if err != nil {
		t.Fatal(err)
	}
	env.worker.RunOnce(context.Background())

	job := env.job(t, jobID)
	assertTerminalCounters(t, job)
	if job.State != catalog.JobStateDone {
		t.Fatalf("state = %q, want done", job.State)
	}
	for _, id := range ids {
		rec, err := env.cat.GetFileByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status != catalog.StatusSynced {
			t.Errorf("file %d status = %q, want synced", id, rec.Status)
		}
		// Mechanism 1 of design section 5.8. Without the pin, the next quota
		// tick takes back everything the owner just asked for.
		if !rec.Pinned || rec.PinSource != catalog.PinSourceRestore {
			t.Errorf("file %d pinned = %v source = %q, want true/restore",
				id, rec.Pinned, rec.PinSource)
		}
	}
}

func TestJobWorkerShouldDriveTrashRestoreAndPermanentDelete(t *testing.T) {
	env := newWorkerEnv(t)
	file := seedSyncedFile(t, env.cat, env.syncDir, "photo.jpg", 100)

	trashJob, err := env.cat.CreateJob(catalog.JobActionTrash, "", []int64{file.ID})
	if err != nil {
		t.Fatal(err)
	}
	env.worker.RunOnce(context.Background())
	assertTerminalCounters(t, env.job(t, trashJob))
	if got := env.status(t, file.ID); got != catalog.StatusTrashed {
		t.Fatalf("status after trash job = %q, want trashed", got)
	}

	restoreJob, err := env.cat.CreateJob(catalog.JobActionUntrash, "", []int64{file.ID})
	if err != nil {
		t.Fatal(err)
	}
	env.worker.RunOnce(context.Background())
	assertTerminalCounters(t, env.job(t, restoreJob))
	if got := env.status(t, file.ID); got != catalog.StatusSynced {
		t.Fatalf("status after untrash job = %q, want synced", got)
	}

	trashAgain, err := env.cat.CreateJob(catalog.JobActionTrash, "", []int64{file.ID})
	if err != nil {
		t.Fatal(err)
	}
	env.worker.RunOnce(context.Background())
	assertTerminalCounters(t, env.job(t, trashAgain))

	deleteJob, err := env.cat.CreateJob(catalog.JobActionDelete, "", []int64{file.ID})
	if err != nil {
		t.Fatal(err)
	}
	env.worker.RunOnce(context.Background())
	assertTerminalCounters(t, env.job(t, deleteJob))
	if _, err := env.cat.GetFileByID(file.ID); !errors.Is(err, catalog.ErrNoSuchFile) {
		t.Fatalf("catalog row after delete job: %v, want ErrNoSuchFile", err)
	}
}

func TestJobWorkerShouldRecordTheFailureAndEndFailedWhenOneIdDoesNotExist(t *testing.T) {
	env := newWorkerEnv(t)
	ids := env.seedMany(t, 5)
	const missingID = 99999
	selection := append([]int64{}, ids...)
	selection = append(selection, missingID)

	jobID, err := env.cat.CreateJob(catalog.JobActionArchive, "", selection)
	if err != nil {
		t.Fatal(err)
	}
	env.worker.RunOnce(context.Background())

	job, failures, err := env.cat.GetJob(jobID, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertTerminalCounters(t, job)
	if job.State != catalog.JobStateFailed {
		t.Errorf("state = %q, want failed", job.State)
	}
	if job.Total != 6 || job.Done != 6 || job.FailedCount != 1 {
		t.Errorf("counters = total %d done %d failed %d, want 6/6/1",
			job.Total, job.Done, job.FailedCount)
	}
	if len(failures) != 1 || failures[0].ID != missingID {
		t.Fatalf("failures = %+v, want exactly the missing id", failures)
	}
	if failures[0].Error != "file not found" {
		t.Errorf("failure text = %q, want %q", failures[0].Error, "file not found")
	}
	// "failed" means partial success, not "nothing happened".
	for _, id := range ids {
		if got := env.status(t, id); got != catalog.StatusArchived {
			t.Errorf("file %d status = %q, want archived", id, got)
		}
	}
}

// Two conditions mean every remaining item would fail identically, so the
// worker must stop trying rather than writing hundreds of identical rows -- and
// the counters must still add up when it does.
func TestJobWorkerShouldFailEveryRemainingItemWhenColdStorageFillsMidJob(t *testing.T) {
	env := newWorkerEnv(t)
	ids := env.seedMany(t, 20)

	var probes int
	env.mover.SetSpaceCheck(func(string) (float64, error) {
		probes++
		if probes > 12 {
			return 1.0, nil // below the 5% floor
		}
		return 90.0, nil
	})

	jobID, err := env.cat.CreateJob(catalog.JobActionArchive, "", ids)
	if err != nil {
		t.Fatal(err)
	}
	env.worker.RunOnce(context.Background())

	job, _, err := env.cat.GetJob(jobID, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertTerminalCounters(t, job)
	if job.State != catalog.JobStateFailed {
		t.Errorf("state = %q, want failed", job.State)
	}
	if job.Done != 20 {
		t.Errorf("done = %d, want 20: every item must be accounted for", job.Done)
	}
	if job.FailedCount != 8 {
		t.Errorf("failed = %d, want 8 (item 13 onwards)", job.FailedCount)
	}
	// The probe must have stopped: 13 attempts, not 20.
	if probes != 13 {
		t.Errorf("space probes = %d, want 13: the worker kept trying after the pool filled", probes)
	}
}

// The FailJob edge. Without it a job the worker cannot read items for stays
// "running" forever and NextPendingJob hands it straight back, one error line
// per iteration, with every later job starved behind it.
func TestJobWorkerShouldAbandonTheJobWhenTheQueueCannotBeRead(t *testing.T) {
	env := newWorkerEnv(t)
	ids := env.seedMany(t, 4)

	jobID, err := env.cat.CreateJob(catalog.JobActionArchive, "", ids)
	if err != nil {
		t.Fatal(err)
	}

	claims := 0
	env.worker.claimItem = func(id int64) (int64, error) {
		claims++
		if claims > 2 {
			return 0, errors.New("database disk image is malformed")
		}
		return env.cat.ClaimJobItem(id)
	}

	env.worker.RunOnce(context.Background())

	job := env.job(t, jobID)
	assertTerminalCounters(t, job)
	if job.State != catalog.JobStateFailed {
		t.Errorf("state = %q, want failed", job.State)
	}
	if !strings.Contains(job.Error, "could not read the job queue") {
		t.Errorf("job error = %q, want the abandonment reason", job.Error)
	}
	// The brake: three consecutive failures and the worker stops, rather than
	// spinning on the same unreadable job.
	if claims > 6 {
		t.Errorf("claim attempts = %d: the worker spun instead of backing off", claims)
	}
}

func TestJobWorkerShouldStopBeforeTheNextItemWhenTheContextIsCancelled(t *testing.T) {
	env := newWorkerEnv(t)
	ids := env.seedMany(t, 5)

	jobID, err := env.cat.CreateJob(catalog.JobActionArchive, "", ids)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env.worker.RunOnce(ctx)

	job := env.job(t, jobID)
	if job.State == catalog.JobStateDone || job.State == catalog.JobStateFailed {
		t.Errorf("state = %q: a cancelled worker must leave the job for recovery", job.State)
	}
	for _, id := range ids {
		if got := env.status(t, id); got != catalog.StatusSynced {
			t.Errorf("file %d status = %q, want synced: nothing should have moved", id, got)
		}
	}
}

// The kill window is between the move committing and CompleteJobItem
// committing: the file is terminal, the item is still "running", and the job
// says fewer done than it really is. This drives the mover directly for item 4
// and skips the bookkeeping, which is exactly the state a SIGKILL leaves.
func TestJobShouldResumeWithoutMovingAnythingTwiceWhenTheDaemonWasKilledMidItem(t *testing.T) {
	env := newWorkerEnv(t)
	ids := env.seedMany(t, 10)

	jobID, err := env.cat.CreateJob(catalog.JobActionArchive, "", ids)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.cat.MarkJobRunning(jobID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	// Items 1-3 complete normally.
	for i := 0; i < 3; i++ {
		fileID, err := env.cat.ClaimJobItem(jobID)
		if err != nil || fileID == 0 {
			t.Fatalf("claiming item %d: %v (id %d)", i, err, fileID)
		}
		if _, err := env.mover.ArchiveByID(fileID); err != nil {
			t.Fatal(err)
		}
		if err := env.cat.CompleteJobItem(jobID, fileID, ""); err != nil {
			t.Fatal(err)
		}
	}

	// Item 4: the move commits, the bookkeeping does not. Kill.
	killedID, err := env.cat.ClaimJobItem(jobID)
	if err != nil || killedID == 0 {
		t.Fatalf("claiming the doomed item: %v (id %d)", err, killedID)
	}
	if _, err := env.mover.ArchiveByID(killedID); err != nil {
		t.Fatal(err)
	}
	killedPath := filepath.Join(env.coldDir, "archive", "TestFolder",
		mustRelPath(t, env.cat, killedID))
	killedInfo, err := os.Stat(killedPath)
	if err != nil {
		t.Fatalf("the archived copy of the killed item is missing: %v", err)
	}

	// Restart: file-level recovery first, then the job queue, then a fresh
	// worker over the same catalog.
	if err := RecoverStartupState(env.cfg, env.cat, testLogger()); err != nil {
		t.Fatal(err)
	}
	items, jobs, err := env.cat.ResumeInterruptedJobs()
	if err != nil {
		t.Fatal(err)
	}
	if items != 1 || jobs != 1 {
		t.Fatalf("resumed %d items and %d jobs, want 1 and 1", items, jobs)
	}

	fresh := NewWorker(env.cat, newTestMover(t, env.cfg, env.cat), env.scanner, testLogger())
	fresh.RunOnce(context.Background())

	job := env.job(t, jobID)
	assertTerminalCounters(t, job)
	if job.State != catalog.JobStateDone {
		t.Errorf("state = %q, want done: a resumed item that already ran is not a failure", job.State)
	}
	if job.Done != 10 || job.FailedCount != 0 {
		t.Errorf("counters = done %d failed %d, want 10 and 0", job.Done, job.FailedCount)
	}
	for _, id := range ids {
		if got := env.status(t, id); got != catalog.StatusArchived {
			t.Errorf("file %d status = %q, want archived", id, got)
		}
	}

	// Nothing moved twice: the killed item's archived copy is the same file,
	// untouched, and no second copy was written beside it.
	after, err := os.Stat(killedPath)
	if err != nil {
		t.Fatalf("the archived copy vanished during the resume: %v", err)
	}
	if !after.ModTime().Equal(killedInfo.ModTime()) || after.Size() != killedInfo.Size() {
		t.Error("the resumed item rewrote a file that had already been archived")
	}
	if _, err := os.Stat(killedPath + supersededSuffix); err == nil {
		t.Error("the resume produced a superseded copy; it moved the file a second time")
	}
}

func mustRelPath(t *testing.T, cat *catalog.Catalog, id int64) string {
	t.Helper()
	rec, err := cat.GetFileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return rec.RelativePath
}

func TestScanJobShouldCatalogueTheFolderWhenItRuns(t *testing.T) {
	env := newWorkerEnv(t)
	if err := os.WriteFile(filepath.Join(env.syncDir, "new.jpg"), make([]byte, 10), 0o644); err != nil {
		t.Fatal(err)
	}

	jobID, err := env.cat.CreateJob(catalog.JobActionScan, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	env.worker.RunOnce(context.Background())

	job := env.job(t, jobID)
	assertTerminalCounters(t, job)
	if job.State != catalog.JobStateDone {
		t.Fatalf("state = %q, want done (error %q)", job.State, job.Error)
	}
	if job.Total != 1 || job.Done != 1 {
		t.Errorf("counters = total %d done %d, want 1 and 1", job.Total, job.Done)
	}
	if _, err := env.cat.GetFile("TestFolder", "new.jpg"); err != nil {
		t.Errorf("the scan job did not catalogue the new file: %v", err)
	}
}

func TestScanJobShouldEndFailedWhenTheFolderDoesNotExist(t *testing.T) {
	env := newWorkerEnv(t)

	jobID, err := env.cat.CreateJob(catalog.JobActionScan, "NoSuchFolder", nil)
	if err != nil {
		t.Fatal(err)
	}
	env.worker.RunOnce(context.Background())

	job := env.job(t, jobID)
	assertTerminalCounters(t, job)
	if job.State != catalog.JobStateFailed {
		t.Errorf("state = %q, want failed", job.State)
	}
	if !strings.Contains(job.Error, "NoSuchFolder") {
		t.Errorf("job error = %q, want it to name the folder", job.Error)
	}
}

func TestJobWorkerShouldDrainEveryQueuedJobWhenSeveralArePending(t *testing.T) {
	env := newWorkerEnv(t)
	ids := env.seedMany(t, 4)

	first, err := env.cat.CreateJob(catalog.JobActionArchive, "", ids[:2])
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.cat.CreateJob(catalog.JobActionArchive, "", ids[2:])
	if err != nil {
		t.Fatal(err)
	}

	env.worker.RunOnce(context.Background())

	for _, id := range []int64{first, second} {
		job := env.job(t, id)
		assertTerminalCounters(t, job)
		if job.State != catalog.JobStateDone {
			t.Errorf("job %d state = %q, want done", id, job.State)
		}
	}
}

// Design section 5.8 mechanism 2, proved rather than asserted: the worker and
// the quota tick share ONE Mover, so their moves cannot overlap in time. With
// two Movers over one catalog this test fails -- the enforcement pass walks
// straight past the held lock while the restore is mid-copy.
func TestQuotaEnforcementShouldWaitWhenTheJobWorkerIsMidRestore(t *testing.T) {
	env := newWorkerEnv(t)
	rec := seedSyncedFile(t, env.cat, env.syncDir, "wanted.jpg", 500)
	other := seedSyncedFile(t, env.cat, env.syncDir, "spare.jpg", 500)
	if _, err := env.mover.ArchiveByID(rec.ID); err != nil {
		t.Fatal(err)
	}
	ageFiles(t, env.cat, env.syncDir, "spare.jpg")

	// A budget that only fits one of the two files, so a tick that ran
	// concurrently would have something to take.
	budget := int64(500)
	if _, err := env.cat.UpdateFolderPolicy("TestFolder", &budget, nil, nil); err != nil {
		t.Fatal(err)
	}

	inMove := make(chan struct{})
	release := make(chan struct{})
	realIn := env.mover.moveIn
	env.mover.SetMoveFuncs(nil, func(src, dst, archiveRoot string) error {
		close(inMove)
		<-release
		return realIn(src, dst, archiveRoot)
	})

	jobID, err := env.cat.CreateJob(catalog.JobActionRestore, "", []int64{rec.ID})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		env.worker.RunOnce(context.Background())
	}()

	<-inMove // the restore is holding the mover mutex

	enforced := make(chan struct{})
	d := New(env.cfg, env.cat, env.mover, testLogger())
	go func() {
		d.EnforceQuotas()
		close(enforced)
	}()

	select {
	case <-enforced:
		t.Fatal("quota enforcement archived something while a restore was mid-move; " +
			"the two actors are not sharing the mover mutex")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	wg.Wait()
	<-enforced

	// Mechanism 1 and 3 together: the restored file is pinned, so the tick that
	// was waiting takes the other file instead of undoing the owner's restore.
	restored, err := env.cat.GetFileByID(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != catalog.StatusSynced || !restored.Pinned {
		t.Errorf("restored file = %q pinned %v, want synced and pinned",
			restored.Status, restored.Pinned)
	}
	if got := env.status(t, other.ID); got != catalog.StatusArchived {
		t.Errorf("the spare file status = %q, want archived: the folder still had to fit", got)
	}

	job := env.job(t, jobID)
	if job.State != catalog.JobStateDone {
		t.Errorf("job state = %q, want done", job.State)
	}
}

func TestWakeShouldNotBlockWhenNobodyIsListening(t *testing.T) {
	env := newWorkerEnv(t)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			env.worker.Wake()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wake blocked; the wake channel must be a signal, not a queue")
	}
}

// The scan job takes the same scanner mutex the daemon's catalog tick takes.
// Two overlapping walks of one tree interleave their generation stamps and the
// loser's final sweep marks the winner's fresh rows missing, so the job is told
// no rather than being allowed to run.
func TestScanJobShouldReportTheConflictWhenAnotherScanHoldsTheLock(t *testing.T) {
	env := newWorkerEnv(t)
	if err := os.WriteFile(filepath.Join(env.syncDir, "a.jpg"), make([]byte, 10), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := env.scanner.ScanAll(); err != nil {
		t.Fatal(err)
	}

	jobID, err := env.cat.CreateJob(catalog.JobActionScan, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	env.scanner.mu.Lock()
	env.worker.RunOnce(context.Background())
	env.scanner.mu.Unlock()

	job := env.job(t, jobID)
	assertTerminalCounters(t, job)
	if job.State != catalog.JobStateFailed {
		t.Errorf("state = %q, want failed", job.State)
	}
	if !strings.Contains(job.Error, "scan is already in progress") {
		t.Errorf("job error = %q, want the in-progress reason", job.Error)
	}
	if got := mustStatus(t, env.cat, "a.jpg"); got != catalog.StatusSynced {
		t.Errorf("status = %q, want synced: the refused scan swept the catalog", got)
	}
}

func mustStatus(t *testing.T, cat *catalog.Catalog, rel string) catalog.FileStatus {
	t.Helper()
	rec, err := cat.GetFile("TestFolder", rel)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Status
}

// The three-strikes thumbnail cap is permanent unless something clears it: a
// file whose thumbnail failed three times drops out of the queue for good, so
// a repaired file, or an ffmpeg installed after the fact, could never produce
// a thumbnail again for the life of the database.
//
// A scan JOB is the owner pressing Rescan; the periodic catalog tick does not
// come through here, which is what stops the cap being reset every fifteen
// minutes and defeating its purpose.
func TestScanJobShouldClearTheThumbnailFailureCapWhenTheOwnerAsksForARescan(t *testing.T) {
	env := newWorkerEnv(t)
	rec := seedSyncedFile(t, env.cat, env.syncDir, "photo.jpg", 100)

	for i := 0; i < maxThumbnailAttemptsForTest; i++ {
		if err := env.cat.RecordThumbnailFailure(rec.ID, "image: unknown format", time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}
	blocked, err := env.cat.FilesNeedingThumbnails([]string{".jpg"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 0 {
		t.Fatalf("the cap is not in force: %d files still queued", len(blocked))
	}

	jobID, err := env.cat.CreateJob(catalog.JobActionScan, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	env.worker.RunOnce(context.Background())

	if job := env.job(t, jobID); job.State != catalog.JobStateDone {
		t.Fatalf("scan job state = %q, want done", job.State)
	}

	requeued, err := env.cat.FilesNeedingThumbnails([]string{".jpg"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 1 {
		t.Errorf("%d files queued after a manual rescan, want 1", len(requeued))
	}
	after, _ := env.cat.GetFileByID(rec.ID)
	if after.ThumbAttempts != 0 || after.ThumbError != "" {
		t.Errorf("thumb state = %d attempts, error %q; want cleared",
			after.ThumbAttempts, after.ThumbError)
	}
}

// maxThumbnailAttemptsForTest mirrors the catalog's unexported cap.
const maxThumbnailAttemptsForTest = 3
