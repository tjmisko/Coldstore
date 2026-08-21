package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

const (
	// workerPollInterval is the safety net, not the mechanism. Every enqueue
	// pokes the wake channel, so a job normally starts within microseconds;
	// the ticker exists so a poke lost to a full channel, or a job requeued by
	// startup recovery before anything pokes anything, still costs at most one
	// interval.
	workerPollInterval = 2 * time.Second

	// maxConsecutiveJobErrors is the brake described in design section 5.3.
	// NextPendingJob hands back the lowest-id job that still has work, so a job
	// the worker cannot drive to a terminal state would otherwise be re-picked
	// on the very next iteration of the inner loop: an unthrottled spin, one
	// error line per iteration in the journal, and every later job starved
	// behind it. After this many consecutive failures the worker falls back to
	// the outer select and lets the ticker retry.
	maxConsecutiveJobErrors = 3
)

// Worker drains the job queue. There is exactly ONE, and it processes items
// serially: parallelism would only contend for the Mover mutex and thrash the
// disk head on a spinning pool.
//
// It shares the process-wide Mover with the quota tick and the API. That is not
// a convenience -- it is mechanism 2 of design section 5.8, without which a bulk
// restore and automatic enforcement can be moving the same file at the same
// time.
type Worker struct {
	cat     *catalog.Catalog
	mover   *Mover
	scanner *Scanner
	logger  *slog.Logger

	// wake is buffered at 1: the poke says "there is work", not "there are N
	// units of work", so a poke that arrives while one is already pending is
	// redundant and is dropped rather than blocking the HTTP handler.
	wake chan struct{}

	pollInterval time.Duration

	// claimItem is a seam. Production binds it to cat.ClaimJobItem; a test
	// binds it to a function that fails, because the FailJob edge of the state
	// machine cannot be reached any other way.
	claimItem func(jobID int64) (int64, error)
}

func NewWorker(cat *catalog.Catalog, mover *Mover, scanner *Scanner, logger *slog.Logger) *Worker {
	return &Worker{
		cat:          cat,
		mover:        mover,
		scanner:      scanner,
		logger:       logger,
		wake:         make(chan struct{}, 1),
		pollInterval: workerPollInterval,
		claimItem:    cat.ClaimJobItem,
	}
}

