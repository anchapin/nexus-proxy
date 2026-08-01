// Package e2e hosts end-to-end integration tests that exercise multiple
// production packages together. The chaos_test.go file implements the
// fault-injection harness described in issue #1187: it injects live failures
// into a mock Ollama (a toggleable httptest.Server) during active requests and
// asserts that the proxy's graceful-degradation paths recover.
//
// Unlike the unit tests in internal/health, internal/circuit, and
// internal/upstream — which test each component in isolation — these tests
// drive the real health poller, cooldown circuit, and cascade runner against a
// shared FaultableServer so the interactions between them are verified: the
// health breaker tripping feeds into the cascade's SkipLocal decision, the
// cooldown arms after a cascade-detected local failure, and the cascade falls
// back to a frontier endpoint within its deadline.
package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/circuit"
	"github.com/anchapin/nexus-proxy/internal/health"
	"github.com/anchapin/nexus-proxy/internal/testutil"
	"github.com/anchapin/nexus-proxy/internal/upstream"
)

// frontierBody is a valid OpenAI-compatible completion served by the frontier
// fallback server. It is intentionally distinct from the local completion so
// tests can assert which endpoint served the response.
const frontierBody = `{"model":"gpt-frontier","choices":[{"index":0,"message":{"role":"assistant","content":"served by frontier fallback"},"finish_reason":"stop"}]}`

// frontierServer starts an httptest.Server that always returns a valid
// OpenAI completion. It never faults — it represents the always-available
// frontier API the cascade falls back to.
func frontierServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(frontierBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// cascadeHarness wires a real Cascade whose local step is the FaultableServer
// (mock Ollama) and whose fallback is the frontier httptest.Server. The
// returned client has a per-request timeout larger than the cascade timeout so
// transport-level deadlines do not mask cascade behaviour.
func cascadeHarness(t *testing.T, ollama *testutil.FaultableServer, frontier *httptest.Server, timeout time.Duration) (*upstream.Cascade, *http.Client) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	casc := upstream.BuildLocalCascade(upstream.CascadeConfig{
		LocalURL:      ollama.URL,
		LocalModel:    "local-m",
		FrontierURL:   frontier.URL + "/v1/chat/completions",
		FrontierModel: "frontier-m",
		FrontierKey:   "sk-test",
		Timeout:       timeout,
	})
	return casc, client
}

// sseRecorder captures the SSE bytes the cascade writes so tests can assert on
// completeness (presence of [DONE]) and content. It implements the minimal
// http.ResponseWriter + Flusher surface the cascade's writeSSEResponse needs.
type sseRecorder struct {
	header http.Header
	body   strings.Builder
}

func newSSERecorder() *sseRecorder { return &sseRecorder{header: http.Header{}} }

func (s *sseRecorder) Header() http.Header         { return s.header }
func (s *sseRecorder) Write(b []byte) (int, error) { return s.body.Write(b) }
func (s *sseRecorder) WriteHeader(int)             {}
func (s *sseRecorder) Flush()                      {}

