//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/daemon"
)

// Design section 5.8, in full, through the real HTTP surface:
//
//	"1. pinned = 1 on restore. A restored file is not an offload candidate.
//	 Without this, restoring 200 files pushes the folder far over budget, the
//	 next tick archives all 200, and each archive is a Syncthing deletion
//	 propagated to the owner's phone -- hundreds of sync deletes of files the
//	 user just asked to get back. This is why D2 and D4 ship together or not at
//	 all."
//
// The bulk restore here deliberately blows the budget with force, which is the
// state the pre-flight exists to warn about, and then asserts that the daemon
// converges by taking the files the owner did NOT select.
func TestBulkRestoreShouldSurviveTheNextQuotaTickWhenItDeliberatelyExceedsTheBudget(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1<<40), withGracePeriod(0))

	wanted := make([]int64, 0, 6)
	for i := 0; i < 6; i++ {
		file := seedAgedFile(t, env, fmt.Sprintf("wanted%d.jpg", i), 300)
		env.archive(t, file.ID)
		wanted = append(wanted, file.ID)
	}
	spares := make([]int64, 0, 4)
	for i := 0; i < 4; i++ {
		spares = append(spares, seedAgedFile(t, env, fmt.Sprintf("spare%d.jpg", i), 300).ID)
	}

	// 1800 restored + 1200 already on the device against a 2000 budget.
	budget := int64(2000)
	setPolicy(t, env, "TestFolder", &budget, nil, nil)

	created := postJob(t, env, map[string]any{
		"action": "restore", "ids": wanted, "force": true,
	})
	env.drainJobs(t)
	if job := getJob(t, env, created.JobID); job.State != "done" || job.Done != 6 {
		t.Fatalf("job = %q done %d, want done and 6", job.State, job.Done)
	}

	env.daemon.EnforceQuotas()

	for _, id := range wanted {
		rec, err := env.cat.GetFileByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status != catalog.StatusSynced {
			t.Errorf("restored file %d status = %q, want synced: the quota tick undid "+
				"the owner's restore", id, rec.Status)
		}
		if !rec.Pinned {
			t.Errorf("restored file %d lost its pin", id)
		}
	}

	// And the daemon did converge: it shed what it was allowed to shed.
	archivedSpares := 0
	for _, id := range spares {
		rec, err := env.cat.GetFileByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status == catalog.StatusArchived {
			archivedSpares++
		}
	}
	if archivedSpares != 4 {
		t.Errorf("%d of 4 unselected files were archived; the daemon must offload "+
			"every unpinned candidate when it cannot converge", archivedSpares)
	}
}

// The other half of the same rule: a quota tick that runs BEFORE the restore
// must not leave the job with items that fail their CAS for a reason the owner
// cannot act on. Both actors take the same mutex and both compare-and-swap, so
// the loser is a clean no-op rather than a corrupted row.
func TestBulkRestoreShouldStillSucceedWhenAQuotaTickArchivedTheSameFilesFirst(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1<<40), withGracePeriod(0))

	ids := make([]int64, 0, 5)
	for i := 0; i < 5; i++ {
		ids = append(ids, seedAgedFile(t, env, fmt.Sprintf("photo%d.jpg", i), 300).ID)
	}

	budget := int64(300)
	setPolicy(t, env, "TestFolder", &budget, nil, nil)
	env.daemon.EnforceQuotas() // archives four of the five

	// Now the owner asks for all five back, including the one that never left.
	created := postJob(t, env, map[string]any{
		"action": "restore", "ids": ids, "force": true,
	})
	env.drainJobs(t)

	job := getJob(t, env, created.JobID)
	if job.State != "done" {
		t.Fatalf("state = %q, want done: an already-synced file is a no-op success, "+
			"not a failure (invariant 12). failures: %+v", job.State, job.Failed)
	}
	if job.Done != 5 || job.FailedCount != 0 {
		t.Errorf("counters = done %d failed %d, want 5 and 0", job.Done, job.FailedCount)
	}
	for _, id := range ids {
		rec, err := env.cat.GetFileByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status != catalog.StatusSynced {
			t.Errorf("file %d status = %q, want synced", id, rec.Status)
		}
	}
}

// A job in flight when the daemon is killed comes back by itself. This drives
// the real startup sequence -- RecoverStartupState, then ResumeInterruptedJobs,
// then a fresh worker -- over the same catalog the "killed" run left behind.
func TestJobShouldResumeAutomaticallyWhenTheDaemonIsRestartedMidJob(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1<<40))

	ids := make([]int64, 0, 8)
	for i := 0; i < 8; i++ {
		ids = append(ids, seedAgedFile(t, env, fmt.Sprintf("photo%d.jpg", i), 100).ID)
	}

	created := postJob(t, env, map[string]any{"action": "archive", "ids": ids})

	// The kill: the worker is stopped after three items, with the fourth
	// claimed and its move already committed but not recorded.
	if err := env.cat.MarkJobRunning(created.JobID, 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		fileID, err := env.cat.ClaimJobItem(created.JobID)
		if err != nil || fileID == 0 {
			t.Fatalf("claiming item %d: %v (id %d)", i, err, fileID)
		}
		env.archive(t, fileID)
		if err := env.cat.CompleteJobItem(created.JobID, fileID, ""); err != nil {
			t.Fatal(err)
		}
	}
	doomed, err := env.cat.ClaimJobItem(created.JobID)
	if err != nil || doomed == 0 {
		t.Fatalf("claiming the doomed item: %v (id %d)", err, doomed)
	}
	env.archive(t, doomed)

	// Restart.
	if err := daemon.RecoverStartupState(env.cfg, env.cat, env.logger); err != nil {
		t.Fatal(err)
	}
	items, jobs, err := env.cat.ResumeInterruptedJobs()
	if err != nil {
		t.Fatal(err)
	}
	if items != 1 || jobs != 1 {
		t.Fatalf("resumed %d items and %d jobs, want 1 and 1", items, jobs)
	}
	if mid := getJob(t, env, created.JobID); mid.State != "pending" {
		t.Errorf("state after resume = %q, want pending", mid.State)
	}

	env.worker.RunOnce(context.Background())

	job := getJob(t, env, created.JobID)
	if job.State != "done" {
		t.Fatalf("state = %q, want done. failures: %+v", job.State, job.Failed)
	}
	if job.Done != 8 || job.FailedCount != 0 {
		t.Errorf("counters = done %d failed %d, want 8 and 0", job.Done, job.FailedCount)
	}
	for _, id := range ids {
		rec, err := env.cat.GetFileByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status != catalog.StatusArchived {
			t.Errorf("file %d status = %q, want archived", id, rec.Status)
		}
	}
}
