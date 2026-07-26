package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSecurityHeadersCrossOriginIsolation verifies the four modern
// cross-origin isolation / feature-policy headers are stamped on every
// response regardless of TLS posture (issue #605):
//
//   - Cross-Origin-Opener-Policy: same-origin
//   - Cross-Origin-Embedder-Policy: require-corp
//   - Cross-Origin-Resource-Policy: same-origin
//   - Permissions-Policy (non-empty, all features locked down)
func TestSecurityHeadersCrossOriginIsolation(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for _, tlsActive := range []bool{false, true} {
		label := "plaintext"
		if tlsActive {
			label = "tls"
		}
		t.Run(label, func(t *testing.T) {
			var srv *httptest.Server
			if tlsActive {
				srv = httptest.NewTLSServer(SecurityHeaders(true)(inner))
			} else {
				srv = httptest.NewServer(SecurityHeaders(false)(inner))
			}
			defer srv.Close()

			client := http.DefaultClient
			if tlsActive {
				client = srv.Client()
			}
			resp := getContextGet(t, client, srv.URL)
			defer resp.Body.Close()

			if got := resp.Header.Get("Cross-Origin-Opener-Policy"); got != "same-origin" {
				t.Errorf("Cross-Origin-Opener-Policy = %q, want same-origin", got)
			}
			if got := resp.Header.Get("Cross-Origin-Embedder-Policy"); got != "require-corp" {
				t.Errorf("Cross-Origin-Embedder-Policy = %q, want require-corp", got)
			}
			if got := resp.Header.Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
				t.Errorf("Cross-Origin-Resource-Policy = %q, want same-origin", got)
			}
			pp := resp.Header.Get("Permissions-Policy")
			if pp == "" {
				t.Fatal("Permissions-Policy header is empty")
			}
			// Every directive must lock down its feature with an empty
			// allow-list "()".
			for _, want := range []string{
				"camera=()",
				"microphone=()",
				"geolocation=()",
				"payment=()",
				"interest-cohort=()",
			} {
				if !containsSubstring(pp, want) {
					t.Errorf("Permissions-Policy %q missing directive %q", pp, want)
				}
			}
		})
	}
}

// containsSubstring reports whether s contains substr. A tiny helper so
// the test does not pull in "strings" for a single call.
func containsSubstring(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
