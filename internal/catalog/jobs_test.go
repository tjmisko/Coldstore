package catalog

import (
	"errors"
	"testing"
	"time"
)

func TestCreateJobShouldSetTotalFromTheItemsWhenTheSelectionContainsDuplicates(t *testing.T) {
	cat := setupTestCatalog(t)

	jobID, err := cat.CreateJob(JobActionRestore, "", []int64{7, 8, 7, 9, 8})
	if err != nil {
		t.Fatal(err)
	}

	job, _, err := cat.GetJob(jobID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if job.Total != 3 {
		t.Errorf("Total = %d, want 3: duplicate ids collapse on the composite primary key", job.Total)
	}
	if job.State != JobStatePending {
		t.Errorf("State = %q, want pending", job.State)
	}
	if job.StartedAt != nil || job.FinishedAt != nil {
		t.Error("a new job has neither a start nor a finish timestamp")
	}
}

func TestCreateJobShouldUseASingleVirtualItemWhenTheActionIsScan(t *testing.T) {
	cat := setupTestCatalog(t)

	jobID, err := cat.CreateJob(JobActionScan, "Photos", nil)
	if err != nil {
		t.Fatal(err)
	}

	job, _, err := cat.GetJob(jobID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if job.Total != 1 {
		t.Errorf("Total = %d, want 1", job.Total)
	}
	if job.FolderLabel != "Photos" {
		t.Errorf("FolderLabel = %q, want Photos", job.FolderLabel)
	}

	var items int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM job_items WHERE job_id = ?`, jobID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if items != 0 {
		t.Errorf("job_items = %d, want 0: a scan job has no per-file items", items)
	}
}

func TestCreateRenameJobShouldPersistItsPayloadAsOneVirtualItem(t *testing.T) {
	cat := setupTestCatalog(t)
	jobID, err := cat.CreateRenameJob("Photos", "2024/01", "2024/Iceland")
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := cat.GetJob(jobID, 1)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := DecodeRenamePayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if job.Action != JobActionRename || job.Total != 1 || payload.From != "2024/01" ||
		payload.To != "2024/Iceland" {
		t.Fatalf("rename job = %+v payload %+v", job, payload)
	}
	var items int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM job_items WHERE job_id = ?`, jobID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if items != 0 {
		t.Fatalf("rename job_items = %d, want 0", items)
	}
}

func TestCompleteVirtualItemShouldBeIdempotentAcrossCrashReplay(t *testing.T) {
	cat := setupTestCatalog(t)
	jobID, err := cat.CreateRenameJob("Photos", "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.CompleteJobItemVirtual(jobID, ""); err != nil {
		t.Fatal(err)
	}
	// Simulates a kill after the result write and before FinishJob. Startup
	// recovery requeues the still-running row and the operation replays.
	if err := cat.CompleteJobItemVirtual(jobID, ""); err != nil {
		t.Fatal(err)
	}
	job, _, err := cat.GetJob(jobID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if job.Done != 1 || job.Total != 1 || job.FailedCount != 0 {
		t.Fatalf("virtual counters = done %d total %d failed %d",
			job.Done, job.Total, job.FailedCount)
	}
}

func TestCreateJobShouldRejectAnUnknownAction(t *testing.T) {
	cat := setupTestCatalog(t)

	if _, err := cat.CreateJob("bogus", "", []int64{1}); err == nil {
		t.Fatal("an unknown job action must be rejected")
	}
	jobs, err := cat.ListJobs(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Errorf("ListJobs returned %d rows, want 0", len(jobs))
	}
}

func TestClaimJobItemShouldHandOutEachItemExactlyOnceWhenTheJobIsDrained(t *testing.T) {
	cat := setupTestCatalog(t)

	jobID, err := cat.CreateJob(JobActionArchive, "", []int64{30, 10, 20})
	if err != nil {
		t.Fatal(err)
	}

	var claimed []int64
	for {
		fileID, err := cat.ClaimJobItem(jobID)
		if err != nil {
			t.Fatal(err)
		}
		if fileID == 0 {
			break
		}
		claimed = append(claimed, fileID)
	}

	want := []int64{10, 20, 30}
	if len(claimed) != len(want) {
		t.Fatalf("claimed %v, want %v", claimed, want)
	}
	for i := range want {
		if claimed[i] != want[i] {
			t.Errorf("claimed[%d] = %d, want %d", i, claimed[i], want[i])
		}
	}
}

func TestClaimJobItemShouldReportNoWorkWhenEveryItemIsAlreadyClaimed(t *testing.T) {
	cat := setupTestCatalog(t)

	jobID, err := cat.CreateJob(JobActionArchive, "", []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ClaimJobItem(jobID); err != nil {
		t.Fatal(err)
	}

	fileID, err := cat.ClaimJobItem(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if fileID != 0 {
		t.Errorf("fileID = %d, want 0", fileID)
	}
}

func TestNextPendingJobShouldReturnTheOldestJobWhenSeveralAreQueued(t *testing.T) {
	cat := setupTestCatalog(t)

	first, err := cat.CreateJob(JobActionArchive, "", []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.CreateJob(JobActionRestore, "", []int64{2}); err != nil {
		t.Fatal(err)
	}

	job, err := cat.NextPendingJob()
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || job.ID != first {
		t.Fatalf("NextPendingJob = %+v, want job %d", job, first)
	}

	// Terminal jobs drop out of the queue.
	if err := cat.CompleteJobItem(first, 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := cat.FinishJob(first, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	job, err = cat.NextPendingJob()
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || job.ID == first {
		t.Fatalf("NextPendingJob = %+v, want the second job", job)
	}
}

func TestNextPendingJobShouldReturnNothingWhenTheQueueIsEmpty(t *testing.T) {
	cat := setupTestCatalog(t)

	job, err := cat.NextPendingJob()
	if err != nil {
		t.Fatal(err)
	}
	if job != nil {
		t.Errorf("NextPendingJob = %+v, want nil", job)
	}
}

func TestCompleteJobItemShouldCountAnItemOnceWhenItIsCompletedTwice(t *testing.T) {
	cat := setupTestCatalog(t)

	jobID, err := cat.CreateJob(JobActionArchive, "", []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.CompleteJobItem(jobID, 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := cat.CompleteJobItem(jobID, 1, "late failure"); err != nil {
		t.Fatal(err)
	}

	job, _, err := cat.GetJob(jobID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if job.Done != 1 {
		t.Errorf("Done = %d, want 1: a terminal item is never counted twice", job.Done)
	}
	if job.FailedCount != 0 {
		t.Errorf("FailedCount = %d, want 0", job.FailedCount)
	}
}

func TestGetJobShouldReportTheFailingIdsWhenSomeItemsFailed(t *testing.T) {
	cat := setupTestCatalog(t)

	jobID, err := cat.CreateJob(JobActionRestore, "", []int64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.CompleteJobItem(jobID, 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := cat.CompleteJobItem(jobID, 2, "file status is synced"); err != nil {
		t.Fatal(err)
	}
	if err := cat.CompleteJobItem(jobID, 3, ""); err != nil {
		t.Fatal(err)
	}
	if err := cat.FinishJob(jobID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	job, failures, err := cat.GetJob(jobID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != JobStateFailed {
		t.Errorf("State = %q, want failed: partial success with one failure is failed", job.State)
	}
	if job.Done != 3 || job.FailedCount != 1 {
		t.Errorf("done/failed = %d/%d, want 3/1", job.Done, job.FailedCount)
	}
	if len(failures) != 1 || failures[0].ID != 2 {
		t.Fatalf("failures = %+v, want the one bad id", failures)
	}
	if failures[0].Error != "file status is synced" {
		t.Errorf("failure error = %q", failures[0].Error)
	}
	if job.FinishedAt == nil {
		t.Error("a terminal job must stamp finished_at")
	}
}

func TestGetJobShouldReturnAnEmptyArrayWhenNothingFailed(t *testing.T) {
	cat := setupTestCatalog(t)

	jobID, err := cat.CreateJob(JobActionRestore, "", []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.CompleteJobItem(jobID, 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := cat.FinishJob(jobID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	job, failures, err := cat.GetJob(jobID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != JobStateDone {
		t.Errorf("State = %q, want done", job.State)
	}
	if failures == nil {
		t.Fatal("failures must be an empty slice, never nil: the UI maps over it")
	}
	if len(failures) != 0 {
		t.Errorf("failures = %+v, want none", failures)
	}
}

func TestGetJobShouldReportNoSuchJobWhenTheIdIsUnknown(t *testing.T) {
	cat := setupTestCatalog(t)

	if _, _, err := cat.GetJob(999, 10); !errors.Is(err, ErrNoSuchJob) {
		t.Fatalf("err = %v, want ErrNoSuchJob", err)
	}
}

func TestJobCountersShouldReachTotalOnEveryTerminalPath(t *testing.T) {
	assertTerminal := func(t *testing.T, cat *Catalog, jobID int64) {
		t.Helper()
		job, _, err := cat.GetJob(jobID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if job.Done != job.Total {
			t.Errorf("done = %d, total = %d: a terminal job must account for every item",
				job.Done, job.Total)
		}
		if job.FailedCount > job.Done {
			t.Errorf("failed = %d > done = %d", job.FailedCount, job.Done)
		}
		if job.State != JobStateDone && job.State != JobStateFailed {
			t.Errorf("State = %q, want a terminal state", job.State)
		}
	}

	t.Run("every item succeeds", func(t *testing.T) {
		cat := setupTestCatalog(t)
		jobID, err := cat.CreateJob(JobActionArchive, "", []int64{1, 2, 3})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []int64{1, 2, 3} {
			if err := cat.CompleteJobItem(jobID, id, ""); err != nil {
				t.Fatal(err)
			}
		}
		if err := cat.FinishJob(jobID, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		assertTerminal(t, cat, jobID)
	})

	t.Run("one item fails", func(t *testing.T) {
		cat := setupTestCatalog(t)
		jobID, err := cat.CreateJob(JobActionArchive, "", []int64{1, 2})
		if err != nil {
			t.Fatal(err)
		}
		if err := cat.CompleteJobItem(jobID, 1, ""); err != nil {
			t.Fatal(err)
		}
		if err := cat.CompleteJobItem(jobID, 2, "boom"); err != nil {
			t.Fatal(err)
		}
		if err := cat.FinishJob(jobID, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		assertTerminal(t, cat, jobID)
	})

	t.Run("cold storage fills partway through", func(t *testing.T) {
		cat := setupTestCatalog(t)
		ids := make([]int64, 800)
		for i := range ids {
			ids[i] = int64(i + 1)
		}
		jobID, err := cat.CreateJob(JobActionArchive, "", ids)
		if err != nil {
			t.Fatal(err)
		}
		for i := int64(1); i <= 11; i++ {
			if err := cat.CompleteJobItem(jobID, i, ""); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := cat.ClaimJobItem(jobID); err != nil {
			t.Fatal(err)
		}
		if err := cat.FailRemainingItems(jobID, "cold storage is full"); err != nil {
			t.Fatal(err)
		}
		if err := cat.FinishJob(jobID, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		assertTerminal(t, cat, jobID)

		job, _, err := cat.GetJob(jobID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if job.FailedCount != 789 {
			t.Errorf("FailedCount = %d, want 789: every abandoned item is a failure", job.FailedCount)
		}
	})

	t.Run("the worker abandons an item job", func(t *testing.T) {
		cat := setupTestCatalog(t)
		jobID, err := cat.CreateJob(JobActionArchive, "", []int64{1, 2, 3})
		if err != nil {
			t.Fatal(err)
		}
		if err := cat.CompleteJobItem(jobID, 1, ""); err != nil {
			t.Fatal(err)
		}
		if err := cat.FailJob(jobID, time.Now().Unix(), "cannot read the queue"); err != nil {
			t.Fatal(err)
		}
		assertTerminal(t, cat, jobID)

		job, _, err := cat.GetJob(jobID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if job.Error != "cannot read the queue" {
			t.Errorf("Error = %q, want the job-level reason", job.Error)
		}
	})

	t.Run("the worker abandons an item-less scan job", func(t *testing.T) {
		cat := setupTestCatalog(t)
		jobID, err := cat.CreateJob(JobActionScan, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := cat.FailJob(jobID, time.Now().Unix(), "scan already in progress"); err != nil {
			t.Fatal(err)
		}
		assertTerminal(t, cat, jobID)
	})

	t.Run("a scan job completes through its virtual item", func(t *testing.T) {
		cat := setupTestCatalog(t)
		jobID, err := cat.CreateJob(JobActionScan, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := cat.CompleteJobItemVirtual(jobID, ""); err != nil {
			t.Fatal(err)
		}
		if err := cat.FinishJob(jobID, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		assertTerminal(t, cat, jobID)
	})
}

func TestResumeInterruptedJobsShouldRequeueInFlightWorkWhenTheDaemonWasKilled(t *testing.T) {
	cat := setupTestCatalog(t)

	jobID, err := cat.CreateJob(JobActionArchive, "", []int64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.MarkJobRunning(jobID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := cat.CompleteJobItem(jobID, 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ClaimJobItem(jobID); err != nil {
		t.Fatal(err)
	}

	items, jobs, err := cat.ResumeInterruptedJobs()
	if err != nil {
		t.Fatal(err)
	}
	if items != 1 {
		t.Errorf("requeued %d items, want 1", items)
	}
	if jobs != 1 {
		t.Errorf("requeued %d jobs, want 1", jobs)
	}

	job, _, err := cat.GetJob(jobID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != JobStatePending {
		t.Errorf("State = %q, want pending", job.State)
	}
	if job.Done != 1 {
		t.Errorf("Done = %d, want 1: an item that was running was never counted", job.Done)
	}
	if job.StartedAt == nil {
		t.Error("started_at is left as it was")
	}

	// The two unfinished items are claimable again.
	var claimed int
	for {
		fileID, err := cat.ClaimJobItem(jobID)
		if err != nil {
			t.Fatal(err)
		}
		if fileID == 0 {
			break
		}
		claimed++
	}
	if claimed != 2 {
		t.Errorf("claimed %d items after recovery, want 2", claimed)
	}
}

func TestMarkJobRunningShouldStampStartedAtOnlyOnceWhenItemsAreClaimedRepeatedly(t *testing.T) {
	cat := setupTestCatalog(t)

	jobID, err := cat.CreateJob(JobActionArchive, "", []int64{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.MarkJobRunning(jobID, 1000); err != nil {
		t.Fatal(err)
	}
	if err := cat.MarkJobRunning(jobID, 2000); err != nil {
		t.Fatal(err)
	}

	job, _, err := cat.GetJob(jobID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if job.StartedAt == nil || *job.StartedAt != 1000 {
		t.Errorf("StartedAt = %v, want 1000", job.StartedAt)
	}
}

func TestPurgeOldJobsShouldDeleteTheItemsAlongsideTheJobWhenItIsPastRetention(t *testing.T) {
	cat := setupTestCatalog(t)

	oldJob, err := cat.CreateJob(JobActionArchive, "", []int64{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if err := cat.CompleteJobItem(oldJob, id, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.FinishJob(oldJob, 1000); err != nil {
		t.Fatal(err)
	}

	recentJob, err := cat.CreateJob(JobActionArchive, "", []int64{3})
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.CompleteJobItem(recentJob, 3, ""); err != nil {
		t.Fatal(err)
	}
	if err := cat.FinishJob(recentJob, 9000); err != nil {
		t.Fatal(err)
	}

	pendingJob, err := cat.CreateJob(JobActionArchive, "", []int64{4})
	if err != nil {
		t.Fatal(err)
	}

	deleted, err := cat.PurgeOldJobs(5000)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("deleted %d jobs, want 1", deleted)
	}

	if _, _, err := cat.GetJob(oldJob, 0); !errors.Is(err, ErrNoSuchJob) {
		t.Errorf("the expired job must be gone, got %v", err)
	}
	if _, _, err := cat.GetJob(recentJob, 0); err != nil {
		t.Errorf("a job inside the retention window must survive: %v", err)
	}
	if _, _, err := cat.GetJob(pendingJob, 0); err != nil {
		t.Errorf("a job that has not finished must never be purged: %v", err)
	}

	var orphans int
	if err := cat.db.QueryRow(`SELECT COUNT(*) FROM job_items WHERE job_id = ?`, oldJob).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("job_items = %d, want 0: there is no ON DELETE CASCADE to do this for us", orphans)
	}
}

func TestListJobsShouldReturnNewestFirstWhenSeveralExist(t *testing.T) {
	cat := setupTestCatalog(t)

	var ids []int64
	for i := 0; i < 3; i++ {
		id, err := cat.CreateJob(JobActionArchive, "", []int64{int64(i + 1)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	jobs, err := cat.ListJobs(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
	if jobs[0].ID != ids[2] || jobs[1].ID != ids[1] {
		t.Errorf("jobs = %d, %d, want %d, %d", jobs[0].ID, jobs[1].ID, ids[2], ids[1])
	}
}

func TestFolderSizesForIDsShouldSumOnlyArchivedBytesWhenTheSelectionIsMixed(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	archivedA := addFile(t, cat, "Photos", "a.jpg", 100, now)
	archivedB := addFile(t, cat, "Photos", "b.jpg", 200, now)
	stillSynced := addFile(t, cat, "Photos", "c.jpg", 400, now)
	archivedDocs := addFile(t, cat, "Docs", "d.pdf", 50, now)

	for _, f := range []*FileRecord{archivedA, archivedB, archivedDocs} {
		if err := cat.UpdateFileStatus(f.ID, StatusArchived); err != nil {
			t.Fatal(err)
		}
	}

	sizes, err := cat.FolderSizesForIDs([]int64{archivedA.ID, archivedB.ID, stillSynced.ID, archivedDocs.ID})
	if err != nil {
		t.Fatal(err)
	}
	if sizes["Photos"] != 300 {
		t.Errorf("Photos = %d, want 300: a synced file brings nothing back", sizes["Photos"])
	}
	if sizes["Docs"] != 50 {
		t.Errorf("Docs = %d, want 50", sizes["Docs"])
	}
}

func TestListFileIDsShouldReportTheTrueTotalWhenTheResultIsTruncated(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	for i := 0; i < 10; i++ {
		addFile(t, cat, "Photos", "f"+itoa(i)+".jpg", 1, now)
	}

	ids, total, err := cat.ListFileIDs(FileQuery{Folder: "Photos"}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 4 {
		t.Errorf("got %d ids, want 4", len(ids))
	}
	if total != 10 {
		t.Errorf("total = %d, want the real count 10", total)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("ids are not ascending: %v", ids)
		}
	}
}

func TestListFileIDsShouldReturnAnEmptySliceWhenNothingMatches(t *testing.T) {
	cat := setupTestCatalog(t)

	ids, total, err := cat.ListFileIDs(FileQuery{Folder: "Nope"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ids == nil {
		t.Fatal("ids must be an empty slice, never nil")
	}
	if len(ids) != 0 || total != 0 {
		t.Errorf("ids = %v, total = %d, want empty", ids, total)
	}
}

func TestListFilesShouldReturnExactlyTheRequestedIdsWhenIDsIsSet(t *testing.T) {
	cat := setupTestCatalog(t)
	now := time.Now().UTC()

	a := addFile(t, cat, "Photos", "a.jpg", 1, now)
	addFile(t, cat, "Photos", "b.jpg", 1, now)
	c := addFile(t, cat, "Docs", "c.pdf", 1, now)

	files, err := cat.ListFiles(FileQuery{Folder: "Photos", IDs: []int64{a.ID, c.ID, 9999}})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2 (the ids that exist)", len(files))
	}
	if files[0].ID != a.ID || files[1].ID != c.ID {
		t.Errorf("files = %d, %d, want %d, %d in id order", files[0].ID, files[1].ID, a.ID, c.ID)
	}
}
