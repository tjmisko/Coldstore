package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tjmisko/coldstore/internal/catalog"
)

// maxInlineIDs caps the ids parameter of GET /files. A selection larger than
// this is what GET /files/ids and POST /jobs are for; rendering it as file
// records is a page nobody asked for.
const maxInlineIDs = 500

// maxPageLimit is the ceiling on GET /files?limit=. Without it one request
// materialises the entire catalog -- 20,000 records on a modest camera roll,
// 50,000 on a real one -- through a single-connection SQLite handle that the
// quota tick and the job worker are also queued behind.
//
// The ids parameter is deliberately NOT paginated (design section 6.1); it is
// bounded by maxInlineIDs instead.
const maxPageLimit = 500

// parseFileFilters reads the filter half of a file query -- everything both
// GET /files and GET /files/ids understand. Sorting and pagination are the
// listing endpoint's business and are not read here.
//
// It reports false when it has already answered the request.
func parseFileFilters(w http.ResponseWriter, r *http.Request, idCap int) (catalog.FileQuery, bool) {
	v := r.URL.Query()
	q := catalog.FileQuery{
		Folder:    v.Get("folder"),
		Status:    catalog.FileStatus(v.Get("status")),
		Extension: v.Get("ext"),
		Search:    v.Get("q"),
		PinSource: v.Get("pin_source"),
	}

	switch v.Get("pinned") {
	case "":
	case "1":
		pinned := true
		q.Pinned = &pinned
	case "0":
		pinned := false
		q.Pinned = &pinned
	default:
		writeError(w, http.StatusBadRequest, `pinned must be "1" or "0"`)
		return q, false
	}

	if raw := v.Get("ids"); raw != "" {
		ids, err := parseIDList(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid ids")
			return q, false
		}
		if len(ids) > idCap {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d ids per request", idCap))
			return q, false
		}
		q.IDs = ids
	}

	return q, true
}

func parseIDList(raw string) ([]int64, error) {
	parts := strings.Split(raw, ",")
	ids := make([]int64, 0, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	q, ok := parseFileFilters(w, r, maxInlineIDs)
	if !ok {
		return
	}
	q.SortBy = r.URL.Query().Get("sort")

	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		offset, err := strconv.Atoi(offsetStr)
		if err != nil || offset < 0 {
			writeError(w, http.StatusBadRequest, "invalid offset")
			return
		}
		q.Offset = offset
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		// A non-positive limit is a client bug, not a request for the default:
		// silently substituting 50 hides it and returns a page the caller did
		// not ask for.
		if err != nil || limit <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		q.Limit = min(limit, maxPageLimit)
	}

	files, err := s.cat.ListFiles(q)
	if err != nil {
		s.logger.Error("listing files", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list files")
		return
	}

	if files == nil {
		files = []catalog.FileRecord{}
	}

	writeJSON(w, http.StatusOK, files)
}

// fileIDsResponse is how the UI turns "select all in this folder" into a
// selection it can hand to POST /jobs without paging through every record.
type fileIDsResponse struct {
	IDs       []int64 `json:"ids"`
	Total     int64   `json:"total"`
	Truncated bool    `json:"truncated"`
}

func (s *Server) handleListFileIDs(w http.ResponseWriter, r *http.Request) {
	// maxJobIDs, not a second constant: an id list this endpoint returns must
	// be one POST /jobs will accept.
	q, ok := parseFileFilters(w, r, maxJobIDs)
	if !ok {
		return
	}

	ids, total, err := s.cat.ListFileIDs(q, maxJobIDs)
	if err != nil {
		s.logger.Error("listing file ids", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list file ids")
		return
	}
	if ids == nil {
		ids = []int64{}
	}

	writeJSON(w, http.StatusOK, fileIDsResponse{
		IDs:       ids,
		Total:     total,
		Truncated: total > int64(len(ids)),
	})
}

func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
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

	var filePath string
	switch file.Status {
	case catalog.StatusSynced:
		// cat.FolderPath, not cfg.Folders: it resolves disabled and orphaned
		// folders too, which is what keeps an orphan's files browsable (3.4).
		folderPath, err := s.cat.FolderPath(file.FolderLabel)
		if err != nil {
			s.logger.Warn("cannot resolve folder for download",
				"folder", file.FolderLabel, "error", err)
			writeError(w, http.StatusConflict,
				fmt.Sprintf("folder %q is not currently available", file.FolderLabel))
			return
		}
		filePath = filepath.Join(folderPath, file.RelativePath)
		// http.ServeFile would answer a vanished path with a 404 and an HTML
		// body. 404 is reserved for "no such file id".
		if _, err := os.Stat(filePath); err != nil {
			writeError(w, http.StatusConflict,
				fmt.Sprintf("folder %q is not currently available", file.FolderLabel))
			return
		}
	case catalog.StatusArchived:
		filePath = filepath.Join(s.cfg.Storage.ColdStoragePath, "archive", file.FolderLabel, file.RelativePath)
	default:
		writeError(w, http.StatusConflict, fmt.Sprintf("file status is %s", file.Status))
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, file.Filename))
	http.ServeFile(w, r, filePath)
}

func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	// URL: /api/v1/thumbnails/{folder}/{relative/path}.thumb.jpg
	thumbPath := strings.TrimPrefix(r.URL.Path, "/api/v1/thumbnails/")
	if thumbPath == "" {
		writeError(w, http.StatusBadRequest, "missing thumbnail path")
		return
	}

	fullPath := filepath.Join(s.cfg.Storage.ColdStoragePath, "thumbnails", thumbPath)
	http.ServeFile(w, r, fullPath)
}
