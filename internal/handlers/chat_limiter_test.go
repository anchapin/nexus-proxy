package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/concurrencylimit"
)

// errLimiterBusy is the sentinel returned by stubLocalLimiter when a
// test wants to simulate the limiter denying a slot.
var errLimiterBusy = errors.New("stub limiter busy")

// stubLocalLimiter is a controllable LocalLimiter for handler tests.
// The acquire closure lets each test dictate whether a slot is granted
// or rejected, and the release hook lets a test observe that the slot
// was released exactly once.
type stubLocalLimiter struct {
	acquire func(ctx context.Context) (release func(), err error)
}

func (s stubLocalLimiter) Acquire(ctx context.Context) (func(), error) {
	return s.acquire(ctx)
}

// TestChatLocalLimiterNilLeavesBehaviourUnchanged confirms the
// acceptance criterion "With probe disabled, limiter behaviour is
// unchanged": a nil LocalLimiter (the default wiring when
// NEXUS_LOCAL_MAX_CONCURRENT is unset) lets the local route proceed
// with zero upstream-side effects.
func TestChatLocalLimiterNilLeavesBehaviourUnchanged(t *testing.T) {
	deps, rt := baseDeps(t)
	// deps.LocalLimiter is nil by default (baseDeps does not set it).
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (nil limiter must not interfere)", rw.Code)
	}
	if len(rt.Calls()) != 1 || rt.Calls()[0].URL != "http://ollama.local/v1/chat/completions" {
		t.Errorf("calls = %+v, want a single local dispatch", rt.Calls())
	}
}

// TestChatLocalLimiterAcquireRejectsReturns503 confirms that when the
// limiter denies a slot (ctx cancelled while queued) the handler
// short-circuits with 503 and never touches the local upstream.
func TestChatLocalLimiterAcquireRejectsReturns503(t *testing.T) {
	deps, rt := baseDeps(t)
	deps.LocalLimiter = stubLocalLimiter{
		acquire: func(_ context.Context) (func(), error) {
			return nil, errLimiterBusy // simulate a cancelled / busy acquire
		},
	}
	// If the handler mistakenly dispatches, this handler records the
	// call so the assertion can catch it.
	served := atomic.Bool{}
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		served.Store(true)
		w.WriteHeader(http.StatusOK)
	})

	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (limiter rejected)", rw.Code)
	}
	if served.Load() {
		t.Error("local upstream was dispatched despite limiter rejection")
	}
	if len(rt.Calls()) != 0 {
		t.Errorf("expected zero upstream calls, got %d", len(rt.Calls()))
	}
}

// TestChatLocalLimiterAcquireHoldsThenReleases confirms the handler
// acquires before dispatch and releases exactly once after the local
// route completes (success path).
func TestChatLocalLimiterAcquireHoldsThenReleases(t *testing.T) {
	deps, rt := baseDeps(t)
	var acquires, releases atomic.Int64
	releaseFn := func() { releases.Add(1) }
	deps.LocalLimiter = stubLocalLimiter{
		acquire: func(_ context.Context) (func(), error) {
			acquires.Add(1)
			return releaseFn, nil
		},
	}
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})

	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if got := acquires.Load(); got != 1 {
		t.Errorf("acquire called %d times, want 1", got)
	}
	if got := releases.Load(); got != 1 {
		t.Errorf("release called %d times, want 1", got)
	}
}

// TestChatLocalLimiterOnlyLocalRouteAcquires confirms the limiter is
// consulted only on route=local — a frontier route must not touch it.
func TestChatLocalLimiterOnlyLocalRouteAcquires(t *testing.T) {
	deps, rt := baseDeps(t)
	var acquires atomic.Int64
	deps.LocalLimiter = stubLocalLimiter{
		acquire: func(_ context.Context) (func(), error) {
			acquires.Add(1)
			return func() {}, nil
		},
	}
	// Large prompt -> guardrail forces frontier (no local dispatch).
	largeUser := strings.Repeat("a", 48500)
	body := `{"messages":[{"role":"user","content":"` + largeUser + `"}]}`
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("frontier stream"))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if got := acquires.Load(); got != 0 {
		t.Errorf("limiter acquired %d times on a frontier route, want 0", got)
	}
}

// TestChatLocalLimiterRealLimiterThrottles wires the real
// concurrencylimit.Limiter behind the handler interface and confirms
// that a ceiling of 1 serialises several concurrent local requests:
// at most one is ever dispatched at a time. This is an end-to-end
// check that the interface contract (Acquire/Release) matches the
// concrete implementation the handler will see in production.
func TestChatLocalLimiterRealLimiterThrottles(t *testing.T) {
	deps, rt := baseDeps(t)
	// Ceiling 1 with ample VRAM -> effective 1, so requests serialise.
	deps.LocalLimiter = concurrencylimit.New(1, 1<<30, func() int64 { return 8 << 30 })

	var inFlight, maxSeen atomic.Int64
	// release is closed once the first request has parked inside the
	// handler so the test can observe maxSeen while later requests are
	// queued behind the limiter. Closing makes every subsequent
	// receive return immediately, so queued requests drain promptly.
	release := make(chan struct{})
	var firstParked sync.Once

	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		cur := inFlight.Add(1)
		for {
			old := maxSeen.Load()
			if cur <= old || maxSeen.CompareAndSwap(old, cur) {
				break
			}
		}
		// Signal that one request is inside the handler body so the
		// main goroutine can close the release channel and let the
		// queue drain.
		firstParked.Do(func() {})
		<-release
		inFlight.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})

	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			Chat(deps).ServeHTTP(httptest.NewRecorder(), req)
		}()
	}

	// Wait for the first request to park inside the handler, then open
	// the gate so all queued requests can proceed serially.
	for i := 0; i < 100 && maxSeen.Load() == 0; i++ {
		time.Sleep(2 * time.Millisecond)
	}
	close(release)
	wg.Wait()

	if got := maxSeen.Load(); got > 1 {
		t.Errorf("max concurrent local dispatches = %d, want <= 1 (ceiling enforced)", got)
	}
	if got := maxSeen.Load(); got != 1 {
		t.Errorf("max concurrent local dispatches = %d, want exactly 1 (serialised)", got)
	}
}
