package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These assert against internal/api/dist, which is whatever `make frontend`
// last copied in. On a tree that has never built the frontend, dist holds only
// .gitkeep -- so each test skips rather than failing, and `go test ./...` on a
// fresh clone stays green. CI's frontend and fresh-clone-build jobs are where
// they actually run.
func serveStatic(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	staticHandler().ServeHTTP(rec, req)
	return rec
}

func skipWithoutBuiltFrontend(t *testing.T) {
	t.Helper()

	if _, err := distFS.ReadFile("dist/index.html"); err != nil {
		t.Skip("frontend not built into internal/api/dist; run `make frontend`")
	}
}

func TestIndexCarriesTheApplicationIdentity(t *testing.T) {
	skipWithoutBuiltFrontend(t)

	// The first deployed build put "web" -- `npm create vite`'s placeholder --
	// in every browser tab, and pointed the icon at a /vite.svg that was never
	// shipped, so the tab fell back to the globe.
	body := serveStatic(t, "/").Body.String()

	if !strings.Contains(body, "<title>Coldstore</title>") {
		t.Error("index.html does not title the page Coldstore")
	}
	if strings.Contains(body, "vite.svg") {
		t.Error("index.html still references the Vite scaffold icon")
	}
	for _, link := range []string{"/favicon.svg", "/apple-touch-icon.png", "/manifest.webmanifest"} {
		if !strings.Contains(body, link) {
			t.Errorf("index.html does not link %s", link)
		}
	}
}

func TestIconAssetsAreEmbeddedAndTyped(t *testing.T) {
	skipWithoutBuiltFrontend(t)

	// Every one of these is referenced by index.html or the manifest. A missing
	// file does not 404 here -- the SPA fallback answers with index.html -- so
	// the assertion is on the content type, which is what a browser acts on.
	cases := []struct {
		path string
		want string
	}{
		{"/favicon.svg", "image/svg+xml"},
		{"/apple-touch-icon.png", "image/png"},
		{"/icon-192.png", "image/png"},
		{"/icon-512.png", "image/png"},
		{"/icon-maskable-512.png", "image/png"},
		{"/manifest.webmanifest", "application/manifest+json"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := serveStatic(t, tc.path)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			got := rec.Header().Get("Content-Type")
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("Content-Type = %q, want %q -- the SPA fallback may have served index.html instead", got, tc.want)
			}
		})
	}
}

func TestIconSourcesAreNotShippedInTheBinary(t *testing.T) {
	skipWithoutBuiltFrontend(t)

	// The SVG sources the PNGs are generated from live in web/icons/, not
	// web/public/, so that Vite does not copy them into the embedded bundle.
	for _, name := range []string{"dist/icon-source.svg", "dist/app-icon.svg", "dist/app-icon-maskable.svg"} {
		if _, err := distFS.ReadFile(name); err == nil {
			t.Errorf("%s is embedded in the binary; icon sources belong in web/icons/", name)
		}
	}
}
