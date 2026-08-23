package catalog

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNoSuchJob is returned when a job id has no row.
var ErrNoSuchJob = errors.New("no such job")

// ErrPairSelectionChanged means a caller preflighted a closed pair selection,
// but a pairing update added a member before the job transaction began.
var ErrPairSelectionChanged = errors.New("pair selection changed after preflight")

const jobColumns = `id, action, folder_label, state, total, done, failed, error, payload,
	created_at, started_at, finished_at`

// CreateJob expands pair groups and inserts the job and its items in ONE
// transaction. Duplicate ids collapse via the (job_id, file_id) primary key
// and total is set from the count actually inserted. A scan job has total = 1
// and no items.
func (c *Catalog) CreateJob(action, folderLabel string, fileIDs []int64) (int64, error) {
	return c.createJob(action, folderLabel, fileIDs, false)
}

// CreateRenameJob writes the one synthetic item and its restart payload in
// the same statement. Rename jobs deliberately have no job_items rows: the
// filesystem/catalog sequence is one idempotent operation, not N file moves.
func (c *Catalog) CreateRenameJob(folderLabel, from, to string) (int64, error) {
	payload, err := json.Marshal(RenamePayload{From: from, To: to})
	if err != nil {
		return 0, fmt.Errorf("encoding rename payload: %w", err)
	}
	res, err := c.db.Exec(`INSERT INTO jobs
		(action, folder_label, total, payload, created_at)
		VALUES (?, ?, 1, ?, ?)`, JobActionRename, folderLabel, string(payload), time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("creating rename job: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("creating rename job: %w", err)
	}
	return id, nil
}

// DecodeRenamePayload validates the durable shape before a worker acts on it.
func DecodeRenamePayload(raw string) (RenamePayload, error) {
	var payload RenamePayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return payload, fmt.Errorf("invalid rename payload: %w", err)
	}
	if payload.From == "" || payload.To == "" {
		return payload, errors.New("invalid rename payload: from and to are required")
	}
	return payload, nil
}

// CreatePreflightedMoveJob is CreateJob with a closure check for restore and
// untrash selections whose bytes were already approved by the API. It never
// silently inserts a companion which was absent from that budget preflight.
func (c *Catalog) CreatePreflightedMoveJob(action, folderLabel string, fileIDs []int64) (int64, error) {
	return c.createJob(action, folderLabel, fileIDs, true)
}

