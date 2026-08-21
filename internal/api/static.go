package api

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// http.FileServer types a response from mime.TypeByExtension, whose Unix table
// is seeded from /etc/mime.types -- a file the daemon's host may not have at
// all, and which on Debian does not list .webmanifest. Without this the
// manifest is sniffed as text/plain and the browser declines to install the
// app, so the iOS home-screen icon silently stops working.
func init() {
	if err := mime.AddExtensionType(".webmanifest", "application/manifest+json"); err != nil {
		panic("registering the webmanifest mime type: " + err.Error())
	}
}

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
