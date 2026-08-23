package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/daemon"
)

// maxJobIDs caps a single selection. It is ONE constant, used by both
// GET /api/v1/files/ids and POST /api/v1/jobs, deliberately: a selection the
// ids endpoint hands the UI must be a selection the jobs endpoint will accept,
// and two constants that drift apart produce a "select all" the user cannot
// then act on.
const maxJobIDs = 20000

// jobFailureLimit truncates the failure detail array so a 20000-item job cannot
// answer GET /jobs/{id} with a multi-megabyte payload. failed_count stays
// authoritative.
const jobFailureLimit = 100

// defaultJobListLimit and maxJobListLimit bound GET /api/v1/jobs.
const (
	defaultJobListLimit = 20
	maxJobListLimit     = 100
)

type jobRequest struct {
	Action string  `json:"action"`
	IDs    []int64 `json:"ids"`
	Folder string  `json:"folder"`
	Force  bool    `json:"force"`
}

type jobCreated struct {
	JobID  int64  `json:"job_id"`
	Action string `json:"action"`
	State  string `json:"state"`
	Total  int    `json:"total"`
}

// jobResponse is GET /jobs/{id}. It cannot be catalog.Job: the row's counter
// column is `failed` and this response's `failed` is the failure detail array,
// so the two cannot be the same struct field (design section 6.9).
type jobResponse struct {
	catalog.Job
	Failed []catalog.JobFailure `json:"failed"`
}

// budgetPreflightError is the 409 body from POST /jobs when a restore would
// blow a folder budget. It carries what the UI needs to write a truthful
// confirm dialog rather than a generic "are you sure" (design section 5.10).
type budgetPreflightError struct {
	Error              string `json:"error"`
	Folder             string `json:"folder"`
	OverageBytes       int64  `json:"overage_bytes"`
	EstimatedEvictions int    `json:"estimated_evictions"`
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	switch req.Action {
	case catalog.JobActionRestore, catalog.JobActionArchive, catalog.JobActionTrash,
		catalog.JobActionUntrash, catalog.JobActionDelete:
		if !s.validateSelection(w, req.IDs) {
			return
		}
		err := s.mover.WithPairLock(r.Context(), func(context.Context) error {
			expanded, err := s.cat.ExpandPairGroups(req.IDs)
			if err != nil {
				return err
			}
			req.IDs = expanded
			if (req.Action == catalog.JobActionRestore || req.Action == catalog.JobActionUntrash) && !req.Force {
				if !s.restorePreflight(w, req.IDs) {
					return nil // restorePreflight already answered the request
				}
			}
			s.enqueue(w, req.Action, "", req.IDs)
			return nil
		})
		if err != nil {
			s.logger.Error("expanding paired job selection", "action", req.Action, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to create job")
		}
		return
	case catalog.JobActionScan:
		// ids are ignored for a scan; folder scopes it, "" means all folders.
		req.IDs = nil
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown action %q", req.Action))
		return
	}

	folder := ""
	if req.Action == catalog.JobActionScan {
		folder = req.Folder
	}
	s.enqueue(w, req.Action, folder, req.IDs)
}

// validateSelection enforces design section 6.8's two id rules. The empty case
// is a 400 at this layer on purpose: catalog.CreateJob would happily write a
// job with total = 0 and no items, and NextPendingJob would then hand that job
// back to the worker forever.
func (s *Server) validateSelection(w http.ResponseWriter, ids []int64) bool {
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "ids must not be empty")
		return false
	}
	if len(ids) > maxJobIDs {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d ids per job", maxJobIDs))
		return false
	}
	return true
}

