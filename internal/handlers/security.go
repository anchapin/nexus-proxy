package handlers

import (
	"net/http"
	"regexp"

	"github.com/anchapin/nexus-proxy/internal/tracing"
)

// permissionsPolicyValue locks down browser features that an LLM proxy
// API endpoint never needs (issue #605). Every feature directive is set
// to an empty allow-list "()" so no origin — including same-origin —
// can activate the camera, microphone, geolocation, payment, or the
// other privacy-sensitive surfaces listed below. This is
// defense-in-depth: if a client context is compromised it cannot
// silently turn these features on.
const permissionsPolicyValue = "accelerometer=(), autoplay=(), camera=(), " +
	"clipboard-read=(), clipboard-write=(), geolocation=(), gyroscope=(), " +
	"magnetometer=(), microphone=(), payment=(), publickey-credentials-get=(), " +
	"usb=(), interest-cohort=()"

// SecurityHeaders returns middleware that stamps standard security
// response headers on every response (issue #39):
//
//   - X-Content-Type-Options: nosniff — blocks MIME sniffing.
//   - X-Frame-Options: DENY — blocks clickjacking via framing.
//   - Referrer-Policy: no-referrer — strips the Referer header on
//     outbound navigations so the proxy's URL is not leaked.
//   - Cross-Origin-Opener-Policy: same-origin — isolates the browsing
//     context group so cross-origin documents cannot manipulate the
//     proxy's window (issue #605).
//   - Cross-Origin-Embedder-Policy: require-corp — opts the context
//     into cross-origin isolation, gating subresource loads on CORP
//     (issue #605).
//   - Cross-Origin-Resource-Policy: same-origin — blocks cross-origin
//     no-cors requests from reading the response (issue #605).
//   - Permissions-Policy — disables privacy-sensitive browser features
//     the proxy never uses (issue #605).
//
// When tlsActive is true, Strict-Transport-Security is added with a
// one-year max-age so clients pin HTTPS and refuse plaintext fallbacks.
// HSTS is intentionally omitted when TLS is not active: emitting it over
// plaintext would be ignored by browsers (and is a spec violation).
//
// The headers are set on the response map before the wrapped handler
// runs, so the wrapped handler may still override them. Downstream
// SSE/JSON writers stamp Content-Type after this middleware, which is
// the expected ordering.
//
// When tracing is enabled a "security.headers" span wraps the header
// mutation (issue #71). The span carries the active TLS posture as an
// attribute so a trace view surfaces "hsts=off" on a plaintext
// deployment without a side-channel grep.
func SecurityHeaders(tlsActive bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var span *tracing.Span
			if tracing.Enabled() {
				r2, s := tracing.StartSpanFromContext(r.Context(), "security.headers")
				span = s
				r = r.WithContext(r2)
				defer span.End()
				span.SetAttr("security.tls_active", tlsActive)
			}
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Embedder-Policy", "require-corp")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Permissions-Policy", permissionsPolicyValue)
			if tlsActive {
				h.Set("Strict-Transport-Security", "max-age=31536000")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requestIDMaxLen caps the length of an inbound X-Request-Id. Longer
// values are truncated so a malicious client cannot bloat logs or the
// telemetry row with an arbitrarily long correlation id (issue #39).
const requestIDMaxLen = 128

// requestIDDisallowedRe matches characters NOT permitted in a sanitized
// request id. The allowed set is [a-zA-Z0-9._:-]; any other character
// (newlines, control bytes, quotes, angle brackets, whitespace, ...) is
// stripped so a crafted X-Request-Id cannot inject log entries or break
// downstream JSON consumers (issue #39).
var requestIDDisallowedRe = regexp.MustCompile(`[^a-zA-Z0-9._:-]`)

// sanitizeRequestID strips characters outside [a-zA-Z0-9._:-] and caps
// the length at requestIDMaxLen. Returns "" when the input is empty
// after sanitization, so the caller falls through to a generated hex id.
func sanitizeRequestID(s string) string {
	s = requestIDDisallowedRe.ReplaceAllString(s, "")
	if len(s) > requestIDMaxLen {
		s = s[:requestIDMaxLen]
	}
	return s
}
