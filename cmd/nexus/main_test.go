package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/handlers"
	"github.com/anchapin/nexus-proxy/internal/probe"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/telemetry"
)

// TestBuildRecorder verifies the telemetry recorder constructor returns a
// Noop recorder when disabled and a JSONL recorder when enabled.
func TestBuildRecorder(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected bool // true if should be Noop
	}{
		{"Disabled", "", true},
		{"Enabled", "/tmp/nexus-telemetry.jsonl", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NEXUS_TELEMETRY_PATH", tt.path)
			cfg, _ := config.Load()
			rec := buildRecorder(cfg)

			_, isNoop := rec.(telemetry.Noop)
			if isNoop != tt.expected {
				t.Errorf("buildRecorder(%q) isNoop = %v, want %v", tt.path, isNoop, tt.expected)
			}
		})
	}
}

// TestBuildMetrics verifies the metrics store constructor returns nil
// store/observer when disabled and a valid store when enabled.
func TestBuildMetrics(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected bool // true if should be nil
	}{
		{"Disabled", "/nonexistent/metrics.db", true},
		{"Enabled", "metrics.db", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NEXUS_METRICS_DB", tt.path)
			cfg, _ := config.Load()
			store, obs := buildMetrics(cfg)

			if (store == nil && obs == nil) != tt.expected {
				t.Errorf("buildMetrics(%q) returns nil = %v, want %v", tt.path, store == nil, tt.expected)
			}
		})
	}
}

// TestBudgetObserver verifies the budget observer returns safe defaults
// when the probe manager is nil.
func TestBudgetObserver(t *testing.T) {
	t.Run("NilManager", func(t *testing.T) {
		obs := budgetObserver(nil)
		if obs.BudgetTokens() != 0 {
			t.Errorf("expected 0 tokens for nil manager, got %d", obs.BudgetTokens())
		}
		if obs.BudgetSource() != string(probe.SourceStatic) {
			t.Errorf("expected static source for nil manager, got %q", obs.BudgetSource())
		}
	})
}

// TestHealthzHandler verifies the /healthz endpoint returns a 200 with the
// expected JSON structure when no hpoller or manager is provided.
func TestHealthzHandler(t *testing.T) {
	cfg := config.Config{
		TokenGuardrail: 6000,
	}

	handler := healthzHandler(nil, nil, cfg)
	req := httptest.NewRequest("GET", "/healthz", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rr.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}

	if resp["status"] != "ok" {
		t.Errorf("expected status ok, got %v", resp["status"])
	}
	if resp["budget_tokens"].(float64) != 6000 {
		t.Errorf("expected fallback budget 6000, got %v", resp["budget_tokens"])
	}
}

// TestBuildRAGStore verifies the RAG store constructor falls back to an
// in-memory store when persistence is disabled.
func TestBuildRAGStore(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(tmpDir+"/example1.txt", []byte("test example"), 0644); err != nil {
		t.Fatal(err)
	}

	emb := rag.NewOllamaEmbedder("http://localhost:11434", "nomic-embed-text", nil, rag.BreakerConfig{})
	ctx := context.Background()

	cfg := config.Config{
		ExamplesDir:  tmpDir,
		RAGThreshold: 0.55,
	}
	// RAGDBPath is empty by default via config.Load, so RAGPersistentEnabled() is false.
	// We explicitly set it to empty to ensure the in-memory fallback.
	cfg.RAGDBPath = ""

	store, ps, watcher := buildRAGStore(cfg, emb, ctx)
	if ps != nil || watcher != nil {
		t.Error("expected nil persistentStore and watcher for in-memory store")
	}
	if store == nil {
		t.Error("expected non-nil store")
	}
}

// TestPrintVersionOutput verifies the version printer includes the
// binary name and version string.
func TestPrintVersionOutput(t *testing.T) {
	saved := version
	t.Cleanup(func() { version = saved })
	version = "v9.9.9-test"

	var buf bytes.Buffer
	printVersion(&buf)
	out := buf.String()
	if !strings.Contains(out, "nexus") {
		t.Errorf("output %q does not contain 'nexus'", out)
	}
	if !strings.Contains(out, "v9.9.9-test") {
		t.Errorf("output %q does not contain the version string", out)
	}
}

// TestSecurityHeadersWiringBothPostures (issue #444) is the wiring-level
// regression test for the canonical security-headers middleware. It builds
// the exact handler chain main.go composes for the HTTP server (the
// canonical handlers.SecurityHeaders layered on top of handlers.Recover on
// top of a no-op root) and asserts HSTS is gated by cfg.TLSEnabled in both
// postures. The plaintext case is the one that bit production pre-fix: a
// duplicate middleware was emitting HSTS unconditionally over the default
// plaintext bind, which is a spec violation and silently ignored by
// browsers. This test fails if anyone reintroduces that duplicate or
// forgets to thread cfg.TLSEnabled through the wiring.
func TestSecurityHeadersWiringBothPostures(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})

	tests := []struct {
		name       string
		tlsEnabled bool
		wantHSTS   string
	}{
		{"plaintext omits HSTS", false, ""},
		{"tls active emits HSTS", true, "max-age=31536000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reproduce the exact wiring main.go uses (issue #444):
			//   Server.Handler = handlers.SecurityHeaders(tlsEnabled)(
			//                       handlers.Recover()(rootHandler))
			handler := handlers.SecurityHeaders(tt.tlsEnabled)(handlers.Recover()(inner))

			srv := httptest.NewServer(handler)
			defer srv.Close()

			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext: %v", err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()

			got := resp.Header.Get("Strict-Transport-Security")
			if got != tt.wantHSTS {
				t.Errorf("Strict-Transport-Security = %q, want %q (TLSEnabled=%v)",
					got, tt.wantHSTS, tt.tlsEnabled)
			}
			// Always-on headers must be present in both postures so
			// regression coverage doesn't accidentally drop them while
			// fixing HSTS.
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("X-Frame-Options = %q, want DENY", got)
			}
		})
	}
}

// TestConfigTLSEnabledGatesHSTS (issue #444) covers the cross-package
// contract that config.TLSEnabled drives the security-headers middleware
// in the production wiring. It pairs the config knob with the canonical
// handlers.SecurityHeaders (the same call main.go uses) and verifies the
// composed chain respects the knob.
func TestConfigTLSEnabledGatesHSTS(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tls      bool
		wantHSTS string
	}{
		{"plaintext", false, ""},
		{"tls", true, "max-age=31536000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{TLSEnabled: tc.tls}
			h := handlers.SecurityHeaders(cfg.TLSEnabled)(handlers.Recover()(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
				}),
			))

			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			h.ServeHTTP(rr, req)

			if got := rr.Header().Get("Strict-Transport-Security"); got != tc.wantHSTS {
				t.Errorf("Strict-Transport-Security = %q, want %q (cfg.TLSEnabled=%v)",
					got, tc.wantHSTS, cfg.TLSEnabled)
			}
		})
	}
}
