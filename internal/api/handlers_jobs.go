package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/tjmisko/coldstore/internal/catalog"
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
	case catalog.JobActionRestore, catalog.JobActionArchive:
		if !s.validateSelection(w, req.IDs) {
			return
		}
	case catalog.JobActionScan:
		// ids are ignored for a scan; folder scopes it, "" means all folders.
		req.IDs = nil
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown action %q", req.Action))
		return
	}

	if req.Action == catalog.JobActionRestore && !req.Force {
		if !s.restorePreflight(w, req.IDs) {
			return
		}
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
		evictions, _, err := s.cat.CountUnpinnedSyncedOldestUpTo(label, overage)
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

// enqueue commits the job and pokes the worker. The poke is what makes a bulk
// action feel immediate; without it the job waits for the worker's 2 s ticker.
func (s *Server) enqueue(w http.ResponseWriter, action, folder string, ids []int64) {
	jobID, err := s.cat.CreateJob(action, folder, ids)
	if err != nil {
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
