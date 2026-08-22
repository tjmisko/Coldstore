package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tjmisko/coldstore/internal/config"
)

const tailscaleLogin = "owner@example.com"

func tailscaleServer(t *testing.T) *Server {
	t.Helper()
	srv, _, _ := setupTestServer(t)
	srv.cfg.Server.ListenAddr = config.DefaultListenAddr
	srv.cfg.Server.AuthMode = config.AuthModeTailscale
	srv.cfg.Server.TailscaleAllowedUsers = []string{tailscaleLogin, "co-owner@example.com"}
	return srv
}

func tailscaleRequest(t *testing.T, srv *Server, method, path string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func withTailscaleLogin(r *http.Request) {
	r.Header.Set(tailscaleUserLoginHeader, tailscaleLogin)
}

func TestTailscaleAuthShouldAllowAnExactConfiguredLogin(t *testing.T) {
	srv := tailscaleServer(t)
	w := tailscaleRequest(t, srv, http.MethodGet, "/api/v1/status", withTailscaleLogin)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
}

func TestTailscaleAuthShouldFailClosedForUnusableIdentityHeaders(t *testing.T) {
	tests := map[string]func(*http.Request){
		"missing": nil,
		"unknown login": func(r *http.Request) {
			r.Header.Set(tailscaleUserLoginHeader, "intruder@example.com")
		},
		"empty login": func(r *http.Request) {
			r.Header[tailscaleUserLoginHeader] = []string{""}
		},
		"surrounding whitespace": func(r *http.Request) {
			r.Header.Set(tailscaleUserLoginHeader, " "+tailscaleLogin)
		},
		"multiple values": func(r *http.Request) {
			r.Header[tailscaleUserLoginHeader] = []string{tailscaleLogin, tailscaleLogin}
		},
		"basic credentials only": func(r *http.Request) {
			r.SetBasicAuth(authUser, authPass)
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			srv := tailscaleServer(t)
			w := tailscaleRequest(t, srv, http.MethodGet, "/api/v1/status", mutate)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body %s", w.Code, w.Body.String())
			}
			if challenge := w.Header().Get("WWW-Authenticate"); challenge != "" {
				t.Errorf("Tailscale rejection triggered a browser auth prompt: %q", challenge)
			}
			if cache := w.Header().Get("Cache-Control"); cache != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cache)
			}
		})
	}
}

func TestTailscaleAuthShouldGuardTheSPAAPIAndMedia(t *testing.T) {
	srv := tailscaleServer(t)
	for _, path := range []string{"/", "/api/v1/files", "/api/v1/thumbnails/x.jpg", "/api/v1/jobs"} {
		w := tailscaleRequest(t, srv, http.MethodGet, path, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without Tailscale identity = %d, want 401", path, w.Code)
		}
	}
}

func TestHandlerShouldFailClosedWhenInvalidTailscaleConfigurationBypassesValidation(t *testing.T) {
	tests := map[string]func(*Server){
		"network listener": func(s *Server) {
			s.cfg.Server.ListenAddr = "192.168.1.10:8090"
		},
		"empty allowlist": func(s *Server) {
			s.cfg.Server.TailscaleAllowedUsers = nil
		},
		"Basic credential mixed in": func(s *Server) {
			s.cfg.Server.Username = authUser
			s.cfg.Server.Password = authPass
		},
	}

	for name, breakConfig := range tests {
		t.Run(name, func(t *testing.T) {
			srv := tailscaleServer(t)
			breakConfig(srv)
			w := tailscaleRequest(t, srv, http.MethodGet, "/api/v1/status", withTailscaleLogin)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want fail-closed 503; body %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestTailscaleAuthShouldRejectCrossOriginMutations(t *testing.T) {
	tests := map[string]func(*http.Request){
		"foreign HTTPS origin": func(r *http.Request) {
			withTailscaleLogin(r)
			r.Header.Set("Origin", "https://malicious.example")
		},
		"same host over HTTP": func(r *http.Request) {
			withTailscaleLogin(r)
			r.Header.Set("Origin", "http://example.com")
		},
		"opaque origin": func(r *http.Request) {
			withTailscaleLogin(r)
			r.Header.Set("Origin", "null")
		},
		"multiple origins": func(r *http.Request) {
			withTailscaleLogin(r)
			r.Header["Origin"] = []string{"https://example.com", "https://malicious.example"}
		},
		"fetch metadata says cross-site": func(r *http.Request) {
			withTailscaleLogin(r)
			r.Header.Set("Sec-Fetch-Site", "cross-site")
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			srv := tailscaleServer(t)
			w := tailscaleRequest(t, srv, http.MethodPatch, "/api/v1/files/not-an-id", mutate)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestTailscaleAuthShouldAllowSameOriginBrowserAndHeaderlessAPIClientMutations(t *testing.T) {
	for name, mutate := range map[string]func(*http.Request){
		"same origin browser": func(r *http.Request) {
			withTailscaleLogin(r)
			r.Header.Set("Origin", "https://example.com")
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		},
		"API client without browser headers": withTailscaleLogin,
	} {
		t.Run(name, func(t *testing.T) {
			srv := tailscaleServer(t)
			w := tailscaleRequest(t, srv, http.MethodPatch, "/api/v1/files/not-an-id", mutate)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want handler's 400 rather than auth rejection; body %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestTailscaleAuthShouldNotApplyMutationOriginRulesToReads(t *testing.T) {
	srv := tailscaleServer(t)
	w := tailscaleRequest(t, srv, http.MethodGet, "/api/v1/status", func(r *http.Request) {
		withTailscaleLogin(r)
		r.Header.Set("Origin", "https://malicious.example")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	if w.Code != http.StatusOK {
		t.Fatalf("cross-origin GET status = %d, want 200", w.Code)
	}
}

func TestMutationAuditShouldAttributeTheTailscaleIdentity(t *testing.T) {
	srv := tailscaleServer(t)
	var logs bytes.Buffer
	srv.logger = slog.New(slog.NewJSONHandler(&logs, nil))

	w := tailscaleRequest(t, srv, http.MethodPost, "/api/v1/scan", withTailscaleLogin)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	line := logs.String()
	for _, want := range []string{
		`"msg":"API mutation completed"`,
		`"actor":"` + tailscaleLogin + `"`,
		`"auth_provider":"tailscale"`,
		`"method":"POST"`,
		`"path":"/api/v1/scan"`,
		`"status":202`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("audit log does not contain %q:\n%s", want, line)
		}
	}
}

func TestMutationAuditShouldNotLogReadRequestsAsMutations(t *testing.T) {
	srv := tailscaleServer(t)
	var logs bytes.Buffer
	srv.logger = slog.New(slog.NewJSONHandler(&logs, nil))

	w := tailscaleRequest(t, srv, http.MethodGet, "/api/v1/status", withTailscaleLogin)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if strings.Contains(logs.String(), "API mutation completed") {
		t.Errorf("read-only request produced a mutation audit record:\n%s", logs.String())
	}
}
