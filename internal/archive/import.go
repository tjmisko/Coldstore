package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tjmisko/coldstore/internal/ingest"
	"github.com/tjmisko/coldstore/internal/store"
)

const (
	maxFailureDetail = 1000
	finishedRunsKept = 16
	importActor      = "import"

	// importReportsDir is under the archive root; one JSON-lines file per run.
	importReportsDir = "import-reports"

	importStateRunning   = "running"
	importStateDone      = "done"
	importStateFailed    = "failed"
	importStateCancelled = "cancelled"
)

var (
	// ErrImportUnknown means no run with that id is held in memory.
	ErrImportUnknown = errors.New("archive: no such import")
	// ErrImportNotRunning means the run has already finished.
	ErrImportNotRunning = errors.New("archive: the import is not running")
)

// importBackoff is the wait before each retry of a failed arrival: one attempt
// plus three retries.
var importBackoff = [...]time.Duration{1 * time.Second, 5 * time.Second, 25 * time.Second}

type ImportFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

type ImportStatus struct {
	ID             int64           `json:"id"`
	Collection     string          `json:"collection"`
	Dir            string          `json:"dir"`
	State          string          `json:"state"` // "running" | "done" | "failed" | "cancelled"
	FilesSeen      int64           `json:"files_seen"`
	FilesCreated   int64           `json:"files_created"`
	FilesUnchanged int64           `json:"files_unchanged"`
	FilesOther     int64           `json:"files_other"` // copied, placed_beside
	BytesIngested  int64           `json:"bytes_ingested"`
	FailedCount    int64           `json:"failed_count"`
	Failed         []ImportFailure `json:"failed"`        // first 1000; {path, error}
	SkippedCount   int64           `json:"skipped_count"` // benign skips only: symlinks, FIFOs, sockets, devices, non-UTF-8 names
	// UnreadableCount is how many paths could not be read (an unreadable
	// directory, a failed lstat or open, a read error). Each is also counted in
	// FailedCount and listed in Failed: an import with any is incomplete.
	UnreadableCount int64  `json:"unreadable_count"`
	ReportPath      string `json:"report_path"` // JSON-lines file of every failure; "" when nothing failed
	Error           string `json:"error"`       // run-level reason, "" normally

	cancel          context.CancelFunc
	cancelRequested bool
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at"`
}

// ImportStart validates the request and starts one import run in the
// background. One run at a time; the source directory is only ever read.
func (s *Service) ImportStart(collection, dir string) (int64, error) {
	if err := s.enter(); err != nil {
		return 0, err
	}
	started := false
	defer func() {
		if !started {
			s.leave()
		}
	}()

	if !s.collections[collection] {
		return 0, fmt.Errorf("importing into %q: %w", collection, ErrCollectionUnknown)
	}
	dir, err := s.checkImportDir(dir)
	if err != nil {
		return 0, err
	}

	s.importMu.Lock()
	if s.importBusy {
		s.importMu.Unlock()
		return 0, ErrImportRunning
	}
	s.importBusy = true
	s.importSeq++
	id := s.importSeq
	s.imports[id] = &ImportStatus{
		ID: id, Collection: collection, Dir: dir, State: "running",
		Failed: []ImportFailure{}, StartedAt: s.now(),
	}
	s.importMu.Unlock()

	started = true
	go func() {
		defer s.leave()
		s.runImport(id, collection, dir)
	}()
	return id, nil
}

// checkImportDir validates dir and returns it with symlinks resolved, so a
// symlinked root is imported rather than skipped as one non-directory entry.
// It refuses a directory that equals, contains or is inside the archive root.
func (s *Service) checkImportDir(dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%w: %q is not absolute", ErrImportDir, dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrImportDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %q is not a directory", ErrImportDir, dir)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrImportDir, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(s.opts.Root)
	if err != nil {
		return "", fmt.Errorf("%w: resolving the archive root: %v", ErrImportDir, err)
	}
	if isWithin(resolvedRoot, resolvedDir) {
		return "", fmt.Errorf("%w: %q is inside the archive root", ErrImportDir, dir)
	}
	if isWithin(resolvedDir, resolvedRoot) {
		return "", fmt.Errorf("%w: %q contains the archive root", ErrImportDir, dir)
	}
	return resolvedDir, nil
}