// restorePreflight refuses a restore that would push a folder over its budget,
// because the daemon's answer to that is to evict a comparable volume of files
// the owner never selected -- and D3's wedge banner is scoped so that it never
// fires for that case (design section 5.10).
//
// It reports false when it has already answered the request.
func (s *Server) restorePreflight(w http.ResponseWriter, ids []int64) bool {
	sizes, err := s.cat.FolderSizesForIDs(ids)
	if err != nil {
		s.logger.Error("restore pre-flight: sizing the selection", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create job")
		return false
	}

	// Map order is randomised; a request that touches two over-budget folders
	// must name the same one every time it is retried.
	labels := make([]string, 0, len(sizes))
	for label := range sizes {
		labels = append(labels, label)
	}
	sort.Strings(labels)

	for _, label := range labels {
		add := sizes[label]
		fc, err := s.cat.FolderConfigByLabel(label)
		if err != nil {
			// No policy row means nothing to exceed. The item itself will fail
			// in the worker with a folder-unavailable error if that is wrong.
			continue
		}
		if fc.BudgetBytes <= 0 {
			continue // an unset budget is no cap at all (D1)
		}

		total, _, err := s.cat.FolderSizes(label)
		if err != nil {
			s.logger.Error("restore pre-flight: folder sizes", "folder", label, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to create job")
			return false
		}
		if total+add <= fc.BudgetBytes {
			continue
		}

		overage := total + add - fc.BudgetBytes
		evictions, err := s.estimateEvictions(label, overage)
		if err != nil {
			s.logger.Error("restore pre-flight: counting evictions", "folder", label, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to create job")
			return false
		}

		writeJSON(w, http.StatusConflict, budgetPreflightError{
			Error:              "restoring these files would exceed the folder budget",
			Folder:             label,
			OverageBytes:       overage,
			EstimatedEvictions: evictions,
		})
		return false
	}
	return true
}

// estimateEvictions counts the files the offloader would really archive out of
// a folder to claw back overage bytes.
//
// "Really" is the whole point, and it is why this is not a bare row count. A
// candidate covered by a persistent glob is never evicted, no matter how old
// it is or how far over budget the folder goes (design section 4.1a). Counting
// it here puts a number in front of the owner -- "coldstore will archive about
// N other files, and Syncthing will delete them from your devices" -- for a
// consequence that cannot occur, which is the same species of untruth section
// 5.10 added this dialog to prevent.
func (s *Server) estimateEvictions(label string, overage int64) (int, error) {
	patterns, err := s.persistentPatterns(label)
	if err != nil {
		return 0, err
	}
	count, _, err := s.cat.CountEvictionsUpTo(label, overage, func(relPath string) bool {
		return daemon.IsPersistent(relPath, patterns)
	})
	return count, err
}

// persistentPatterns reads a folder's persistent globs from the database. The
// database is the only source: the TOML globs were copied into
// persistent_paths when the folder was seeded (design section 3.2), so merging
// cfg here would resurrect a glob the owner has since deleted.
func (s *Server) persistentPatterns(label string) ([]string, error) {
	paths, err := s.cat.ListPersistentPaths(label)
	if err != nil {
		return nil, err
	}
	patterns := make([]string, 0, len(paths))
	for _, p := range paths {
		patterns = append(patterns, p.Pattern)
	}
	return patterns, nil
}

// enqueue commits the job and pokes the worker. The poke is what makes a bulk
// action feel immediate; without it the job waits for the worker's 2 s ticker.
func (s *Server) enqueue(w http.ResponseWriter, action, folder string, ids []int64) {
	var jobID int64
	var err error
	if action == catalog.JobActionRestore || action == catalog.JobActionUntrash {
		jobID, err = s.cat.CreatePreflightedMoveJob(action, folder, ids)
	} else {
		jobID, err = s.cat.CreateJob(action, folder, ids)
	}
	if err != nil {
		if errors.Is(err, catalog.ErrPairSelectionChanged) {
			writeError(w, http.StatusConflict, "pair membership changed; retry the restore")
			return
		}
		s.logger.Error("creating job", "action", action, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create job")
		return
	}

	// Read the row back for `total`: duplicate ids collapse on the composite
	// primary key, so the deduplicated count is only knowable after the insert.
	job, _, err := s.cat.GetJob(jobID, 1)
	if err != nil {
		s.logger.Error("reading job after creating it", "job", jobID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create job")
		return
	}

	s.worker.Wake()

	writeJSON(w, http.StatusAccepted, jobCreated{
		JobID:  job.ID,
		Action: job.Action,
		State:  job.State,
		Total:  job.Total,
	})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}

	job, failures, err := s.cat.GetJob(id, jobFailureLimit)
	if err != nil {
		if errors.Is(err, catalog.ErrNoSuchJob) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		s.logger.Error("reading job", "job", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read job")
		return
	}

	if failures == nil {
		failures = []catalog.JobFailure{}
	}
	writeJSON(w, http.StatusOK, jobResponse{Job: *job, Failed: failures})
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	limit := defaultJobListLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = min(parsed, maxJobListLimit)
	}

	jobs, err := s.cat.ListJobs(limit)
	if err != nil {
		s.logger.Error("listing jobs", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}
	if jobs == nil {
		jobs = []catalog.Job{}
	}
	writeJSON(w, http.StatusOK, jobs)
}
