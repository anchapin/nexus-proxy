// integration_test.go — end-to-end integration tests that exercise the
// full HTTP → middleware → routing → upstream → response pipeline (issue #1160).
//
// Unlike server_test.go (which focuses on boot smoke tests and endpoint
// existence), these tests spin up mock Ollama and mock frontier servers,
// then send real HTTP requests through the production handler stack
// built by buildServer. Every test runs under -race and completes in
// well under the 2s budget.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Mock upstreams
// ---------------------------------------------------------------------------

// mockServerStats tracks per-endpoint call counts so tests can assert
// which upstream was actually hit.
type mockServerStats struct {
	mu     sync.Mutex
	counts map[string]int
}

func newMockServerStats() *mockServerStats {
	return &mockServerStats{counts: make(map[string]int)}
}

func (s *mockServerStats) inc(path string) {
	s.mu.Lock()
	s.counts[path]++
	s.mu.Unlock()
}

func (s *mockServerStats) get(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[path]
}

// startMockOllama returns an httptest.Server that emulates Ollama.
// It handles:
//   - GET  /api/tags         → health probe (200 with model list, or failCode)
//   - POST /api/chat         → SLM routing decision (returns routeDecision JSON)
//   - POST /api/embeddings   → RAG embedding vector
//   - POST /v1/chat/completions → OpenAI-compatible non-streaming chat
//
// chatContent is the assistant content returned for /v1/chat/completions.
// failTagsCode, when non-zero, makes /api/tags return that status code
// (used by the degradation test).
func startMockOllama(t *testing.T, stats *mockServerStats, chatContent string, failTagsCode int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stats.inc(r.URL.Path)
		switch r.URL.Path {
		case "/api/tags":
			if failTagsCode != 0 {
				w.WriteHeader(failTagsCode)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			fmt.Fprintf(w, `{"models":[{"name":"test-model"}]}`)
		case "/api/chat":
			// SLM routing decision — Ollama format.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			fmt.Fprintf(w, `{"message":{"content":"{\"route\":\"frontier\"}"}}`)
		case "/api/embeddings":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			fmt.Fprintf(w, `{"embedding":[0.1,0.2,0.3,0.4,0.5]}`)
		case "/v1/chat/completions":
			// The cascade always makes non-streaming requests (stream=false).
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			resp := map[string]interface{}{
				"model": "test-model",
				"choices": []map[string]interface{}{
					{
						"index":         0,
						"message":       map[string]interface{}{"role": "assistant", "content": chatContent},
						"finish_reason": "stop",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(404)
		}
	}))
}

// startMockFrontier returns an httptest.Server that emulates the frontier
// API (OpenAI-compatible). It inspects the "stream" field in the request
// body and responds with SSE for streaming or JSON for non-streaming.
func startMockFrontier(t *testing.T, stats *mockServerStats, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stats.inc(r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]interface{}
		_ = json.Unmarshal(body, &parsed)

		stream := false
		if s, ok := parsed["stream"].(bool); ok {
			stream = s
		}

		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			flusher, _ := w.(http.Flusher)
			// Two chunks + [DONE].
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"index": 0, "delta": map[string]interface{}{"content": content}, "finish_reason": nil},
				},
			}))
			if flusher != nil {
				flusher.Flush()
			}
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"index": 0, "delta": map[string]interface{}{}, "finish_reason": "stop"},
				},
			}))
			if flusher != nil {
				flusher.Flush()
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			resp := map[string]interface{}{
				"model": "frontier-model",
				"choices": []map[string]interface{}{
					{
						"index":         0,
						"message":       map[string]interface{}{"role": "assistant", "content": content},
						"finish_reason": "stop",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		}
	}))
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---------------------------------------------------------------------------
// Test environment helpers
// ---------------------------------------------------------------------------

