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
	scanner  *daemon.Scanner
	logger   *slog.Logger
	mux      *http.ServeMux
}

func NewServer(cfg *config.Config, cat *catalog.Catalog, restorer *daemon.Restorer, scanner *daemon.Scanner, logger *slog.Logger) *Server {
	s := &Server{
		cfg:      cfg,
		cat:      cat,
		restorer: restorer,
		scanner:  scanner,
		logger:   logger,
		mux:      http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) Handler() http.Handler {
	if s.cfg.Server.Username != "" && s.cfg.Server.Password != "" {
		return s.basicAuth(s.mux)
	}
	return s.mux
}

func (s *Server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != s.cfg.Server.Username || pass != s.cfg.Server.Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="coldstore"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ListenAndServe(addr string) error {
	if s.cfg.Server.Username == "" || s.cfg.Server.Password == "" {
		s.logger.Warn("API server starting without authentication")
	}
	s.logger.Info("API server starting", "addr", addr)
	return http.ListenAndServe(addr, s.Handler())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
