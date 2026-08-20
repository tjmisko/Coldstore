package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/daemon"
)

// Deps is what the API server needs to do its job. It is a struct rather than
// a positional parameter list because that list has already broken the e2e
// helper once and this work keeps adding to it (design section 6.14).
type Deps struct {
	Cfg   *config.Config
	Cat   *catalog.Catalog
	Mover *daemon.Mover
	// Worker is needed only for the enqueue poke. It must be the SAME worker
	// the daemon is running, or POST /jobs wakes a goroutine nobody started and
	// every job waits for the running worker's 2 s ticker instead.
	Worker  *daemon.Worker
	Scanner *daemon.Scanner
	Logger  *slog.Logger
}

type Server struct {
	cfg *config.Config
	cat *catalog.Catalog
	// mover is the process-wide serialisation point. The server must be
	// handed the SAME one the daemon uses, or a pin arriving over HTTP is no
	// longer serialised against the quota tick's move.
	mover  *daemon.Mover
	worker *daemon.Worker
	// scanner is held for design section 6.14's Deps shape. Nothing in the API
	// calls it any more: POST /api/v1/scan enqueues a job and the worker runs
	// the walk, which is what removed the unguarded inline scan from the
	// request path.
	scanner *daemon.Scanner
	logger  *slog.Logger
	mux     *http.ServeMux
}

func NewServer(d Deps) *Server {
	s := &Server{
		cfg:     d.Cfg,
		cat:     d.Cat,
		mover:   d.Mover,
		worker:  d.Worker,
		scanner: d.Scanner,
		logger:  d.Logger,
		mux:     http.NewServeMux(),
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