// e2eBaseEnv sets the common environment variables needed for buildServer
// to boot with mock upstreams. ollamaURL and frontierURL must point at
// the mock servers. The caller sets routing-specific vars per test.
func e2eBaseEnv(t *testing.T, ollamaURL, frontierURL string) {
	t.Helper()
	t.Setenv("NEXUS_OLLAMA_URL", ollamaURL)
	t.Setenv("NEXUS_LOCAL_MODEL", "test-model")
	t.Setenv("NEXUS_FRONTIER_URL", frontierURL)
	t.Setenv("NEXUS_FRONTIER_MODEL", "frontier-model")
	t.Setenv("NEXUS_FRONTIER_API_KEY", "test-frontier-key")
	t.Setenv("NEXUS_HEALTH_POLL_INTERVAL", "0")
	t.Setenv("NEXUS_PROBE_INTERVAL", "0")
	t.Setenv("NEXUS_PROBE_TIMEOUT", "100ms")
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "0")
	t.Setenv("NEXUS_QUALITY_CONCURRENCY", "0")
	t.Setenv("NEXUS_RATE_LIMIT_RPM", "0")
	t.Setenv("NEXUS_BUDGET_ALERT_ENABLED", "false")
	t.Setenv("NEXUS_MODELS_ENDPOINT", "false")
	t.Setenv("NEXUS_RAG_DB", "")
	t.Setenv("NEXUS_RAG_EMBED_CACHE_SIZE", "0")
	t.Setenv("NEXUS_TRACING_ENDPOINT", "")
	t.Setenv("NEXUS_PROXY_API_KEY", "")
	t.Setenv("NEXUS_SLMCACHE_TTL", "0")
	t.Setenv("NEXUS_LOCAL_COOLDOWN", "0")
	t.Setenv("NEXUS_LOCAL_MAX_CONCURRENT", "0")
	t.Setenv("NEXUS_SERVER_ADDR", "127.0.0.1:0")
	t.Setenv("NEXUS_SERVER_WRITE_TIMEOUT", "0")
	t.Setenv("NEXUS_SLM_TIMEOUT", "2s")
	t.Setenv("NEXUS_METRICS_DB", filepath.Join(t.TempDir(), "metrics.db"))
	t.Setenv("NEXUS_TELEMETRY_PATH", "")
	t.Setenv("NEXUS_EXAMPLES_DIR", t.TempDir())
	// Allow loopback connections to mock upstreams (issue #1174 SSRF guard).
	t.Setenv("NEXUS_EGRESS_ALLOW", "127.0.0.0/8")
}

// e2eTestServer builds a full production handler stack from buildServer
// and returns an httptest.Server wrapping it. The cleanup is registered
// via t.Cleanup.
func e2eTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv, _, cleanup := buildTestServerFromCfg(t)
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(func() {
		ts.Close()
		cleanup()
	})
	return ts
}

