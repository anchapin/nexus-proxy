package handlers

import "net/http"

// SecurityHeaders returns middleware that stamps standard security
// response headers on every response (issue #39):
//
//   - X-Content-Type-Options: nosniff — blocks MIME sniffing.
//   - X-Frame-Options: DENY — blocks clickjacking via framing.
//   - Referrer-Policy: no-referrer — strips the Referer header on
//     outbound navigations so the proxy's URL is not leaked.
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
func SecurityHeaders(tlsActive bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			if tlsActive {
				h.Set("Strict-Transport-Security", "max-age=31536000")
			}
			next.ServeHTTP(w, r)
		})
	}
}
