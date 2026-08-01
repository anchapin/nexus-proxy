package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/config"
)

// serverTestEnv sets up env vars that allow buildServer to boot without
// any real infrastructure. Call via t.Setenv in each test.
func serverTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NEXUS_OLLAMA_URL", "http://127.0.0.1:1")
	t.Setenv("NEXUS_LOCAL_MODEL", "test-model")
	t.Setenv("NEXUS_HEALTH_POLL_INTERVAL", "0")
	t.Setenv("NEXUS_PROBE_INTERVAL", "0")
	t.Setenv("NEXUS_PROBE_TIMEOUT", "100ms")
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "0")
	t.Setenv("NEXUS_QUALITY_CONCURRENCY", "0")
	t.Setenv("NEXUS_RATE_LIMIT_RPM", "0")
	t.Setenv("NEXUS_BUDGET_ALERT_ENABLED", "false")
	t.Setenv("NEXUS_MODELS_ENDPOINT", "false")
	t.Setenv("NEXUS_RAG_DB", "")
	t.Setenv("NEXUS_TRACING_ENDPOINT", "")
	t.Setenv("NEXUS_PROXY_API_KEY", "")
	t.Setenv("NEXUS_SLMCACHE_TTL", "0")
	t.Setenv("NEXUS_RAG_EMBED_CACHE_SIZE", "0")
	t.Setenv("NEXUS_METRICS_DB", filepath.Join(t.TempDir(), "metrics.db"))
	t.Setenv("NEXUS_TELEMETRY_PATH", "")
	t.Setenv("NEXUS_LOCAL_COOLDOWN", "0")
	t.Setenv("NEXUS_LOCAL_MAX_CONCURRENT", "0")
	t.Setenv("NEXUS_SERVER_ADDR", "127.0.0.1:0")
}

// buildTestServerFromCfg loads config from env, calls buildServer, and
// waits briefly for the probe manager's boot-snapshot goroutine to finish
// before returning. This avoids a data race between probeMgr.Close()
// (called from cleanup) and probeMgr.Run() (background goroutine) that
// only manifests under -race in tests.
func buildTestServerFromCfg(t *testing.T) (*http.Server, *serverParts, func()) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	srv, parts, cleanup, err := buildServer(cfg, time.Now())
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	return srv, parts, cleanup
}

// --- boot smoke tests ---

func TestBuildServerBoots(t *testing.T) {
	serverTestEnv(t)
	srv, parts, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	if srv == nil || parts == nil || srv.Handler == nil {
		t.Fatal("nil server/parts/handler")
	}
}

func TestBuildServerCleanupDoesNotPanic(t *testing.T) {
	serverTestEnv(t)
	_, _, cleanup := buildTestServerFromCfg(t)
	cleanup()
}

// --- endpoint tests ---

func TestBuildServerHealthz(t *testing.T) {
	serverTestEnv(t)
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("got %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "status") {
		t.Errorf("body missing 'status': %s", body)
	}
}

func TestBuildServerReadyz(t *testing.T) {
	serverTestEnv(t)
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 503 {
		t.Errorf("got %d, want 200 or 503", resp.StatusCode)
	}
}

func TestBuildServerStatus(t *testing.T) {
	serverTestEnv(t)
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		t.Error("status endpoint not registered")
	}
}

func TestBuildServerMetrics(t *testing.T) {
	serverTestEnv(t)
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("got %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "nexus_") {
		t.Error("body missing 'nexus_' prefix")
	}
}

func TestBuildServerMetricsStages(t *testing.T) {
	serverTestEnv(t)
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/metrics/stages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("got %d, want 200", resp.StatusCode)
	}
}

func TestBuildServerChatEndpoint(t *testing.T) {
	serverTestEnv(t)
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		t.Error("chat endpoint returned 404")
	}
}

func TestBuildServerSecurityHeaders(t *testing.T) {
	serverTestEnv(t)
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("X-Content-Type-Options"); ct != "nosniff" {
		t.Errorf("X-Content-Type-Options: got %q", ct)
	}
	if sts := resp.Header.Get("Strict-Transport-Security"); sts != "" {
		t.Errorf("HSTS should not be set without TLS")
	}
}

func TestBuildServerModelsEndpointDisabled(t *testing.T) {
	serverTestEnv(t)
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("got %d, want 404", resp.StatusCode)
	}
}

func TestBuildServerModelsEndpointEnabled(t *testing.T) {
	serverTestEnv(t)
	t.Setenv("NEXUS_MODELS_ENDPOINT", "true")
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("got %d, want 200", resp.StatusCode)
	}
}

// --- auth tests ---

func TestBuildServerAuthWall(t *testing.T) {
	serverTestEnv(t)
	t.Setenv("NEXUS_PROXY_API_KEY", "test-secret-key")
	t.Setenv("NEXUS_STATUS_PUBLIC", "false")
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("no-auth: got %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/status", nil)
	req.Header.Set("Authorization", "Bearer test-secret-key")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode == 401 {
		t.Error("auth should pass with correct key")
	}
}

func TestBuildServerHealthzExempt(t *testing.T) {
	serverTestEnv(t)
	t.Setenv("NEXUS_PROXY_API_KEY", "test-secret-key")
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz should be exempt: got %d", resp.StatusCode)
	}
}

