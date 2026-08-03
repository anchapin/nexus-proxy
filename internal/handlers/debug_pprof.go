// Package handlers — debug_pprof.go registers net/http/pprof and expvar
// debug endpoints on the proxy's mux, gated by config so the debug
// surface is only exposed when an operator explicitly opts in (issue
// #1150).
//
// Security model:
//   - NEXUS_DEBUG_PPROF_ENABLED=false (default): no handlers are
//     registered; the mux returns 404 for /debug/* paths.
//   - Enabled + NEXUS_DEBUG_PPROF_API_KEY set: every /debug/* request
//     must carry Authorization: Bearer <key>; mismatched or missing
//     tokens get 401.
//   - Enabled + key empty: only loopback (127.0.0.1 / ::1) connections
//     are served; non-loopback peers get 403.
//
// The debug subtree is registered on the same mux as /metrics and is
// exempt from the main inbound auth gate (see publicPathExempt in
// main.go) because it carries its own independent gate. This mirrors
// how /metrics bypasses auth for Prometheus scrapers.
package handlers

import (
	"crypto/subtle"
	"encoding/json"
	"expvar"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"strings"

	"github.com/anchapin/nexus-proxy/internal/config"
)

// RegisterDebugPprof registers pprof and expvar debug endpoints on the
// given mux when cfg.DebugPprofEnabled is true. When disabled, no
// handlers are registered and the mux returns 404 for /debug/* paths
// by default. All /debug/* traffic passes through DebugPprofGate which
// enforces API-key or loopback-only access (issue #1150).
func RegisterDebugPprof(mux *http.ServeMux, cfg config.Config) {
	if !cfg.DebugPprofEnabled {
		return
	}

	dm := http.NewServeMux()
	dm.HandleFunc("/debug/pprof/", pprof.Index)
	dm.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	dm.HandleFunc("/debug/pprof/profile", pprof.Profile)
	dm.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	dm.HandleFunc("/debug/pprof/trace", pprof.Trace)
	dm.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
	dm.Handle("/debug/pprof/block", pprof.Handler("block"))
	dm.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	dm.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	dm.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	dm.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	dm.Handle("/debug/vars", expvar.Handler())

	gate := DebugPprofGate(cfg)
	mux.Handle("/debug/", gate(dm))

	// Register the debug exposure mode for operator visibility.
	slog.Info("debug pprof endpoints enabled",
		slog.String("mode", debugExposureMode(cfg)),
	)
}

// debugExposureMode returns a human-readable description of the access
// control mode for the debug endpoints, used in logs and the nexus
// check diagnostic.
func debugExposureMode(cfg config.Config) string {
	if !cfg.DebugPprofEnabled {
		return "disabled"
	}
	if cfg.DebugPprofAPIKey != "" {
		return "api-key"
	}
	return "localhost"
}

// DebugPprofExposureMode returns a human-readable description of the
// debug endpoint access control mode: "disabled", "api-key", or
// "localhost". Used by the nexus check diagnostic (issue #1150).
func DebugPprofExposureMode(cfg config.Config) string {
	return debugExposureMode(cfg)
}

// DebugPprofGate returns middleware that gates /debug/* endpoints
// according to the config's debug pprof settings (issue #1150):
//   - When DebugPprofAPIKey is set: a matching Bearer token is
//     required (401 on mismatch or absence).
//   - When the key is empty: only loopback connections are allowed
//     (403 for non-loopback peers).
func DebugPprofGate(cfg config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.DebugPprofAPIKey != "" {
				if !checkDebugBearerToken(r, cfg.DebugPprofAPIKey) {
					writeDebugJSONError(w, http.StatusUnauthorized,
						"unauthorized: missing or invalid debug API key")
					return
				}
			} else {
				if !isLoopbackPeer(r.RemoteAddr) {
					writeDebugJSONError(w, http.StatusForbidden,
						"forbidden: debug endpoints require loopback connection when no API key is set")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// checkDebugBearerToken validates the Authorization header against the
// expected key using constant-time comparison to prevent timing
// attacks.
func checkDebugBearerToken(r *http.Request, expected string) bool {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	token := auth[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

// isLoopbackPeer reports whether the RemoteAddr (direct TCP peer) is a
// loopback address (127.0.0.0/8 or ::1). The direct peer is used
// rather than the trusted-proxy-resolved client IP because the debug
// surface is a low-level diagnostic tool that should only be reached
// from the same host.
func isLoopbackPeer(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// writeDebugJSONError writes a JSON error envelope consistent with the
// proxy's other error responses.
func writeDebugJSONError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "debug_endpoint_error",
		},
	})
}
