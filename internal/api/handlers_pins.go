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

// archivedDuringPinWarning is returned when a pin lands on a file the daemon
// was already moving out. The pin still applies -- it becomes a latent
// instruction that takes effect on restore -- but the owner is told the truth
// rather than being shown a filled badge on a file that has left the device.
const archivedDuringPinWarning = "this file finished archiving while the pin was being applied; " +
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
		Warning: pinWarning(pinned, file.Status, folder),
	})
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

// pinWarning implements the two warnings of design section 6.5.
//
// The budget warning is a STATE test -- pinned bytes exceed the budget after
// the write -- and not a transition test. With a transition reading, pinning
// one more photo into an already-wedged folder would report no warning at all
// and look routine, which is the opposite of what D3 is for. The pin is
// applied either way: the owner is allowed to wedge their own folder as long
// as they are told.
func pinWarning(pinned bool, status catalog.FileStatus, folder folderStatus) *string {
	if !pinned {
		return nil
	}
	// The archiving warning wins when both apply, because it explains
	// something the owner is otherwise about to misread.
	if status == catalog.StatusArchived {
		w := archivedDuringPinWarning
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
