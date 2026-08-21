package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/daemon"
)

type renameRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type renameResponse struct {
	jobCreated
	Warnings []string `json:"warnings"`
}

func (s *Server) handleRenameFolderPath(w http.ResponseWriter, r *http.Request) {
	label := r.PathValue("label")
	if !s.folderMustExist(w, label) {
		return
	}
	var req renameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var jobID int64
	var warnings []string
	err := s.mover.WithRenamePreflight(r.Context(), label, req.From, req.To,
		func(foundWarnings []string) error {
			var err error
			jobID, err = s.cat.CreateRenameJob(label, req.From, req.To)
			warnings = foundWarnings
			return err
		})
	if err != nil {
		s.writeRenameError(w, label, err)
		return
	}
	job, _, err := s.cat.GetJob(jobID, 1)
	if err != nil {
		s.logger.Error("reading rename job after enqueue", "job", jobID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create rename job")
		return
	}
	s.worker.Wake()
	writeJSON(w, http.StatusAccepted, renameResponse{
		jobCreated: jobCreated{JobID: job.ID, Action: job.Action, State: job.State, Total: job.Total},
		Warnings:   warnings,
	})
}

func (s *Server) writeRenameError(w http.ResponseWriter, label string, err error) {
	switch {
	case errors.Is(err, daemon.ErrInvalidRenamePath):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, daemon.ErrRenameSource), errors.Is(err, catalog.ErrNoSuchFile):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, daemon.ErrRenameConflict),
		errors.Is(err, daemon.ErrRenameTransient),
		errors.Is(err, daemon.ErrRenamePair),
		errors.Is(err, catalog.ErrRenameConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		s.logger.Error("preflighting rename", "folder", label, "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to rename path in %q", label))
	}
}
