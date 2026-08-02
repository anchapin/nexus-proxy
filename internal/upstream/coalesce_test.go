package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlightKey_Deterministic(t *testing.T) {
	t.Parallel()
	body := jsonBody(t, map[string]any{"messages": []string{"hi"}, "model": "llama3"})
	k1 := FlightKey(http.MethodPost, "llama3", body)
	k2 := FlightKey(http.MethodPost, "llama3", body)
	if k1 != k2 {
		t.Fatalf("identical inputs produced different keys: %s vs %s", k1, k2)
	}
}

func TestFlightKey_DifferentInputs(t *testing.T) {
	t.Parallel()
	body := jsonBody(t, map[string]any{"messages": []string{"hi"}, "model": "llama3"})
	k1 := FlightKey(http.MethodPost, "llama3", body)
	k2 := FlightKey(http.MethodPost, "qwen3", body)
	if k1 == k2 {
		t.Fatal("different models produced the same key")
	}

	body2 := jsonBody(t, map[string]any{"messages": []string{"bye"}, "model": "llama3"})
	k3 := FlightKey(http.MethodPost, "llama3", body2)
	if k1 == k3 {
		t.Fatal("different bodies produced the same key")
	}
}

func TestCoalescer_SingleflightDedup(t *testing.T) {
	ResetCoalesceCountersForTest()

	var callCount atomic.Int32
	co := NewCoalescer(250*time.Millisecond, 512)

	// Use a barrier so all goroutines arrive at Do simultaneously.
	var wg sync.WaitGroup
	const n = 10
	start := make(chan struct{})
	results := make([]AssistantMessage, n)

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			msg, _, _, err := co.Do("key-1", func() (AssistantMessage, string, []byte, error) {
				callCount.Add(1)
				time.Sleep(20 * time.Millisecond) // simulate upstream latency
				return AssistantMessage{Content: "shared"}, "test-model", nil, nil
			})
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			results[idx] = msg
		}(i)
	}
	close(start)
	wg.Wait()

	if got := callCount.Load(); got != 1 {
		t.Fatalf("expected 1 upstream call, got %d", got)
	}
	for i, r := range results {
		if r.Content != "shared" {
			t.Errorf("goroutine %d got %q, want %q", i, r.Content, "shared")
		}
	}

	// 1 miss (leader) + n-1 hits (waiters)
	if got := CoalesceMissesTotal(); got != 1 {
		t.Errorf("expected 1 miss, got %d", got)
	}
	if got := CoalesceHitsTotal(); got != uint64(n-1) {
		t.Errorf("expected %d hits, got %d", n-1, got)
	}
}

func TestCoalescer_TTLCacheHit(t *testing.T) {
	ResetCoalesceCountersForTest()

	var callCount atomic.Int32
	co := NewCoalescer(500*time.Millisecond, 512)

	callFn := func() (AssistantMessage, string, []byte, error) {
		callCount.Add(1)
		return AssistantMessage{Content: "cached"}, "model", nil, nil
	}

	// First call: miss, executes fn.
	msg1, _, _, _ := co.Do("key-ttl", callFn)
	if msg1.Content != "cached" {
		t.Fatalf("first call: %q", msg1.Content)
	}
	if callCount.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", callCount.Load())
	}

	// Second call within TTL: hit, fn NOT executed.
	msg2, _, _, _ := co.Do("key-ttl", callFn)
	if msg2.Content != "cached" {
		t.Fatalf("second call: %q", msg2.Content)
	}
	if callCount.Load() != 1 {
		t.Fatalf("expected 1 call after cache hit, got %d", callCount.Load())
	}

	if CoalesceHitsTotal() != 1 {
		t.Errorf("expected 1 hit, got %d", CoalesceHitsTotal())
	}
	if CoalesceMissesTotal() != 1 {
		t.Errorf("expected 1 miss, got %d", CoalesceMissesTotal())
	}
}

func TestCoalescer_TTLExpiry(t *testing.T) {
	ResetCoalesceCountersForTest()

	var callCount atomic.Int32
	co := NewCoalescer(30*time.Millisecond, 512)
	co.now = func() time.Time {
		return time.Now()
	}

	callFn := func() (AssistantMessage, string, []byte, error) {
		callCount.Add(1)
		return AssistantMessage{Content: "expiring"}, "model", nil, nil
	}

	// First call: miss.
	co.Do("key-exp", callFn)
	if callCount.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", callCount.Load())
	}

	// Wait for TTL to expire.
	time.Sleep(60 * time.Millisecond)

	// Second call: cache expired, fn executes again.
	co.Do("key-exp", callFn)
	if callCount.Load() != 2 {
		t.Fatalf("expected 2 calls after expiry, got %d", callCount.Load())
	}
}

func TestCoalescer_LRUEviction(t *testing.T) {
	ResetCoalesceCountersForTest()

	// maxEntries = 2: after inserting 3 distinct keys, the oldest is evicted.
	co := NewCoalescer(5*time.Second, 2)

	co.Do("a", stubFn("a"))
	co.Do("b", stubFn("b"))
	co.Do("c", stubFn("c"))

	co.mu.Lock()
	defer co.mu.Unlock()
	if len(co.cache) > 2 {
		t.Errorf("cache size %d exceeds cap %d", len(co.cache), 2)
	}
	if _, ok := co.cache["a"]; ok {
		t.Error("oldest entry 'a' should have been evicted")
	}
	if _, ok := co.cache["c"]; !ok {
		t.Error("newest entry 'c' should be present")
	}
}

