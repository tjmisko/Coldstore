package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/tjmisko/coldstore/internal/catalog"
)

// maxBulkPinIDs caps one bulk pin request. It is deliberately smaller than the
// job-queue cap: a pin writes one row per id and returns one folder block per
// folder, so the request is cheap, but it is still a single SQL statement per
// 500 ids and there is no progress reporting to hide a long one behind.
const maxBulkPinIDs = 5000

// The two archived-file warnings. Both say the pin is latent, because in both
// cases the owner is looking at a filled badge on a file that is not on the
// device; they differ in what they claim happened, and only one of them may
// claim a race.
//
// archivedDuringPinWarning is for a pin that lost a race it never knew it was
// in: Mover.SetPinned takes the mover mutex, so a pin issued against a file the
// daemon is already offloading blocks until that move finishes and returns a
// row that was synced when the request arrived and archived when it left.
//
// archivedPinWarning is for the ordinary case -- pinning something that was
// already in cold storage, which is legal and expected (design section 6.5).
// Telling that owner their file "finished archiving while the pin was being
// applied" describes an event that did not happen, and sends them looking for
// a daemon that just took a file they were trying to keep.
const archivedDuringPinWarning = "this file finished archiving while the pin was being applied; " +
	"the pin will take effect when you restore it"

const archivedPinWarning = "this file is in cold storage; " +
	"the pin will take effect when you restore it"

type pinResponse struct {
	File   catalog.FileRecord `json:"file"`
	Folder folderStatus       `json:"folder"`
	// Warning is null, never omitted (design section 6.0 rule 1).
	Warning *string `json:"warning"`
}

type bulkPinRequest struct {
	IDs    []int64 `json:"ids"`
	Pinned bool    `json:"pinned"`
}

type bulkPinResponse struct {
	Updated int64          `json:"updated"`
	Folders []folderStatus `json:"folders"`
	Warning *string        `json:"warning"`
}

func (s *Server) handlePinFile(w http.ResponseWriter, r *http.Request)   { s.setPin(w, r, true) }
func (s *Server) handleUnpinFile(w http.ResponseWriter, r *http.Request) { s.setPin(w, r, false) }

// setPin pins or unpins one file. It moves nothing: this is "pin in place",
// and it is legal in every status, including archived and missing.
func (s *Server) setPin(w http.ResponseWriter, r *http.Request, pinned bool) {
	id, ok := s.fileIDParam(w, r)
	if !ok {
		return
	}

	// The status BEFORE the write is the only way to tell the two archived
	// warnings apart. Reading it outside the mover mutex is not a weakness of
	// the check, it is the check: the window this read leaves open is exactly
	// the window in which a concurrent offload can overtake the request.
	statusBefore, ok := s.statusBeforePin(w, id)
	if !ok {
		return
	}

	// Through the mover, so the write cannot commit while a move of this file
	// is in flight. If the daemon was already offloading it, this blocks until
	// the move finishes and then writes against the settled row (D2).
	file, err := s.mover.SetPinned(id, pinned)
	if err != nil {
		if errors.Is(err, catalog.ErrNoSuchFile) {
			writeError(w, http.StatusNotFound, "file not found")
			return
		}
		s.logger.Error("setting pin", "id", id, "pinned", pinned, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to set pin")
		return
	}

	folder, err := s.folderStatusForLabel(file.FolderLabel)
	if err != nil {
		s.logger.Error("reading folder after pin", "folder", file.FolderLabel, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read folder status")
		return
	}

	writeJSON(w, http.StatusOK, pinResponse{
		File:    *file,
		Folder:  folder,
		Warning: pinWarning(pinned, statusBefore, file.Status, folder),
	})
}

// statusBeforePin reads the row the caller is about to pin. It answers the
// request itself -- 404 for an id that does not exist, 500 for a database that
// cannot be read -- and reports false when it has.
func (s *Server) statusBeforePin(w http.ResponseWriter, id int64) (catalog.FileStatus, bool) {
	file, err := s.cat.GetFileByID(id)
	if err == nil {
		return file.Status, true
	}
	if errors.Is(err, catalog.ErrNoSuchFile) {
		writeError(w, http.StatusNotFound, "file not found")
		return "", false
	}
	s.logger.Error("reading file before pin", "id", id, "error", err)
	writeError(w, http.StatusInternalServerError, "failed to read file")
	return "", false
}

// handleBulkPin is one request, one mover-mutex acquisition, one chunked
// update and one recomputed folder block per folder touched. The alternative
// -- the UI looping over 400 selected files -- is 400 round trips and 800
// aggregates, all serialised behind a single database connection.
func (s *Server) handleBulkPin(w http.ResponseWriter, r *http.Request) {
	var req bulkPinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids must not be empty")
		return
	}
	if len(req.IDs) > maxBulkPinIDs {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("at most %d ids per request", maxBulkPinIDs))
		return
	}

	// Which folders are affected has to be read BEFORE the write only in the
	// sense that the rows must exist; the labels themselves do not change, so
	// reading them after keeps this to one query either way.
	labels, err := s.cat.FolderLabelsForIDs(req.IDs)
	if err != nil {
		s.logger.Error("resolving folders for bulk pin", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to set pins")
		return
	}

	updated, err := s.mover.SetPinnedBulk(req.IDs, req.Pinned)
	if err != nil {
		s.logger.Error("bulk pin", "count", len(req.IDs), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to set pins")
		return
	}

	folders := make([]folderStatus, 0, len(labels))
	var warning *string
	for _, label := range labels {
		folder, err := s.folderStatusForLabel(label)
		if err != nil {
			s.logger.Error("reading folder after bulk pin", "folder", label, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to read folder status")
			return
		}
		folders = append(folders, folder)
		if warning == nil && req.Pinned && folder.Wedged {
			warning = wedgeWarning(folder)
		}
	}

	writeJSON(w, http.StatusOK, bulkPinResponse{
		Updated: updated,
		Folders: folders,
		Warning: warning,
	})
}

// pinWarning implements the warnings of design section 6.5.
//
// The budget warning is a STATE test -- pinned bytes exceed the budget after
// the write -- and not a transition test. With a transition reading, pinning
// one more photo into an already-wedged folder would report no warning at all
// and look routine, which is the opposite of what D3 is for. The pin is
// applied either way: the owner is allowed to wedge their own folder as long
// as they are told.
//
// The archived warning, by contrast, IS a transition test, which is what
// before is for. Both wordings say the pin is latent; only a row that arrived
// un-archived and left archived may say the archive happened during the
// request.
func pinWarning(pinned bool, before, after catalog.FileStatus, folder folderStatus) *string {
	if !pinned {
		return nil
	}
	// An archived warning wins when both apply, because it explains something
	// the owner is otherwise about to misread.
	if after == catalog.StatusArchived {
		w := archivedPinWarning
		if before != catalog.StatusArchived {
			w = archivedDuringPinWarning
		}
		return &w
	}
	if folder.Wedged {
		return wedgeWarning(folder)
	}
	return nil
}

func wedgeWarning(folder folderStatus) *string {
	w := fmt.Sprintf("%s pinned exceeds the %s budget - nothing left to offload",
		catalog.HumanSI(folder.PinnedBytes), catalog.HumanSI(folder.BudgetBytes))
	return &w
}
