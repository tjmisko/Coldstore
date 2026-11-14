package archive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tjmisko/coldstore/internal/ingest"
	"github.com/tjmisko/coldstore/internal/store"
)

const (
	maxFailureDetail = 1000
	finishedRunsKept = 16
	importActor      = "import"
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
	State          string          `json:"state"` // "running" | "done" | "failed"
	FilesSeen      int64           `json:"files_seen"`
	FilesCreated   int64           `json:"files_created"`
	FilesUnchanged int64           `json:"files_unchanged"`
	FilesOther     int64           `json:"files_other"` // copied, placed_beside
	BytesIngested  int64           `json:"bytes_ingested"`
	FailedCount    int64           `json:"failed_count"`
	Failed         []ImportFailure `json:"failed"` // first 1000; {path, error}
	SkippedCount   int64           `json:"skipped_count"`
	Error          string          `json:"error"` // run-level reason, "" normally
	StartedAt      time.Time       `json:"started_at"`
	FinishedAt     *time.Time      `json:"finished_at"`
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
	if err := s.checkImportDir(dir); err != nil {
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

func (s *Service) checkImportDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: %q is not absolute", ErrImportDir, dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrImportDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %q is not a directory", ErrImportDir, dir)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrImportDir, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(s.opts.Root)
	if err != nil {
		return fmt.Errorf("%w: resolving the archive root: %v", ErrImportDir, err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedDir)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %q is inside the archive root", ErrImportDir, dir)
	}
	return nil
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

func (s *Service) runImport(id int64, collection, dir string) {
	runCtx, cancelSource := context.WithCancel(s.ctx)
	defer cancelSource()

	skipped := func(path, reason string) {
		s.opts.Logger.Info("archive: import skipped a path", "path", path, "reason", reason)
		s.updateImport(id, func(st *ImportStatus) { st.SkippedCount++ })
	}
	source := ingest.ImportDir(dir, collection, skipped)

	runError := ""
	for arrival := range source.Arrivals(runCtx) {
		s.updateImport(id, func(st *ImportStatus) { st.FilesSeen++ })
		result, err := s.ingestWithRetry(runCtx, arrival)
		if err == nil {
			s.countOutcome(id, result)
			continue
		}
		s.countFailure(id, arrival, err)
		if errors.Is(err, syscall.ENOSPC) {
			runError = fmt.Sprintf("disk full while importing %s: %v", arrival.Path, err)
			break
		}
		if s.ctx.Err() != nil {
			runError = "interrupted: the service is shutting down"
			break
		}
	}
	cancelSource()
	if runError == "" && s.ctx.Err() != nil {
		runError = "interrupted: the service is shutting down"
	}
	s.finishImport(id, runError)
}

// ingestWithRetry makes one attempt plus up to three retries, waiting 1 s, 5 s
// and 25 s through the injected Sleep. A mismatched hash, a bad path and an
// undeclared collection are never retried.
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
	case errors.Is(err, store.ErrHashMismatch),
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

func (s *Service) countFailure(id int64, a ingest.Arrival, err error) {
	s.updateImport(id, func(st *ImportStatus) {
		st.FailedCount++
		if len(st.Failed) < maxFailureDetail {
			st.Failed = append(st.Failed, ImportFailure{Path: a.Path, Error: err.Error()})
		}
	})
}

func (s *Service) finishImport(id int64, runError string) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	status := s.imports[id]
	finished := s.now()
	status.FinishedAt = &finished
	status.State, status.Error = "done", runError
	if runError != "" {
		status.State = "failed"
	}
	s.importBusy = false

	s.importOrder = append(s.importOrder, id)
	for len(s.importOrder) > finishedRunsKept {
		delete(s.imports, s.importOrder[0])
		s.importOrder = s.importOrder[1:]
	}
}
