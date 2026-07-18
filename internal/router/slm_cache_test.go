package router

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSLMCache_GetSet(t *testing.T) {
	c := NewSLMCache(100*time.Millisecond, 0)
	ctx := context.Background()
	if got, ok, _ := c.Get(ctx, "hello"); ok || got != "" {
		t.Errorf("empty cache: got (%v, %v, _), want (\"\", false, _)", got, ok)
	}

	c.Set(ctx, "hello", RouteLocal)
	got, ok, kind := c.Get(ctx, "hello")
	if !ok || got != RouteLocal || kind != CacheHitExact {
		t.Errorf("after Set: got (%v, %v, %v), want (RouteLocal, true, CacheHitExact)", got, ok, kind)
	}

	// Different key is still empty.
	if got, ok, _ := c.Get(ctx, "other"); ok || got != "" {
		t.Errorf("different key: got (%v, %v, _), want (\"\", false, _)", got, ok)
	}
}

func TestSLMCache_TTLExpiry(t *testing.T) {
	c := NewSLMCache(50*time.Millisecond, 0)
	ctx := context.Background()
	c.Set(ctx, "key", RouteFrontier)

	// Should be present immediately.
	if _, ok, _ := c.Get(ctx, "key"); !ok {
		t.Fatal("key missing immediately after Set")
	}

	// Wait for TTL to pass.
	time.Sleep(120 * time.Millisecond)

	// Should be expired now.
	if got, ok, _ := c.Get(ctx, "key"); ok || got != "" {
		t.Errorf("after TTL: got (%v, %v, _), want (\"\", false, _)", got, ok)
	}
}

func TestSLMCache_Overwrite(t *testing.T) {
	c := NewSLMCache(time.Hour, 0) // long TTL so expiry doesn't interfere
	ctx := context.Background()
	c.Set(ctx, "key", RouteLocal)
	c.Set(ctx, "key", RouteFrontier)

	got, ok, kind := c.Get(ctx, "key")
	if !ok || got != RouteFrontier || kind != CacheHitExact {
		t.Errorf("after overwrite: got (%v, %v, %v), want (RouteFrontier, true, CacheHitExact)", got, ok, kind)
	}
}

func TestSLMCache_Concurrent(t *testing.T) {
	c := NewSLMCache(time.Hour, 0)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "key"
			if i%2 == 0 {
				c.Set(ctx, key, RouteLocal)
			} else {
				c.Get(ctx, key)
			}
		}(i)
	}
	wg.Wait() // wait for all goroutines to complete
}

func TestSLMCache_Len(t *testing.T) {
	c := NewSLMCache(time.Hour, 0)
	ctx := context.Background()
	if n := c.Len(); n != 0 {
		t.Errorf("empty cache Len = %d, want 0", n)
	}
	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)
	if n := c.Len(); n != 2 {
		t.Errorf("after 2 sets: Len = %d, want 2", n)
	}
}

func TestNewSLMCache_ZeroTTL(t *testing.T) {
	c := NewSLMCache(0, 0)
	if c == nil {
		t.Fatal("NewSLMCache(0, 0) returned nil")
	}
	ctx := context.Background()
	// Should use default TTL (30s).
	c.Set(ctx, "k", RouteFrontier)
	if got, ok, kind := c.Get(ctx, "k"); !ok || got != RouteFrontier || kind != CacheHitExact {
		t.Errorf("with default TTL: got (%v, %v, %v), want (RouteFrontier, true, CacheHitExact)", got, ok, kind)
	}
}

func TestSLMCache_Stats(t *testing.T) {
	c := NewSLMCache(50*time.Millisecond, 0)
	ctx := context.Background()
	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)

	stats := c.Stats()
	if stats.Entries != 2 {
		t.Errorf("Entries = %d, want 2", stats.Entries)
	}
	if stats.Expired != 0 {
		t.Errorf("Expired = %d, want 0 immediately after set", stats.Expired)
	}

	// Wait for TTL to pass — entries become expired but are not evicted.
	time.Sleep(120 * time.Millisecond)
	stats = c.Stats()
	if got, ok, _ := c.Get(ctx, "a"); ok || got != "" {
		t.Errorf("a expired: got (%v, %v, _), want (\"\", false, _)", got, ok)
	}
	// Entries count still includes expired (not evicted until next write).
	if stats.Expired != 2 {
		t.Errorf("Expired = %d, want 2 after TTL", stats.Expired)
	}
}

