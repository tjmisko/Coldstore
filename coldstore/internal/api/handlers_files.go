package api

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	q := catalog.FileQuery{
		Folder:    r.URL.Query().Get("folder"),
		Status:    catalog.FileStatus(r.URL.Query().Get("status")),
		Extension: r.URL.Query().Get("ext"),
		Search:    r.URL.Query().Get("q"),
		SortBy:    r.URL.Query().Get("sort"),
	}

	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		offset, err := strconv.Atoi(offsetStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid offset")
			return
		}
		q.Offset = offset
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		q.Limit = limit
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
		folderPath := s.folderPath(file.FolderLabel)
		if folderPath == "" {
			writeError(w, http.StatusNotFound, "folder not configured")
			return
		}
		filePath = filepath.Join(folderPath, file.RelativePath)
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

func (s *Server) folderPath(label string) string {
	for _, f := range s.cfg.Folders {
		if f.Label == label {
			return f.Path
		}
	}
	return ""
}