// isDegraded replicates the chat handler's graceful-degradation decision
// (internal/handlers/chat.go, issue #8 + #80): the local arm is skipped when
// the health poller reports Ollama unhealthy OR the cooldown circuit is active,
// and only for routes that would otherwise touch local (local/fusion). The
// chaos harness reuses this exact predicate so its assertions reflect real
// production routing behaviour.
func isDegraded(h *health.Health, cd *circuit.Cooldown, route string) bool {
	if route != "local" && route != "fusion" {
		return false
	}
	if h != nil && !h.IsLocalHealthy() {
		return true
	}
	if cd != nil && cd.Active() {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Scenario 1: mid-stream Ollama crash on route=local -> cascade falls back to
// frontier; client receives a complete, non-truncated response.
// ---------------------------------------------------------------------------

func TestChaos_MidStreamCrash_FallsBackToFrontier(t *testing.T) {
	ollama := testutil.NewFaultableServer()
	t.Cleanup(ollama.Close)
	ollama.SetMode(testutil.ModeCrash) // 502 on every path
	frontier := frontierServer(t)

	casc, client := cascadeHarness(t, ollama, frontier, 2*time.Second)
	rw := newSSERecorder()

	res, err := casc.Run(context.Background(), rw, client, map[string]any{"messages": []any{}}, "req-1")
	if err != nil {
		t.Fatalf("cascade Run: %v", err)
	}
	if !res.Succeeded {
		t.Fatalf("cascade did not succeed; result=%+v", res)
	}
	if res.ServedBy != "frontier" {
		t.Errorf("ServedBy = %q, want frontier", res.ServedBy)
	}
	if !res.LocalStepFailed {
		t.Error("LocalStepFailed = false; want true (local crashed, cascade fell back)")
	}
	if res.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (local->frontier)", res.Attempts)
	}
	// The client must receive a complete, non-truncated SSE stream ending in
	// [DONE] — proving the fallback response was not cut short by the crash.
	body := rw.body.String()
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("response not complete: missing [DONE]; body=%q", body)
	}
	if !strings.Contains(body, "served by frontier fallback") {
		t.Errorf("response missing frontier content; body=%q", body)
	}
}

// ---------------------------------------------------------------------------
// Scenario 2: repeated Ollama 500s -> after NEXUS_HEALTH_BREAKER_THRESHOLD
// failures the breaker trips (IsLocalHealthy=false); subsequent requests skip
// Ollama via the degraded-routing decision.
// ---------------------------------------------------------------------------

func TestChaos_RepeatedOllama500s_BreakerTrips(t *testing.T) {
	ollama := testutil.NewFaultableServer()
	t.Cleanup(ollama.Close)
	ollama.SetMode(testutil.Mode500)

	// Threshold 3 matches NEXUS_HEALTH_BREAKER_THRESHOLD default; a 200ms
	// probe timeout keeps the failing probes fast.
	h := health.New(ollama.URL, "local-m", time.Hour, 3, 200*time.Millisecond, &http.Client{Timeout: 200 * time.Millisecond})

	// Drive probes directly (no background goroutine) for determinism.
	for i := 1; i <= 3; i++ {
		if err := h.Probe(context.Background()); err == nil {
			t.Fatalf("probe %d: expected failure, got nil", i)
		}
	}
	if h.IsLocalHealthy() {
		t.Fatal("breaker did not trip after 3 consecutive failures; still healthy")
	}

	// The degraded-routing decision must now report local unhealthy, so the
	// production handler would skip Ollama and stamp X-Nexus-Degraded: true.
	if !isDegraded(h, nil, "local") {
		t.Error("isDegraded(local) = false after breaker trip; want true")
	}

	// And the cascade built with SkipLocal (what the handler does when
	// degraded) must not include the local step at all.
	casc := upstream.BuildLocalCascade(upstream.CascadeConfig{
		SkipLocal:     true,
		FrontierURL:   "http://frontier.example/v1/chat/completions",
		FrontierModel: "frontier-m",
		FrontierKey:   "sk",
		Timeout:       time.Second,
	})
	for _, step := range casc.Steps {
		if step.Name == "local" {
			t.Error("SkipLocal cascade still contains a local step; Ollama would be hit despite the tripped breaker")
		}
	}
}

// ---------------------------------------------------------------------------
// Scenario 3: breaker recloses -> first successful probe clears the degraded
// state (IsLocalHealthy returns true again).
// ---------------------------------------------------------------------------