// --- Semantic deduplication tests (issue #245) ---

// stubEmbedder records calls and returns configurable embeddings.
// It must be constructed via newStubEmbedder() to ensure the map is initialized.
type stubEmbedder struct {
	embeddings map[string][]float64 // prompt -> embedding
	calls      []string
}

func newStubEmbedder() *stubEmbedder {
	return &stubEmbedder{embeddings: make(map[string][]float64)}
}

func (s *stubEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	s.calls = append(s.calls, text)
	if emb, ok := s.embeddings[text]; ok {
		return emb, nil
	}
	// Return a unique embedding for each unseen prompt based on its hash
	// so different prompts always get different embeddings.
	h := uint64(0)
	for i := 0; i < len(text); i++ {
		h = h*31 + uint64(text[i])
	}
	dim := 4 // small fixed dimension for tests
	emb := make([]float64, dim)
	for i := range emb {
		emb[i] = float64((h >> uint(i)) & 1) // bit pattern from hash
	}
	s.embeddings[text] = emb
	return emb, nil
}

// vectorEmbedder returns a fixed embedding for all inputs; useful for
// testing that semantically different prompts do NOT match.
type vectorEmbedder struct {
	vec []float64
	err error
}

func (v *vectorEmbedder) Embed(_ context.Context, _ string) ([]float64, error) {
	out := make([]float64, len(v.vec))
	copy(out, v.vec)
	return out, v.err
}

func TestSLMCache_SemanticExactMatch(t *testing.T) {
	// Exact String match should always win over semantic when both exist.
	emb := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, 0, emb, 0.5)
	ctx := context.Background()

	// Set with semantic embedder.
	c.Set(ctx, "write a fibonacci function", RouteLocal)

	// Exact String match — should hit even though semantic could also match.
	got, ok, kind := c.Get(ctx, "write a fibonacci function")
	if !ok || got != RouteLocal || kind != CacheHitExact {
		t.Errorf("exact match: got (%v, %v, %v), want (RouteLocal, true, CacheHitExact)", got, ok, kind)
	}
}

