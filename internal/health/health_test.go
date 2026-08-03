package health

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/testutil"
)

// flakyServer is an httptest.Server whose /api/tags and /api/chat endpoints
// return the configured status code and increment call counters. Tests
// dial it directly so the breaker and probe path can be exercised
// without timing flake.
type flakyServer struct {
	*httptest.Server
	mu        sync.Mutex
	tagsCalls int
	chatCalls int
	tagsFail  int // when > 0, the next N /api/tags requests return 503
	chatFail  int // when > 0, the next N /api/chat requests return 503
}

func newFlakyServer() *flakyServer {
	f := &flakyServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		switch r.URL.Path {
		case "/api/tags":
			f.tagsCalls++
			fail := f.tagsFail
			if fail > 0 {
				f.tagsFail--
			}
			f.mu.Unlock()
			if fail > 0 {
				http.Error(w, "boom", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/chat":
			f.chatCalls++
			fail := f.chatFail
			if fail > 0 {
				f.chatFail--
			}
			f.mu.Unlock()
			if fail > 0 {
				http.Error(w, "model not loaded", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			resp := struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{}
			resp.Message.Content = "ok"
			_ = json.NewEncoder(w).Encode(resp)
		default:
			f.mu.Unlock()
			http.NotFound(w, r)
		}
	}))
	return f
}

func (f *flakyServer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tagsCalls + f.chatCalls
}

func (f *flakyServer) failNextN(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Fail both endpoints for the next n requests.
	f.tagsFail = n
	f.chatFail = n
}

// failChatNextN fails only the /api/chat endpoint for the next n requests.
// Used to test the case where Ollama is up but the model is not loaded.
func (f *flakyServer) failChatNextN(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chatFail = n
}

func TestIsLocalHealthyNilSafe(t *testing.T) {
	var h *Health
	if !h.IsLocalHealthy() {
		t.Fatal("nil Health must report healthy")
	}
	if err := h.Probe(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Probe on nil Health = %v, want ErrDisabled", err)
	}
}

func TestInitialProbeHealthy(t *testing.T) {
	srv := newFlakyServer()
	defer srv.Close()
	h := New(srv.URL, "qwen3-coder:8b", 50*time.Millisecond, 3, time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Run(ctx)
	defer h.Close()
	if !h.IsLocalHealthy() {
		t.Fatalf("expected healthy after boot; failureCount=%d", h.FailureCount())
	}
}

func TestInitialProbeUnhealthyTripsBreakerAfterThreshold(t *testing.T) {
	srv := newFlakyServer()
	defer srv.Close()
	// fail budget is intentionally large so the server keeps
	// returning 503 for the entire test window — otherwise a
	// recovery probe would reset the counter mid-test and the
	// post-trip assertion would race with a later success probe.
	srv.failNextN(10000)
	h := New(srv.URL, "qwen3-coder:8b", 30*time.Millisecond, 3, time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Run(ctx)
	defer h.Close()

	// Initial probe fires synchronously inside Run; the breaker
	// does not flip until the background loop has accumulated
	// BreakerThreshold consecutive failures. Poll briefly for the
	// unhealthy state.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !h.IsLocalHealthy() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.IsLocalHealthy() {
		t.Fatalf("expected unhealthy after breaker threshold; failureCount=%d",
			h.FailureCount())
	}
	if h.FailureCount() < 3 {
		t.Fatalf("failureCount=%d, want >=3", h.FailureCount())
	}
}

func TestBreakerRecoversOnSuccess(t *testing.T) {
	srv := newFlakyServer()
	defer srv.Close()
	// Fail 3 then succeed.
	srv.failNextN(3)
	h := New(srv.URL, "qwen3-coder:8b", 30*time.Millisecond, 3, time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Run(ctx)
	defer h.Close()

	// Wait for the breaker to trip (3 consecutive failures).
	tripDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(tripDeadline) {
		if !h.IsLocalHealthy() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.IsLocalHealthy() {
		t.Fatal("expected unhealthy after initial failures")
	}

	// Allow the next poll to fire — server has switched back to
	// success mode so the breaker should reopen.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.IsLocalHealthy() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !h.IsLocalHealthy() {
		t.Fatalf("expected healthy after recovery; failureCount=%d calls=%d",
			h.FailureCount(), srv.callCount())
	}
	if h.FailureCount() != 0 {
		t.Fatalf("failureCount=%d after recovery, want 0", h.FailureCount())
	}
}

func TestProbeErrorOnUnreachableHost(t *testing.T) {
	// Port 1 is virtually guaranteed to refuse connections.
	h := New("http://127.0.0.1:1", "qwen3-coder:8b", 50*time.Millisecond, 3, 200*time.Millisecond, &http.Client{
		Timeout: 200 * time.Millisecond,
	})
	if err := h.Probe(context.Background()); err == nil {
		t.Fatal("expected error on unreachable host")
	}
}

func TestSubThresholdFailuresStayHealthy(t *testing.T) {
	srv := newFlakyServer()
	defer srv.Close()
	// Fail twice (below threshold of 3) then succeed; subsequent
	// probes should succeed and reset the counter.
	srv.failNextN(2)
	h := New(srv.URL, "qwen3-coder:8b", 30*time.Millisecond, 3, time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Run(ctx)
	defer h.Close()

	// Wait for at least one success probe to bring the counter to 0.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.FailureCount() == 0 && srv.callCount() >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !h.IsLocalHealthy() {
		t.Fatalf("expected healthy with sub-threshold failures; failureCount=%d",
			h.FailureCount())
	}
	if h.FailureCount() != 0 {
		t.Fatalf("failureCount=%d, want 0 after recovery", h.FailureCount())
	}
}

func TestProbe4xxTreatedAsHealthy(t *testing.T) {
	// A 401 on /api/tags (e.g. reverse proxy in front of Ollama) means
	// Ollama is up. The probe also calls /api/chat, which must succeed to
	// confirm the model is available.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		// /api/chat must return 200 for the model to be considered available.
		w.Header().Set("Content-Type", "application/json")
		resp := struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}{}
		resp.Message.Content = "ok"
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	h := New(srv.URL, "qwen3-coder:8b", 50*time.Millisecond, 3, time.Second, srv.Client())
	if err := h.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !h.IsLocalHealthy() {
		t.Fatal("4xx on /api/tags must be treated as healthy when /api/chat succeeds")
	}
}

func TestConcurrentIsLocalHealthyIsRaceFree(t *testing.T) {
	srv := newFlakyServer()
	defer srv.Close()
	h := New(srv.URL, "qwen3-coder:8b", 20*time.Millisecond, 3, time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Run(ctx)
	defer h.Close()

	var wg sync.WaitGroup
	var mismatches atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = h.IsLocalHealthy()
				_ = h.FailureCount()
			}
		}()
	}
	wg.Wait()
	if mismatches.Load() != 0 {
		t.Fatalf("got %d mismatches", mismatches.Load())
	}
}

