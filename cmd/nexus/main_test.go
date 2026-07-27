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
	"time"

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
		// Issue #676: force NewJSONLRecorder to fail by using a path that
		// triggers mkdir error (/sys/nexus is not writable). This exercises
		// the "r, err := NewJSONLRecorder(...); if err != nil { return
		// telemetry.Noop{} }" fallback branch that was previously untested.
		{"NewJSONLRecorderError", "/sys/nexus/telemetry.jsonl", true},
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

	// Issue #676: verify the returned Noop is not the same pointer on each
	// call (stateless factory). Noop{} is a struct value type returned by
	// value (not pointer), so each call returns a distinct struct value.
	// The factory is inherently stateless for value types.
	t.Run("StatelessNoopFactory", func(t *testing.T) {
		t.Setenv("NEXUS_TELEMETRY_PATH", "/sys/nexus/telemetry.jsonl")
		cfg, _ := config.Load()

		rec1 := buildRecorder(cfg)
		rec2 := buildRecorder(cfg)

		_, ok1 := rec1.(telemetry.Noop)
		_, ok2 := rec2.(telemetry.Noop)
		if !ok1 || !ok2 {
			t.Fatal("expected both recorders to be Noop")
		}

		// Noop is a struct value type returned by value, not a pointer.
		// Each call to telemetry.Noop{} produces a fresh struct value,
		// so the factory is inherently stateless for value types.
	})
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
		// Issue #678: force the OpenWithRetention error path by using a
		// directory that exists but is not writable (/sys/nexus).  This
		// exercises the "store, err := OpenWithRetention(...); if err != nil
		// { return nil, nil }" branch that was previously untested.
		{"OpenError", "/sys/nexus/metrics.db", true},
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

// mockCollector records calls to circuit-breaker and embedder methods for
// testing the circuitBreakerAdapter.
type mockCollector struct {
	RecordCircuitFailureArg    string
	RecordCircuitRecoveryArg   string
	IncEmbedderFailureArg      string
	RecordCircuitFailureCalls  int
	RecordCircuitRecoveryCalls int
	IncEmbedderFailureCalls    int
}

func (m *mockCollector) RecordCircuitFailure(circuit string) {
	m.RecordCircuitFailureArg = circuit
	m.RecordCircuitFailureCalls++
}

func (m *mockCollector) RecordCircuitRecovery(circuit string) {
	m.RecordCircuitRecoveryArg = circuit
	m.RecordCircuitRecoveryCalls++
}

func (m *mockCollector) IncEmbedderFailure(kind string) {
	m.IncEmbedderFailureArg = kind
	m.IncEmbedderFailureCalls++
}

