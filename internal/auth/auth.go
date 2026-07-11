// Package auth provides bearer-token authentication middleware that gates
// inbound requests to the proxy when one or more API keys are configured
// (issue #37).
//
// When at least one key is set (NEXUS_PROXY_API_KEY or
// NEXUS_PROXY_API_KEYS), requests to /v1/chat/completions must present a
// valid `Authorization: Bearer <key>` or `X-API-Key` header or the proxy
// returns HTTP 401 with an OpenAI-compatible error envelope. /healthz is
// exempt by default so orchestrator liveness probes keep working.
//
// When no key is configured the middleware is a no-op pass-through, so
// the proxy behaves identically to today (zero breaking change for
// localhost dev).
package auth

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/anchapin/nexus-proxy/internal/tracing"
)

// Middleware wraps next with bearer-token authentication. Requests that
// do not present a valid key in `Authorization: Bearer <key>` (scheme
// matched case-insensitively) or `X-API-Key` are rejected with HTTP 401
// and an OpenAI-compatible error envelope.
//
// When keys is empty (or contains only blank entries) the returned
// middleware is an identity pass-through so callers can unconditionally
// wrap the mux without paying for an "auth disabled?" branch on the hot
// path — the proxy behaves identically to today when no key is
// configured.
//
// The exempt predicate, when non-nil, short-circuits the check for
// specific paths (e.g. /healthz for liveness probes). Exempt requests
// are forwarded without consuming a key comparison.
//
// Key comparison uses crypto/subtle.ConstantTimeCompare to prevent
// timing attacks against the configured keys. Every configured key is
// compared even after a match, so the function always spends time
// proportional to len(keys) rather than leaking the position of the
// accepted key.
func Middleware(keys []string, exempt func(*http.Request) bool) func(http.Handler) http.Handler {
	// Defensive copy + drop blanks so callers cannot mutate the slice
	// after wiring and so a stray empty entry cannot accept an
	// unauthenticated request.
	valid := make([]string, 0, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			valid = append(valid, k)
		}
	}
	if len(valid) == 0 {
		// No key configured — authentication disabled. Return an
		// identity middleware so the proxy behaves identically to
		// today (zero breaking change for localhost dev).
		return func(next http.Handler) http.Handler {
			return next
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Distributed tracing (issue #71). When the operator
			// has not configured a collector, Enabled() returns
			// false and this block disappears entirely — zero
			// overhead on the hot path.
			//
			// The span attaches to whichever parent the tracing
			// middleware (or the chat handler) has already
			// installed in r.Context(). When no parent exists
			// the span becomes its own root with a fresh trace
			// id, so an auth-rejected request that arrives before
			// any upstream caller still produces a visible trace.
			var span *tracing.Span
			if tracing.Enabled() {
				r2, s := tracing.StartSpanFromContext(r.Context(), "auth.check")
				span = s
				r = r.WithContext(r2)
				defer span.End()
				span.SetAttr("auth.method", authMethod(r))
				span.SetAttr("auth.exempt", exempt != nil && exempt(r))
			}
			if exempt != nil && exempt(r) {
				if span != nil {
					span.SetAttr("auth.outcome", "exempt")
				}
				next.ServeHTTP(w, r)
				return
			}
			if !keyMatches(extractKey(r), valid) {
				if span != nil {
					span.SetAttr("auth.outcome", "rejected")
					span.SetStatus(tracing.StatusError, "invalid_key")
				}
				writeUnauthorized(w)
				return
			}
			if span != nil {
				span.SetAttr("auth.outcome", "accepted")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// authMethod reports whether the request presented a bearer
// credential, an X-API-Key header, or no credential at all. The
// span attributes mirror that distinction so a trace view can
// distinguish "presented the wrong key" from "presented no key".
func authMethod(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		return "bearer"
	}
	if k := r.Header.Get("X-API-Key"); k != "" {
		return "x-api-key"
	}
	return "none"
}

// extractKey pulls the presented credential from the request. It checks
// `Authorization: Bearer <key>` first (scheme matched
// case-insensitively per RFC 7235) and falls back to the `X-API-Key`
// header. Returns "" when neither header carries a usable value.
func extractKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "bearer "
		// EqualFold handles "Bearer", "bearer", "BEARER", etc. The
		// prefix check guards against an empty-scheme header
		// ("Bearer") producing a negative slice below.
		if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			if v := strings.TrimSpace(h[len(prefix):]); v != "" {
				return v
			}
		}
	}
	if k := strings.TrimSpace(r.Header.Get("X-API-Key")); k != "" {
		return k
	}
	return ""
}

// keyMatches reports whether presented matches any of the configured
// keys using a constant-time comparison. A missing/empty presented value
// never matches, even if a configured key happens to be empty (blanks
// are already filtered by Middleware, but this guard keeps keyMatches
// safe for direct callers).
//
// Every configured key is compared even after a match (the result is
// folded via OR) so the function always runs for len(keys) iterations;
// an early return would leak the index of the accepted key through the
// request latency.
func keyMatches(presented string, keys []string) bool {
	if presented == "" {
		return false
	}
	pb := []byte(presented)
	var ok byte
	for _, k := range keys {
		if subtle.ConstantTimeCompare(pb, []byte(k)) == 1 {
			ok = 1
			// Deliberately do NOT return early; see comment above.
		}
	}
	return ok == 1
}

// writeUnauthorized emits the OpenAI-compatible error envelope for a
// missing/invalid key. The shape mirrors handlers.writeJSONError:
// `{"error":{"message":...,"type":...}}`. The type token is the
// lowercase "unauthorized" value per the issue #37 spec, which matches
// the snake_case convention OpenAI uses for its error `type` field.
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": "Missing or invalid API key",
			"type":    "unauthorized",
		},
	})
}