// TestModelNotLoadedVerifiesModel tests that when /api/tags succeeds but
// /api/chat fails (model not loaded), the health probe reports unhealthy.
// This is the core fix for issue #204: the health probe must verify model
// availability, not just server reachability.
func TestModelNotLoadedVerifiesModel(t *testing.T) {
	srv := newFlakyServer()
	defer srv.Close()
	// Make the chat endpoint fail to simulate the model not being loaded.
	srv.failChatNextN(3)
	h := New(srv.URL, "qwen3-coder:8b", 30*time.Millisecond, 3, time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Run(ctx)
	defer h.Close()

	// Wait for the breaker to trip (3 consecutive failures from chat failures).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !h.IsLocalHealthy() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.IsLocalHealthy() {
		t.Fatalf("expected unhealthy when model not loaded; failureCount=%d",
			h.FailureCount())
	}
	if h.FailureCount() < 3 {
		t.Fatalf("failureCount=%d, want >=3", h.FailureCount())
	}
}

// TestBackoffIntervalIncrease verifies that the polling interval increases
// via exponential backoff after consecutive failures. This is the core
// fix for issue #311.
func TestBackoffIntervalIncrease(t *testing.T) {
	srv := newFlakyServer()
	defer srv.Close()
	// Fail enough probes to trigger breaker and then exercise backoff tiers.
	srv.failNextN(10000)
	// Use a short poll interval so we can observe backoff quickly.
	pollInterval := 50 * time.Millisecond
	h := New(srv.URL, "qwen3-coder:8b", pollInterval, 3, time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Run(ctx)
	defer h.Close()

	// Wait for breaker to trip and verify state.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !h.IsLocalHealthy() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.IsLocalHealthy() {
		t.Fatal("expected breaker to trip")
	}
	if h.FailureCount() < 3 {
		t.Fatalf("expected failureCount >= 3, got %d", h.FailureCount())
	}

	// After the breaker trips, the interval should be backoff (2x = 100ms).
	// Verify via PollingInterval().
	interval := h.PollingInterval()
	if interval < 80*time.Millisecond {
		t.Fatalf("expected backoff interval >= 80ms, got %v", interval)
	}
}