// TestCircuitBreakerAdapterCalls verifies the circuitBreakerAdapter correctly
// forwards each method call to the underlying collector with the correct
// argument. This was the 0%-covered symbol described in issue #575 — a
// one-line swap (e.g. RecordCircuitFailure ↔ RecordCircuitRecovery) would
// silently corrupt circuit-breaker metrics on the Prometheus /metrics endpoint
// without any unit-test catching it.
func TestCircuitBreakerAdapterCalls(t *testing.T) {
	t.Run("RecordCircuitFailure", func(t *testing.T) {
		mock := &mockCollector{}
		adapter := circuitBreakerAdapter{
			recordFailure: mock.RecordCircuitFailure,
		}
		adapter.RecordCircuitFailure("rag")
		if mock.RecordCircuitFailureArg != "rag" {
			t.Errorf("RecordCircuitFailure arg = %q, want %q", mock.RecordCircuitFailureArg, "rag")
		}
		if mock.RecordCircuitFailureCalls != 1 {
			t.Errorf("RecordCircuitFailure calls = %d, want 1", mock.RecordCircuitFailureCalls)
		}
	})

	t.Run("RecordCircuitRecovery", func(t *testing.T) {
		mock := &mockCollector{}
		adapter := circuitBreakerAdapter{
			recordRecovery: mock.RecordCircuitRecovery,
		}
		adapter.RecordCircuitRecovery("ollama")
		if mock.RecordCircuitRecoveryArg != "ollama" {
			t.Errorf("RecordCircuitRecovery arg = %q, want %q", mock.RecordCircuitRecoveryArg, "ollama")
		}
		if mock.RecordCircuitRecoveryCalls != 1 {
			t.Errorf("RecordCircuitRecovery calls = %d, want 1", mock.RecordCircuitRecoveryCalls)
		}
	})

	t.Run("IncEmbedderFailure", func(t *testing.T) {
		mock := &mockCollector{}
		adapter := circuitBreakerAdapter{
			incEmbedderFailure: mock.IncEmbedderFailure,
		}
		adapter.IncEmbedderFailure("cohere")
		if mock.IncEmbedderFailureArg != "cohere" {
			t.Errorf("IncEmbedderFailure arg = %q, want %q", mock.IncEmbedderFailureArg, "cohere")
		}
		if mock.IncEmbedderFailureCalls != 1 {
			t.Errorf("IncEmbedderFailure calls = %d, want 1", mock.IncEmbedderFailureCalls)
		}
	})

	t.Run("AllMethodsTogether", func(t *testing.T) {
		mock := &mockCollector{}
		adapter := circuitBreakerAdapter{
			recordFailure:      mock.RecordCircuitFailure,
			recordRecovery:     mock.RecordCircuitRecovery,
			incEmbedderFailure: mock.IncEmbedderFailure,
		}
		adapter.RecordCircuitFailure("local")
		adapter.RecordCircuitRecovery("fusion")
		adapter.IncEmbedderFailure("openai")

		if mock.RecordCircuitFailureArg != "local" {
			t.Errorf("RecordCircuitFailure arg = %q, want %q", mock.RecordCircuitFailureArg, "local")
		}
		if mock.RecordCircuitRecoveryArg != "fusion" {
			t.Errorf("RecordCircuitRecovery arg = %q, want %q", mock.RecordCircuitRecoveryArg, "fusion")
		}
		if mock.IncEmbedderFailureArg != "openai" {
			t.Errorf("IncEmbedderFailure arg = %q, want %q", mock.IncEmbedderFailureArg, "openai")
		}
		if mock.RecordCircuitFailureCalls != 1 || mock.RecordCircuitRecoveryCalls != 1 || mock.IncEmbedderFailureCalls != 1 {
			t.Errorf("call counts: failures=%d, recoveries=%d, embedder=%d; want all 1",
				mock.RecordCircuitFailureCalls, mock.RecordCircuitRecoveryCalls, mock.IncEmbedderFailureCalls)
		}
	})
}

// stubProbe is a minimal Probe implementation for testing budgetObserver.
type stubProbe struct {
	budget probe.Budget
}

func (s *stubProbe) Budget(_ context.Context) (probe.Budget, error) {
	return s.budget, nil
}