func (c *Catalog) createJob(action, folderLabel string, fileIDs []int64, requireClosed bool) (int64, error) {
	switch action {
	case JobActionRestore, JobActionArchive, JobActionScan,
		JobActionTrash, JobActionUntrash, JobActionDelete:
	default:
		return 0, fmt.Errorf("invalid job action %q", action)
	}

	tx, err := c.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("creating job: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`INSERT INTO jobs (action, folder_label, total, created_at)
		VALUES (?, ?, 0, ?)`, action, folderLabel, time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("creating job: %w", err)
	}
	jobID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("creating job: %w", err)
	}

	total := 0
	if action == JobActionScan {
		total = 1
	} else {
		// Expansion belongs in the same transaction as job_items. A pairing
		// write can never land between discovering a companion and committing
		// the selection, which would enqueue half a Live Photo.
		expanded, expandErr := expandPairGroups(tx, fileIDs)
		if expandErr != nil {
			return 0, fmt.Errorf("creating job items: %w", expandErr)
		}
		if requireClosed && !equalIDSets(fileIDs, expanded) {
			return 0, ErrPairSelectionChanged
		}
		fileIDs = expanded
		stmt, err := tx.Prepare(`INSERT OR IGNORE INTO job_items (job_id, file_id) VALUES (?, ?)`)
		if err != nil {
			return 0, fmt.Errorf("creating job items: %w", err)
		}
		defer stmt.Close()

		for _, fileID := range fileIDs {
			r, err := stmt.Exec(jobID, fileID)
			if err != nil {
				return 0, fmt.Errorf("creating job item %d: %w", fileID, err)
			}
			n, err := r.RowsAffected()
			if err != nil {
				return 0, fmt.Errorf("creating job item %d: %w", fileID, err)
			}
			total += int(n)
		}
	}

	if _, err := tx.Exec(`UPDATE jobs SET total = ? WHERE id = ?`, total, jobID); err != nil {
		return 0, fmt.Errorf("setting job total: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("creating job: commit: %w", err)
	}
	return jobID, nil
}

func equalIDSets(a, b []int64) bool {
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

// NextPendingJob returns the lowest-id job that still has work, or nil.
func (c *Catalog) NextPendingJob() (*Job, error) {
	row := c.db.QueryRow(`SELECT ` + jobColumns + ` FROM jobs
		WHERE state IN ('pending','running') ORDER BY id LIMIT 1`)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return job, err
}

// ClaimJobItem atomically takes the next pending item of a job.
// It returns (0, nil) when the job has no pending items left.
func (c *Catalog) ClaimJobItem(jobID int64) (fileID int64, err error) {
	err = c.db.QueryRow(`
		UPDATE job_items SET state = 'running'
		WHERE job_id = ? AND file_id = (
			SELECT file_id FROM job_items
			WHERE job_id = ? AND state = 'pending'
			ORDER BY file_id LIMIT 1)
		RETURNING file_id`, jobID, jobID).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("claiming job item: %w", err)
	}
	return fileID, nil
}

// MarkJobRunning sets state='running' and started_at if not already set.
func (c *Catalog) MarkJobRunning(jobID int64, at int64) error {
	_, err := c.db.Exec(`UPDATE jobs SET state = 'running', started_at = COALESCE(started_at, ?)
		WHERE id = ? AND state IN ('pending','running')`, at, jobID)
	return err
}

// CompleteJobItem writes the item's terminal state and bumps jobs.done (and
// jobs.failed when errMsg is non-empty) in the SAME transaction, so progress
// can never drift from the items. An item that is already terminal is not
// counted twice.
func (c *Catalog) CompleteJobItem(jobID, fileID int64, errMsg string) error {
	state := JobStateDone
	failedDelta := 0
	if errMsg != "" {
		state = JobStateFailed
		failedDelta = 1
	}

	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("completing job item: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE job_items SET state = ?, error = ?
		WHERE job_id = ? AND file_id = ? AND state IN ('pending','running')`,
		state, errMsg, jobID, fileID)
	if err != nil {
		return fmt.Errorf("completing job item: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("completing job item: %w", err)
	}
	if n == 0 {
		return tx.Commit()
	}

	if _, err := tx.Exec(`UPDATE jobs SET done = done + 1, failed = failed + ? WHERE id = ?`,
		failedDelta, jobID); err != nil {
		return fmt.Errorf("advancing job counters: %w", err)
	}
	return tx.Commit()
}

// CompleteJobItemVirtual records the single synthetic item of a scan job, which
// has total = 1 and no job_items rows. It does not stamp finished_at; FinishJob
// does that.
func (c *Catalog) CompleteJobItemVirtual(jobID int64, errMsg string) error {
	_, err := c.db.Exec(`
		UPDATE jobs SET
			failed = failed + CASE WHEN done < total AND ? <> '' THEN 1 ELSE 0 END,
			done = CASE WHEN done < total THEN done + 1 ELSE done END,
			error = CASE WHEN done < total THEN ? ELSE error END
		WHERE id = ?`, errMsg, errMsg, jobID)
	return err
}

// FailRemainingItems flips every still-pending or running item of a job to
// failed with the same message AND advances both job counters by the same
// count, in ONE transaction.
//
// Bumping the items without bumping the counters is the bug this signature
// exists to prevent: it produces a terminal job reporting
// {total: 800, done: 12, failed_count: 1} while 788 files actually failed.
func (c *Catalog) FailRemainingItems(jobID int64, errMsg string) error {
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("failing remaining items: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE job_items SET state = 'failed', error = ?
		WHERE job_id = ? AND state IN ('pending','running')`, errMsg, jobID)
	if err != nil {
		return fmt.Errorf("failing remaining items: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failing remaining items: %w", err)
	}
	if n == 0 {
		return tx.Commit()
	}

	if _, err := tx.Exec(`UPDATE jobs SET done = done + ?, failed = failed + ? WHERE id = ?`,
		n, n, jobID); err != nil {
		return fmt.Errorf("advancing job counters: %w", err)
	}
	return tx.Commit()
}

// FinishJob sets the terminal state from the counters and stamps finished_at.
func (c *Catalog) FinishJob(jobID int64, at int64) error {
	_, err := c.db.Exec(`
		UPDATE jobs SET state = CASE WHEN failed > 0 THEN 'failed' ELSE 'done' END,
			finished_at = ?
		WHERE id = ?`, at, jobID)
	return err
}

// FailJob terminates a job the worker has abandoned for a reason not
// attributable to one item. It fails every remaining item first, then forces
// the counters to total so that the terminal-job invariant
// (done == total, failed <= done) holds for item-less scan jobs too.
func (c *Catalog) FailJob(jobID int64, at int64, reason string) error {
	if err := c.FailRemainingItems(jobID, reason); err != nil {
		return err
	}
	// SQLite evaluates every SET expression against the pre-update row, so
	// done and total below are the values before this statement.
	_, err := c.db.Exec(`
		UPDATE jobs SET failed = failed + MAX(0, total - done),
			done = MAX(done, total),
			error = ?, state = 'failed', finished_at = ?
		WHERE id = ?`, reason, at, jobID)
	return err
}

// GetJob returns the job row plus up to failureLimit failed items.
// The failures slice is always non-nil.
func (c *Catalog) GetJob(jobID int64, failureLimit int) (*Job, []JobFailure, error) {
	row := c.db.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE id = ?`, jobID)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: %d", ErrNoSuchJob, jobID)
	}
	if err != nil {
		return nil, nil, err
	}

	if failureLimit <= 0 {
		failureLimit = 100
	}
	rows, err := c.db.Query(`SELECT file_id, error FROM job_items
		WHERE job_id = ? AND state = 'failed' ORDER BY file_id LIMIT ?`, jobID, failureLimit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	failures := []JobFailure{}
	for rows.Next() {
		var f JobFailure
		if err := rows.Scan(&f.ID, &f.Error); err != nil {
			return nil, nil, err
		}
		failures = append(failures, f)
	}
	return job, failures, rows.Err()
}

// ListJobs returns the most recent jobs, newest first. The slice is always
// non-nil.
func (c *Catalog) ListJobs(limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := c.db.Query(`SELECT `+jobColumns+` FROM jobs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jobs := []Job{}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, *job)
	}
	return jobs, rows.Err()
}

// ResumeInterruptedJobs resets in-flight work left behind by a kill.
//
// started_at is left as it was, and done/failed are untouched: CompleteJobItem
// only increments them inside the same transaction that makes an item terminal,
// so an item that was running was never counted.
func (c *Catalog) ResumeInterruptedJobs() (items int64, jobs int64, err error) {
	tx, err := c.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("resuming jobs: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE job_items SET state = 'pending', error = '' WHERE state = 'running'`)
	if err != nil {
		return 0, 0, fmt.Errorf("resuming job items: %w", err)
	}
	items, err = res.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("resuming job items: %w", err)
	}

	res, err = tx.Exec(`UPDATE jobs SET state = 'pending' WHERE state = 'running'`)
	if err != nil {
		return 0, 0, fmt.Errorf("resuming jobs: %w", err)
	}
	jobs, err = res.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("resuming jobs: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("resuming jobs: commit: %w", err)
	}
	return items, jobs, nil
}

