package middleware

import (
	"net/http"
)

// SecurityHeaders returns HTTP middleware that hardens all responses by
// adding defence-in-depth headers recommended by OWASP and the HTTP
// Observatory:
//
//   - X-Content-Type-Options: nosniff
//     Prevents browsers from MIME-sniffing a response away from the
//     declared Content-Type, closing an attack vector for XSS via
//     type-confusion.
//
//   - X-Frame-Options: DENY
//     Disables embedding the response in a frame or iframe, mitigating
//     clickjacking attacks. Use Content-Security-Policy frame-ancestors
//     as the modern replacement where browser support allows.
//
//   - Content-Security-Policy: default-src 'none'; frame-ancestors 'none'
//     The strictest CSP: no scripts, styles, images, fonts, frames, or
//     other subresources may be loaded from any origin. This is
//     intentionally paranoid for an API proxy whose responses contain
//     no user-facing content that requires inline execution. Operators
//     running a web UI behind the same domain should tighten this to
//     their actual asset origins.
//
//   - Strict-Transport-Security: max-age=31536000; includeSubDomains
//     Enforces TLS for all subdomains for one year, preventing
//     protocol-downgrade and cookie-hijacking attacks. Note: this
//     header is only effective when the connection is already TLS;
//     plaintext HTTP connections ignore it.
//
//   - Permissions-Policy: none
//     Disables all browser features that could be abused for
//     surveillance or cross-origin attacks (camera, microphone,
//     geolocation, etc.). The proxy has no legitimate need for any
//     of them.
func SecurityHeaders() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Apply headers to every response before it leaves the proxy.
			// These are safe to set unconditionally — they impose no
			// constraints on correct clients and add no overhead to the
			// request path beyond a handful of map lookups.
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			w.Header().Set("Permissions-Policy", "none")
			next.ServeHTTP(w, r)
		})
	}
}
