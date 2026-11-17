//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// The wire shapes, spelled out rather than reusing the Go types, so a rename on
// the server side shows up here as a failing test instead of silently passing.
type jobCreatedJSON struct {
	JobID  int64  `json:"job_id"`
	Action string `json:"action"`
	State  string `json:"state"`
	Total  int    `json:"total"`
}

type jobFailureJSON struct {
	ID    int64  `json:"id"`
	Error string `json:"error"`
}

type jobJSON struct {
	ID          int64            `json:"id"`
	Action      string           `json:"action"`
	FolderLabel string           `json:"folder_label"`
	State       string           `json:"state"`
	Total       int              `json:"total"`
	Done        int              `json:"done"`
	FailedCount int              `json:"failed_count"`
	Error       string           `json:"error"`
	CreatedAt   int64            `json:"created_at"`
	StartedAt   *int64           `json:"started_at"`
	FinishedAt  *int64           `json:"finished_at"`
	Failed      []jobFailureJSON `json:"failed"`
}

type budgetPreflightJSON struct {
	Error              string `json:"error"`
	Folder             string `json:"folder"`
	OverageBytes       int64  `json:"overage_bytes"`
	EstimatedEvictions int    `json:"estimated_evictions"`
}

type fileIDsJSON struct {
	IDs       []int64 `json:"ids"`
	Total     int64   `json:"total"`
	Truncated bool    `json:"truncated"`
}

func postJob(t *testing.T, env *testEnv, body any) *jobCreatedJSON {
	t.Helper()
	w := do(t, env, "POST", "/api/v1/jobs", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST /jobs status = %d, want 202, body: %s", w.Code, w.Body.String())
	}
	var created jobCreatedJSON
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	return &created
}

func getJob(t *testing.T, env *testEnv, id int64) jobJSON {
	t.Helper()
	w := do(t, env, "GET", "/api/v1/jobs/"+strconv.FormatInt(id, 10), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /jobs/%d status = %d, want 200, body: %s", id, w.Code, w.Body.String())
	}
	var job jobJSON
	if err := json.NewDecoder(w.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	return job
}

// The whole point of the step: a selection of hundreds of files comes back
// without holding an HTTP connection open for the duration, and every one of
// them comes back pinned so the next quota tick cannot take them away again.
func TestBulkRestoreJobShouldCompleteAndPinEverythingWhenItIsDrained(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1<<40))

	ids := make([]int64, 0, 20)
	for i := 0; i < 20; i++ {
		file := seedAgedFile(t, env, fmt.Sprintf("photo%02d.jpg", i), 100)
		env.archive(t, file.ID)
		ids = append(ids, file.ID)
	}

	created := postJob(t, env, map[string]any{"action": "restore", "ids": ids})
	if created.Total != 20 {
		t.Errorf("total = %d, want 20", created.Total)
	}
	if created.State != "pending" {
		t.Errorf("state = %q, want pending", created.State)
	}

	// The response arrived before any work happened: nothing is restored yet.
	if before := getJob(t, env, created.JobID); before.Done != 0 {
		t.Errorf("done = %d before the worker ran; POST /jobs did the work inline", before.Done)
	}

	env.drainJobs(t)

	job := getJob(t, env, created.JobID)
	if job.State != "done" {
		t.Fatalf("state = %q, want done (error %q, failed %+v)", job.State, job.Error, job.Failed)
	}
	if job.Done != 20 || job.FailedCount != 0 {
		t.Errorf("counters = done %d failed %d, want 20 and 0", job.Done, job.FailedCount)
	}
	if job.StartedAt == nil || job.FinishedAt == nil {
		t.Errorf("timestamps = started %v finished %v, want both set", job.StartedAt, job.FinishedAt)
	}
	for _, id := range ids {
		rec, err := env.cat.GetFileByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status != "synced" || !rec.Pinned || rec.PinSource != "restore" {
			t.Errorf("file %d = %q pinned %v source %q, want synced/true/restore",
				id, rec.Status, rec.Pinned, rec.PinSource)
		}
	}
}

func TestBulkArchiveJobShouldMoveEveryFileWhenItIsDrained(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1<<40))

	ids := make([]int64, 0, 5)
	for i := 0; i < 5; i++ {
		ids = append(ids, seedAgedFile(t, env, fmt.Sprintf("photo%d.jpg", i), 100).ID)
	}

	created := postJob(t, env, map[string]any{"action": "archive", "ids": ids})
	env.drainJobs(t)

	job := getJob(t, env, created.JobID)
	if job.State != "done" || job.Done != 5 {
		t.Fatalf("job = %q done %d, want done and 5", job.State, job.Done)
	}
	for _, id := range ids {
		rec, err := env.cat.GetFileByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status != "archived" {
			t.Errorf("file %d status = %q, want archived", id, rec.Status)
		}
	}
}