func TestSLMCache_SemanticMatch(t *testing.T) {
	// When the embedder returns similar vectors for two different prompts,
	// semantic match should succeed if similarity > threshold.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, 0, stub, 0.5)
	ctx := context.Background()

	// Pre-populate the embeddings so that both prompts return similar vectors.
	// cosineSimilarity([1,0,0,0], [0.9,0.1,0,0]) ≈ 0.995
	stub.embeddings["write a fibonacci function"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["implement fibonacci recursively"] = []float64{0.9, 0.1, 0.0, 0.0}

	// Set the first prompt (this stores its embedding via Embed call).
	c.Set(ctx, "write a fibonacci function", RouteLocal)

	// Now Get with a semantically similar prompt — should hit via semantic match.
	got, ok, kind := c.Get(ctx, "implement fibonacci recursively")
	if !ok || got != RouteLocal || kind != CacheHitSemantic {
		t.Errorf("semantic match: got (%v, %v, %v), want (RouteLocal, true, CacheHitSemantic)", got, ok, kind)
	}
}

func TestSLMCache_SemanticNoMatch(t *testing.T) {
	// Two prompts whose embeddings are orthogonal should not match.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, 0, stub, 0.5)
	ctx := context.Background()

	// [1,0,0,0] and [0,1,0,0] are orthogonal — cosine = 0
	stub.embeddings["write a fibonacci function"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["explain quantum entanglement"] = []float64{0.0, 1.0, 0.0, 0.0}

	c.Set(ctx, "write a fibonacci function", RouteLocal)

	got, ok, kind := c.Get(ctx, "explain quantum entanglement")
	if ok || got != "" || kind != "" {
		t.Errorf("semantic mismatch: got (%v, %v, %v), want (\"\", false, \"\")", got, ok, kind)
	}
}

func TestSLMCache_SemanticBelowThreshold(t *testing.T) {
	// Embeddings that score below the threshold should not match.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, 0, stub, 0.85) // high threshold
	ctx := context.Background()

	// [1,0,0,0] and [0.5,0.0,0.5,0] — cosine = 0.5, below 0.85
	stub.embeddings["write a fibonacci function"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["implement fibonacci recursively"] = []float64{0.5, 0.0, 0.5, 0.0}

	c.Set(ctx, "write a fibonacci function", RouteLocal)

	got, ok, kind := c.Get(ctx, "implement fibonacci recursively")
	if ok || got != "" || kind != "" {
		t.Errorf("below threshold: got (%v, %v, %v), want (\"\", false, \"\")", got, ok, kind)
	}
}

func TestSLMCache_SemanticAboveThreshold(t *testing.T) {
	// Embeddings that score above the threshold should match.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, 0, stub, 0.85)
	ctx := context.Background()

	// [1,0,0,0] and [0.8,0.2,0,0] — cosine ≈ 0.98, above 0.85
	stub.embeddings["write a fibonacci function"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["implement fibonacci recursively"] = []float64{0.8, 0.2, 0.0, 0.0}

	c.Set(ctx, "write a fibonacci function", RouteLocal)

	got, ok, kind := c.Get(ctx, "implement fibonacci recursively")
	if !ok || got != RouteLocal || kind != CacheHitSemantic {
		t.Errorf("above threshold: got (%v, %v, %v), want (RouteLocal, true, CacheHitSemantic)", got, ok, kind)
	}
}

func TestSLMCache_SemanticEmbedError(t *testing.T) {
	// Embedder that always errors should fall back to no semantic match.
	errEmbed := &vectorEmbedder{err: errors.New("embedder unavailable")}
	c := NewSLMCacheWithEmbedder(time.Hour, 0, errEmbed, 0.5)
	ctx := context.Background()

	c.Set(ctx, "write a fibonacci function", RouteLocal)

	got, ok, kind := c.Get(ctx, "write a fibonacci function")
	// Exact match should still work.
	if !ok || got != RouteLocal || kind != CacheHitExact {
		t.Errorf("exact match failed: got (%v, %v, %v), want (RouteLocal, true, CacheHitExact)", got, ok, kind)
	}

	// Semantic lookup should fail gracefully (no panic) when embedder errors.
	_, ok, kind = c.Get(ctx, "different prompt")
	if ok || kind != "" {
		t.Errorf("semantic with embed error: got (%v, %v, %v), want (\"\", false, \"\")", got, ok, kind)
	}
}

func TestSLMCache_SemanticDisabled(t *testing.T) {
	// Cache without embedder should behave like exact-match only.
	c := NewSLMCache(time.Hour, 0) // no embedder
	ctx := context.Background()

	c.Set(ctx, "write a fibonacci function", RouteLocal)

	// Exact match works.
	got, ok, kind := c.Get(ctx, "write a fibonacci function")
	if !ok || got != RouteLocal || kind != CacheHitExact {
		t.Errorf("exact match: got (%v, %v, %v), want (RouteLocal, true, CacheHitExact)", got, ok, kind)
	}

	// Semantic should not apply — different string is a miss.
	got, ok, kind = c.Get(ctx, "implement fibonacci recursively")
	if ok || got != "" || kind != "" {
		t.Errorf("semantic not enabled: got (%v, %v, %v), want (\"\", false, \"\")", got, ok, kind)
	}
}

func TestSLMCache_SetEmbedding(t *testing.T) {
	// SetEmbedding stores a pre-computed embedding without calling Embed.
	c := NewSLMCacheWithEmbedder(time.Hour, 0, newStubEmbedder(), 0.5)
	ctx := context.Background()

	emb := []float64{1.0, 0.5, 0.0, 0.0}
	c.SetEmbedding("write a fibonacci function", RouteLocal, emb)

	got, ok, kind := c.Get(ctx, "write a fibonacci function")
	if !ok || got != RouteLocal || kind != CacheHitExact {
		t.Errorf("SetEmbedding exact match: got (%v, %v, %v), want (RouteLocal, true, CacheHitExact)", got, ok, kind)
	}
}

func TestSLMCache_SemanticTTLExpiry(t *testing.T) {
	// Semantic matches should respect TTL.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(50*time.Millisecond, 0, stub, 0.5)
	ctx := context.Background()

	stub.embeddings["write a fibonacci function"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["implement fibonacci recursively"] = []float64{0.9, 0.1, 0.0, 0.0}
	c.Set(ctx, "write a fibonacci function", RouteLocal)

	// Immediate semantic hit.
	got, ok, kind := c.Get(ctx, "implement fibonacci recursively")
	if !ok || got != RouteLocal || kind != CacheHitSemantic {
		t.Errorf("immediate semantic hit: got (%v, %v, %v), want (RouteLocal, true, CacheHitSemantic)", got, ok, kind)
	}

	// Wait for TTL to pass.
	time.Sleep(120 * time.Millisecond)

	// Both exact and semantic should be expired.
	got, ok, kind = c.Get(ctx, "write a fibonacci function")
	if ok || got != "" || kind != "" {
		t.Errorf("exact expired: got (%v, %v, %v), want (\"\", false, \"\")", got, ok, kind)
	}

	got, ok, kind = c.Get(ctx, "implement fibonacci recursively")
	if ok || got != "" || kind != "" {
		t.Errorf("semantic expired: got (%v, %v, %v), want (\"\", false, \"\")", got, ok, kind)
	}
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name   string
		a, b   []float64
		want   float64
		absTol float64
	}{
		{"identical", []float64{1, 0, 0}, []float64{1, 0, 0}, 1.0, 0.001},
		{"orthogonal", []float64{1, 0, 0}, []float64{0, 1, 0}, 0.0, 0.001},
		{"opposite", []float64{1, 0, 0}, []float64{-1, 0, 0}, -1.0, 0.001},
		{"partial", []float64{1, 0, 0}, []float64{0.5, 0.5, 0}, 0.707, 0.01},
		{"longer a", []float64{1, 0, 0, 0, 0}, []float64{1, 0, 0}, 1.0, 0.001},
		{"longer b", []float64{1, 0, 0}, []float64{1, 0, 0, 0, 0}, 1.0, 0.001},
		{"zero a", []float64{0, 0, 0}, []float64{1, 0, 0}, 0.0, 0.001},
		{"zero b", []float64{1, 0, 0}, []float64{0, 0, 0}, 0.0, 0.001},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cosineSimilarity(tt.a, tt.b)
			if got < tt.want-tt.absTol || got > tt.want+tt.absTol {
				t.Errorf("cosineSimilarity(%v, %v) = %v, want %v ± %v",
					tt.a, tt.b, got, tt.want, tt.absTol)
			}
		})
	}
}

func TestNewSLMCacheWithEmbedder_Defaults(t *testing.T) {
	emb := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(0, 0, emb, 0) // zero ttl and threshold

	if c == nil {
		t.Fatal("NewSLMCacheWithEmbedder(0, 0, emb, 0) returned nil")
	}
	if c.ttl != DefaultSLMCacheTTL {
		t.Errorf("ttl = %v, want %v", c.ttl, DefaultSLMCacheTTL)
	}
	if c.semThreshold != DefaultSemanticThreshold {
		t.Errorf("semThreshold = %v, want %v", c.semThreshold, DefaultSemanticThreshold)
	}

	ctx := context.Background()
	c.Set(ctx, "k", RouteFrontier)
	if got, ok, kind := c.Get(ctx, "k"); !ok || got != RouteFrontier || kind != CacheHitExact {
		t.Errorf("basic operation: got (%v, %v, %v), want (RouteFrontier, true, CacheHitExact)", got, ok, kind)
	}
}

func TestSLMCache_MaxEntriesEviction(t *testing.T) {
	// When maxEntries is reached, expired entries are evicted first,
	// then LRU (oldest by stamp) if still over capacity.
	c := NewSLMCache(time.Hour, 3) // max 3 entries
	ctx := context.Background()

	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)
	c.Set(ctx, "c", RouteLocal)
	if c.Len() != 3 {
		t.Errorf("Len = %d, want 3", c.Len())
	}

	// d should evict the oldest (a) since all are non-expired.
	c.Set(ctx, "d", RouteFrontier)
	if c.Len() != 3 {
		t.Errorf("after d: Len = %d, want 3", c.Len())
	}

	// a should be evicted, but b, c, d should remain.
	if got, ok, _ := c.Get(ctx, "a"); ok || got != "" {
		t.Errorf("a was evicted: got (%v, %v, _), want (\"\", false, _)", got, ok)
	}
	for _, key := range []string{"b", "c", "d"} {
		if got, ok, _ := c.Get(ctx, key); !ok || got == "" {
			t.Errorf("%s still present: got (%v, %v, _), want (Route, true, _)", key, got, ok)
		}
	}
}

func TestSLMCache_ExpiredEvictionOnSet(t *testing.T) {
	// Expired entries should be evicted before LRU eviction.
	// Use TTL=100ms so "a" expires but "b" doesn't after 60ms.
	c := NewSLMCache(100*time.Millisecond, 2) // TTL 100ms, max 2 entries
	ctx := context.Background()

	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteFrontier)

	// Wait for "a" to expire but "b" should still be valid.
	// a expires at ~50ms, b at ~51ms. After 60ms, only a is expired.
	time.Sleep(60 * time.Millisecond)

	// Adding "c" should evict the expired "a" first, not "b".
	c.Set(ctx, "c", RouteLocal)

	if c.Len() != 2 {
		t.Errorf("Len = %d, want 2", c.Len())
	}

	// "a" should be gone (expired).
	if got, ok, _ := c.Get(ctx, "a"); ok || got != "" {
		t.Errorf("a expired: got (%v, %v, _), want (\"\", false, _)", got, ok)
	}

	// "b" and "c" should remain.
	if got, ok, _ := c.Get(ctx, "b"); !ok || got != RouteFrontier {
		t.Errorf("b present: got (%v, %v, _), want (RouteFrontier, true, _)", got, ok)
	}
	if got, ok, _ := c.Get(ctx, "c"); !ok || got != RouteLocal {
		t.Errorf("c present: got (%v, %v, _), want (RouteLocal, true, _)", got, ok)
	}
}

