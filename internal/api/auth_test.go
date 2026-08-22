package api

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

const (
	authUser = "owner"
	authPass = "correct-horse-battery-staple"
)

// authServer is the standard test server with credentials configured, so every
// route below it is behind basicAuth.
func authServer(t *testing.T) *Server {
	t.Helper()
	srv, _, _ := setupTestServer(t)
	srv.cfg.Server.Username = authUser
	srv.cfg.Server.Password = authPass
	return srv
}

func do(t *testing.T, srv *Server, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestShouldAllowRequestWhenCredentialsAreCorrect(t *testing.T) {
	srv := authServer(t)

	w := do(t, srv, func(r *http.Request) { r.SetBasicAuth(authUser, authPass) })

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
}

func TestShouldRejectRequestWhenAuthorizationHeaderIsMissing(t *testing.T) {
	srv := authServer(t)

	w := do(t, srv, nil)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	// Without a challenge the browser never offers a login prompt.
	if got := w.Header().Get("WWW-Authenticate"); got != `Basic realm="coldstore"` {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", got)
	}
}

func TestShouldRejectRequestWhenPasswordIsWrong(t *testing.T) {
	srv := authServer(t)

	w := do(t, srv, func(r *http.Request) { r.SetBasicAuth(authUser, "wrong") })

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestShouldRejectRequestWhenUsernameIsWrong(t *testing.T) {
	srv := authServer(t)

	w := do(t, srv, func(r *http.Request) { r.SetBasicAuth("intruder", authPass) })

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// A missing header and a wrong password must be indistinguishable from the
// outside: same status, same challenge, same body. Anything else tells an
// attacker which half they got right.
func TestShouldAnswerIdenticallyWhenHeaderIsMissingAndWhenPasswordIsWrong(t *testing.T) {
	srv := authServer(t)

	missing := do(t, srv, nil)
	wrong := do(t, srv, func(r *http.Request) { r.SetBasicAuth(authUser, "wrong") })

	if missing.Code != wrong.Code {
		t.Errorf("status differs: missing header %d, wrong password %d", missing.Code, wrong.Code)
	}
	if missing.Body.String() != wrong.Body.String() {
		t.Errorf("body differs:\n missing: %s\n wrong:   %s", missing.Body, wrong.Body)
	}
	if missing.Header().Get("WWW-Authenticate") != wrong.Header().Get("WWW-Authenticate") {
		t.Error("WWW-Authenticate differs between a missing header and a wrong password")
	}
}

func TestShouldRejectRequestWhenAuthorizationHeaderIsMalformed(t *testing.T) {
	cases := map[string]string{
		"not base64":        "Basic !!!!not-base64!!!!",
		"no colon":          "Basic " + base64.StdEncoding.EncodeToString([]byte("ownernopassword")),
		"wrong scheme":      "Bearer " + base64.StdEncoding.EncodeToString([]byte(authUser+":"+authPass)),
		"scheme only":       "Basic",
		"empty header":      "",
		"empty credentials": "Basic " + base64.StdEncoding.EncodeToString([]byte(":")),
	}

	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			srv := authServer(t)
			w := do(t, srv, func(r *http.Request) { r.Header.Set("Authorization", header) })
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 for %s", w.Code, name)
			}
		})
	}
}

// The comparison is length-independent because both sides are hashed first. A
// prefix of the real password must not be accepted, and neither must a longer
// string that starts with it.
func TestShouldRejectRequestWhenPasswordIsAPrefixOrExtensionOfTheRealOne(t *testing.T) {
	srv := authServer(t)

	for _, pass := range []string{authPass[:len(authPass)-1], authPass + "x", "", authPass + "\x00"} {
		w := do(t, srv, func(r *http.Request) { r.SetBasicAuth(authUser, pass) })
		if w.Code != http.StatusUnauthorized {
			t.Errorf("password %q was accepted", pass)
		}
	}
}

// Basic auth wraps the whole mux, not just /api. An unauthenticated request for
// the SPA or a thumbnail must be challenged too, or the file listing leaks
// through the static handler.
func TestShouldGuardEveryRouteWhenCredentialsAreConfigured(t *testing.T) {
	srv := authServer(t)

	for _, path := range []string{"/", "/api/v1/files", "/api/v1/thumbnails/x.jpg", "/api/v1/jobs"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated = %d, want 401", path, w.Code)
		}
	}
}

func TestShouldServeWithoutChallengeWhenNoCredentialsAreConfigured(t *testing.T) {
	// Reachable only with a loopback bind; config.ValidateServerBinding refuses
	// this combination on any other address.
	srv, _, _ := setupTestServer(t)

	w := do(t, srv, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when auth is off", w.Code)
	}
}

func TestShouldFailClosedWhenOnlyOneCredentialReachesTheHandler(t *testing.T) {
	// Config validation rejects this before startup. The handler also fails
	// closed so a future alternate construction path cannot expose the mux.
	srv, _, _ := setupTestServer(t)
	srv.cfg.Server.Username = authUser

	if w := do(t, srv, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for invalid authentication configuration", w.Code)
	}
}