// TestBackoffResetsOnRecovery verifies that the polling interval resets
// to normal after a successful probe.
func TestBackoffResetsOnRecovery(t *testing.T) {
	srv := newFlakyServer()
	defer srv.Close()
	// Fail 3 times (breaker trips), then succeed.
	srv.failNextN(3)
	pollInterval := 50 * time.Millisecond
	h := New(srv.URL, "qwen3-coder:8b", pollInterval, 3, time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Run(ctx)
	defer h.Close()

	// Wait for breaker to trip.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !h.IsLocalHealthy() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.IsLocalHealthy() {
		t.Fatal("expected breaker to trip")
	}

	// Now the server will succeed on next probe.
	// Wait for recovery.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.IsLocalHealthy() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !h.IsLocalHealthy() {
		t.Fatal("expected recovery after success")
	}

	// After recovery, the interval should be back to normal.
	interval := h.PollingInterval()
	if interval != pollInterval {
		t.Fatalf("expected normal interval %v after recovery, got %v", pollInterval, interval)
	}
}

// TestCalcBackoffInterval verifies the tier calculation.
func TestCalcBackoffInterval(t *testing.T) {
	srv := newFlakyServer()
	defer srv.Close()
	pollInterval := 100 * time.Millisecond
	h := New(srv.URL, "qwen3-coder:8b", pollInterval, 3, time.Second, nil)

	tests := []struct {
		count          int
		wantMultiplier int64
	}{
		{1, 1},    // tier 0: 1x
		{2, 1},    // tier 0: 1x
		{3, 2},    // tier 1: 2x
		{4, 2},    // tier 1: 2x
		{5, 4},    // tier 2: 4x
		{6, 4},    // tier 2: 4x
		{7, 8},    // tier 3: 8x
		{8, 8},    // tier 3: 8x
		{9, 15},   // tier 4: capped at 15x
		{100, 15}, // tier 4: capped at 15x
	}

	for _, tc := range tests {
		got := h.calcBackoffInterval(tc.count)
		want := pollInterval * time.Duration(tc.wantMultiplier)
		if got != want {
			t.Errorf("calcBackoffInterval(%d) = %v, want %v", tc.count, got, want)
		}
	}
}

// TestCalcBackoffIntervalTier4Plus verifies the 15x ceiling (tier 4+)
// is enforced. Tier 4 nominally yields a 16x multiplier but is capped
// at maxBackoffMultiplier (15). This exercises the capping branch in
// calcBackoffInterval that the tier 0–3 table does not reach.
func TestCalcBackoffIntervalTier4Plus(t *testing.T) {
	srv := newFlakyServer()
	defer srv.Close()
	pollInterval := 100 * time.Millisecond
	h := New(srv.URL, "qwen3-coder:8b", pollInterval, 3, time.Second, nil)

	want := pollInterval * maxBackoffMultiplier

	// count=9 → tier (9-1)>>1 = 4 → multiplier 16, capped to 15.
	if got := h.calcBackoffInterval(9); got != want {
		t.Errorf("calcBackoffInterval(9) = %v, want %v (capped 15x)", got, want)
	}
	// count=100 → tier 49 → multiplier far above cap, still 15x.
	if got := h.calcBackoffInterval(100); got != want {
		t.Errorf("calcBackoffInterval(100) = %v, want %v (capped 15x)", got, want)
	}
}