func TestSLMCache_MaxEntriesDefault(t *testing.T) {
	// Zero maxEntries should use the default.
	c := NewSLMCache(time.Hour, 0)
	if c.maxEntries != DefaultSLMCacheMaxEntries {
		t.Errorf("maxEntries = %d, want %d", c.maxEntries, DefaultSLMCacheMaxEntries)
	}
}

// --- Eviction counters (issue #449) ---

func TestSLMCache_EvictionCounters_LRUPressure(t *testing.T) {
	// When the cache is at capacity and a new key arrives, the LRU
	// eviction path must bump LRUEvictions exactly once and leave
	// TTLEvictions untouched. Stats() must surface both counters.
	c := NewSLMCache(time.Hour, 3)
	ctx := context.Background()

	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)
	c.Set(ctx, "c", RouteLocal)

	stats := c.Stats()
	if stats.TTLEvictions != 0 || stats.LRUEvictions != 0 {
		t.Fatalf("pre-eviction counters: ttl=%d lru=%d, want 0/0", stats.TTLEvictions, stats.LRUEvictions)
	}

	// Fourth insert at full capacity should trigger exactly one LRU eviction.
	c.Set(ctx, "d", RouteFrontier)

	stats = c.Stats()
	if stats.TTLEvictions != 0 {
		t.Errorf("TTLEvictions = %d, want 0 (no TTL expiry expected)", stats.TTLEvictions)
	}
	if stats.LRUEvictions != 1 {
		t.Errorf("LRUEvictions = %d, want 1", stats.LRUEvictions)
	}

	// Two more inserts at full capacity → two more LRU evictions.
	c.Set(ctx, "e", RouteLocal)
	c.Set(ctx, "f", RouteLocal)

	stats = c.Stats()
	if stats.TTLEvictions != 0 {
		t.Errorf("TTLEvictions = %d, want 0", stats.TTLEvictions)
	}
	if stats.LRUEvictions != 3 {
		t.Errorf("LRUEvictions = %d, want 3", stats.LRUEvictions)
	}
}

