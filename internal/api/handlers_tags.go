package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

const maxTagListLimit = 500

type fileTagsRequest struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

type fileTagsResponse struct {
	File    catalog.FileRecord `json:"file"`
	Created []string           `json:"created"`
}

// bulkFileTagsRequest has two mutually-exclusive forms. The ordinary form is
// ids/add/remove. The changes form is how the exact inverse returned by this
// endpoint is replayed in one request and one transaction.
type bulkFileTagsRequest struct {
	IDs     []int64                   `json:"ids"`
	Add     []string                  `json:"add"`
	Remove  []string                  `json:"remove"`
	Changes []catalog.TagFileMutation `json:"changes"`
}

type renameTagRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleListTags(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = min(parsed, maxTagListLimit)
	}
	tags, err := s.cat.ListTags(r.URL.Query().Get("folder"), r.URL.Query().Get("prefix"), limit)
	if err != nil {
		s.writeTagError(w, "listing tags", err)
		return
	}
	if tags == nil {
		tags = []catalog.Tag{}
	}
	writeJSON(w, http.StatusOK, tags)
}

func (s *Server) handleAddFileTags(w http.ResponseWriter, r *http.Request) {
	s.mutateFileTags(w, r, false)
}

func (s *Server) handleRemoveFileTags(w http.ResponseWriter, r *http.Request) {
	s.mutateFileTags(w, r, true)
}

func (s *Server) handleBulkFileTags(w http.ResponseWriter, r *http.Request) {
	var req bulkFileTagsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var changes []catalog.TagFileMutation
	if req.Changes != nil {
		if len(req.IDs)+len(req.Add)+len(req.Remove) != 0 {
			writeError(w, http.StatusBadRequest, "changes cannot be combined with ids, add, or remove")
			return
		}
		if len(req.Changes) == 0 {
			writeError(w, http.StatusBadRequest, "changes must not be empty")
			return
		}
		for _, change := range req.Changes {
			if len(change.IDs) == 0 || len(change.Add)+len(change.Remove) == 0 {
				writeError(w, http.StatusBadRequest, "each change needs ids and add or remove")
				return
			}
		}
		changes = req.Changes
	} else {
		if len(req.IDs) == 0 {
			writeError(w, http.StatusBadRequest, "ids must not be empty")
			return
		}
		if len(req.Add)+len(req.Remove) == 0 {
			writeError(w, http.StatusBadRequest, "add or remove must not be empty")
			return
		}
		changes = []catalog.TagFileMutation{{IDs: req.IDs, Add: req.Add, Remove: req.Remove}}
	}

	result, err := s.cat.ApplyTagMutations(changes, time.Now().Unix())
	if err != nil {
		s.writeTagError(w, "updating bulk file tags", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) mutateFileTags(w http.ResponseWriter, r *http.Request, removeOnly bool) {
	id, ok := s.fileIDParam(w, r)
	if !ok {
		return
	}
	if _, err := s.cat.GetFileByID(id); err != nil {
		s.writeTagError(w, "reading file before tag mutation", err)
		return
	}

	var req fileTagsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if removeOnly && len(req.Add) != 0 {
		writeError(w, http.StatusBadRequest, "add is not allowed in a DELETE request")
		return
	}
	if len(req.Add)+len(req.Remove) == 0 {
		writeError(w, http.StatusBadRequest, "add or remove must not be empty")
		return
	}

	result, err := s.cat.ApplyTags([]int64{id}, req.Add, req.Remove, time.Now().Unix())
	if err != nil {
		s.writeTagError(w, "updating file tags", err)
		return
	}
	file, err := s.cat.GetFileByID(id)
	if err != nil {
		s.writeTagError(w, "reading file after tag mutation", err)
		return
	}
	writeJSON(w, http.StatusOK, fileTagsResponse{File: *file, Created: result.Created})
}

func (s *Server) handleRenameTag(w http.ResponseWriter, r *http.Request) {
	var req renameTagRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	result, err := s.cat.RenameTag(r.PathValue("name"), req.Name, time.Now().Unix())
	if err != nil {
		s.writeTagError(w, "renaming tag", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	var cascade bool
	switch r.URL.Query().Get("cascade") {
	case "", "0":
	case "1":
		cascade = true
	default:
		writeError(w, http.StatusBadRequest, `cascade must be "1" or "0"`)
		return
	}
	result, err := s.cat.DeleteTag(r.PathValue("name"), cascade)
	if err != nil {
		s.writeTagError(w, "deleting tag", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) writeTagError(w http.ResponseWriter, operation string, err error) {
	var invalid *catalog.InvalidTagNameError
	var descendants *catalog.TagHasDescendantsError
	var limit *catalog.TagMutationLimitError
	switch {
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, invalid.Error())
	case errors.As(err, &limit):
		writeError(w, http.StatusBadRequest, limit.Error())
	case errors.As(err, &descendants):
		writeError(w, http.StatusConflict, descendants.Error())
	case errors.Is(err, catalog.ErrNoSuchTag):
		writeError(w, http.StatusNotFound, "tag not found")
	case errors.Is(err, catalog.ErrNoSuchFile):
		writeError(w, http.StatusNotFound, "file not found")
	default:
		s.logger.Error(operation, "error", err)
		writeError(w, http.StatusInternalServerError, "tag operation failed")
	}
}
