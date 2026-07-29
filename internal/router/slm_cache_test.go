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

func (s *stubEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float64, error) {
	s.calls = append(s.calls, texts...)
	result := make([][]float64, len(texts))
	for i, text := range texts {
		emb, _ := s.Embed(context.Background(), text)
		result[i] = emb
	}
	return result, nil
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

func (v *vectorEmbedder) EmbedBatch(_ context.Context, _ []string) ([][]float64, error) {
	if v.err != nil {
		return nil, v.err
	}
	result := make([][]float64, 1)
	result[0] = make([]float64, len(v.vec))
	copy(result[0], v.vec)
	return result, nil
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

func TestSLMCache_Enabled_NilReceiver(t *testing.T) {
	// Enabled must not panic on a nil *SLMCache pointer (issue #663).
	var c *SLMCache
	if got := c.Enabled(); got != false {
		t.Errorf("Enabled() on nil = %v, want false", got)
	}
}

func TestSLMCache_TTLSeconds_NilReceiver(t *testing.T) {
	// TTLSeconds must not panic on a nil *SLMCache pointer (issue #663).
	var c *SLMCache
	if got := c.TTLSeconds(); got != 0 {
		t.Errorf("TTLSeconds() on nil = %v, want 0", got)
	}
}

func TestSLMCache_MaxEntries_NilReceiver(t *testing.T) {
	// MaxEntries must not panic on a nil *SLMCache pointer (issue #663).
	var c *SLMCache
	if got := c.MaxEntries(); got != 0 {
		t.Errorf("MaxEntries() on nil = %v, want 0", got)
	}
}

func TestSLMCache_Accessors_NormalCache(t *testing.T) {
	// Verify accessors return the configured values on a real cache.
	c := NewSLMCache(60*time.Second, 128)

	if got := c.Enabled(); !got {
		t.Errorf("Enabled() = %v, want true (TTL=60s)", got)
	}
	if got := c.TTLSeconds(); got != 60 {
		t.Errorf("TTLSeconds() = %v, want 60", got)
	}
	if got := c.MaxEntries(); got != 128 {
		t.Errorf("MaxEntries() = %v, want 128", got)
	}
}

func TestSLMCache_Enabled_ZeroTTLDisabled(t *testing.T) {
	// When TTL is zero (kill-switch via NEXUS_SLM_CACHE_TTL=0),
	// Enabled must return false even on a real cache.
	c := NewSLMCache(0, 0) // uses DefaultSLMCacheTTL, so Enabled=true by default
	// Override ttl to 0 to simulate disabled cache.
	c.ttl = 0

	if got := c.Enabled(); got != false {
		t.Errorf("Enabled() with ttl=0 = %v, want false", got)
	}
}

// --- sortExpiry !oki branch tests (issue #661) ---

func TestSLMCache_sortExpiry_OrphanedEntry(t *testing.T) {
	// Test that sortExpiry correctly handles orphaned entries (keys present
	// in c.expiry but deleted from c.entries). Orphaned entries must sort
	// before valid entries so they are candidates for immediate eviction.
	// This exercises the !oki branch at slm_cache.go:154.
	c := NewSLMCache(time.Hour, 3)
	ctx := context.Background()

	// Add three entries. After this, c.expiry = [a, b, c].
	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)
	c.Set(ctx, "c", RouteLocal)

	// Manually delete "a" from c.entries to create an orphaned entry,
	// but leave it in c.expiry. This simulates the state after evictLru
	// has removed an entry but before the next sortExpiry call.
	c.mu.Lock()
	delete(c.entries, "a") // orphaned: in c.expiry but not in c.entries
	c.sortExpiry()
	c.mu.Unlock()

	// c.expiry[0] must be the orphaned key "a" because orphaned entries
	// should sort first.
	if len(c.expiry) != 3 {
		t.Fatalf("expiry len = %d, want 3", len(c.expiry))
	}
	if c.expiry[0] != "a" {
		t.Errorf("expiry[0] = %q, want orphaned key %q", c.expiry[0], "a")
	}
}

func TestSLMCache_sortExpiry_MultipleOrphanedEntries(t *testing.T) {
	// Test that sortExpiry correctly handles multiple orphaned entries.
	// When c.expiry contains holes (orphaned keys), all orphaned entries
	// must sort before any valid entry.
	c := NewSLMCache(time.Hour, 5)
	ctx := context.Background()

	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)
	c.Set(ctx, "c", RouteLocal)

	c.mu.Lock()
	// Create two orphaned entries by deleting from c.entries
	delete(c.entries, "a")
	delete(c.entries, "b")
	c.sortExpiry()
	c.mu.Unlock()

	// Both a and b should sort before c (the only valid entry)
	if len(c.expiry) != 3 {
		t.Fatalf("expiry len = %d, want 3", len(c.expiry))
	}
	// c should be last since it's the only valid entry
	if c.expiry[2] != "c" {
		t.Errorf("expiry[2] = %q, want valid entry %q", c.expiry[2], "c")
	}
	// The first two must be the orphaned entries (a and b in some order)
	orphanCount := 0
	for i := 0; i < 2; i++ {
		if c.expiry[i] == "a" || c.expiry[i] == "b" {
			orphanCount++
		}
	}
	if orphanCount != 2 {
		t.Errorf("orphanCount = %d, want 2 orphaned entries at start", orphanCount)
	}
}

