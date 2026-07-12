// Package auth implements the inbound API-key gateway (issue #37/#109).
//
// When NEXUS_PROXY_API_KEY is set, every non-exempt endpoint requires
// a matching Bearer token in the Authorization header. Endpoints used
// by infrastructure probes (/healthz, /metrics) are exempt so K8s
// liveness probes and Prometheus scrapers continue to work without
// credentials. The /status endpoint is exempt only when
// NEXUS_STATUS_PUBLIC=true (default false).
package auth

import (
	"net/http"
	"strings"
)

// Middleware gates HTTP requests behind a bearer token. When key is
// empty the middleware is a pass-through (auth disabled), so a
// development proxy with no NEXUS_PROXY_API_KEY behaves identically
// to the pre-auth binary.
type Middleware struct {
	key    string
	exempt func(*http.Request) bool
}

// NewMiddleware returns a middleware that rejects requests without a
// matching Bearer token, unless exempt(r) returns true. A empty key
// disables auth entirely (Wrap returns the handler unchanged).
func NewMiddleware(key string, exempt func(*http.Request) bool) *Middleware {
	return &Middleware{key: key, exempt: exempt}
}

// Enabled reports whether the middleware actually enforces auth.
func (m *Middleware) Enabled() bool { return m.key != "" }

// Wrap returns an http.Handler that enforces the bearer-token gate.
// When auth is disabled (empty key) the handler is returned as-is.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	if !m.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.exempt != nil && m.exempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		token := BearerToken(r)
		if token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-proxy"`)
			http.Error(w, `{"error":"missing or malformed Authorization header"}`, http.StatusUnauthorized)
			return
		}
		if token != m.key {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-proxy", error="invalid_token"`)
			http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// BearerToken extracts the token from the Authorization header.
// Returns "" if the header is absent, malformed, or not a Bearer
// scheme. The comparison is case-insensitive on the scheme name.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 {
		return ""
	}
	if !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
