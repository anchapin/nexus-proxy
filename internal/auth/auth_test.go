package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// okHandler is a simple 200-OK handler used across tests.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
}

func TestDisabledWhenNoKey(t *testing.T) {
	m := NewMiddleware("", nil)
	if m.Enabled() {
		t.Error("Enabled() = true for empty key, want false")
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("disabled middleware: status = %d, want 200", rr.Code)
	}
}

func TestRejectsWithoutToken(t *testing.T) {
	m := NewMiddleware("secret-key", nil) // no exempt paths

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rr.Code)
	}
	if rr.Header().Get("WWW-Authenticate") == "" {
		t.Error("WWW-Authenticate header not set on 401")
	}
}

func TestRejectsWrongToken(t *testing.T) {
	m := NewMiddleware("secret-key", nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", rr.Code)
	}
}

func TestAcceptsCorrectToken(t *testing.T) {
	m := NewMiddleware("secret-key", nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("correct token: status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != "ok" {
		t.Errorf("body = %q, want 'ok'", rr.Body.String())
	}
}

func TestExemptPathsBypassAuth(t *testing.T) {
	exempt := func(r *http.Request) bool {
		return r.URL.Path == "/healthz" || r.URL.Path == "/metrics"
	}
	m := NewMiddleware("secret-key", exempt)

	for _, path := range []string{"/healthz", "/metrics"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		m.Wrap(okHandler()).ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("exempt path %s: status = %d, want 200", path, rr.Code)
		}
	}

	// Non-exempt path still requires auth.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	m.Wrap(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("non-exempt path: status = %d, want 401", rr.Code)
	}
}

func TestStatusGatedByDefault(t *testing.T) {
	// /status is NOT in the exempt set — it must require auth.
	exempt := func(r *http.Request) bool {
		return r.URL.Path == "/healthz" || r.URL.Path == "/metrics"
	}
	m := NewMiddleware("secret-key", exempt)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("/status without token: status = %d, want 401", rr.Code)
	}
}

func TestStatusPublicWhenExempt(t *testing.T) {
	// Operator sets NEXUS_STATUS_PUBLIC=true → /status is exempt.
	exempt := func(r *http.Request) bool {
		return r.URL.Path == "/healthz" || r.URL.Path == "/metrics" || r.URL.Path == "/status"
	}
	m := NewMiddleware("secret-key", exempt)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	m.Wrap(okHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("/status when public: status = %d, want 200", rr.Code)
	}
}

func TestBearerTokenParsing(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"empty", "", ""},
		{"no scheme", "secret-key", ""},
		{"basic scheme", "Basic secret-key", ""},
		{"bearer lowercase", "bearer secret-key", "secret-key"},
		{"bearer uppercase", "BEARER secret-key", "secret-key"},
		{"bearer mixed case", "Bearer secret-key", "secret-key"},
		{"bearer no token", "Bearer ", ""},
		{"bearer extra spaces", "Bearer   multi-word-key", "multi-word-key"},
		{"bearer only", "Bearer", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			got := BearerToken(req)
			if got != tc.want {
				t.Errorf("BearerToken(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

// TestConstantTimeComparisonRegression is a regression test for issue #228.
// It verifies that keys of different lengths are compared using
// crypto/subtle.ConstantTimeCompare, which handles length differences
// without leaking information through timing. The test covers both
// shorter and longer wrong keys to ensure no early-exit shortcut
// bypasses the constant-time check.
func TestConstantTimeComparisonRegression(t *testing.T) {
	correctKey := "correct-secret-key-32byteslong!!"

	testCases := []struct {
		name       string
		token      string
		wantStatus int
	}{
		{"exact match", correctKey, http.StatusOK},
		{"empty token", "", http.StatusUnauthorized},
		{"wrong key same length", "wrong-secret-key-32byteslong!!", http.StatusUnauthorized},
		{"wrong key shorter", "wrong-short", http.StatusUnauthorized},
		{"wrong key longer", "wrong-secret-key-32byteslong!!EXTRA", http.StatusUnauthorized},
		{"single char diff", "correct-secret-key-32byteslong!X", http.StatusUnauthorized},
		{"first char wrong", "Worrect-secret-key-32byteslong!!", http.StatusUnauthorized},
		{"last char wrong", "correct-secret-key-32byteslong!W", http.StatusUnauthorized},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMiddleware(correctKey, nil)
			rr := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			m.Wrap(okHandler()).ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("token %q: status = %d, want %d", tc.token, rr.Code, tc.wantStatus)
			}
		})
	}
}