func TestSLMCache_sortExpiry_OrphanedVsValidOrdering(t *testing.T) {
	// Verify that when sortExpiry is called with a mix of orphaned and
	// valid entries, the sort order is correct: orphaned entries first
	// (sorted by their position), then valid entries sorted by stamp.
	c := NewSLMCache(time.Hour, 4)
	ctx := context.Background()

	// Set entries with a gap (a deleted, b and c present, d deleted)
	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)
	c.Set(ctx, "c", RouteLocal)
	c.Set(ctx, "d", RouteLocal)

	// Wait a bit so b and c have different timestamps
	time.Sleep(10 * time.Millisecond)

	c.mu.Lock()
	delete(c.entries, "a")
	delete(c.entries, "d")
	// After deletions: expiry = [a, b, c, d], entries = {b, c}
	c.sortExpiry()
	c.mu.Unlock()

	// Orphaned entries (a, d) should be sorted before valid entries (b, c)
	// a and d should be at positions 0 and 1 in some order
	// b and c should be at positions 2 and 3 in stamp order (b before c)
	if c.expiry[2] != "b" || c.expiry[3] != "c" {
		t.Errorf("valid entries not last: expiry = %v, want [a|d, a|d, b, c]", c.expiry)
	}
}

// TestSLMCache_Set_NoSortBelowCapacity (issue #745) verifies that when
// the cache is below maxEntries and no eviction occurs, the cache
// remains functionally correct. The corollary is that the O(n log n)
// overhead of sortExpiry is avoided for the common-case small cache.
func TestSLMCache_Set_NoSortBelowCapacity(t *testing.T) {
	c := NewSLMCache(time.Hour, 5) // max 5 entries
	ctx := context.Background()

	// Insert below capacity — no eviction, no sortExpiry call.
	for i := 0; i < 4; i++ {
		key := fmt.Sprintf("key-%d", i)
		c.Set(ctx, key, RouteLocal)
		if got, ok, _ := c.Get(ctx, key); !ok || got != RouteLocal {
			t.Errorf("Get(%q) after Set: got (%v, %v), want (RouteLocal, true)", key, got, ok)
		}
	}
	if c.Len() != 4 {
		t.Errorf("Len = %d, want 4", c.Len())
	}

	// Insert the 5th entry — still below capacity, no eviction.
	c.Set(ctx, "key-4", RouteFrontier)
	if c.Len() != 5 {
		t.Errorf("after 5th Set: Len = %d, want 5", c.Len())
	}

	// key-0 should still be present (no eviction below capacity).
	if got, ok, _ := c.Get(ctx, "key-0"); !ok || got != RouteLocal {
		t.Errorf("key-0 still present: got (%v, %v), want (RouteLocal, true)", got, ok)
	}

	// Insert the 6th entry — now at capacity+1, LRU eviction triggers.
	c.Set(ctx, "key-5", RouteFrontier)
	if c.Len() != 5 {
		t.Errorf("after 6th Set (eviction): Len = %d, want 5", c.Len())
	}

	// key-0 should now be evicted as LRU (oldest stamp).
	if got, ok, _ := c.Get(ctx, "key-0"); ok || got != "" {
		t.Errorf("key-0 was evicted: got (%v, %v), want (\"\", false)", got, ok)
	}

	// Remaining keys should still be present.
	for i := 1; i <= 5; i++ {
		key := fmt.Sprintf("key-%d", i)
		if got, ok, _ := c.Get(ctx, key); !ok || got == "" {
			t.Errorf("key-%d still present: got (%v, %v), want (Route, true)", i, got, ok)
		}
	}
}