func TestJobShouldReportFailedWithTheOffendingIdWhenOneItemFails(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1<<40))

	ids := make([]int64, 0, 6)
	for i := 0; i < 5; i++ {
		ids = append(ids, seedAgedFile(t, env, fmt.Sprintf("photo%d.jpg", i), 100).ID)
	}
	const bogus = 4242
	ids = append(ids, bogus)

	created := postJob(t, env, map[string]any{"action": "archive", "ids": ids})
	env.drainJobs(t)

	job := getJob(t, env, created.JobID)
	if job.State != "failed" {
		t.Fatalf("state = %q, want failed", job.State)
	}
	if job.Done != 6 || job.FailedCount != 1 {
		t.Errorf("counters = done %d failed %d, want 6 and 1", job.Done, job.FailedCount)
	}
	if len(job.Failed) != 1 || job.Failed[0].ID != bogus {
		t.Fatalf("failed = %+v, want exactly the bogus id", job.Failed)
	}
	if job.Failed[0].Error == "" {
		t.Error("the failure carries no message")
	}
	// The job-level error stays empty: no infrastructural failure happened.
	if job.Error != "" {
		t.Errorf("job error = %q, want empty on an item-level failure", job.Error)
	}
}

// Design section 6.0 rule 2: arrays are never null.
func TestJobDetailShouldCarryAnEmptyArrayWhenNothingFailed(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1<<40))
	file := seedAgedFile(t, env, "one.jpg", 100)

	created := postJob(t, env, map[string]any{"action": "archive", "ids": []int64{file.ID}})
	env.drainJobs(t)

	w := do(t, env, "GET", "/api/v1/jobs/"+strconv.FormatInt(created.JobID, 10), nil)
	body := w.Body.String()
	if !strings.Contains(body, `"failed":[]`) {
		t.Errorf("body = %s, want a literal empty array for `failed`", body)
	}
}

// D-6. catalog.CreateJob would happily write a job with total = 0 and no items,
// and NextPendingJob would then hand it to the worker forever.
func TestJobsEndpointShouldRejectAnEmptySelection(t *testing.T) {
	env := newTestEnv(t)

	for _, action := range []string{"restore", "archive"} {
		t.Run(action, func(t *testing.T) {
			for name, body := range map[string]any{
				"empty array": map[string]any{"action": action, "ids": []int64{}},
				"absent":      map[string]any{"action": action},
				"null":        map[string]any{"action": action, "ids": nil},
			} {
				w := do(t, env, "POST", "/api/v1/jobs", body)
				if w.Code != http.StatusBadRequest {
					t.Errorf("%s: status = %d, want 400, body: %s", name, w.Code, w.Body.String())
				}
				if !strings.Contains(w.Body.String(), "ids must not be empty") {
					t.Errorf("%s: body = %s, want the empty-ids message", name, w.Body.String())
				}
			}
		})
	}

	// Nothing was written: no zero-item job is sitting in the queue.
	jobs, err := env.cat.ListJobs(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Errorf("%d jobs were created by rejected requests", len(jobs))
	}
}

func TestJobsEndpointShouldRejectAnUnknownAction(t *testing.T) {
	env := newTestEnv(t)

	// "delete" was the example here until permanent delete became a real job
	// action; the name must be one the endpoint will never grow.
	w := do(t, env, "POST", "/api/v1/jobs", map[string]any{"action": "shred", "ids": []int64{1}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unknown action") ||
		!strings.Contains(w.Body.String(), "shred") {
		t.Errorf("body = %s, want it to name the action", w.Body.String())
	}
}

func TestJobsEndpointShouldRejectASelectionLargerThanTheSharedCap(t *testing.T) {
	env := newTestEnv(t)

	ids := make([]int64, 20001)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	w := do(t, env, "POST", "/api/v1/jobs", map[string]any{"action": "archive", "ids": ids})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "at most 20000 ids per job") {
		t.Errorf("body = %s, want the cap message", w.Body.String())
	}
}

func TestJobsEndpointShouldDeduplicateTheSelectionWhenTheSameIdIsSentTwice(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1<<40))
	file := seedAgedFile(t, env, "one.jpg", 100)

	created := postJob(t, env, map[string]any{
		"action": "archive",
		"ids":    []int64{file.ID, file.ID, file.ID},
	})
	if created.Total != 1 {
		t.Errorf("total = %d, want 1: duplicate ids must collapse", created.Total)
	}

	env.drainJobs(t)
	job := getJob(t, env, created.JobID)
	if job.State != "done" || job.Done != 1 {
		t.Errorf("job = %q done %d, want done and 1", job.State, job.Done)
	}
}

