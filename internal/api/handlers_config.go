package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/daemon"
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

// updateFolderRequest carries the folder fields the UI owns (D1). The pointers
// are mandatory: with value types a PATCH-style request that only changes the
// budget decodes "enabled" as false and silently stops the folder being
// managed at all, which under D1 also stops it being enforced.
//
// syncthing_id and syncthing_path are deliberately absent. They are
// infrastructure, declared in coldstore.toml, and the UI may not write them.
type updateFolderRequest struct {
	BudgetBytes  *int64  `json:"budget_bytes"`
	GraceMinutes *int    `json:"grace_minutes"`
	Enabled      *bool   `json:"enabled"`
	DisplayName  *string `json:"display_name"`
}

func (s *Server) handleUpdateFolder(w http.ResponseWriter, r *http.Request) {
	label := r.PathValue("label")

	var req updateFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.BudgetBytes != nil && *req.BudgetBytes <= 0 {
		writeError(w, http.StatusBadRequest, "budget_bytes must be positive")
		return
	}
	if req.GraceMinutes != nil && *req.GraceMinutes < 0 {
		writeError(w, http.StatusBadRequest, "grace_minutes cannot be negative")
		return
	}
	if req.DisplayName != nil {
		clean := strings.TrimSpace(*req.DisplayName)
		if utf8.RuneCountInString(clean) > 128 || strings.IndexFunc(clean, unicode.IsControl) >= 0 {
			writeError(w, http.StatusBadRequest, "display_name must be at most 128 characters without controls")
			return
		}
		req.DisplayName = &clean
	}

	found, err := s.cat.UpdateFolderSettings(label, req.BudgetBytes, req.GraceMinutes,
		req.Enabled, req.DisplayName)
	if err != nil {
		s.logger.Error("updating folder policy", "folder", label, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update folder")
		return
	}
	// Only TOML reconciliation may create a folder row, so an unknown label is
	// a 404 rather than a phantom folder the daemon would never enforce.
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no folder named %q", label))
		return
	}

	fc, err := s.cat.FolderConfigByLabel(label)
	if err != nil {
		s.logger.Error("reading folder policy after update", "folder", label, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update folder")
		return
	}

	writeJSON(w, http.StatusOK, fc)
}

// folderMustExist resolves a label to a folder_configs row and answers the
// request itself when there is none.
//
// Only TOML reconciliation may bring a folder into existence (D1). Without
// this the persistent-path collection was the one door left open through which
// a phantom folder could be conjured -- POST to /folders/Typo/persistent
// returned 201 and created a glob for a folder the daemon would never enforce
// -- which is exactly what PUT /folders/{label} was tightened to prevent.
func (s *Server) folderMustExist(w http.ResponseWriter, label string) bool {
	_, err := s.cat.FolderConfigByLabel(label)
	if err == nil {
		return true
	}
	if errors.Is(err, catalog.ErrNoSuchFolder) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no folder named %q", label))
		return false
	}
	s.logger.Error("resolving folder", "folder", label, "error", err)
	writeError(w, http.StatusInternalServerError, "failed to read folder")
	return false
}

func (s *Server) handleListPersistent(w http.ResponseWriter, r *http.Request) {
	label := r.PathValue("label")
	if !s.folderMustExist(w, label) {
		return
	}

	paths, err := s.cat.ListPersistentPaths(label)
	if err != nil {
		s.logger.Error("listing persistent paths", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list persistent paths")
		return
	}

	if paths == nil {
		paths = []catalog.PersistentPath{}
	}

	// match_count. A pattern that protects nothing looks identical to one that
	// protects everything, so the editor has to say which the owner just typed
	// (design section 4.1a). One pass over the folder's on-device paths, shared
	// by every pattern, rather than a query each.
	relPaths, err := s.cat.OnDeviceRelativePaths(label)
	if err != nil {
		s.logger.Error("counting persistent matches", "folder", label, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list persistent paths")
		return
	}
	for i := range paths {
		pattern := []string{paths[i].Pattern}
		var count int64
		for _, rel := range relPaths {
			if daemon.IsPersistent(rel, pattern) {
				count++
			}
		}
		paths[i].MatchCount = count
	}

	writeJSON(w, http.StatusOK, paths)
}

type addPersistentRequest struct {
	Pattern string `json:"pattern"`
}

func (s *Server) handleAddPersistent(w http.ResponseWriter, r *http.Request) {
	label := r.PathValue("label")
	if !s.folderMustExist(w, label) {
		return
	}

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
	if !s.folderMustExist(w, r.PathValue("label")) {
		return
	}

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