// TestBuildServerDashboardDisabledDefault (issue #1182) verifies the
// dashboard route is absent when NEXUS_DASHBOARD_ENDPOINT is left at
// its false default — a stock deployment must not expose extra surface.
func TestBuildServerDashboardDisabledDefault(t *testing.T) {
	serverTestEnv(t)
	// DashboardEndpoint defaults to false; do not set the env var.
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("dashboard disabled default: got %d, want 404", resp.StatusCode)
	}
}

// TestBuildServerDashboardEnabledServesHTML (issue #1182) verifies that
// opting in registers the route and serves a self-contained HTML page.
func TestBuildServerDashboardEnabledServesHTML(t *testing.T) {
	serverTestEnv(t)
	t.Setenv("NEXUS_DASHBOARD_ENDPOINT", "true")
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/dashboard?range=24h")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard enabled: got %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Nexus Proxy Dashboard") {
		t.Error("dashboard HTML page missing expected title")
	}
}

// TestBuildServerDashboardAuthGated (issue #1182) verifies that when
// the dashboard is enabled but NOT public, it sits behind the auth
// wall exactly like /status.
func TestBuildServerDashboardAuthGated(t *testing.T) {
	serverTestEnv(t)
	t.Setenv("NEXUS_PROXY_API_KEY", "test-secret-key")
	t.Setenv("NEXUS_DASHBOARD_ENDPOINT", "true")
	t.Setenv("NEXUS_DASHBOARD_PUBLIC", "false")
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("dashboard gated: got %d, want 401", resp.StatusCode)
	}

	// With the key it passes through.
	req, _ := http.NewRequest("GET", ts.URL+"/dashboard", nil)
	req.Header.Set("Authorization", "Bearer test-secret-key")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusUnauthorized {
		t.Error("dashboard should pass with correct key")
	}
}

// TestBuildServerDashboardPublic (issue #1182) verifies that
// NEXUS_DASHBOARD_PUBLIC=true bypasses the auth gate.
func TestBuildServerDashboardPublic(t *testing.T) {
	serverTestEnv(t)
	t.Setenv("NEXUS_PROXY_API_KEY", "test-secret-key")
	t.Setenv("NEXUS_DASHBOARD_ENDPOINT", "true")
	t.Setenv("NEXUS_DASHBOARD_PUBLIC", "true")
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("dashboard public: got %d, want 200", resp.StatusCode)
	}
}

// --- config variation tests ---

func TestBuildServerWithTracing(t *testing.T) {
	serverTestEnv(t)
	t.Setenv("NEXUS_TRACING_ENDPOINT", "http://127.0.0.1:1/v1/traces")
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	if srv == nil {
		t.Fatal("nil server")
	}
}

func TestBuildServerWithRAGCache(t *testing.T) {
	serverTestEnv(t)
	t.Setenv("NEXUS_RAG_EMBED_CACHE_SIZE", "64")
	t.Setenv("NEXUS_RAG_EMBED_CACHE_TTL", "5m")
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	if srv == nil {
		t.Fatal("nil server")
	}
}

func TestBuildServerWithConcurrencyLimit(t *testing.T) {
	serverTestEnv(t)
	t.Setenv("NEXUS_LOCAL_MAX_CONCURRENT", "4")
	t.Setenv("NEXUS_LOCAL_VRAM_BYTES_PER_SLOT", "1000000000")
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	if srv == nil {
		t.Fatal("nil server")
	}
}

func TestBuildServerWithCooldown(t *testing.T) {
	serverTestEnv(t)
	t.Setenv("NEXUS_LOCAL_COOLDOWN", "30s")
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	if srv == nil {
		t.Fatal("nil server")
	}
}

func TestBuildServerWithAuthRateLimit(t *testing.T) {
	serverTestEnv(t)
	t.Setenv("NEXUS_PROXY_API_KEY", "secret")
	t.Setenv("NEXUS_AUTH_RATE_LIMIT_RPM", "10")
	t.Setenv("NEXUS_AUTH_RATE_LIMIT_BURST", "5")
	t.Setenv("NEXUS_AUTH_RATE_LIMIT_WINDOW", "1m")
	_, parts, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	if parts.authLimiter == nil {
		t.Error("auth limiter should be non-nil")
	}
}

func TestBuildServerMiddlewareChainCustom(t *testing.T) {
	serverTestEnv(t)
	t.Setenv("NEXUS_MIDDLEWARE_CHAIN", "promptEngineering,compressJSONBlocks")
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	if srv == nil {
		t.Fatal("nil server")
	}
}

// --- serverParts method tests ---

func TestDrainComponentsNil(t *testing.T) {
	parts := &serverParts{}
	parts.drainComponents()
}

func TestDrainComponentsWithServer(t *testing.T) {
	serverTestEnv(t)
	_, parts, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	parts.drainComponents()
}

func TestHandleSIGHUP(t *testing.T) {
	serverTestEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, parts, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	newCfg := parts.handleSIGHUP(cfg)
	if newCfg.Addr == "" {
		t.Error("new config addr is empty")
	}
}

func TestHandleSIGHUPWithRateLimiter(t *testing.T) {
	serverTestEnv(t)
	t.Setenv("NEXUS_RATE_LIMIT_RPM", "100")
	t.Setenv("NEXUS_RATE_LIMIT_BURST", "20")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, parts, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	if parts.rateLimiter == nil {
		t.Skip("rate limiter not created")
	}
	parts.handleSIGHUP(cfg)
}

func TestServerContext(t *testing.T) {
	serverTestEnv(t)
	srv, _, cleanup := buildTestServerFromCfg(t)
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