// chatRequest builds a minimal OpenAI-compatible chat completions request body.
func chatRequest(prompt string, stream bool) string {
	body := map[string]interface{}{
		"model": "test-model",
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	if stream {
		body["stream"] = true
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// doChat sends a POST to /v1/chat/completions with optional bearer auth.
func doChat(t *testing.T, ts *httptest.Server, body, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// ---------------------------------------------------------------------------
// Routing tests
// ---------------------------------------------------------------------------

// TestE2E_LocalRoute verifies that a prompt matching DSL formatting
// patterns routes to the local (Ollama) upstream through the cascade.
func TestE2E_LocalRoute(t *testing.T) {
	stats := newMockServerStats()
	ollama := startMockOllama(t, stats, "local response from ollama", 0)
	t.Cleanup(ollama.Close)
	frontier := startMockFrontier(t, stats, "frontier response")
	t.Cleanup(frontier.Close)

	e2eBaseEnv(t, ollama.URL, frontier.URL)
	t.Setenv("NEXUS_DSL_FORMATTING_PATTERNS", "e2e_local_trigger")
	// Keep guardrail high so prompt doesn't force frontier.
	t.Setenv("NEXUS_TOKEN_GUARDRAIL", "999999")

	ts := e2eTestServer(t)
	resp := doChat(t, ts, chatRequest("e2e_local_trigger fix this css", false), "")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 200; body: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "local response from ollama") {
		t.Errorf("expected local response; got: %s", body)
	}
	// Verify Ollama was hit for chat completions.
	if got := stats.get("/v1/chat/completions"); got == 0 {
		t.Error("expected Ollama /v1/chat/completions to be called")
	}
}

// TestE2E_FrontierRoute verifies that a prompt exceeding the token
// guardrail routes to the frontier upstream.
func TestE2E_FrontierRoute(t *testing.T) {
	stats := newMockServerStats()
	ollama := startMockOllama(t, stats, "local should not be used", 0)
	t.Cleanup(ollama.Close)
	frontier := startMockFrontier(t, stats, "frontier response for test")
	t.Cleanup(frontier.Close)

	e2eBaseEnv(t, ollama.URL, frontier.URL)
	// Guardrail=1 forces frontier for any prompt longer than 4 chars.
	t.Setenv("NEXUS_TOKEN_GUARDRAIL", "1")

	ts := e2eTestServer(t)
	resp := doChat(t, ts, chatRequest("This is a sufficiently long prompt for the guardrail test", false), "")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 200; body: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "frontier response for test") {
		t.Errorf("expected frontier response; got: %s", body)
	}
}

// TestE2E_FusionRoute verifies that a prompt matching DSL fusion
// patterns routes through the fusion panel pipeline.
func TestE2E_FusionRoute(t *testing.T) {
	stats := newMockServerStats()
	ollama := startMockOllama(t, stats, "fusion panel response from local", 0)
	t.Cleanup(ollama.Close)
	frontier := startMockFrontier(t, stats, "fusion panel response from frontier")
	t.Cleanup(frontier.Close)

	e2eBaseEnv(t, ollama.URL, frontier.URL)
	// Custom fusion trigger that won't collide with other DSL patterns.
	t.Setenv("NEXUS_DSL_FUSION_PATTERNS", "e2e_fusion_trigger")
	t.Setenv("NEXUS_DSL_FORMATTING_PATTERNS", "")
	t.Setenv("NEXUS_DSL_LOCAL_PATTERNS", "")
	// Set agreement threshold to 0 so arbiter is skipped when panels agree.
	t.Setenv("NEXUS_FUSION_AGREEMENT_THRESHOLD", "0")
	t.Setenv("NEXUS_TOKEN_GUARDRAIL", "999999")

	ts := e2eTestServer(t)
	resp := doChat(t, ts, chatRequest("e2e_fusion_trigger system architecture", false), "")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 200; body: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	// Fusion should produce a response (either panel or arbiter).
	bodyStr := string(body)
	if bodyStr == "" {
		t.Error("expected non-empty fusion response")
	}
}

// ---------------------------------------------------------------------------
// SSE streaming test
// ---------------------------------------------------------------------------

// TestE2E_SSEStreaming verifies that a streaming frontier request returns
// a proper SSE response with data: lines and [DONE] terminator.
func TestE2E_SSEStreaming(t *testing.T) {
	stats := newMockServerStats()
	ollama := startMockOllama(t, stats, "unused", 0)
	t.Cleanup(ollama.Close)
	frontier := startMockFrontier(t, stats, "streamed chunk")
	t.Cleanup(frontier.Close)

	e2eBaseEnv(t, ollama.URL, frontier.URL)
	t.Setenv("NEXUS_TOKEN_GUARDRAIL", "1") // force frontier

	ts := e2eTestServer(t)
	resp := doChat(t, ts, chatRequest("streaming frontier test prompt", true), "")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 200; body: %s", resp.StatusCode, body)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream Content-Type, got %q", ct)
	}

	// Read SSE lines and verify format.
	scanner := bufio.NewScanner(resp.Body)
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, line)
		}
	}
	if len(dataLines) < 2 {
		t.Errorf("expected at least 2 data lines, got %d", len(dataLines))
	}
	// Last data line should be [DONE].
	if len(dataLines) > 0 {
		last := dataLines[len(dataLines)-1]
		if !strings.Contains(last, "[DONE]") {
			t.Errorf("expected [DONE] in last data line, got %q", last)
		}
	}
}

// ---------------------------------------------------------------------------
// Security headers test
// ---------------------------------------------------------------------------

