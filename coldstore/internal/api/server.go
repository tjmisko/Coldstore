package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/daemon"
)

type Server struct {
	cfg      *config.Config
	cat      *catalog.Catalog
	restorer *daemon.Restorer
	logger   *slog.Logger
	mux      *http.ServeMux
}

func NewServer(cfg *config.Config, cat *catalog.Catalog, restorer *daemon.Restorer, logger *slog.Logger) *Server {
	s := &Server{
		cfg:      cfg,
		cat:      cat,
		restorer: restorer,
		logger:   logger,
		mux:      http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) ListenAndServe(addr string) error {
	s.logger.Info("API server starting", "addr", addr)
	return http.ListenAndServe(addr, s.mux)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