func TestCoalescer_ErrorResultsShared(t *testing.T) {
	ResetCoalesceCountersForTest()

	var callCount atomic.Int32
	co := NewCoalescer(500*time.Millisecond, 512)

	errFn := func() (AssistantMessage, string, []byte, error) {
		callCount.Add(1)
		return AssistantMessage{}, "", nil, fmt.Errorf("upstream error")
	}

	// First call: miss, returns error.
	_, _, _, err1 := co.Do("key-err", errFn)
	if err1 == nil {
		t.Fatal("expected error on first call")
	}

	// Second call: cached error returned (within TTL).
	_, _, _, err2 := co.Do("key-err", errFn)
	if err2 == nil {
		t.Fatal("expected cached error on second call")
	}
	if callCount.Load() != 1 {
		t.Fatalf("fn should only execute once, got %d", callCount.Load())
	}
}

func TestCoalescer_DifferentKeysNoInterference(t *testing.T) {
	ResetCoalesceCountersForTest()

	var callCount atomic.Int32
	co := NewCoalescer(500*time.Millisecond, 512)

	co.Do("key-a", func() (AssistantMessage, string, []byte, error) {
		callCount.Add(1)
		return AssistantMessage{Content: "a"}, "model", nil, nil
	})
	co.Do("key-b", func() (AssistantMessage, string, []byte, error) {
		callCount.Add(1)
		return AssistantMessage{Content: "b"}, "model", nil, nil
	})

	if callCount.Load() != 2 {
		t.Fatalf("expected 2 calls for different keys, got %d", callCount.Load())
	}
	if CoalesceMissesTotal() != 2 {
		t.Errorf("expected 2 misses, got %d", CoalesceMissesTotal())
	}
	if CoalesceHitsTotal() != 0 {
		t.Errorf("expected 0 hits, got %d", CoalesceHitsTotal())
	}
}

func TestCoalescer_NilCoalescerNoOp(t *testing.T) {
	t.Parallel()
	// Cascade with nil Coalescer should behave normally (no coalescing).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"test","choices":[{"message":{"content":"hello"}}]}`)
	}))
	defer srv.Close()

	cas := &Cascade{
		Steps:   []CascadeStep{{Name: "test", URL: srv.URL, Model: "test"}},
		Timeout: 5 * time.Second,
		// Coalescer is nil — coalescing disabled.
	}

	var w httptest.ResponseRecorder
	payload := map[string]interface{}{"messages": []map[string]string{{"role": "user", "content": "hi"}}}
	res, err := cas.Run(t.Context(), &w, srv.Client(), payload, "test-req")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Succeeded {
		t.Fatal("expected cascade to succeed")
	}
}

func TestCascade_CoalescerDedupesConcurrentRequests(t *testing.T) {
	ResetCoalesceCountersForTest()

	var upstreamHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		time.Sleep(30 * time.Millisecond) // simulate latency so concurrent requests overlap
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"test","choices":[{"message":{"content":"coalesced"}}]}`)
	}))
	defer srv.Close()

	co := NewCoalescer(250*time.Millisecond, 512)
	cas := &Cascade{
		Steps:     []CascadeStep{{Name: "test", URL: srv.URL, Model: "test"}},
		Timeout:   5 * time.Second,
		Coalescer: co,
	}

	var wg sync.WaitGroup
	const n = 5
	start := make(chan struct{})
	recorders := make([]*httptest.ResponseRecorder, n)

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			rw := httptest.NewRecorder()
			recorders[idx] = rw
			payload := map[string]interface{}{"messages": []map[string]string{{"role": "user", "content": "identical"}}}
			cas.Run(t.Context(), rw, srv.Client(), payload, fmt.Sprintf("req-%d", idx))
		}(i)
	}
	close(start)
	wg.Wait()

	// Only 1 upstream call should have been made.
	if got := upstreamHits.Load(); got != 1 {
		t.Errorf("expected 1 upstream call, got %d", got)
	}

	// All responses should contain the same content.
	for i, rw := range recorders {
		body := rw.Body.String()
		if !strings.Contains(body, "coalesced") {
			t.Errorf("recorder %d body does not contain 'coalesced': %s", i, body)
		}
	}

	// 1 miss + n-1 hits
	if CoalesceMissesTotal() != 1 {
		t.Errorf("expected 1 miss, got %d", CoalesceMissesTotal())
	}
	if got := CoalesceHitsTotal(); got != uint64(n-1) {
		t.Errorf("expected %d hits, got %d", n-1, got)
	}
}

// --- helpers ---

func jsonBody(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func stubFn(content string) func() (AssistantMessage, string, []byte, error) {
	return func() (AssistantMessage, string, []byte, error) {
		return AssistantMessage{Content: content}, "model", nil, nil
	}
}