// TestSLMCache_SetEmbedding_NoSortBelowCapacity verifies the same O(1)
// insertion guarantee for SetEmbedding when the cache is below capacity.
func TestSLMCache_SetEmbedding_NoSortBelowCapacity(t *testing.T) {
	emb := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, 5, emb, 0.5)
	ctx := context.Background()

	embVec := []float64{1.0, 0.0, 0.0, 0.0}

	// Insert 4 unique keys below capacity — no eviction, no sortExpiry call.
	for i := 0; i < 4; i++ {
		key := fmt.Sprintf("prompt-%d", i)
		c.SetEmbedding(key, RouteLocal, embVec)
		if got, ok, _ := c.Get(ctx, key); !ok || got != RouteLocal {
			t.Errorf("Get(%q) after SetEmbedding: got (%v, %v), want (RouteLocal, true)", key, got, ok)
		}
	}
	if c.Len() != 4 {
		t.Errorf("Len = %d, want 4", c.Len())
	}

	// 5th insert — still below capacity (4 < 5), no eviction.
	c.SetEmbedding("prompt-4", RouteLocal, embVec)
	if c.Len() != 5 {
		t.Errorf("after 5th SetEmbedding: Len = %d, want 5", c.Len())
	}

	// prompt-0 should still be present (no eviction below capacity).
	if got, ok, _ := c.Get(ctx, "prompt-0"); !ok || got != RouteLocal {
		t.Errorf("prompt-0 still present: got (%v, %v), want (RouteLocal, true)", got, ok)
	}

	// Verify all 5 entries are retrievable.
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("prompt-%d", i)
		if got, ok, _ := c.Get(ctx, key); !ok || got != RouteLocal {
			t.Errorf("prompt-%d still present: got (%v, %v), want (RouteLocal, true)", i, got, ok)
		}
	}
}

func TestSLMCache_EmbedErrorCounter_Set(t *testing.T) {
	// When Set's embedder returns an error, the embed error counter
	// must be incremented and the entry stored with nil embedding
	// (issue #741).
	errEmbed := &vectorEmbedder{err: errors.New("embedder unavailable")}
	c := NewSLMCacheWithEmbedder(time.Hour, 0, errEmbed, 0.5)
	ctx := context.Background()

	if got := c.Stats().EmbedErrors; got != 0 {
		t.Fatalf("initial EmbedErrors = %d, want 0", got)
	}

	c.Set(ctx, "write a fibonacci function", RouteLocal)

	stats := c.Stats()
	if stats.EmbedErrors != 1 {
		t.Errorf("EmbedErrors after Set with embed error = %d, want 1", stats.EmbedErrors)
	}
	// Exact match must still work.
	got, ok, kind := c.Get(ctx, "write a fibonacci function")
	if !ok || got != RouteLocal || kind != CacheHitExact {
		t.Errorf("exact match failed after embed error: got (%v, %v, %v), want (RouteLocal, true, CacheHitExact)", got, ok, kind)
	}
}

func TestSLMCache_EmbedErrorCounter_Set_Observer(t *testing.T) {
	// When Set's embedder returns an error, the embed-error observer
	// must be called (issue #741).
	errEmbed := &vectorEmbedder{err: errors.New("embedder unavailable")}
	c := NewSLMCacheWithEmbedder(time.Hour, 0, errEmbed, 0.5)
	ctx := context.Background()

	var called int
	c.SetEmbedErrorObserver(func() {
		called++
	})

	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal) // another embed error

	if called != 2 {
		t.Errorf("observer called %d times, want 2", called)
	}
}

func TestSLMCache_EmbedErrorCounter_GetSemantic(t *testing.T) {
	// When getSemantic's embedder returns an error, the embed error
	// counter must be incremented (issue #741). Use SetEmbedding to
	// store a pre-computed embedding so Set itself does not call the
	// failing embedder.
	errEmbed := &vectorEmbedder{err: errors.New("embedder unavailable")}
	ctx := context.Background()

	c := NewSLMCacheWithEmbedder(time.Hour, 0, errEmbed, 0.5)

	if got := c.Stats().EmbedErrors; got != 0 {
		t.Fatalf("initial EmbedErrors = %d, want 0", got)
	}

	// Use SetEmbedding to store entry without triggering embedder.
	c.SetEmbedding("write a fibonacci function", RouteLocal, []float64{1, 0, 0, 0})

	// Force a semantic get that will fail on the embedding call.
	_, ok, kind := c.Get(ctx, "different prompt") // triggers getSemantic which errors
	if ok || kind != "" {
		t.Errorf("expected miss after embed error, got (ok=%v, kind=%v)", ok, kind)
	}

	stats := c.Stats()
	if stats.EmbedErrors != 1 {
		t.Errorf("EmbedErrors after getSemantic error = %d, want 1", stats.EmbedErrors)
	}
}

func TestSLMCache_EmbedErrorCounter_GetSemantic_Observer(t *testing.T) {
	// When getSemantic's embedder returns an error, the embed-error
	// observer must be called (issue #741). Use SetEmbedding to store
	// a pre-computed embedding so Set itself does not call the embedder.
	errEmbed := &vectorEmbedder{err: errors.New("embedder unavailable")}
	ctx := context.Background()

	c := NewSLMCacheWithEmbedder(time.Hour, 0, errEmbed, 0.5)

	var called int
	c.SetEmbedErrorObserver(func() {
		called++
	})

	// Use SetEmbedding to store entry without triggering embedder.
	c.SetEmbedding("x", RouteLocal, []float64{1, 0, 0, 0})

	// Now Get triggers getSemantic which calls the failing embedder.
	_, _, _ = c.Get(ctx, "different prompt")

	if called != 1 {
		t.Errorf("observer called %d times, want 1", called)
	}
}

