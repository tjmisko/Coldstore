package api

import "net/http"

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/v1/files", s.handleListFiles)
	s.mux.HandleFunc("GET /api/v1/files/ids", s.handleListFileIDs)
	s.mux.HandleFunc("GET /api/v1/files/{id}/download", s.handleDownloadFile)
	s.mux.HandleFunc("GET /api/v1/files/{id}/preview", s.handlePreviewFile)
	s.mux.HandleFunc("GET /api/v1/files/{id}/epub", s.handleEPUBSpine)
	// {resource...} is a zip ENTRY NAME, and the untrusted component of this
	// route. It is validated against the archive root inside epub.Resource --
	// the mux's path cleaning operates on a URL and knows nothing about what
	// is inside a zip.
	s.mux.HandleFunc("GET /api/v1/files/{id}/epub/{resource...}", s.handleEPUBResource)
	s.mux.HandleFunc("PATCH /api/v1/files/{id}", s.handleUpdateFile)
	s.mux.HandleFunc("POST /api/v1/files/{id}/restore", s.handleRestoreFile)
	s.mux.HandleFunc("POST /api/v1/files/{id}/archive", s.handleArchiveFile)
	s.mux.HandleFunc("POST /api/v1/files/{id}/pin", s.handlePinFile)
	s.mux.HandleFunc("DELETE /api/v1/files/{id}/pin", s.handleUnpinFile)
	s.mux.HandleFunc("POST /api/v1/files/pin", s.handleBulkPin)
	s.mux.HandleFunc("POST /api/v1/files/{id}/tags", s.handleAddFileTags)
	s.mux.HandleFunc("DELETE /api/v1/files/{id}/tags", s.handleRemoveFileTags)
	s.mux.HandleFunc("POST /api/v1/files/tags", s.handleBulkFileTags)
	s.mux.HandleFunc("POST /api/v1/files/{id}/resolve", s.handleResolveConflict)
	s.mux.HandleFunc("GET /api/v1/tags", s.handleListTags)
	s.mux.HandleFunc("PUT /api/v1/tags/{name...}", s.handleRenameTag)
	s.mux.HandleFunc("DELETE /api/v1/tags/{name...}", s.handleDeleteTag)

	s.mux.HandleFunc("GET /api/v1/folders", s.handleListFolders)
	s.mux.HandleFunc("PUT /api/v1/folders/{label}", s.handleUpdateFolder)
	s.mux.HandleFunc("GET /api/v1/folders/{label}/persistent", s.handleListPersistent)
	s.mux.HandleFunc("POST /api/v1/folders/{label}/persistent", s.handleAddPersistent)
	s.mux.HandleFunc("DELETE /api/v1/folders/{label}/persistent/{id}", s.handleDeletePersistent)

	s.mux.HandleFunc("GET /api/v1/thumbnails/", s.handleThumbnail)

	s.mux.HandleFunc("POST /api/v1/jobs", s.handleCreateJob)
	s.mux.HandleFunc("GET /api/v1/jobs", s.handleListJobs)
	s.mux.HandleFunc("GET /api/v1/jobs/{id}", s.handleGetJob)

	s.mux.HandleFunc("GET /api/v1/events", s.handleListEvents)

	s.mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	s.mux.HandleFunc("POST /api/v1/scan", s.handleScan)

	// Every unmatched /api/ path is a JSON 404, registered AHEAD of the SPA
	// fallback in specificity so the fallback can never answer an API call.
	// Without this an unimplemented or mistyped endpoint returns 200 with an
	// HTML body, the client's JSON.parse throws on markup, and a missing
	// route looks to the frontend exactly like a parser bug.
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})

	// Serve embedded React frontend as fallback for all non-API routes
	s.mux.Handle("/", staticHandler())
}
