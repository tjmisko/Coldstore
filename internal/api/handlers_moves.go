package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/daemon"
)

// handleRestoreFile moves one archived file back to its syncthing folder,
// synchronously. A single file on a LAN is a bounded wait; anything bulk goes
// through the job queue.
func (s *Server) handleRestoreFile(w http.ResponseWriter, r *http.Request) {
	id, ok := s.fileIDParam(w, r)
	if !ok {
		return
	}

	file, err := s.mover.RestoreByID(id)
	if err != nil {
		s.writeMoveError(w, "restore", id, err)
		return
	}
	writeJSON(w, http.StatusOK, file)
}

// handleArchiveFile moves one synced file to cold storage on demand. It
// bypasses the grace period and the budget -- the owner asked for it -- and it
// clears the pin.
func (s *Server) handleArchiveFile(w http.ResponseWriter, r *http.Request) {
	id, ok := s.fileIDParam(w, r)
	if !ok {
		return
	}

	file, err := s.mover.ArchiveByID(id)
	if err != nil {
		s.writeMoveError(w, "archive", id, err)
		return
	}
	writeJSON(w, http.StatusOK, file)
}

type resolveRequest struct {
	Keep string `json:"keep"`
}

// handleResolveConflict settles a section 4.1b conflict: a file has reappeared
// at the path of a row coldstore believed was archived, so there are two
// candidate copies of the same photo and coldstore will not guess.
func (s *Server) handleResolveConflict(w http.ResponseWriter, r *http.Request) {
	id, ok := s.fileIDParam(w, r)
	if !ok {
		return
	}

	var req resolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	file, err := s.mover.ResolveConflict(id, req.Keep)
	if err != nil {
		if errors.Is(err, daemon.ErrInvalidResolution) {
			writeError(w, http.StatusBadRequest, `keep must be "device" or "archive"`)
			return
		}
		s.writeMoveError(w, "resolve", id, err)
		return
	}
	writeJSON(w, http.StatusOK, file)
}

// fileIDParam parses {id} and answers the request itself if it is not a
// number. The bool is false when the response has already been written.
func (s *Server) fileIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid file id")
		return 0, false
	}
	return id, true
}

// writeMoveError maps the mover's typed errors onto the status codes the API
// contract promises. Everything the caller could act on is a 4xx; only a
// genuine failure of the move itself is a 500, which is what the old handler
// got wrong by returning a raw Go error string with a 500 for every case
// including "this file is already where you want it".
func (s *Server) writeMoveError(w http.ResponseWriter, action string, id int64, err error) {
	switch {
	case errors.Is(err, catalog.ErrNoSuchFile):
		writeError(w, http.StatusNotFound, "file not found")
	case errors.Is(err, daemon.ErrFileStateChanged):
		writeError(w, http.StatusConflict,
			"file state changed before the "+action+" could start")
	case errors.Is(err, daemon.ErrFolderUnavailable):
		writeError(w, http.StatusConflict, "folder is not currently available")
	case errors.Is(err, daemon.ErrDestinationExists):
		writeError(w, http.StatusConflict,
			"a newer copy of this file already exists in the folder; coldstore adopted it")
	case errors.Is(err, daemon.ErrSourceChanged):
		writeError(w, http.StatusConflict, "the source file changed during the move")
	case errors.Is(err, daemon.ErrColdStorageFull):
		writeError(w, http.StatusInsufficientStorage, "cold storage below 5% free")
	default:
		s.logger.Error(action+" failed", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, action+" failed")
	}
}