// The number in the refusal is a promise about what coldstore will do to the
// owner's other devices, so it may not count a file coldstore will never
// touch. A persistent glob protects its matches permanently (design section
// 4.1a): the offloader skips them at every budget, at every age, forever.
func TestRestoreRefusalShouldNotPromiseEvictionsThatAPersistentGlobForbids(t *testing.T) {
	env := newTestEnv(t,
		withBudget("TestFolder", 1000),
		withPersistent("TestFolder", "Starred/*"))

	// One archived file to bring back, and one on-device file that is the only
	// thing old enough and loose enough to be a candidate -- except that it is
	// starred, which takes it out of the running for good.
	cold := seedAgedFile(t, env, "cold.jpg", 900)
	env.archive(t, cold.ID)
	seedAgedFile(t, env, "Starred/fav.jpg", 400)

	w := do(t, env, "POST", "/api/v1/jobs",
		map[string]any{"action": "restore", "ids": []int64{cold.ID}})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body: %s", w.Code, w.Body.String())
	}
	var refusal budgetPreflightJSON
	if err := json.NewDecoder(w.Body).Decode(&refusal); err != nil {
		t.Fatal(err)
	}
	// 400 on device + 900 restored - 1000 budget.
	if refusal.OverageBytes != 300 {
		t.Errorf("overage_bytes = %d, want 300", refusal.OverageBytes)
	}
	if refusal.EstimatedEvictions != 0 {
		t.Errorf("estimated_evictions = %d, want 0: the only candidate is protected by "+
			"Starred/* and can never be evicted, so the dialog would promise a deletion "+
			"that cannot happen", refusal.EstimatedEvictions)
	}
}

// The mirror of the test above, so the fix cannot be "always report zero": an
// unprotected candidate still has to be counted.
func TestRestoreRefusalShouldStillCountTheCandidatesNoGlobProtects(t *testing.T) {
	env := newTestEnv(t,
		withBudget("TestFolder", 1000),
		withPersistent("TestFolder", "Starred/*"))

	cold := seedAgedFile(t, env, "cold.jpg", 900)
	env.archive(t, cold.ID)
	seedAgedFile(t, env, "Starred/fav.jpg", 200)
	seedAgedFile(t, env, "loose.jpg", 200)

	w := do(t, env, "POST", "/api/v1/jobs",
		map[string]any{"action": "restore", "ids": []int64{cold.ID}})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body: %s", w.Code, w.Body.String())
	}
	var refusal budgetPreflightJSON
	if err := json.NewDecoder(w.Body).Decode(&refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.EstimatedEvictions != 1 {
		t.Errorf("estimated_evictions = %d, want 1: loose.jpg is unprotected and would go",
			refusal.EstimatedEvictions)
	}
}

// Design section 5.10 fix 1. A restore the daemon would immediately undo, by
// evicting a comparable volume of files the owner never selected, is refused
// with the numbers the UI needs to say so out loud.
func TestRestoreJobShouldBeRefusedWithoutForceWhenItWouldExceedTheBudget(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1000))

	// 5 archived files of 300 bytes each, and 3 on-device files of 200.
	archived := make([]int64, 0, 5)
	for i := 0; i < 5; i++ {
		file := seedAgedFile(t, env, fmt.Sprintf("cold%d.jpg", i), 300)
		env.archive(t, file.ID)
		archived = append(archived, file.ID)
	}
	for i := 0; i < 3; i++ {
		seedAgedFile(t, env, fmt.Sprintf("warm%d.jpg", i), 200)
	}

	w := do(t, env, "POST", "/api/v1/jobs", map[string]any{"action": "restore", "ids": archived})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body: %s", w.Code, w.Body.String())
	}
	var refusal budgetPreflightJSON
	if err := json.NewDecoder(w.Body).Decode(&refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Folder != "TestFolder" {
		t.Errorf("folder = %q, want TestFolder", refusal.Folder)
	}
	// 600 on device + 1500 restored - 1000 budget.
	if refusal.OverageBytes != 1100 {
		t.Errorf("overage_bytes = %d, want 1100", refusal.OverageBytes)
	}
	if refusal.EstimatedEvictions <= 0 {
		t.Errorf("estimated_evictions = %d, want a positive count: the dialog has "+
			"nothing to warn the owner with", refusal.EstimatedEvictions)
	}
	if refusal.Error == "" {
		t.Error("the refusal carries no message")
	}

	// Refused means refused: no job was queued.
	jobs, err := env.cat.ListJobs(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Errorf("%d jobs were queued by a refused restore", len(jobs))
	}

	// The same request with force goes through.
	created := postJob(t, env, map[string]any{
		"action": "restore", "ids": archived, "force": true,
	})
	if created.Total != 5 {
		t.Errorf("forced total = %d, want 5", created.Total)
	}
}