// TestCalcBackoffIntervalZeroOrNegative exercises the tier < 0 guard in
// calcBackoffInterval. A non-positive count yields a negative tier
// ((count-1)>>1 < 0) which is clamped back to tier 0 (1x multiplier).
func TestCalcBackoffIntervalZeroOrNegative(t *testing.T) {
	srv := newFlakyServer()
	defer srv.Close()
	pollInterval := 100 * time.Millisecond
	h := New(srv.URL, "qwen3-coder:8b", pollInterval, 3, time.Second, nil)

	want := pollInterval // tier clamped to 0 → 1x multiplier

	for _, count := range []int{0, -1, -5} {
		if got := h.calcBackoffInterval(count); got != want {
			t.Errorf("calcBackoffInterval(%d) = %v, want %v (tier<0 clamped to 1x)", count, got, want)
		}
	}
}

// TestRecordFailureBackoffCapsAtMax drives recordFailure through enough
// consecutive failures to reach tier 4 inside recordFailure, covering
// the multiplier > maxBackoffMultiplier capping branch (lines 396–397)
// and confirming the stored interval never exceeds 15x pollInterval.
func TestRecordFailureBackoffCapsAtMax(t *testing.T) {
	srv := newFlakyServer()
	defer srv.Close()
	pollInterval := 100 * time.Millisecond
	// Low threshold so the backoff path runs from the first failure.
	h := New(srv.URL, "qwen3-coder:8b", pollInterval, 3, time.Second, nil)

	maxInterval := pollInterval * maxBackoffMultiplier
	err := errors.New("probe failed")

	// 9 failures → tier 4 → multiplier 16, capped to 15.
	for i := 0; i < 9; i++ {
		h.recordFailure(err)
	}

	if got := h.PollingInterval(); got != maxInterval {
		t.Errorf("PollingInterval after 9 failures = %v, want %v (capped 15x)", got, maxInterval)
	}

	// Additional failures must not exceed the cap.
	for i := 0; i < 20; i++ {
		h.recordFailure(err)
	}
	if got := h.PollingInterval(); got != maxInterval {
		t.Errorf("PollingInterval after 29 failures = %v, want %v (still capped 15x)", got, maxInterval)
	}
}

// TestRecordFailureNilErrOmitsErrField ensures recordFailure(nil) does
// not render a "<nil>" placeholder in its structured logs. Both the
// below-threshold debug log and the breaker-tripped warn log must omit
// the err field entirely when err is nil (issue #697).
func TestRecordFailureNilErrOmitsErrField(t *testing.T) {
	// Redirect the default slog logger into an in-memory buffer so we
	// can assert on the rendered output. The shared, mutex-serialized
	// helper restores the original at exit (issue #1138).
	var buf bytes.Buffer
	testutil.SetDefault(t, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	srv := newFlakyServer()
	defer srv.Close()

	// Below-threshold failure (count < threshold) → debug log path.
	t.Run("below_threshold_debug", func(t *testing.T) {
		buf.Reset()
		// threshold 3 → first failure stays below threshold; wasHealthy=true.
		h := New(srv.URL, "qwen3-coder:8b", 100*time.Millisecond, 3, time.Second, nil)
		h.recordFailure(nil) // would previously render err=<nil>

		out := buf.String()
		if strings.Contains(out, "<nil>") {
			t.Errorf("below-threshold debug log contains <nil>:\n%s", out)
		}
		if !strings.Contains(out, "ollama probe failed (below threshold)") {
			t.Errorf("expected debug log line, got:\n%s", out)
		}
	})

	// Threshold-reaching failure (count >= threshold) → warn log path.
	t.Run("breaker_tripped_warn", func(t *testing.T) {
		buf.Reset()
		// threshold 1 → first failure trips the breaker; wasHealthy=true.
		h := New(srv.URL, "qwen3-coder:8b", 100*time.Millisecond, 1, time.Second, nil)
		h.recordFailure(nil) // would previously render err=<nil>

		out := buf.String()
		if strings.Contains(out, "<nil>") {
			t.Errorf("breaker-tripped warn log contains <nil>:\n%s", out)
		}
		if !strings.Contains(out, "ollama health: breaker tripped") {
			t.Errorf("expected warn log line, got:\n%s", out)
		}
	})
}