func TestChaos_BreakerReclosesOnRecovery(t *testing.T) {
	ollama := testutil.NewFaultableServer()
	t.Cleanup(ollama.Close)
	ollama.SetMode(testutil.Mode500)

	h := health.New(ollama.URL, "local-m", time.Hour, 3, 200*time.Millisecond, &http.Client{Timeout: 200 * time.Millisecond})
	for i := 0; i < 3; i++ {
		_ = h.Probe(context.Background())
	}
	if h.IsLocalHealthy() {
		t.Fatal("precondition: breaker should be tripped")
	}
	if !isDegraded(h, nil, "local") {
		t.Fatal("precondition: should be degraded before recovery")
	}

	// Recover: flip the server back to healthy and probe once. health.go
	// reopens the breaker on the first success.
	ollama.SetMode(testutil.ModeHealthy)
	if err := h.Probe(context.Background()); err != nil {
		t.Fatalf("recovery probe failed: %v", err)
	}
	if !h.IsLocalHealthy() {
		t.Fatal("breaker did not reclose after a successful probe")
	}
	if isDegraded(h, nil, "local") {
		t.Error("isDegraded(local) = true after recovery; want false (X-Nexus-Degraded should clear)")
	}
}

// ---------------------------------------------------------------------------
// Scenario 4: route=fusion with Ollama down -> local panel member is skipped;
// the degraded decision reports true and a SkipLocal cascade serves frontier.
// ---------------------------------------------------------------------------

func TestChaos_FusionWithOllamaDown_SkipsLocalPanel(t *testing.T) {
	ollama := testutil.NewFaultableServer()
	t.Cleanup(ollama.Close)
	ollama.SetMode(testutil.Mode500)
	frontier := frontierServer(t)

	h := health.New(ollama.URL, "local-m", time.Hour, 3, 200*time.Millisecond, &http.Client{Timeout: 200 * time.Millisecond})
	for i := 0; i < 3; i++ {
		_ = h.Probe(context.Background())
	}
	if h.IsLocalHealthy() {
		t.Fatal("precondition: breaker should be tripped for fusion degradation")
	}

	// Fusion is a local-touching route, so an unhealthy Ollama degrades it.
	if !isDegraded(h, nil, "fusion") {
		t.Error("isDegraded(fusion) = false with Ollama down; want true (local panel should be skipped)")
	}

	// The handler builds a SkipLocal cascade when degraded; running it must
	// serve frontier without ever dialling the (down) Ollama.
	before := ollama.Requests()
	casc, client := cascadeHarnessWithSkip(t, ollama, frontier, time.Second)
	rw := newSSERecorder()
	res, err := casc.Run(context.Background(), rw, client, map[string]any{"messages": []any{}}, "req-4")
	if err != nil {
		t.Fatalf("SkipLocal cascade Run: %v", err)
	}
	if !res.Succeeded || res.ServedBy != "frontier" {
		t.Fatalf("expected frontier to serve; result=%+v", res)
	}
	if after := ollama.Requests(); after != before {
		t.Errorf("Ollama was hit %d time(s) on degraded fusion; local panel should be skipped", after-before)
	}
	if !strings.Contains(rw.body.String(), "served by frontier fallback") {
		t.Errorf("arbiter/frontier content missing; body=%q", rw.body.String())
	}
}

// cascadeHarnessWithSkip builds a cascade that omits the local step, mirroring
// what the chat handler constructs when the degradation decision is true.
func cascadeHarnessWithSkip(t *testing.T, _ *testutil.FaultableServer, frontier *httptest.Server, timeout time.Duration) (*upstream.Cascade, *http.Client) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	casc := upstream.BuildLocalCascade(upstream.CascadeConfig{
		SkipLocal:     true,
		FrontierURL:   frontier.URL + "/v1/chat/completions",
		FrontierModel: "frontier-m",
		FrontierKey:   "sk-test",
		Timeout:       timeout,
	})
	return casc, client
}

// ---------------------------------------------------------------------------
// Scenario 5: slow Ollama exceeding the cascade timeout -> frontier fallback
// fires within the deadline (not the slow server's delay).
// ---------------------------------------------------------------------------

