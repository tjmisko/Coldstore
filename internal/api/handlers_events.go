package api

import (
	"net/http"
	"strconv"

	"github.com/tjmisko/coldstore/internal/catalog"
)

const (
	defaultEventLimit = 50
	maxEventLimit     = 500
)

// handleListEvents is the events table's first reader.
//
// Every warning this control plane learned to write -- folder_wedged,
// cold_storage_low, cold_storage_unmounted, folder_unavailable, bulk_eviction, restore_conflict,
// conflict, source_changed, source_vanished, archive_unconfirmed, job_failed,
// scan_failed, scan_incomplete
// -- was until now unreachable from outside SQLite. D3 requires the wedge
// condition to log ONE deduped warning; writing it into a table nothing reads
// satisfies the letter of D3 and none of its intent (design section 6.12).
//
// class=warning is not a nicety. A single enforcement pass writes hundreds of
// archive rows, so an unfiltered "last 10 events for this folder" would never
// once show the three things the list exists to show.
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()

	limit := defaultEventLimit
	if raw := v.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = min(parsed, maxEventLimit)
	}

	class := v.Get("class")
	if class != "" && class != catalog.EventClassWarning {
		writeError(w, http.StatusBadRequest, `class must be "warning"`)
		return
	}

	events, err := s.cat.ListEvents(v.Get("folder"), v.Get("type"), class, limit)
	if err != nil {
		s.logger.Error("listing events", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list events")
		return
	}
	if events == nil {
		events = []catalog.Event{}
	}

	writeJSON(w, http.StatusOK, events)
}
