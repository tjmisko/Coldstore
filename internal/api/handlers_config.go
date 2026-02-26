package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	configs, err := s.cat.ListFolderConfigs()
	if err != nil {
		s.logger.Error("listing folders", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list folders")
		return
	}

	if configs == nil {
		configs = []catalog.FolderConfigRecord{}
	}

	writeJSON(w, http.StatusOK, configs)
}

type updateFolderRequest struct {
	SyncthingID   string `json:"syncthing_id"`
	SyncthingPath string `json:"syncthing_path"`
	BudgetBytes   int64  `json:"budget_bytes"`
	GraceMinutes  int    `json:"grace_minutes"`
	Enabled       bool   `json:"enabled"`
}

func (s *Server) handleUpdateFolder(w http.ResponseWriter, r *http.Request) {
	label := r.PathValue("label")

	var req updateFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	fc := &catalog.FolderConfigRecord{
		FolderLabel:   label,
		SyncthingID:   req.SyncthingID,
		SyncthingPath: req.SyncthingPath,
		BudgetBytes:   req.BudgetBytes,
		GraceMinutes:  req.GraceMinutes,
		Enabled:       req.Enabled,
	}

	if err := s.cat.UpsertFolderConfig(fc); err != nil {
		s.logger.Error("updating folder config", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update folder")
		return
	}

	writeJSON(w, http.StatusOK, fc)
}

func (s *Server) handleListPersistent(w http.ResponseWriter, r *http.Request) {
	label := r.PathValue("label")

	paths, err := s.cat.ListPersistentPaths(label)
	if err != nil {
		s.logger.Error("listing persistent paths", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list persistent paths")
		return
	}

	if paths == nil {
		paths = []catalog.PersistentPath{}
	}

	writeJSON(w, http.StatusOK, paths)
}

type addPersistentRequest struct {
	Pattern string `json:"pattern"`
}

func (s *Server) handleAddPersistent(w http.ResponseWriter, r *http.Request) {
	label := r.PathValue("label")

	var req addPersistentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Pattern == "" {
		writeError(w, http.StatusBadRequest, "pattern is required")
		return
	}

	if err := s.cat.AddPersistentPath(label, req.Pattern); err != nil {
		s.logger.Error("adding persistent path", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to add persistent path")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}

func (s *Server) handleDeletePersistent(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	if err := s.cat.DeletePersistentPath(id); err != nil {
		s.logger.Error("deleting persistent path", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete persistent path")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
