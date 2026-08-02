package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/config"
)

// TestRegisterDebugPprofDisabled verifies that when DebugPprofEnabled
// is false (the default), /debug/* paths return 404 (issue #1150).
func TestRegisterDebugPprofDisabled(t *testing.T) {
	cfg := config.Config{DebugPprofEnabled: false}
	mux := http.NewServeMux()
	RegisterDebugPprof(mux, cfg)

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("disabled: want 404, got %d", rec.Code)
	}
}

// TestRegisterDebugPprofLoopback verifies that when enabled with no
// API key, a loopback request succeeds and returns valid content
// (issue #1150).
func TestRegisterDebugPprofLoopback(t *testing.T) {
	cfg := config.Config{DebugPprofEnabled: true}
	mux := http.NewServeMux()
	RegisterDebugPprof(mux, cfg)

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("loopback enabled: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestRegisterDebugPprofNonLoopback verifies that when enabled with no
// API key, a non-loopback request gets 403 (issue #1150).
func TestRegisterDebugPprofNonLoopback(t *testing.T) {
	cfg := config.Config{DebugPprofEnabled: true}
	mux := http.NewServeMux()
	RegisterDebugPprof(mux, cfg)

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.RemoteAddr = "10.0.0.5:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("non-loopback: want 403, got %d", rec.Code)
	}
}

// TestRegisterDebugPprofKeyGated verifies that when an API key is set,
// a request without the correct Bearer token gets 401 (issue #1150).
func TestRegisterDebugPprofKeyGated(t *testing.T) {
	cfg := config.Config{
		DebugPprofEnabled: true,
		DebugPprofAPIKey:  "secret-key-123",
	}
	mux := http.NewServeMux()
	RegisterDebugPprof(mux, cfg)

	// No Authorization header → 401.
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no bearer: want 401, got %d", rec.Code)
	}

	// Wrong key → 401.
	req2 := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req2.RemoteAddr = "127.0.0.1:12345"
	req2.Header.Set("Authorization", "Bearer wrong-key")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("wrong bearer: want 401, got %d", rec2.Code)
	}
}

// TestRegisterDebugPprofKeyValid verifies that when an API key is set,
// a request with the correct Bearer token succeeds — even from a
// non-loopback address (issue #1150).
func TestRegisterDebugPprofKeyValid(t *testing.T) {
	cfg := config.Config{
		DebugPprofEnabled: true,
		DebugPprofAPIKey:  "secret-key-123",
	}
	mux := http.NewServeMux()
	RegisterDebugPprof(mux, cfg)

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.RemoteAddr = "10.0.0.5:12345"
	req.Header.Set("Authorization", "Bearer secret-key-123")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("valid key non-loopback: want 200, got %d", rec.Code)
	}
}

// TestRegisterDebugPprofExpvar verifies that /debug/vars serves the
// expvar handler when enabled (issue #1150).
func TestRegisterDebugPprofExpvar(t *testing.T) {
	cfg := config.Config{DebugPprofEnabled: true}
	mux := http.NewServeMux()
	RegisterDebugPprof(mux, cfg)

	req := httptest.NewRequest(http.MethodGet, "/debug/vars", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expvar: want 200, got %d", rec.Code)
	}
}

// TestRegisterDebugPprofIPv6Loopback verifies that IPv6 loopback (::1)
// is treated as loopback (issue #1150).
func TestRegisterDebugPprofIPv6Loopback(t *testing.T) {
	cfg := config.Config{DebugPprofEnabled: true}
	mux := http.NewServeMux()
	RegisterDebugPprof(mux, cfg)

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.RemoteAddr = "[::1]:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("ipv6 loopback: want 200, got %d", rec.Code)
	}
}

// TestDebugPprofExposureMode verifies the exposure mode string used by
// the nexus check diagnostic (issue #1150).
func TestDebugPprofExposureMode(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"disabled", config.Config{}, "disabled"},
		{"localhost", config.Config{DebugPprofEnabled: true}, "localhost"},
		{"api-key", config.Config{DebugPprofEnabled: true, DebugPprofAPIKey: "k"}, "api-key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DebugPprofExposureMode(tt.cfg)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
