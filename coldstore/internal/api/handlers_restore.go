package api

import (
	"net/http"
	"strconv"
)

func (s *Server) handleRestoreFile(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid file id")
		return
	}

	file, err := s.cat.GetFileByID(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	if err := s.restorer.Restore(file); err != nil {
		s.logger.Error("restore failed", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Re-fetch to get updated status
	file, _ = s.cat.GetFileByID(id)
	writeJSON(w, http.StatusOK, file)
}