// isWithin reports whether path is parent or lies below it.
func isWithin(parent, path string) bool {
	rel, err := filepath.Rel(parent, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ImportStatus returns a copy of the status of run id. The last 16 finished
// runs are kept in memory.
func (s *Service) ImportStatus(id int64) (ImportStatus, bool) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	status, ok := s.imports[id]
	if !ok {
		return ImportStatus{}, false
	}
	out := *status
	out.cancel = nil
	out.Failed = append([]ImportFailure{}, status.Failed...)
	if status.FinishedAt != nil {
		finished := *status.FinishedAt
		out.FinishedAt = &finished
	}
	return out, true
}

func (s *Service) updateImport(id int64, fn func(*ImportStatus)) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	fn(s.imports[id])
}

// ImportCancel asks a running import to stop. The run finishes in state
// "cancelled"; imports are idempotent, so running it again resumes.
func (s *Service) ImportCancel(id int64) error {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	status, ok := s.imports[id]
	if !ok {
		return ErrImportUnknown
	}
	if status.State != importStateRunning {
		return ErrImportNotRunning
	}
	status.cancelRequested = true
	if status.cancel != nil {
		status.cancel()
	}
	return nil
}

// failureReport appends every failed or unreadable path to
// <root>/import-reports/<run>.jsonl as it happens, so the complete list
// survives a crash and is not bounded by memory. The file is created on the
// first failure.
type failureReport struct {
	mu     sync.Mutex
	dir    string
	name   string
	logger *slog.Logger
	file   *os.File
	path   string
	broken bool
}

type failureLine struct {
	Kind  string `json:"kind"` // "failed" | "unreadable"
	Path  string `json:"path"`
	Error string `json:"error"`
}

// record returns the report path once the file exists, "" otherwise.
func (r *failureReport) record(kind, path string, err error) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.broken {
		return ""
	}
	if r.file == nil {
		if mkErr := os.MkdirAll(r.dir, 0o755); mkErr != nil {
			r.fail(mkErr)
			return ""
		}
		full := filepath.Join(r.dir, r.name)
		f, openErr := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if openErr != nil {
			r.fail(openErr)
			return ""
		}
		r.file, r.path = f, full
	}
	line, _ := json.Marshal(failureLine{Kind: kind, Path: path, Error: err.Error()})
	if _, writeErr := r.file.Write(append(line, '\n')); writeErr != nil {
		r.fail(writeErr)
		return ""
	}
	return r.path
}

func (r *failureReport) fail(err error) {
	r.broken = true
	r.logger.Warn("archive: cannot write the import failure report", "dir", r.dir, "err", err)
}

func (r *failureReport) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return
	}
	if err := r.file.Sync(); err != nil {
		r.logger.Warn("archive: syncing the import failure report", "path", r.path, "err", err)
	}
	_ = r.file.Close()
	r.file = nil
}

func (s *Service) runImport(id int64, collection, dir string) {
	runCtx, cancelRun := context.WithCancel(s.ctx)
	defer cancelRun()
	s.updateImport(id, func(st *ImportStatus) {
		st.cancel = cancelRun
		if st.cancelRequested { // asked to cancel before the goroutine got here
			cancelRun()
		}
	})

	started := s.now()
	report := &failureReport{
		dir:    filepath.Join(s.opts.Root, importReportsDir),
		name:   fmt.Sprintf("%s-%d.jsonl", started.Format("20060102T150405Z"), id),
		logger: s.opts.Logger,
	}
	defer report.close()
	noteFailure := func(path string, err error, unreadable bool) {
		kind := "failed"
		if unreadable {
			kind = "unreadable"
		}
		reportPath := report.record(kind, path, err)
		s.updateImport(id, func(st *ImportStatus) {
			st.FailedCount++
			if unreadable {
				st.UnreadableCount++
			}
			if reportPath != "" {
				st.ReportPath = reportPath
			}
			if len(st.Failed) < maxFailureDetail {
				st.Failed = append(st.Failed, ImportFailure{Path: path, Error: err.Error()})
			}
		})
	}

	skipped := func(path, reason string) {
		s.opts.Logger.Info("archive: import skipped a path", "path", path, "reason", reason)
		s.updateImport(id, func(st *ImportStatus) { st.SkippedCount++ })
	}
	unreadable := func(path string, err error) {
		s.opts.Logger.Warn("archive: import could not read a path", "path", path, "err", err)
		noteFailure(path, err, true)
	}
	source := ingest.ImportDirReporting(dir, collection, skipped, unreadable)

	runError := ""
	for arrival := range source.Arrivals(runCtx) {
		if runCtx.Err() != nil {
			break
		}
		s.updateImport(id, func(st *ImportStatus) { st.FilesSeen++ })
		result, err := s.ingestWithRetry(runCtx, arrival)
		if err == nil {
			s.countOutcome(id, result)
			continue
		}
		if runCtx.Err() != nil && errors.Is(err, context.Canceled) {
			// Stopped by cancel or shutdown, not by this file.
			break
		}
		noteFailure(arrival.Path, err, errors.Is(err, ingest.ErrSourceRead))
		if s.ctx.Err() != nil {
			break // finishImport reports the shutdown
		}
		if archiveSideFailure(err) {
			runError = s.abortReason(id, arrival.Path, err)
			break
		}
	}
	cancelRun()
	s.finishImport(id, runError)
}