func TestRestoreJobShouldBeAcceptedWhenItFitsInsideTheBudget(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 10000))
	file := seedAgedFile(t, env, "cold.jpg", 300)
	env.archive(t, file.ID)

	created := postJob(t, env, map[string]any{"action": "restore", "ids": []int64{file.ID}})
	if created.Total != 1 {
		t.Errorf("total = %d, want 1", created.Total)
	}
}

func TestRestoreJobShouldBeAcceptedWhenTheFolderHasNoBudget(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 10000))
	file := seedAgedFile(t, env, "cold.jpg", 300)
	env.archive(t, file.ID)

	// An unset budget is no cap at all (D1), so there is nothing to exceed.
	zero := int64(0)
	setPolicy(t, env, "TestFolder", &zero, nil, nil)

	postJob(t, env, map[string]any{"action": "restore", "ids": []int64{file.ID}})
}

// Design section 6.11. The old handler walked the whole tree inside the request
// with no guard against the daemon's own scan.
func TestScanEndpointShouldEnqueueAJobInsteadOfWalkingInline(t *testing.T) {
	env := newTestEnv(t)

	w := do(t, env, "POST", "/api/v1/scan", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body: %s", w.Code, w.Body.String())
	}
	var created jobCreatedJSON
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Action != "scan" || created.State != "pending" || created.Total != 1 {
		t.Errorf("body = %+v, want a pending scan job with total 1", created)
	}

	env.drainJobs(t)
	if job := getJob(t, env, created.JobID); job.State != "done" {
		t.Errorf("state = %q, want done (error %q)", job.State, job.Error)
	}
}

func TestJobsListShouldReturnTheNewestJobsFirst(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1<<40))
	first := seedAgedFile(t, env, "a.jpg", 100)
	second := seedAgedFile(t, env, "b.jpg", 100)

	oldest := postJob(t, env, map[string]any{"action": "archive", "ids": []int64{first.ID}})
	newest := postJob(t, env, map[string]any{"action": "archive", "ids": []int64{second.ID}})

	w := do(t, env, "GET", "/api/v1/jobs", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var jobs []jobJSON
	if err := json.NewDecoder(w.Body).Decode(&jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
	if jobs[0].ID != newest.JobID || jobs[1].ID != oldest.JobID {
		t.Errorf("order = %d, %d; want newest (%d) first",
			jobs[0].ID, jobs[1].ID, newest.JobID)
	}
	// The list omits the failure detail array.
	if jobs[0].Failed != nil {
		t.Errorf("list entries carry `failed`: %+v", jobs[0].Failed)
	}
}

func TestJobsListShouldReturnAnEmptyArrayWhenNothingHasEverRun(t *testing.T) {
	env := newTestEnv(t)
	w := do(t, env, "GET", "/api/v1/jobs", nil)
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Errorf("body = %s, want []", got)
	}
}

func TestGetJobShouldReportNotFoundWhenTheIdIsUnknown(t *testing.T) {
	env := newTestEnv(t)
	w := do(t, env, "GET", "/api/v1/jobs/999", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body: %s", w.Code, w.Body.String())
	}
}

// GET /files/ids and POST /jobs share ONE cap, so a selection the ids endpoint
// hands the UI is always one the jobs endpoint will accept.
func TestFileIDsShouldReturnTheWholeSelectionWhenTheFilterMatches(t *testing.T) {
	env := newTestEnv(t, withBudget("TestFolder", 1<<40))
	for i := 0; i < 4; i++ {
		seedAgedFile(t, env, fmt.Sprintf("photo%d.jpg", i), 100)
	}
	archivedFile := seedAgedFile(t, env, "cold.jpg", 100)
	env.archive(t, archivedFile.ID)

	w := do(t, env, "GET", "/api/v1/files/ids?folder=TestFolder&status=archived", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	var resp fileIDsJSON
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.IDs) != 1 || resp.IDs[0] != archivedFile.ID {
		t.Errorf("ids = %v, want just %d", resp.IDs, archivedFile.ID)
	}
	if resp.Total != 1 || resp.Truncated {
		t.Errorf("total = %d truncated = %v, want 1 and false", resp.Total, resp.Truncated)
	}

	// And the selection round-trips into a job.
	created := postJob(t, env, map[string]any{"action": "restore", "ids": resp.IDs})
	if created.Total != 1 {
		t.Errorf("total = %d, want 1", created.Total)
	}
}

func TestFileIDsShouldReturnAnEmptyArrayWhenNothingMatches(t *testing.T) {
	env := newTestEnv(t)
	w := do(t, env, "GET", "/api/v1/files/ids?folder=Nope", nil)
	if !strings.Contains(w.Body.String(), `"ids":[]`) {
		t.Errorf("body = %s, want a literal empty array", w.Body.String())
	}
}