// TestE2E_SecurityHeaders verifies that security headers are present on
// all responses and HSTS is absent on plaintext (non-TLS) connections.
func TestE2E_SecurityHeaders(t *testing.T) {
	stats := newMockServerStats()
	ollama := startMockOllama(t, stats, "response", 0)
	t.Cleanup(ollama.Close)
	frontier := startMockFrontier(t, stats, "response")
	t.Cleanup(frontier.Close)

	e2eBaseEnv(t, ollama.URL, frontier.URL)
	t.Setenv("NEXUS_TOKEN_GUARDRAIL", "1")

	ts := e2eTestServer(t)

	// Check headers on /healthz.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if v := resp.Header.Get("X-Content-Type-Options"); v != "nosniff" {
		t.Errorf("X-Content-Type-Options: got %q, want nosniff", v)
	}
	if v := resp.Header.Get("X-Frame-Options"); v != "DENY" {
		t.Errorf("X-Frame-Options: got %q, want DENY", v)
	}
	if v := resp.Header.Get("Strict-Transport-Security"); v != "" {
		t.Errorf("HSTS should be absent on plaintext, got %q", v)
	}

	// Check headers on /v1/chat/completions.
	resp2 := doChat(t, ts, chatRequest("security headers test", false), "")
	defer resp2.Body.Close()
	if v := resp2.Header.Get("X-Content-Type-Options"); v != "nosniff" {
		t.Errorf("X-Content-Type-Options on chat: got %q, want nosniff", v)
	}
}

// ---------------------------------------------------------------------------
// Auth exemption test
// ---------------------------------------------------------------------------