func TestSLMCache_EvictionCounters_TTLChurn(t *testing.T) {
	// When entries are already expired when Set runs, the TTL path
	// must bump TTLEvictions and leave LRUEvictions untouched.
	// Use maxEntries=2 so that the very next Set is at capacity and
	// triggers the eviction path even before any LRU pressure.
	c := NewSLMCache(50*time.Millisecond, 2)
	ctx := context.Background()

	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)

	// Wait for TTL to elapse on both entries.
	time.Sleep(80 * time.Millisecond)

	// Inserting "c" fills the cache to capacity → evictExpired runs
	// → both "a" and "b" are removed by the TTL path. "c" is fresh
	// and fits, so no LRU eviction is needed.
	c.Set(ctx, "c", RouteFrontier)

	stats := c.Stats()
	if stats.TTLEvictions != 2 {
		t.Errorf("TTLEvictions = %d, want 2", stats.TTLEvictions)
	}
	if stats.LRUEvictions != 0 {
		t.Errorf("LRUEvictions = %d, want 0", stats.LRUEvictions)
	}
	if got, ok, _ := c.Get(ctx, "a"); ok {
		t.Errorf("a should be evicted by TTL: got (%v, %v)", got, ok)
	}
	if got, ok, _ := c.Get(ctx, "b"); ok {
		t.Errorf("b should be evicted by TTL: got (%v, %v)", got, ok)
	}
	if got, ok, _ := c.Get(ctx, "c"); !ok || got != RouteFrontier {
		t.Errorf("c should be present: got (%v, %v)", got, ok)
	}
}