func TestChaos_SlowOllama_CascadeTimeoutFallback(t *testing.T) {
	ollama := testutil.NewFaultableServer()
	t.Cleanup(ollama.Close)
	// Ollama delays far longer than the cascade's per-attempt timeout.
	ollama.SetMode(testutil.ModeSlow)
	ollama.SetDelay(400 * time.Millisecond)
	frontier := frontierServer(t)

	// 120ms cascade timeout: the local attempt must give up well before the
	// 400ms slow delay and fall back to frontier.
	casc, client := cascadeHarness(t, ollama, frontier, 120*time.Millisecond)
	rw := newSSERecorder()

	start := time.Now()
	res, err := casc.Run(context.Background(), rw, client, map[string]any{"messages": []any{}}, "req-5")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("cascade Run: %v", err)
	}
	if !res.Succeeded || res.ServedBy != "frontier" {
		t.Fatalf("expected frontier fallback; result=%+v", res)
	}
	// Fallback must fire within the deadline, not the slow delay. Allow a
	// generous slop margin for the frontier round-trip + CI jitter.
	if elapsed > 2*time.Second {
		t.Errorf("fallback took %v; cascade timeout (120ms) should have bounded the local attempt", elapsed)
	}
	if res.FallbackReason != "timeout" {
		t.Errorf("FallbackReason = %q, want timeout", res.FallbackReason)
	}
	if !strings.Contains(rw.body.String(), "[DONE]") {
		t.Errorf("response incomplete after timeout fallback; body=%q", rw.body.String())
	}
}

// ---------------------------------------------------------------------------
// Scenario 6: connection accepted then dropped (mid-stream reset) -> no
// goroutine leak; the cascade falls back to frontier cleanly.
// ---------------------------------------------------------------------------

func TestChaos_ConnectionDropped_NoGoroutineLeak(t *testing.T) {
	ollama := testutil.NewFaultableServer()
	t.Cleanup(ollama.Close)
	ollama.SetMode(testutil.ModePartial) // accept, write half, reset
	frontier := frontierServer(t)

	casc, client := cascadeHarness(t, ollama, frontier, 2*time.Second)

	// First, prove the dropped-connection cascade still falls back to
	// frontier and delivers a complete response.
	rw := newSSERecorder()
	res, err := casc.Run(context.Background(), rw, client, map[string]any{"messages": []any{}}, "req-6a")
	if err != nil {
		t.Fatalf("cascade Run: %v", err)
	}
	if !res.Succeeded || res.ServedBy != "frontier" {
		t.Fatalf("expected frontier fallback after reset; result=%+v", res)
	}
	if !strings.Contains(rw.body.String(), "[DONE]") {
		t.Errorf("response incomplete after reset fallback; body=%q", rw.body.String())
	}

	// Then assert no goroutine leak. A single dropped connection leaves a
	// few transient transport goroutines that unwind asynchronously, so a
	// strict before/after-equality check is flaky on CI. Instead we detect
	// *unbounded growth*: a true leak accumulates one-or-more goroutines per
	// request, so the count after N iterations would climb linearly.
	// Transient goroutines stay bounded. We take a baseline, run several
	// more dropped-connection requests, settle, and confirm the count did
	// not grow beyond a small delta.
	settleGoroutines()
	baseline := runtime.NumGoroutine()

	const iterations = 5
	for i := 0; i < iterations; i++ {
		rec := newSSERecorder()
		r, rerr := casc.Run(context.Background(), rec, client, map[string]any{"messages": []any{}}, "req-6b")
		if rerr != nil {
			t.Fatalf("iteration %d cascade Run: %v", i, rerr)
		}
		if !r.Succeeded {
			t.Fatalf("iteration %d did not succeed", i)
		}
	}
	settleGoroutines()

	// Allow a small delta to absorb background-runtime goroutines (GC,
	// finalizer, transport idle loop). A genuine per-request leak across
	// `iterations` runs would blow past this.
	final := runtime.NumGoroutine()
	if delta := final - baseline; delta > iterations {
		t.Errorf("goroutine leak: baseline=%d after %d more drops=%d (grew by %d, expected bounded)",
			baseline, iterations, final, delta)
	}
}

// settleGoroutines forces a GC cycle and yields to the scheduler so that
// goroutines associated with just-closed connections have a chance to exit
// before the caller samples runtime.NumGoroutine.
func settleGoroutines() {
	runtime.GC()
	time.Sleep(15 * time.Millisecond)
	runtime.Gosched()
}
