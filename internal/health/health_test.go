package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