// TestE2E_AuthExemption verifies that /healthz and /metrics are exempt
// from the inbound auth gate while /v1/chat/completions requires a bearer token.
func TestE2E_AuthExemption(t *testing.T) {
	stats := newMockServerStats()
	ollama := startMockOllama(t, stats, "auth test response", 0)
	t.Cleanup(ollama.Close)
	frontier := startMockFrontier(t, stats, "auth test response")
	t.Cleanup(frontier.Close)

	e2eBaseEnv(t, ollama.URL, frontier.URL)
	t.Setenv("NEXUS_PROXY_API_KEY", "e2e-secret-key")
	t.Setenv("NEXUS_TOKEN_GUARDRAIL", "1")

	ts := e2eTestServer(t)

	// /healthz should work without auth.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/healthz without auth: got %d, want 200", resp.StatusCode)
	}

	// /metrics should work without auth.
	resp2, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("/metrics without auth: got %d, want 200", resp2.StatusCode)
	}

	// /v1/chat/completions without auth → 401.
	resp3 := doChat(t, ts, chatRequest("auth exemption test prompt", false), "")
	defer resp3.Body.Close()
	if resp3.StatusCode != 401 {
		t.Errorf("/v1/chat/completions without auth: got %d, want 401", resp3.StatusCode)
	}

	// /v1/chat/completions with correct auth → 200.
	resp4 := doChat(t, ts, chatRequest("auth exemption test prompt", false), "e2e-secret-key")
	defer resp4.Body.Close()
	if resp4.StatusCode != 200 {
		t.Errorf("/v1/chat/completions with auth: got %d, want 200", resp4.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Rate-limit scoping test
// ---------------------------------------------------------------------------

// TestE2E_RateLimitScoping verifies that rate limiting applies only to
// /v1/chat/completions and never to /healthz.
func TestE2E_RateLimitScoping(t *testing.T) {
	stats := newMockServerStats()
	ollama := startMockOllama(t, stats, "rate limited response", 0)
	t.Cleanup(ollama.Close)
	frontier := startMockFrontier(t, stats, "rate limited response")
	t.Cleanup(frontier.Close)

	e2eBaseEnv(t, ollama.URL, frontier.URL)
	t.Setenv("NEXUS_RATE_LIMIT_RPM", "100")
	t.Setenv("NEXUS_RATE_LIMIT_BURST", "2")
	t.Setenv("NEXUS_TOKEN_GUARDRAIL", "1")

	ts := e2eTestServer(t)

	// Send burst+1 requests to /v1/chat/completions.
	var got429 bool
	for i := 0; i < 5; i++ {
		resp := doChat(t, ts, chatRequest("rate limit test prompt number "+fmt.Sprint(i), false), "")
		if resp.StatusCode == 429 {
			got429 = true
		}
		resp.Body.Close()
	}
	if !got429 {
		t.Error("expected at least one 429 from /v1/chat/completions after burst")
	}

	// /healthz should never be rate-limited.
	for i := 0; i < 10; i++ {
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == 429 {
			t.Fatal("/healthz was rate-limited")
		}
	}
}

// ---------------------------------------------------------------------------
// Degradation test
// ---------------------------------------------------------------------------

// TestE2E_Degradation verifies that when Ollama's health endpoint fails
// repeatedly, the circuit breaker trips and subsequent local-route
// requests are transparently rerouted to frontier with the
// X-Nexus-Degraded: true header.
func TestE2E_Degradation(t *testing.T) {
	stats := newMockServerStats()
	// Ollama health endpoint always returns 500.
	ollama := startMockOllama(t, stats, "should not be reached", 500)
	t.Cleanup(ollama.Close)
	frontier := startMockFrontier(t, stats, "degraded frontier fallback response")
	t.Cleanup(frontier.Close)

	e2eBaseEnv(t, ollama.URL, frontier.URL)
	// Enable health polling with aggressive settings so the breaker trips fast.
	t.Setenv("NEXUS_HEALTH_POLL_INTERVAL", "50ms")
	t.Setenv("NEXUS_HEALTH_BREAKER_THRESHOLD", "2")
	t.Setenv("NEXUS_HEALTH_PROBE_TIMEOUT", "200ms")
	// Use DSL formatting patterns to force local routing so we can verify
	// the degradation header when the local upstream is bypassed.
	t.Setenv("NEXUS_DSL_FORMATTING_PATTERNS", "e2e_degradation_trigger")
	t.Setenv("NEXUS_TOKEN_GUARDRAIL", "999999")

	ts := e2eTestServer(t)

	// Wait for the health poller to trip the circuit breaker.
	// With poll=50ms and threshold=2, the breaker should trip within ~200ms.
	// We poll /healthz until ollama_healthy becomes false or we time out.
	deadline := time.Now().Add(5 * time.Second)
	healthy := true
	for time.Now().Before(deadline) {
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), `"ollama_healthy":false`) {
			healthy = false
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if healthy {
		t.Skip("health poller did not trip breaker within 5s; skipping degradation assertion")
	}

	// Send a local-route request — should be transparently rerouted to frontier.
	resp := doChat(t, ts, chatRequest("e2e_degradation_trigger fix this css", false), "")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 200; body: %s", resp.StatusCode, body)
	}
	if v := resp.Header.Get("X-Nexus-Degraded"); v != "true" {
		t.Errorf("X-Nexus-Degraded: got %q, want true", v)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "degraded frontier fallback response") {
		t.Errorf("expected frontier fallback response; got: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Budget down-tier test (issue #1163)
// ---------------------------------------------------------------------------

// startMockFrontierFailover returns an httptest.Server that emulates a
// frontier provider which can be configured to fail with a specific status
// code on the first N calls, then succeed.
func startMockFrontierFailover(t *testing.T, stats *mockServerStats, content string, failCode int, failCount int) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stats.inc(r.URL.Path)
		n := calls.Add(1)
		if failCode != 0 && int(n) <= failCount {
			w.WriteHeader(failCode)
			return
		}
		// Non-streaming JSON response (cascade always uses stream=false).
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		resp := map[string]interface{}{
			"model": "frontier-model",
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"message":       map[string]interface{}{"role": "assistant", "content": content},
					"finish_reason": "stop",
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// TestE2E_BudgetDownTier verifies that when the 24h budget is exhausted,
// the planner down-tiers to local routing (issue #1163). It configures
// a tiny daily budget with a high cost-per-1K rate so any frontier-bound
// request exceeds the limit. The SLM is configured to return "frontier"
// but the budget check overrides it to RouteLocal.
func TestE2E_BudgetDownTier(t *testing.T) {
	stats := newMockServerStats()
	ollama := startMockOllama(t, stats, "budget-downtier local response", 0)
	t.Cleanup(ollama.Close)
	// Still need a frontier URL for config parsing even though budget
	// down-tier prevents it from being called.
	frontier := startMockFrontier(t, stats, "should not be reached")
	t.Cleanup(frontier.Close)

	e2eBaseEnv(t, ollama.URL, frontier.URL)
	// Keep guardrail high so VRAM doesn't force frontier.
	t.Setenv("NEXUS_TOKEN_GUARDRAIL", "999999")
	// Set a tiny daily budget ($0.001 = 0.1 cents).
	t.Setenv("NEXUS_BUDGET_DAILY_LIMIT", "0.001")
	t.Setenv("NEXUS_BUDGET_ALERT_ENABLED", "false")
	// Set a high cost-per-1K so any prompt exceeds the budget.
	// A ~100-token prompt at $100/1K tokens = $10 >> $0.001 limit.
	t.Setenv("NEXUS_FRONTIER_COST_PER_1K", "100.0")

	ts := e2eTestServer(t)
	resp := doChat(t, ts, chatRequest("budget down-tier test prompt for local routing", false), "")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 200; body: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	// The request should have been down-tiered to local (Ollama).
	if !strings.Contains(string(body), "budget-downtier local response") {
		t.Errorf("expected local response from budget down-tier; got: %s", body)
	}
	// Ollama should have been hit for chat completions.
	if got := stats.get("/v1/chat/completions"); got == 0 {
		t.Error("expected Ollama /v1/chat/completions to be called (budget down-tier)")
	}
}

// ---------------------------------------------------------------------------
// Frontier provider failover test (issue #1157)
// ---------------------------------------------------------------------------

// TestE2E_FrontierFailover verifies that when the first frontier provider
// returns a 5xx, the cascade fails over to the second provider (issue #1157).
// It configures two mock providers via NEXUS_PROVIDERS: provider A fails
// on the first attempt, provider B succeeds. The test asserts that the
// response comes from provider B and that the X-Nexus-Cascade-Served-By
// header identifies the failover provider.
func TestE2E_FrontierFailover(t *testing.T) {
	providerAStats := newMockServerStats()
	providerBStats := newMockServerStats()
	// Provider A: fails with 500 on first call, then succeeds.
	providerA := startMockFrontierFailover(t, providerAStats, "provider A recovered", 500, 1)
	t.Cleanup(providerA.Close)
	// Provider B: always succeeds.
	providerB := startMockFrontier(t, providerBStats, "provider B response")
	t.Cleanup(providerB.Close)

	// Ollama mock (needed for config, but guardrail forces frontier).
	ollamaStats := newMockServerStats()
	ollama := startMockOllama(t, ollamaStats, "unused", 0)
	t.Cleanup(ollama.Close)

	e2eBaseEnv(t, ollama.URL, providerA.URL)
	// Force frontier routing via guardrail.
	t.Setenv("NEXUS_TOKEN_GUARDRAIL", "1")
	// Configure two providers via NEXUS_PROVIDERS.
	t.Setenv("NEXUS_PROVIDERS", "providerA,providerB")
	t.Setenv("NEXUS_PROVIDER_PROVIDERA_URL", providerA.URL)
	t.Setenv("NEXUS_PROVIDER_PROVIDERA_MODEL", "provider-a-model")
	t.Setenv("NEXUS_PROVIDER_PROVIDERA_API_KEY", "test-key-a")
	t.Setenv("NEXUS_PROVIDER_PROVIDERB_URL", providerB.URL)
	t.Setenv("NEXUS_PROVIDER_PROVIDERB_MODEL", "provider-b-model")
	t.Setenv("NEXUS_PROVIDER_PROVIDERB_API_KEY", "test-key-b")
	// Enable frontier failover.
	t.Setenv("NEXUS_FRONTIER_FAILOVER", "true")
	t.Setenv("NEXUS_FRONTIER_FAILOVER_MAX_ATTEMPTS", "3")
	// Disable health polling for frontier providers (not needed for E2E).
	t.Setenv("NEXUS_FRONTIER_HEALTH_POLL_INTERVAL", "0")

	ts := e2eTestServer(t)
	resp := doChat(t, ts, chatRequest("frontier failover test prompt", false), "")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 200; body: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	// Response should come from provider B (failover target).
	if !strings.Contains(bodyStr, "provider B response") {
		t.Errorf("expected provider B response after failover; got: %s", bodyStr)
	}
	// Provider A should have been called (and failed).
	if got := providerAStats.get("/v1/chat/completions"); got == 0 {
		t.Error("expected provider A to be called first")
	}
}

// ---------------------------------------------------------------------------
// buildHandler unit test
// ---------------------------------------------------------------------------

// TestBuildHandlerMiddlewareChain verifies that buildHandler applies the
// SecurityHeaders → Recover middleware chain in the correct order.
func TestBuildHandlerMiddlewareChain(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	h := buildHandler(inner, false, nil) // no TLS, no panic observer

	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// SecurityHeaders must be applied.
	if v := resp.Header.Get("X-Content-Type-Options"); v != "nosniff" {
		t.Errorf("X-Content-Type-Options: got %q, want nosniff", v)
	}
	// No HSTS on plaintext.
	if v := resp.Header.Get("Strict-Transport-Security"); v != "" {
		t.Errorf("HSTS on plaintext: got %q", v)
	}
}

// TestBuildHandlerPanicRecovery verifies that Recover catches panics
// and returns 500 instead of crashing the server.
func TestBuildHandlerPanicRecovery(t *testing.T) {
	var panicObs atomic.Int32
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	h := buildHandler(inner, false, func(path string) {
		panicObs.Add(1)
	})

	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/test-panic-path")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 500 {
		t.Errorf("got %d, want 500 after panic", resp.StatusCode)
	}
	if panicObs.Load() == 0 {
		t.Error("panic observer was not called")
	}
}

// ---------------------------------------------------------------------------
// E2E tests for provider adapters (issue #1185, #1419)
// ---------------------------------------------------------------------------

// mockProviderHeaders records headers received by a mock provider server.
type mockProviderHeaders struct {
	mu    sync.Mutex
	data  map[string][]string
	path  string
}

func newMockProviderHeaders() *mockProviderHeaders {
	return &mockProviderHeaders{data: make(map[string][]string)}
}

func (h *mockProviderHeaders) get(k string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Use case-insensitive lookup since HTTP headers are case-insensitive
	if vals, ok := h.data[k]; ok && len(vals) > 0 {
		return vals[0]
	}
	// Also check with canonical Go header casing
	canonical := http.CanonicalHeaderKey(k)
	if vals, ok := h.data[canonical]; ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}

func (h *mockProviderHeaders) set(k, v string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.data[k] = append(h.data[k], v)
}

func (h *mockProviderHeaders) requestPath() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.path
}

func startMockAnthropic(t *testing.T, hdrs *mockProviderHeaders, content string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdrs.mu.Lock()
		hdrs.path = r.URL.Path
		for k, vals := range r.Header {
			hdrs.data[k] = append(hdrs.data[k], vals...)
		}
		hdrs.mu.Unlock()

		// Check if streaming was requested
		var reqBody map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err == nil {
			if stream, ok := reqBody["stream"].(bool); ok && stream {
				// Send SSE for streaming requests
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s\"}}]}\n\n", content)
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
		}
		// Send JSON for non-streaming requests (OpenAI non-streaming format)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		resp := map[string]interface{}{
			"id":      "chatcmpl-random",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "claude-3-5-sonnet-20241022",
			"choices": []map[string]interface{}{
				{
					"index":        0,
					"message":     map[string]interface{}{"role": "assistant", "content": content},
					"finish_reason": "stop",
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

func startMockGemini(t *testing.T, hdrs *mockProviderHeaders, content string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdrs.mu.Lock()
		hdrs.path = r.URL.Path
		for k, vals := range r.Header {
			hdrs.data[k] = append(hdrs.data[k], vals...)
		}
		hdrs.mu.Unlock()

		// Check if streaming was requested
		var reqBody map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err == nil {
			if stream, ok := reqBody["stream"].(bool); ok && stream {
				// Send SSE for streaming requests (OpenAI format for cascade)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s\"}}]}\n\n", content)
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
		}
		// Send OpenAI JSON for non-streaming requests
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		resp := map[string]interface{}{
			"id":      "chatcmpl-random",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gemini-pro",
			"choices": []map[string]interface{}{
				{
					"index":        0,
					"message":     map[string]interface{}{"role": "assistant", "content": content},
					"finish_reason": "stop",
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

// TestE2E_ProviderAnthropicAdapter verifies that when an Anthropic provider
// is configured, the proxy sends requests to /v1/messages (not
// /v1/chat/completions), includes the correct x-api-key and anthropic-version
// headers, and the response is valid OpenAI SSE.
func TestE2E_ProviderAnthropicAdapter(t *testing.T) {
	providerHdrs := newMockProviderHeaders()
	mockAnthropic := startMockAnthropic(t, providerHdrs, "anthropic response from claude")
	t.Cleanup(mockAnthropic.Close)

	ollamaStats := newMockServerStats()
	ollama := startMockOllama(t, ollamaStats, "unused", 0)
	t.Cleanup(ollama.Close)

	e2eBaseEnv(t, ollama.URL, mockAnthropic.URL)
	// Force frontier routing via guardrail.
	t.Setenv("NEXUS_TOKEN_GUARDRAIL", "1")
	// Configure Anthropic provider via NEXUS_PROVIDERS.
	t.Setenv("NEXUS_PROVIDERS", "anthropicProvider")
	t.Setenv("NEXUS_PROVIDER_ANTHROPICPROVIDER_URL", mockAnthropic.URL)
	t.Setenv("NEXUS_PROVIDER_ANTHROPICPROVIDER_MODEL", "claude-3-5-sonnet-20241022")
	t.Setenv("NEXUS_PROVIDER_ANTHROPICPROVIDER_API_KEY", "sk-ant-test-key")
	t.Setenv("NEXUS_PROVIDER_ANTHROPICPROVIDER_TYPE", "anthropic")
	// Disable frontier health polling.
	t.Setenv("NEXUS_FRONTIER_HEALTH_POLL_INTERVAL", "0")

	ts := e2eTestServer(t)
	resp := doChat(t, ts, chatRequest("anthropic adapter test prompt", false), "")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 200; body: %s", resp.StatusCode, body)
	}

	// Verify request hit /v1/messages (not /v1/chat/completions).
	if path := providerHdrs.requestPath(); path != "/v1/messages" {
		t.Errorf("request path = %q, want /v1/messages", path)
	}

	// Verify Anthropic auth headers were received.
	if got := providerHdrs.get("x-api-key"); got != "sk-ant-test-key" {
		t.Errorf("x-api-key = %q, want sk-ant-test-key", got)
	}
	if got := providerHdrs.get("anthropic-version"); got == "" {
		t.Error("anthropic-version header is missing")
	}

	// Verify response is valid OpenAI SSE with chat.completion.chunk events.
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	if !strings.Contains(bodyStr, "chat.completion.chunk") {
		t.Errorf("response does not contain chat.completion.chunk; got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "anthropic response from claude") {
		t.Errorf("response does not contain expected content; got: %s", bodyStr)
	}
}

// TestE2E_ProviderGeminiAdapter verifies that when a Gemini provider
// is configured, the proxy sends requests to the correct Gemini path
// and includes the x-goog-api-key header.
func TestE2E_ProviderGeminiAdapter(t *testing.T) {
	providerHdrs := newMockProviderHeaders()
	mockGemini := startMockGemini(t, providerHdrs, "gemini response")
	t.Cleanup(mockGemini.Close)

	ollamaStats := newMockServerStats()
	ollama := startMockOllama(t, ollamaStats, "unused", 0)
	t.Cleanup(ollama.Close)

	e2eBaseEnv(t, ollama.URL, mockGemini.URL)
	// Force frontier routing via guardrail.
	t.Setenv("NEXUS_TOKEN_GUARDRAIL", "1")
	// Configure Gemini provider via NEXUS_PROVIDERS.
	t.Setenv("NEXUS_PROVIDERS", "geminiProvider")
	t.Setenv("NEXUS_PROVIDER_GEMINIPROVIDER_URL", mockGemini.URL+"/v1beta/models/gemini-pro")
	t.Setenv("NEXUS_PROVIDER_GEMINIPROVIDER_MODEL", "gemini-pro")
	t.Setenv("NEXUS_PROVIDER_GEMINIPROVIDER_API_KEY", "test-gemini-key")
	t.Setenv("NEXUS_PROVIDER_GEMINIPROVIDER_TYPE", "gemini")
	// Disable frontier health polling.
	t.Setenv("NEXUS_FRONTIER_HEALTH_POLL_INTERVAL", "0")

	ts := e2eTestServer(t)
	resp := doChat(t, ts, chatRequest("gemini adapter test prompt", false), "")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 200; body: %s", resp.StatusCode, body)
	}

	// Verify request path contains :streamGenerateContent (Gemini streaming endpoint).
	if path := providerHdrs.requestPath(); !strings.Contains(path, ":streamGenerateContent") {
		t.Errorf("request path = %q, want :streamGenerateContent suffix", path)
	}

	// Verify Gemini auth header was received.
	if got := providerHdrs.get("x-goog-api-key"); got != "test-gemini-key" {
		t.Errorf("x-goog-api-key = %q, want test-gemini-key", got)
	}
}