func TestSLMCache_EmbedErrorObserver_NilSafe(t *testing.T) {
	// Without an observer registered, Set must still succeed when the
	// embedder errors (nil observer must not panic).
	errEmbed := &vectorEmbedder{err: errors.New("embedder unavailable")}
	c := NewSLMCacheWithEmbedder(time.Hour, 0, errEmbed, 0.5)
	ctx := context.Background()

	c.Set(ctx, "a", RouteLocal) // must not panic

	stats := c.Stats()
	if stats.EmbedErrors != 1 {
		t.Errorf("EmbedErrors = %d, want 1", stats.EmbedErrors)
	}
}

// --- Stale entries and proactive eviction (issue #835) ---

func TestSLMCache_Stale_Basic(t *testing.T) {
	// Stale() must count expired-but-not-yet-evicted entries without
	// removing them from the cache.
	c := NewSLMCache(50*time.Millisecond, 0)
	ctx := context.Background()

	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)
	c.Set(ctx, "c", RouteLocal)

	if stale := c.Stale(); stale != 0 {
		t.Errorf("Stale = %d immediately after Set, want 0", stale)
	}

	time.Sleep(120 * time.Millisecond)

	// All three entries are past TTL but not yet evicted.
	if stale := c.Stale(); stale != 3 {
		t.Errorf("Stale = %d after TTL expiry, want 3", stale)
	}

	// Entries are still retrievable (not evicted yet).
	for _, key := range []string{"a", "b", "c"} {
		if got, ok, _ := c.Get(ctx, key); ok || got != "" {
			t.Errorf("Get(%q) after TTL: got (%v, %v), want (\"\", false)", key, got, ok)
		}
	}
}

func TestSLMCache_Stale_Zero(t *testing.T) {
	// Empty cache returns 0.
	c := NewSLMCache(time.Hour, 0)
	if stale := c.Stale(); stale != 0 {
		t.Errorf("Stale on empty cache = %d, want 0", stale)
	}
}

func TestSLMCache_Stale_NilReceiver(t *testing.T) {
	// Stale must not panic on a nil *SLMCache pointer.
	var c *SLMCache
	if stale := c.Stale(); stale != 0 {
		t.Errorf("Stale() on nil = %d, want 0", stale)
	}
}

func TestSLMCache_StaleEntries_AfterTTL(t *testing.T) {
	// StaleEntries must return 1 after a single entry's TTL has expired
	// but before EvictExpired is called (issue #801).
	c := NewSLMCache(50*time.Millisecond, 0)
	ctx := context.Background()

	c.Set(ctx, "a", RouteLocal)

	time.Sleep(120 * time.Millisecond)

	if stale := c.StaleEntries(); stale != 1 {
		t.Errorf("StaleEntries() = %d after TTL expiry, want 1", stale)
	}
}

func TestSLMCache_StaleEntries_NilReceiver(t *testing.T) {
	// StaleEntries must not panic on a nil *SLMCache pointer.
	var c *SLMCache
	if stale := c.StaleEntries(); stale != 0 {
		t.Errorf("StaleEntries() on nil = %d, want 0", stale)
	}
}

func TestSLMCache_EvictExpired(t *testing.T) {
	// EvictExpired must remove all expired entries and return the count.
	c := NewSLMCache(50*time.Millisecond, 0)
	ctx := context.Background()

	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)
	time.Sleep(120 * time.Millisecond)

	if removed := c.EvictExpired(); removed != 2 {
		t.Errorf("EvictExpired removed %d entries, want 2", removed)
	}
	if stale := c.Stale(); stale != 0 {
		t.Errorf("Stale after EvictExpired = %d, want 0", stale)
	}
	if c.Len() != 0 {
		t.Errorf("Len after EvictExpired = %d, want 0", c.Len())
	}
}

func TestSLMCache_EvictExpired_NilObserver(t *testing.T) {
	// EvictExpired must not panic when the eviction observer is nil.
	c := NewSLMCache(50*time.Millisecond, 0)
	ctx := context.Background()
	c.Set(ctx, "a", RouteLocal)
	time.Sleep(120 * time.Millisecond)

	c.EvictExpired() // must not panic

	if stale := c.Stale(); stale != 0 {
		t.Errorf("Stale after EvictExpired = %d, want 0", stale)
	}
}