// abortReason is the run-level error for an archive-side failure that
// survived its retries, with how far the run got.
func (s *Service) abortReason(id int64, path string, err error) string {
	status, _ := s.ImportStatus(id)
	cause := "archive-side failure"
	if errors.Is(err, syscall.ENOSPC) {
		cause = "disk full"
	}
	return fmt.Sprintf("%s while importing %s: %v; run stopped after %d files seen (%d created, %d unchanged, %d other, %d failed); "+
		"fix the cause and run the import again, it resumes where this one stopped",
		cause, path, err, status.FilesSeen, status.FilesCreated, status.FilesUnchanged, status.FilesOther, status.FailedCount)
}

// archiveSideFailure reports whether err says something about the archive
// rather than about this one file. Such a failure survived its retries, so
// every later file would fail the same way, ~31 s at a time.
func archiveSideFailure(err error) bool {
	switch {
	case errors.Is(err, ingest.ErrSourceRead),
		errors.Is(err, ingest.ErrChangedWhileReading),
		errors.Is(err, ingest.ErrBadPath),
		errors.Is(err, ingest.ErrPathConflict),
		errors.Is(err, store.ErrHashMismatch):
		return false
	}
	return true
}

// ingestWithRetry makes one attempt plus up to three retries, waiting 1 s, 5 s
// and 25 s through the injected Sleep. A mismatched hash, a bad path, an
// undeclared collection and an unreadable source are never retried.
func (s *Service) ingestWithRetry(ctx context.Context, a ingest.Arrival) (ingest.Result, error) {
	for attempt := 0; ; attempt++ {
		result, err := s.Ingest(ctx, importActor, a)
		if err == nil {
			return result, nil
		}
		if attempt >= len(importBackoff) || !retryable(err) || ctx.Err() != nil {
			return ingest.Result{}, err
		}
		s.opts.Sleep(importBackoff[attempt])
	}
}

func retryable(err error) bool {
	switch {
	case errors.Is(err, ingest.ErrSourceRead),
		errors.Is(err, store.ErrHashMismatch),
		errors.Is(err, ingest.ErrBadPath),
		errors.Is(err, ErrCollectionUnknown),
		errors.Is(err, ErrClosed),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return false
	}
	return true
}

func (s *Service) countOutcome(id int64, result ingest.Result) {
	s.updateImport(id, func(st *ImportStatus) {
		switch result.Outcome {
		case ingest.OutcomeUnchanged:
			st.FilesUnchanged++
			return
		case ingest.OutcomeCreated:
			st.FilesCreated++
		default:
			st.FilesOther++
		}
		st.BytesIngested += result.Size
	})
}

func (s *Service) finishImport(id int64, runError string) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	status := s.imports[id]
	finished := s.now()
	status.FinishedAt = &finished
	status.State, status.Error = importStateDone, runError
	status.cancel = nil
	switch {
	case status.cancelRequested && s.ctx.Err() == nil:
		status.State = importStateCancelled
		status.Error = "cancelled by request; run the import again to resume"
	case runError != "":
		status.State = importStateFailed
	case s.ctx.Err() != nil:
		status.State = importStateFailed
		status.Error = "interrupted: the service is shutting down"
	}
	s.importBusy = false

	s.importOrder = append(s.importOrder, id)
	for len(s.importOrder) > finishedRunsKept {
		delete(s.imports, s.importOrder[0])
		s.importOrder = s.importOrder[1:]
	}
}
