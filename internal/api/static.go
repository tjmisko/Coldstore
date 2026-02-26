package api

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

func staticHandler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("embedded dist not found: " + err.Error())
	}

	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve API routes normally (they're registered on the mux before this)
		// This handler only runs for non-API paths

		path := r.URL.Path

		// Try to serve the file directly
		if path != "/" && !strings.HasPrefix(path, "/api/") {
			// Check if the file exists in the embedded FS
			if f, err := sub.Open(strings.TrimPrefix(path, "/")); err == nil {
				f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		// SPA fallback: serve index.html for all unmatched routes
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