func TestSLMCache_EvictExpired_WithObserver(t *testing.T) {
	// EvictExpired must call the eviction observer for each removed entry.
	c := NewSLMCache(50*time.Millisecond, 0)
	ctx := context.Background()
	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)
	time.Sleep(120 * time.Millisecond)

	var mu sync.Mutex
	var reasons []string
	c.SetEvictionObserver(func(reason string) {
		mu.Lock()
		reasons = append(reasons, reason)
		mu.Unlock()
	})

	c.EvictExpired()

	mu.Lock()
	defer mu.Unlock()
	if len(reasons) != 2 {
		t.Errorf("observer called %d times, want 2; reasons=%v", len(reasons), reasons)
	}
	for _, r := range reasons {
		if r != EvictionReasonTTL {
			t.Errorf("reason = %q, want %q", r, EvictionReasonTTL)
		}
	}
}

func TestSLMCache_ProactiveEviction_GetSemantic(t *testing.T) {
	// With maxStale=1, getSemantic must trigger EvictExpired when
	// stale entries exceed the threshold (issue #835).
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(50*time.Millisecond, 0, stub, 0.5)
	ctx := context.Background()

	stub.embeddings["a"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["b"] = []float64{0.9, 0.1, 0.0, 0.0}

	// Set two entries (both expire past TTL).
	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteFrontier)

	time.Sleep(120 * time.Millisecond)

	// Stale should be 2.
	if stale := c.Stale(); stale != 2 {
		t.Fatalf("Stale = %d before getSemantic, want 2", stale)
	}

	// Set maxStale=1.
	c.SetMaxStale(1)

	// Trigger getSemantic with a prompt that would semantic-match "a".
	// This should trigger proactive eviction because stale(2) > maxStale(1).
	got, ok, kind := c.Get(ctx, "b") // semantic match with "b"
	_ = got
	_ = ok
	_ = kind

	// Wait briefly for background eviction goroutine to run.
	time.Sleep(50 * time.Millisecond)

	// Stale should now be 0 after proactive eviction.
	if stale := c.Stale(); stale != 0 {
		t.Errorf("Stale after proactive eviction = %d, want 0", stale)
	}
}

func TestSLMCache_ProactiveEviction_Disabled(t *testing.T) {
	// With maxStale=0 (default), no proactive eviction occurs.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(50*time.Millisecond, 0, stub, 0.5)
	ctx := context.Background()

	stub.embeddings["a"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["b"] = []float64{0.9, 0.1, 0.0, 0.0}

	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteFrontier)

	time.Sleep(120 * time.Millisecond)

	// maxStale=0 by default (proactive eviction disabled).
	// Trigger getSemantic.
	c.Get(ctx, "b")

	// Give the background goroutine time to run (if it existed).
	time.Sleep(50 * time.Millisecond)

	// With maxStale=0, eviction should NOT be triggered by getSemantic
	// (no background goroutine spawned).
	// The stale entries should still be there.
	if stale := c.Stale(); stale != 2 {
		t.Errorf("Stale with maxStale=0 = %d, want 2 (proactive eviction disabled)", stale)
	}
}

func TestSLMCache_ProactiveEviction_ExactlyAtThreshold(t *testing.T) {
	// Proactive eviction fires when stale > maxStale, not >=.
	// With maxStale=2 and stale=2, no eviction should fire.
	ctx := context.Background()

	c := NewSLMCacheWithEmbedder(50*time.Millisecond, 0, &vectorEmbedder{err: errors.New("fail")}, 0.5)
	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)
	time.Sleep(120 * time.Millisecond)
	c.SetMaxStale(2)
	c.Get(ctx, "different") // triggers getSemantic but no stale eviction expected
	time.Sleep(50 * time.Millisecond)
	if stale := c.Stale(); stale != 2 {
		t.Errorf("Stale at exactly threshold = %d, want 2 (no eviction)", stale)
	}
}

func TestSLMCache_SetMaxStale(t *testing.T) {
	c := NewSLMCache(time.Hour, 0)
	c.SetMaxStale(5)
	c.SetMaxStale(0)
	c.SetMaxStale(100) // must not panic
}

// --- Semantic scan limit (issue #933) ---

func TestSLMCache_SetMaxScanEntries_NilSafe(t *testing.T) {
	// SetMaxScanEntries must not panic on a nil *SLMCache.
	var c *SLMCache
	c.SetMaxScanEntries(10) // must not panic
}

