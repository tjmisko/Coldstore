package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tjmisko/coldstore/internal/config"
)

const tailscaleUserLoginHeader = "Tailscale-User-Login"

type requestIdentity struct {
	Provider string
	Login    string
}

type requestIdentityContextKey struct{}

func withRequestIdentity(next http.Handler, identity requestIdentity) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), requestIdentityContextKey{}, identity)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func identityFromRequest(r *http.Request) requestIdentity {
	identity, _ := r.Context().Value(requestIdentityContextKey{}).(requestIdentity)
	return identity
}

// tailscaleAuth trusts the identity header that Tailscale Serve inserts after
// stripping any caller-supplied value. Configuration validation makes this
// middleware legal only on a loopback listener: a direct network client could
// otherwise bypass Serve and forge the header.
func (s *Server) tailscaleAuth(next http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(s.cfg.Server.TailscaleAllowedUsers))
	for _, login := range s.cfg.Server.TailscaleAllowedUsers {
		allowed[login] = struct{}{}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(tailscaleUserLoginHeader)
		login := ""
		if len(values) == 1 {
			login = values[0]
		}
		_, ok := allowed[login]
		if len(values) != 1 || login == "" || login != strings.TrimSpace(login) || !ok {
			w.Header().Set("Cache-Control", "no-store")
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		withRequestIdentity(next, requestIdentity{
			Provider: config.AuthModeTailscale,
			Login:    login,
		}).ServeHTTP(w, r)
	})
}

// requireSameOriginMutations blocks browser CSRF against the passwordless
// Tailscale path. API clients do not normally send Origin or Sec-Fetch-Site and
// remain supported; browsers that identify a cross-site request are rejected.
// TLS terminates in Serve, so r.TLS is intentionally not consulted here.
func (s *Server) requireSameOriginMutations(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isReadOnlyMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		origins := r.Header.Values("Origin")
		origin := ""
		if len(origins) == 1 {
			origin = origins[0]
		}
		fetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
		if fetchSite == "cross-site" || len(origins) > 1 || (origin != "" && !sameServeOrigin(origin, r.Host)) {
			identity := identityFromRequest(r)
			s.logger.Warn("cross-origin API mutation rejected",
				"actor", identity.Login,
				"auth_provider", identity.Provider,
				"method", r.Method,
				"path", r.URL.Path)
			writeError(w, http.StatusForbidden, "cross-origin mutation rejected")
			return
		}

		next.ServeHTTP(w, r)
	})
}

func sameServeOrigin(rawOrigin, requestHost string) bool {
	origin, err := url.Parse(rawOrigin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil ||
		origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return false
	}
	return strings.EqualFold(origin.Host, requestHost)
}

func isReadOnlyMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

type auditResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController recover optional interfaces implemented
// by the underlying writer.
func (w *auditResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (s *Server) auditMutations(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isReadOnlyMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		started := time.Now()
		recorder := &auditResponseWriter{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		identity := identityFromRequest(r)
		s.logger.Info("API mutation completed",
			"actor", identity.Login,
			"auth_provider", identity.Provider,
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration", time.Since(started))
	})
}