// Wake tells the worker a job has been enqueued. It never blocks: the channel
// is a signal, not a queue, and the queue itself is the jobs table.
func (w *Worker) Wake() {
	if w == nil {
		return
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run drains the queue whenever it is poked and once per poll interval, until
// the context is cancelled.
func (w *Worker) Run(ctx context.Context) {
	tick := time.NewTicker(w.pollInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("job worker stopping")
			return
		case <-w.wake:
		case <-tick.C:
		}
		w.RunOnce(ctx)
	}
}

// RunOnce drains the queue until nothing is left to do, the context is
// cancelled, or the consecutive-error brake trips. Run calls it; tests call it
// directly so they do not have to race a goroutine.
func (w *Worker) RunOnce(ctx context.Context) {
	consecutive := 0
	for ctx.Err() == nil {
		job, err := w.cat.NextPendingJob()
		if err != nil {
			w.logger.Error("reading job queue", "error", err)
			return
		}
		if job == nil {
			return
		}
		if err := w.runJob(ctx, job); err != nil {
			consecutive++
			if consecutive >= maxConsecutiveJobErrors {
				w.logger.Error("job worker backing off", "job", job.ID, "error", err)
				return
			}
			continue
		}
		consecutive = 0
	}
}

// runJob drives one job to a terminal state.
//
// It returns a non-nil error ONLY when the job could not be driven to a
// terminal state for an infrastructural reason. An item that fails is not an
// error: it is recorded against the item and the job ends "failed", which means
// "partial success with at least one failure", not "nothing happened".
func (w *Worker) runJob(ctx context.Context, job *catalog.Job) error {
	if err := w.cat.MarkJobRunning(job.ID, time.Now().Unix()); err != nil {
		return fmt.Errorf("marking job %d running: %w", job.ID, err)
	}

	if job.Action == catalog.JobActionScan {
		return w.runScanJob(ctx, job)
	}
	if job.Action == catalog.JobActionRename {
		return w.runRenameJob(ctx, job)
	}
	return w.runItemJob(ctx, job)
}

// runRenameJob executes the one synthetic, restartable rename item. Like a
// scan, it owns no job_items rows; unlike a scan, the payload is durable and
// each filesystem/catalog step is idempotent after a crash.
func (w *Worker) runRenameJob(ctx context.Context, job *catalog.Job) error {
	payload, err := catalog.DecodeRenamePayload(job.Payload)
	if err == nil {
		err = w.mover.RenamePath(ctx, job.FolderLabel, payload.From, payload.To)
	}
	if ctx.Err() != nil {
		w.logger.Info("rename job interrupted by shutdown", "job", job.ID)
		return nil
	}
	msg := ""
	if err != nil {
		msg = err.Error()
		w.logger.Error("rename job failed", "job", job.ID, "folder", job.FolderLabel, "error", err)
	}
	if cerr := w.cat.CompleteJobItemVirtual(job.ID, msg); cerr != nil {
		return fmt.Errorf("recording rename result for job %d: %w", job.ID, cerr)
	}
	return w.cat.FinishJob(job.ID, time.Now().Unix())
}

// runScanJob runs the single synthetic item of a scan job. A scan job has
// total = 1 and no job_items rows.
func (w *Worker) runScanJob(ctx context.Context, job *catalog.Job) error {
	err := w.scan(ctx, job.FolderLabel)

	// A scan cut short by shutdown is not a failure. Leaving the job in
	// "running" is what makes ResumeInterruptedJobs re-queue it on the next
	// start, which is exactly what the owner wants from a refresh they pressed
	// a second before the daemon went down.
	if ctx.Err() != nil {
		w.logger.Info("scan job interrupted by shutdown", "job", job.ID)
		return nil
	}

	msg := ""
	if err != nil {
		msg = err.Error()
		w.logger.Error("scan job failed", "job", job.ID, "folder", job.FolderLabel, "error", err)
	}
	if cerr := w.cat.CompleteJobItemVirtual(job.ID, msg); cerr != nil {
		return fmt.Errorf("recording scan result for job %d: %w", job.ID, cerr)
	}
	return w.cat.FinishJob(job.ID, time.Now().Unix())
}

func (w *Worker) scan(ctx context.Context, folderLabel string) error {
	// A scan JOB is the owner pressing Rescan; the periodic catalog tick does
	// not come through here. That distinction makes this the right place to
	// clear both derived-data failure caps: a repaired file, or a decoder that
	// has since been installed, must be retryable on request without making the
	// periodic tick retry genuinely unreadable files forever.
	reset, err := w.cat.ResetThumbnailFailures(folderLabel)
	if err != nil {
		w.logger.Warn("cannot clear thumbnail failures before a manual scan",
			"folder", folderLabel, "error", err)
	} else if reset > 0 {
		w.logger.Info("cleared thumbnail failures for a manual scan",
			"folder", folderLabel, "files", reset)
	}
	metadataReset, err := w.cat.ResetMetadataFailures(folderLabel)
	if err != nil {
		w.logger.Warn("cannot clear metadata failures before a manual scan",
			"folder", folderLabel, "error", err)
	} else if metadataReset > 0 {
		w.logger.Info("cleared metadata failures for a manual scan",
			"folder", folderLabel, "files", metadataReset)
	}

	if folderLabel == "" {
		return w.scanner.ScanAllContext(ctx)
	}
	fc, err := w.cat.FolderConfigByLabel(folderLabel)
	if err != nil {
		return fmt.Errorf("no folder named %q", folderLabel)
	}
	return w.scanner.ScanFolderContext(ctx, *fc)
}

func (w *Worker) runItemJob(ctx context.Context, job *catalog.Job) error {
	for {
		// Cancellation lands BETWEEN items, never inside a move. A ctx check
		// inside the copy converts a graceful shutdown into a torn move.
		if ctx.Err() != nil {
			w.logger.Info("job interrupted by shutdown, leaving it to recovery", "job", job.ID)
			return nil
		}

		fileID, err := w.claimItem(job.ID)
		if err != nil {
			// Terminal, not a silent return. Leaving the job "running" makes
			// NextPendingJob hand it straight back on the next iteration
			// (design section 5.3).
			w.logger.Error("claiming job item", "job", job.ID, "error", err)
			reason := "could not read the job queue: " + err.Error()
			if ferr := w.cat.FailJob(job.ID, time.Now().Unix(), reason); ferr != nil {
				w.logger.Error("cannot fail an abandoned job", "job", job.ID, "error", ferr)
			}
			w.cat.LogEvent("job_failed", job.FolderLabel, "",
				fmt.Sprintf("job %d abandoned: %v", job.ID, err))
			return err
		}
		if fileID == 0 {
			break // no pending items left
		}

		opErr := w.runItem(job.Action, fileID)

		msg := ""
		if opErr != nil {
			msg = opErr.Error()
		}
		if err := w.cat.CompleteJobItem(job.ID, fileID, msg); err != nil {
			w.logger.Error("recording job item result",
				"job", job.ID, "file", fileID, "error", err)
			reason := "could not record item results: " + err.Error()
			if ferr := w.cat.FailJob(job.ID, time.Now().Unix(), reason); ferr != nil {
				w.logger.Error("cannot fail an abandoned job", "job", job.ID, "error", ferr)
			}
			w.cat.LogEvent("job_failed", job.FolderLabel, "",
				fmt.Sprintf("job %d abandoned: %v", job.ID, err))
			return err
		}

		// Two conditions mean every remaining item would fail identically.
		// Fail the rest of the job fast rather than writing hundreds of
		// identical rows, one slow filesystem probe at a time.
		if errors.Is(opErr, ErrColdStorageFull) || errors.Is(opErr, ErrFolderUnavailable) {
			if err := w.cat.FailRemainingItems(job.ID, opErr.Error()); err != nil {
				return fmt.Errorf("failing the rest of job %d: %w", job.ID, err)
			}
			break
		}
	}

	return w.cat.FinishJob(job.ID, time.Now().Unix())
}

// runItem is the per-file body. Both entry points are idempotent no-op
// successes when the file is already where the item wants it (invariant 12),
// which is what makes an item resumed after a kill able to succeed rather than
// reporting a bookkeeping artefact as a failure (design section 5.6).
func (w *Worker) runItem(action string, fileID int64) error {
	var err error
	switch action {
	case catalog.JobActionRestore:
		_, err = w.mover.RestoreByID(fileID)
	case catalog.JobActionArchive:
		_, err = w.mover.ArchiveByID(fileID)
	default:
		return fmt.Errorf("unknown job action %q", action)
	}

	// An id the selection carried but the catalog no longer has is accepted at
	// enqueue and fails here, in the words the contract promises rather than
	// the catalog's internal phrasing (design section 6.8).
	if errors.Is(err, catalog.ErrNoSuchFile) {
		return errors.New("file not found")
	}
	return err
}