// PurgeOldJobs deletes terminal jobs finished before the cutoff, items first,
// in one transaction. There is no ON DELETE CASCADE because foreign keys are
// off. It returns the number of jobs deleted.
func (c *Catalog) PurgeOldJobs(before int64) (int64, error) {
	tx, err := c.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("purging jobs: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM job_items WHERE job_id IN (
		SELECT id FROM jobs WHERE state IN ('done','failed')
		AND finished_at IS NOT NULL AND finished_at < ?)`, before); err != nil {
		return 0, fmt.Errorf("purging job items: %w", err)
	}

	res, err := tx.Exec(`DELETE FROM jobs WHERE state IN ('done','failed')
		AND finished_at IS NOT NULL AND finished_at < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("purging jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purging jobs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("purging jobs: commit: %w", err)
	}
	return n, nil
}

func scanJob(s fileScanner) (*Job, error) {
	var j Job
	var startedAt, finishedAt sql.NullInt64
	err := s.Scan(&j.ID, &j.Action, &j.FolderLabel, &j.State, &j.Total, &j.Done,
		&j.FailedCount, &j.Error, &j.Payload, &j.CreatedAt, &startedAt, &finishedAt)
	if err != nil {
		return nil, err
	}
	if startedAt.Valid {
		j.StartedAt = &startedAt.Int64
	}
	if finishedAt.Valid {
		j.FinishedAt = &finishedAt.Int64
	}
	return &j, nil
}