func TestSLMCache_EvictionObserver(t *testing.T) {
	// The eviction observer must fire once per removed entry with the
	// correct reason. It must also fire AFTER the cache lock is
	// released (we verify this indirectly by re-entering Set from
	// inside the observer without deadlocking).
	c := NewSLMCache(50*time.Millisecond, 1)
	ctx := context.Background()

	var mu sync.Mutex
	var events []string
	c.SetEvictionObserver(func(reason string) {
		mu.Lock()
		events = append(events, reason)
		mu.Unlock()
	})

	c.Set(ctx, "a", RouteLocal)
	time.Sleep(80 * time.Millisecond) // "a" now expired
	c.Set(ctx, "b", RouteLocal)       // triggers TTL eviction of "a"

	c.Set(ctx, "c", RouteLocal) // triggers LRU eviction of "b" (still fresh)

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("observer fired %d times, want 2; events=%v", len(events), events)
	}
	if events[0] != EvictionReasonTTL {
		t.Errorf("first event = %q, want %q", events[0], EvictionReasonTTL)
	}
	if events[1] != EvictionReasonLRU {
		t.Errorf("second event = %q, want %q", events[1], EvictionReasonLRU)
	}
}

func TestSLMCache_EvictionObserver_NilSafe(t *testing.T) {
	// Without an observer registered, Set must still succeed and the
	// counters must still bump. This guards against accidentally
	// nil-derefing on the hot path.
	c := NewSLMCache(time.Hour, 2)
	ctx := context.Background()
	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)
	c.Set(ctx, "c", RouteLocal) // should LRU-evict "a" without crashing

	stats := c.Stats()
	if stats.LRUEvictions != 1 {
		t.Errorf("LRUEvictions = %d, want 1", stats.LRUEvictions)
	}
}

func TestSLMCache_EvictionCounters_Concurrent(t *testing.T) {
	// Run the race detector over the eviction paths: a stream of
	// goroutines Set unique keys (so LRU pressure kicks in) while
	// another stream calls Stats(). The eviction counters must end
	// up consistent: every eviction either bumps TTL or LRU, never
	// both, and Stats() must never race.
	//
	// Capacity is intentionally small (4) so LRU evictions happen
	// quickly; keys are unique per iteration so the cache churns.
	c := NewSLMCache(time.Hour, 4)
	ctx := context.Background()

	var writers sync.WaitGroup
	var readers sync.WaitGroup
	const goroutines = 8
	const iterations = 200

	for g := 0; g < goroutines; g++ {
		writers.Add(1)
		go func(g int) {
			defer writers.Done()
			for i := 0; i < iterations; i++ {
				// Unique key per goroutine+iteration so we always
				// exceed capacity and trigger LRU evictions.
				key := fmt.Sprintf("k-%d-%d", g, i)
				c.Set(ctx, key, RouteLocal)
			}
		}(g)
	}

	// Readers: continuously poll Stats() to exercise the atomic loads.
	// They stop on a channel signal so the test can finish promptly
	// once the writers are done.
	stop := make(chan struct{})
	for g := 0; g < 4; g++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = c.Stats()
				}
			}
		}()
	}

	writers.Wait()
	close(stop)
	readers.Wait()

	stats := c.Stats()
	// With goroutines*iterations = 1600 unique keys into a 4-entry
	// cache, LRU evictions must dominate. Sanity-check that the
	// counter moved.
	if stats.LRUEvictions == 0 {
		t.Fatalf("expected LRU evictions under sustained write load, got 0 (ttl=%d)", stats.TTLEvictions)
	}
	t.Logf("concurrent eviction counters: ttl=%d lru=%d", stats.TTLEvictions, stats.LRUEvictions)
}

func TestSLMCache_EvictionReasonConstants(t *testing.T) {
	// Pin the label values so observability documentation cannot drift
	// silently from the cache implementation (issue #449).
	if EvictionReasonTTL != "ttl" {
		t.Errorf("EvictionReasonTTL = %q, want %q", EvictionReasonTTL, "ttl")
	}
	if EvictionReasonLRU != "lru" {
		t.Errorf("EvictionReasonLRU = %q, want %q", EvictionReasonLRU, "lru")
	}
}