// TestBudgetObserver verifies the budget observer returns safe defaults
// when the probe manager is nil, and correctly reflects the manager's
// current budget otherwise (issue #6).
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

	t.Run("ValidManager", func(t *testing.T) {
		stub := &stubProbe{budget: probe.Budget{Tokens: 8192, Source: probe.SourceOllamaPS}}
		mgr := probe.NewManager(stub, time.Hour, time.Second)
		mgr.Run(context.Background())
		defer mgr.Close()

		obs := budgetObserver(mgr)
		if obs.BudgetTokens() != 8192 {
			t.Errorf("BudgetTokens() = %d, want %d", obs.BudgetTokens(), 8192)
		}
		if obs.BudgetSource() != string(probe.SourceOllamaPS) {
			t.Errorf("BudgetSource() = %q, want %q", obs.BudgetSource(), probe.SourceOllamaPS)
		}
	})

	t.Run("EmptySourceFallback", func(t *testing.T) {
		stub := &stubProbe{budget: probe.Budget{Tokens: 4096, Source: ""}}
		mgr := probe.NewManager(stub, time.Hour, time.Second)
		mgr.Run(context.Background())
		defer mgr.Close()

		obs := budgetObserver(mgr)
		if obs.BudgetTokens() != 4096 {
			t.Errorf("BudgetTokens() = %d, want %d", obs.BudgetTokens(), 4096)
		}
		if obs.BudgetSource() != string(probe.SourceStatic) {
			t.Errorf("BudgetSource() with empty source = %q, want %q", obs.BudgetSource(), probe.SourceStatic)
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
			//                       handlers.Recover(nil)(rootHandler))
			handler := handlers.SecurityHeaders(tt.tlsEnabled)(handlers.Recover(nil)(inner))

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
			h := handlers.SecurityHeaders(cfg.TLSEnabled)(handlers.Recover(nil)(
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

// TestPublicPathExempt (issue #538) is the regression guard for the
// auth-bypass predicate wired into the inbound auth middleware
// (cmd/nexus/main.go:1179 → auth.NewMiddleware). publicPathExempt is the
// single function an attacker would target to silently disable
// authentication on protected surfaces — this test pins its four branches
// (always-exempt paths, status-public-on, status-public-off, fallthrough)
// and documents the real path-normalization behavior so a refactor that
// flips a case or drops cfg.StatusPublic cannot ship unnoticed.
//
// Security posture documented here (and asserted below): the predicate is
// deliberately conservative — it returns false for anything that is not an
// EXACT match for an exempt path. Trailing slashes, case variants, double
// slashes, and traversal sequences (/healthz/../etc) all fall through to
// the default branch and therefore REQUIRE auth. There is no path
// normalisation, so there is no traversal bypass surface.
func TestPublicPathExempt(t *testing.T) {
	// alwaysExempt are hard-coded bypass paths (issue #109). They must be
	// exempt regardless of StatusPublic so K8s probes and Prometheus
	// scrapers work without credentials — dropping one would lock out
	// liveness probes.
	alwaysExempt := []string{"/healthz", "/metrics", "/readyz"}

	tests := []struct {
		name         string
		statusPublic bool
		method       string
		path         string // raw request target (path[?query])
		want         bool
	}{
		// --- Branch 1: always-exempt paths, StatusPublic=false (default) ---
		{"healthz exempt default", false, http.MethodGet, "/healthz", true},
		{"metrics exempt default", false, http.MethodGet, "/metrics", true},
		{"readyz exempt default", false, http.MethodGet, "/readyz", true},
		// --- Branch 1b: always-exempt paths also exempt when StatusPublic=true ---
		{"healthz exempt status-public", true, http.MethodGet, "/healthz", true},
		{"metrics exempt status-public", true, http.MethodGet, "/metrics", true},
		{"readyz exempt status-public", true, http.MethodGet, "/readyz", true},

		// --- Branch 2: /status exempt ONLY when StatusPublic=true ---
		{"status exempt when public", true, http.MethodGet, "/status", true},
		// --- Branch 3: /status NOT exempt when StatusPublic=false (security-critical default) ---
		// A regression here re-exposes the diagnostics surface (frontier
		// config, judge state, VRAM) without auth — the exact bug #109 fixed.
		{"status gated by default", false, http.MethodGet, "/status", false},

		// --- Branch 4: fallthrough — protected paths never exempt ---
		{"chat completions protected", false, http.MethodPost, "/v1/chat/completions", false},
		{"chat completions protected status-public", true, http.MethodPost, "/v1/chat/completions", false},
		{"root protected", false, http.MethodGet, "/", false},
		{"unknown path protected", false, http.MethodGet, "/admin/users", false},
		{"near-miss statusz protected", false, http.MethodGet, "/statusz", false},
		{"empty-ish path protected", false, http.MethodGet, "/status/", false},

		// --- Path-normalisation edges (all REQUIRE auth — safe direction) ---
		// Query strings are stripped from URL.Path, so /healthz?x=1 → /healthz
		// remains exempt. This is the ONLY normalisation that occurs and it is
		// harmless (query params don't change the protectedness of a path).
		{"healthz with query exempt", false, http.MethodGet, "/healthz?probe=ready", true},
		{"status with query gated", false, http.MethodGet, "/status?detail=1", false},
		// Trailing slash is NOT exempt — exact-match only.
		{"healthz trailing slash gated", false, http.MethodGet, "/healthz/", false},
		{"metrics trailing slash gated", false, http.MethodGet, "/metrics/", false},
		// Case sensitivity: /HEALTHZ does not match.
		{"uppercase healthz gated", false, http.MethodGet, "/HEALTHZ", false},
		{"mixed case status gated", true, http.MethodGet, "/Status", false},
		// Double slashes are NOT exempt.
		{"double-leading slash gated", false, http.MethodGet, "//healthz", false},
		{"double-trailing slash gated", false, http.MethodGet, "/healthz//", false},
		// Path traversal: url.Parse does NOT clean the path, so
		// /healthz/../v1/chat/completions stays as-is, misses the exact match,
		// and falls through to default (false) → requires auth. Safe.
		{"traversal to healthz gated", false, http.MethodGet, "/healthz/../etc", false},
		{"traversal from protected gated", false, http.MethodGet, "/v1/chat/completions/../healthz", false},
		// Encoded slash (%2f) is decoded into URL.Path by the server, but the
		// decoded form is not an exact exempt match either way.
		{"encoded slash healthz gated", false, http.MethodGet, "/healthz%2f", false},

		// --- Method insensitivity ---
		// The predicate reads only r.URL.Path; method is never consulted. A
		// POST to /healthz is therefore exempt too. This is acceptable: the
		// healthz handler only answers GET, and exempt == "skip bearer check",
		// not "grant access to data". Assert the method-agnostic behaviour so
		// a future change is intentional.
		{"healthz POST exempt", false, http.MethodPost, "/healthz", true},
		{"healthz PUT exempt", false, http.MethodPut, "/healthz", true},
		{"healthz DELETE exempt", false, http.MethodDelete, "/healthz", true},
		{"status POST gated by default", false, http.MethodPost, "/status", false},
		{"status POST exempt when public", true, http.MethodPost, "/status", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{StatusPublic: tt.statusPublic}
			exempt := publicPathExempt(cfg)

			req := httptest.NewRequest(tt.method, tt.path, nil)
			got := exempt(req)
			if got != tt.want {
				t.Errorf("publicPathExempt(StatusPublic=%v)(%s %q) = %v, want %v",
					tt.statusPublic, tt.method, tt.path, got, tt.want)
			}
		})
	}

	// Sanity: exhaustively assert every always-exempt path is exempt under
	// BOTH StatusPublic postures, so a dropped case entry cannot slip past.
	for _, public := range []bool{false, true} {
		for _, p := range alwaysExempt {
			if !publicPathExempt(config.Config{StatusPublic: public})(httptest.NewRequest(http.MethodGet, p, nil)) {
				t.Errorf("always-exempt path %q must bypass auth under StatusPublic=%v", p, public)
			}
		}
	}

	// cfg is captured by value: the decision is fixed at construction time,
	// so mutating the original struct after building the predicate must NOT
	// flip /status from gated to exempt. (This guards against an accidental
	// pointer-capture refactor.)
	cfg := config.Config{StatusPublic: false}
	exempt := publicPathExempt(cfg)
	cfg.StatusPublic = true // mutate the caller's copy
	if exempt(httptest.NewRequest(http.MethodGet, "/status", nil)) {
		t.Error("mutating caller cfg after predicate construction affected /status; cfg must be captured by value")
	}
	// And the converse: a freshly-built predicate with StatusPublic=true
	// immediately exempts /status.
	if !publicPathExempt(config.Config{StatusPublic: true})(httptest.NewRequest(http.MethodGet, "/status", nil)) {
		t.Error("predicate built with StatusPublic=true must exempt /status")
	}
}