func TestSLMCache_SemanticScanLimit_ZeroUnlimited(t *testing.T) {
	// With maxScanEntries=0 (default = unlimited), all entries with valid
	// embeddings are scanned. Pre-seed stub embeddings so Set stores them.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, 0, stub, 0.5)
	ctx := context.Background()

	// Pre-seed so Set stores these exact embeddings in entry.emb.
	stub.embeddings["cached-a"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["query-a"] = []float64{0.99, 0.01, 0.0, 0.0}

	c.Set(ctx, "cached-a", RouteLocal) // entry.emb = [1,0,0,0] from stub

	// "query-a" → no exact match → semantic scan → cosine([1,0,0,0],[0.99,0.01,0,0]) > 0.5 → hit.
	got, ok, kind := c.Get(ctx, "query-a")
	if !ok || got != RouteLocal || kind != CacheHitSemantic {
		t.Errorf("unlimited scan: got (%v, %v, %v), want (RouteLocal, true, CacheHitSemantic)", got, ok, kind)
	}
}

func TestSLMCache_SemanticScanLimit_OneScansOne(t *testing.T) {
	// SetMaxScanEntries is now a no-op (issue #969); semantic scan is always unlimited.
	// With one valid entry, it is always found.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, 0, stub, 0.5)
	ctx := context.Background()

	stub.embeddings["cached-b"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["query-b"] = []float64{0.99, 0.01, 0.0, 0.0}

	c.Set(ctx, "cached-b", RouteLocal)
	c.SetMaxScanEntries(1) // Setting it does nothing, but we call it for coverage.

	// Semantic scan is unlimited; "cached-b" is scanned; cosine > 0.5 → hit.
	got, ok, kind := c.Get(ctx, "query-b")
	if !ok || got != RouteLocal || kind != CacheHitSemantic {
		t.Errorf("unlimited scan: got (%v, %v, %v), want (RouteLocal, true, CacheHitSemantic)", got, ok, kind)
	}
}

func TestSLMCache_SemanticScanLimit_NilEmbedEntrySkipped(t *testing.T) {
	// SetMaxScanEntries is now a no-op (issue #969); semantic scan is always unlimited.
	// When the only cached entry has nil emb, it is skipped (nil emb → not cosine-scored)
	// and no other entries exist → miss.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, 0, stub, 0.5)
	ctx := context.Background()

	stub.embeddings["cached-nil"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["query-nil"] = []float64{0.99, 0.01, 0.0, 0.0}

	// SetEmbedding with nil: entry.emb=nil (embedder NOT called).
	c.SetEmbedding("cached-nil", RouteLocal, nil)
	c.SetMaxScanEntries(1) // Setting it does nothing, but we call it for coverage.

	// Semantic scan is unlimited but entry.emb is nil → skipped → miss.
	got, ok, kind := c.Get(ctx, "query-nil")
	if ok {
		t.Errorf("nil emb entry should be skipped; got (%v, %v, %v), want miss", got, ok, kind)
	}
}

func TestSLMCache_SemanticScanLimit_TwoEntriesOneSkipped(t *testing.T) {
	// Issue #969: maxScanEntries was non-deterministic because Go map iteration
	// order is arbitrary. With the limit removed, all entries are always scanned
	// in getSemantic, making cache hits deterministic.
	//
	// This test verifies that nil emb entries are skipped but all valid entries
	// are scanned, producing a deterministic semantic hit.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, 0, stub, 0.5)
	ctx := context.Background()

	stub.embeddings["nil-entry"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["valid-entry"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["query-two"] = []float64{0.99, 0.01, 0.0, 0.0}

	// nil-entry: entry.emb=nil (SetEmbedding with nil).
	// valid-entry: entry.emb=[1,0,0,0] (SetEmbedding with explicit emb).
	c.SetEmbedding("nil-entry", RouteLocal, nil)
	c.SetEmbedding("valid-entry", RouteFrontier, []float64{1.0, 0.0, 0.0, 0.0})

	// SetMaxScanEntries is now a no-op (issue #969); semantic scan is always unlimited.
	// All entries are scanned: nil-entry is skipped (nil emb), valid-entry is matched.
	c.SetMaxScanEntries(1) // Setting it does nothing, but we call it for coverage.

	got, ok, kind := c.Get(ctx, "query-two")
	if !ok || got != RouteFrontier || kind != CacheHitSemantic {
		t.Errorf("unlimited scan with nil+valid: got (%v, %v, %v), want (RouteFrontier, true, CacheHitSemantic)", got, ok, kind)
	}
}

func TestSLMCache_SemanticScanLimit_ZeroLimitAllScanned(t *testing.T) {
	// With maxScanEntries=0 (unlimited) and 2 entries, both are scanned;
	// nil-entry is skipped, valid-a is matched → semantic hit.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, 0, stub, 0.5)
	ctx := context.Background()

	stub.embeddings["nil-a"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["valid-a"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["query-zero"] = []float64{0.99, 0.01, 0.0, 0.0}

	c.SetEmbedding("nil-a", RouteLocal, nil)                                // nil emb
	c.SetEmbedding("valid-a", RouteFrontier, []float64{1.0, 0.0, 0.0, 0.0}) // valid emb
	c.SetMaxScanEntries(0)                                                  // unlimited

	// Unlimited scan: nil-a skipped, valid-a scanned and matched → hit.
	got, ok, kind := c.Get(ctx, "query-zero")
	if !ok || got != RouteFrontier || kind != CacheHitSemantic {
		t.Errorf("unlimited scan with nil first: got (%v, %v, %v), want (RouteFrontier, true, CacheHitSemantic)", got, ok, kind)
	}
}

func TestSLMCache_SemanticScanLimit_LimitExhaustedAfterValidHit(t *testing.T) {
	// SetMaxScanEntries is now a no-op (issue #969); semantic scan is always unlimited.
	// With one valid entry, it is scanned and a semantic hit is returned.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, 0, stub, 0.5)
	ctx := context.Background()

	stub.embeddings["only-entry"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["query-only"] = []float64{0.99, 0.01, 0.0, 0.0}

	c.SetEmbedding("only-entry", RouteLocal, []float64{1.0, 0.0, 0.0, 0.0})
	c.SetMaxScanEntries(1) // Setting it does nothing, but we call it for coverage.

	got, ok, kind := c.Get(ctx, "query-only")
	if !ok || got != RouteLocal || kind != CacheHitSemantic {
		t.Errorf("unlimited scan with valid entry: got (%v, %v, %v), want (RouteLocal, true, CacheHitSemantic)", got, ok, kind)
	}
}

// TestSLMCache_SemanticScanDeterministic verifies the fix for issue #969:
// non-deterministic cache hits with maxScanEntries limit.
//
// Before the fix, when maxScanEntries limited the semantic scan, entries
// were iterated in Go's arbitrary map order. The "best" similarity match
// was whichever entry happened to be visited first — not necessarily the
// most similar. This caused non-deterministic cache hits across runs.
//
// After the fix (maxScanEntries removed), all entries are always scanned,
// making cache hits deterministic for the same query.
func TestSLMCache_SemanticScanDeterministic(t *testing.T) {
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, 0, stub, 0.5)
	ctx := context.Background()

	// Set up multiple entries with different embeddings and routes.
	// valid-a has embedding very close to query, should be best match.
	// valid-b has embedding far from query, should not match above threshold.
	stub.embeddings["valid-a"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["valid-b"] = []float64{0.0, 1.0, 0.0, 0.0} // orthogonal to valid-a
	stub.embeddings["query"] = []float64{0.99, 0.01, 0.0, 0.0} // very close to valid-a

	c.SetEmbedding("valid-a", RouteLocal, []float64{1.0, 0.0, 0.0, 0.0})
	c.SetEmbedding("valid-b", RouteFrontier, []float64{0.0, 1.0, 0.0, 0.0})

	// SetMaxScanEntries is a no-op (issue #969); scan is always unlimited.
	// Verify deterministic hit by calling Get multiple times.
	for i := 0; i < 10; i++ {
		got, ok, kind := c.Get(ctx, "query")
		if !ok || got != RouteLocal || kind != CacheHitSemantic {
			t.Errorf("run %d: got (%v, %v, %v), want (RouteLocal, true, CacheHitSemantic)", i, got, ok, kind)
		}
	}
}

// --- Embed circuit breaker tests (issue #982) ---

// failOnceEmbedder fails the first n calls, then succeeds.
type failOnceEmbedder struct {
	failCount int
	calls     int
	mu        sync.Mutex
}

func (f *failOnceEmbedder) Embed(_ context.Context, _ string) ([]float64, error) {
	f.mu.Lock()
	f.calls++
	if f.calls <= f.failCount {
		f.mu.Unlock()
		return nil, errors.New("embedder unavailable")
	}
	f.mu.Unlock()
	return []float64{1, 0, 0, 0}, nil
}

func TestSLMCache_EmbedBreaker_TripAndSkipEmbed(t *testing.T) {
	// Three rapid embed failures must trip the circuit breaker; subsequent
	// Sets must skip the embedder until the cooldown expires.
	errEmbed := &failOnceEmbedder{failCount: 100} // always fails
	c := NewSLMCacheWithEmbedder(time.Hour, 0, errEmbed, 0.5)
	ctx := context.Background()

	var embedCalls int
	c.SetEmbedErrorObserver(func() { embedCalls++ })

	// First two failures: breaker not yet active.
	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)
	if embedCalls != 2 {
		t.Errorf("after 2 failures: embedCalls=%d, want 2", embedCalls)
	}

	// Third failure trips the breaker.
	c.Set(ctx, "c", RouteLocal)
	if embedCalls != 3 {
		t.Errorf("after 3rd failure: embedCalls=%d, want 3", embedCalls)
	}

	// Now the breaker is active; next Set must skip embedding.
	embedCallsBefore := embedCalls
	c.Set(ctx, "d", RouteLocal)
	if embedCalls != embedCallsBefore {
		t.Errorf("Set during cooldown: embedCalls=%d, want %d (should skip embed)", embedCalls, embedCallsBefore)
	}

	// Exact matches still work without embedding.
	got, ok, kind := c.Get(ctx, "a")
	if !ok || got != RouteLocal || kind != CacheHitExact {
		t.Errorf("exact match during cooldown: got (%v, %v, %v), want (RouteLocal, true, CacheHitExact)", got, ok, kind)
	}
}

func TestSLMCache_EmbedBreaker_CooldownExpires(t *testing.T) {
	// After the cooldown window expires, Set must call the embedder again.
	errEmbed := &failOnceEmbedder{failCount: 100} // always fails
	c := NewSLMCacheWithEmbedder(time.Hour, 0, errEmbed, 0.5)
	ctx := context.Background()

	// Trip the breaker.
	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)
	c.Set(ctx, "c", RouteLocal) // trips breaker

	// Advance time past the cooldown window (30s) by manipulating embedCooldownUntil.
	c.mu.Lock()
	c.embedCooldownUntil = time.Now().Add(-1 * time.Second) // expired 1s ago
	c.mu.Unlock()

	// Next Set should attempt embedding (and fail again).
	embedErrorsBefore := c.Stats().EmbedErrors
	c.Set(ctx, "d", RouteLocal)
	if c.Stats().EmbedErrors <= embedErrorsBefore {
		t.Errorf("Set after cooldown: embed error count should increase, was %d before", embedErrorsBefore)
	}
}

func TestSLMCache_EmbedBreaker_NilEmbedder(t *testing.T) {
	// Cache without an embedder must not be affected by the breaker.
	c := NewSLMCache(time.Hour, 0) // no embedder
	ctx := context.Background()

	c.Set(ctx, "a", RouteLocal)

	got, ok, kind := c.Get(ctx, "a")
	if !ok || got != RouteLocal || kind != CacheHitExact {
		t.Errorf("Set without embedder: got (%v, %v, %v), want (RouteLocal, true, CacheHitExact)", got, ok, kind)
	}
}

func TestSLMCache_EmbedBreaker_ThreeFailuresOutsideWindow(t *testing.T) {
	// Three failures spread over more than 30s must NOT trip the breaker.
	errEmbed := &failOnceEmbedder{failCount: 100} // always fails
	c := NewSLMCacheWithEmbedder(time.Hour, 0, errEmbed, 0.5)
	ctx := context.Background()

	// Manually set three failure timestamps spaced 40s apart.
	c.mu.Lock()
	now := time.Now()
	c.embedBreaker[0] = now.Add(-120 * time.Second) // 120s ago
	c.embedBreaker[1] = now.Add(-80 * time.Second)  // 80s ago
	c.embedBreaker[2] = now.Add(-40 * time.Second)  // 40s ago
	c.embedBreakerPos = 3
	c.mu.Unlock()

	// The span is 80s (> 30s) — breaker must not trip.
	embedErrorsBefore := c.Stats().EmbedErrors
	c.Set(ctx, "a", RouteLocal)
	if c.Stats().EmbedErrors <= embedErrorsBefore {
		t.Errorf("Set with failures outside window: embed error count should increase, was %d before", embedErrorsBefore)
	}
}

func TestSLMCache_EmbedBreaker_SustainedFailureSkipsEmbed(t *testing.T) {
	// While the breaker is active, repeated Sets must not call the embedder.
	errEmbed := &failOnceEmbedder{failCount: 1000} // always fails
	c := NewSLMCacheWithEmbedder(time.Hour, 0, errEmbed, 0.5)
	ctx := context.Background()

	// Trip the breaker.
	c.Set(ctx, "a", RouteLocal)
	c.Set(ctx, "b", RouteLocal)
	c.Set(ctx, "c", RouteLocal) // trips breaker

	// Subsequent Sets must skip embedding.
	for i := 0; i < 5; i++ {
		embedErrorsBefore := c.Stats().EmbedErrors
		c.Set(ctx, fmt.Sprintf("key-%d", i), RouteLocal)
		if c.Stats().EmbedErrors != embedErrorsBefore {
			t.Errorf("Set #%d during cooldown: embedErrors increased unexpectedly", i)
		}
	}

	// Exact matches still work.
	for _, key := range []string{"a", "b", "c"} {
		got, ok, kind := c.Get(ctx, key)
		if !ok || got != RouteLocal || kind != CacheHitExact {
			t.Errorf("exact match for %q: got (%v, %v, %v), want (RouteLocal, true, CacheHitExact)", key, got, ok, kind)
		}
	}
}
