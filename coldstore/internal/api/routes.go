package api

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/v1/files", s.handleListFiles)
	s.mux.HandleFunc("GET /api/v1/files/{id}/download", s.handleDownloadFile)
	s.mux.HandleFunc("POST /api/v1/files/{id}/restore", s.handleRestoreFile)

	s.mux.HandleFunc("GET /api/v1/folders", s.handleListFolders)
	s.mux.HandleFunc("PUT /api/v1/folders/{label}", s.handleUpdateFolder)
	s.mux.HandleFunc("GET /api/v1/folders/{label}/persistent", s.handleListPersistent)
	s.mux.HandleFunc("POST /api/v1/folders/{label}/persistent", s.handleAddPersistent)
	s.mux.HandleFunc("DELETE /api/v1/folders/{label}/persistent/{id}", s.handleDeletePersistent)

	s.mux.HandleFunc("GET /api/v1/thumbnails/", s.handleThumbnail)

	s.mux.HandleFunc("GET /api/v1/status", s.handleStatus)

	// Serve embedded React frontend as fallback for all non-API routes
	s.mux.Handle("/", staticHandler())
}
