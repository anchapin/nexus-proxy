package router

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSLMCache_GetSet(t *testing.T) {
	c := NewSLMCache(100 * time.Millisecond)
	ctx := context.Background()
	if got, ok := c.Get(ctx, "hello"); ok || got != "" {
		t.Errorf("empty cache: got (%v, %v), want (\"\", false)", got, ok)
	}

	c.Set(ctx, "hello", RouteLocal)
	got, ok := c.Get(ctx, "hello")
	if !ok || got != RouteLocal {
		t.Errorf("after Set: got (%v, %v), want (RouteLocal, true)", got, ok)
	}

	// Different key is still empty.
	if got, ok := c.Get(ctx, "other"); ok || got != "" {
		t.Errorf("different key: got (%v, %v), want (\"\", false)", got, ok)
	}
}

func TestSLMCache_TTLExpiry(t *testing.T) {
	c := NewSLMCache(50 * time.Millisecond)
	ctx := context.Background()
	c.Set(ctx, "key", RouteFrontier)

	// Should be present immediately.
	if _, ok := c.Get(ctx, "key"); !ok {
		t.Fatal("key missing immediately after Set")
	}

	// Wait for TTL to pass.
	time.Sleep(120 * time.Millisecond)

	// Should be expired now.
	if got, ok := c.Get(ctx, "key"); ok || got != "" {
		t.Errorf("after TTL: got (%v, %v), want (\"\", false)", got, ok)
	}
}

func TestSLMCache_Overwrite(t *testing.T) {
	c := NewSLMCache(time.Hour) // long TTL so expiry doesn't interfere
	ctx := context.Background()
	c.Set(ctx, "key", RouteLocal)
	c.Set(ctx, "key", RouteFrontier)

	got, ok := c.Get(ctx, "key")
	if !ok || got != RouteFrontier {
		t.Errorf("after overwrite: got (%v, %v), want (RouteFrontier, true)", got, ok)
	}
}

func TestSLMCache_Concurrent(t *testing.T) {
	c := NewSLMCache(time.Hour)
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
	c := NewSLMCache(time.Hour)
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
	c := NewSLMCache(0)
	if c == nil {
		t.Fatal("NewSLMCache(0) returned nil")
	}
	ctx := context.Background()
	// Should use default TTL (30s).
	c.Set(ctx, "k", RouteFrontier)
	if got, ok := c.Get(ctx, "k"); !ok || got != RouteFrontier {
		t.Errorf("with default TTL: got (%v, %v), want (RouteFrontier, true)", got, ok)
	}
}

func TestSLMCache_Stats(t *testing.T) {
	c := NewSLMCache(50 * time.Millisecond)
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
	if got, ok := c.Get(ctx, "a"); ok || got != "" {
		t.Errorf("a expired: got (%v, %v), want (\"\", false)", got, ok)
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
	c := NewSLMCacheWithEmbedder(time.Hour, emb, 0.5)
	ctx := context.Background()

	// Set with semantic embedder.
	c.Set(ctx, "write a fibonacci function", RouteLocal)

	// Exact String match — should hit even though semantic could also match.
	got, ok := c.Get(ctx, "write a fibonacci function")
	if !ok || got != RouteLocal {
		t.Errorf("exact match: got (%v, %v), want (RouteLocal, true)", got, ok)
	}
}

func TestSLMCache_SemanticMatch(t *testing.T) {
	// When the embedder returns similar vectors for two different prompts,
	// semantic match should succeed if similarity > threshold.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, stub, 0.5)
	ctx := context.Background()

	// Pre-populate the embeddings so that both prompts return similar vectors.
	// cosineSimilarity([1,0,0,0], [0.9,0.1,0,0]) ≈ 0.995
	stub.embeddings["write a fibonacci function"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["implement fibonacci recursively"] = []float64{0.9, 0.1, 0.0, 0.0}

	// Set the first prompt (this stores its embedding via Embed call).
	c.Set(ctx, "write a fibonacci function", RouteLocal)

	// Now Get with a semantically similar prompt — should hit via semantic match.
	got, ok := c.Get(ctx, "implement fibonacci recursively")
	if !ok || got != RouteLocal {
		t.Errorf("semantic match: got (%v, %v), want (RouteLocal, true)", got, ok)
	}
}

func TestSLMCache_SemanticNoMatch(t *testing.T) {
	// Two prompts whose embeddings are orthogonal should not match.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, stub, 0.5)
	ctx := context.Background()

	// [1,0,0,0] and [0,1,0,0] are orthogonal — cosine = 0
	stub.embeddings["write a fibonacci function"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["explain quantum entanglement"] = []float64{0.0, 1.0, 0.0, 0.0}

	c.Set(ctx, "write a fibonacci function", RouteLocal)

	got, ok := c.Get(ctx, "explain quantum entanglement")
	if ok || got != "" {
		t.Errorf("semantic mismatch: got (%v, %v), want (\"\", false)", got, ok)
	}
}

func TestSLMCache_SemanticBelowThreshold(t *testing.T) {
	// Embeddings that score below the threshold should not match.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, stub, 0.85) // high threshold
	ctx := context.Background()

	// [1,0,0,0] and [0.5,0.0,0.5,0] — cosine = 0.5, below 0.85
	stub.embeddings["write a fibonacci function"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["implement fibonacci recursively"] = []float64{0.5, 0.0, 0.5, 0.0}

	c.Set(ctx, "write a fibonacci function", RouteLocal)

	got, ok := c.Get(ctx, "implement fibonacci recursively")
	if ok || got != "" {
		t.Errorf("below threshold: got (%v, %v), want (\"\", false)", got, ok)
	}
}

func TestSLMCache_SemanticAboveThreshold(t *testing.T) {
	// Embeddings that score above the threshold should match.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(time.Hour, stub, 0.85)
	ctx := context.Background()

	// [1,0,0,0] and [0.8,0.2,0,0] — cosine ≈ 0.98, above 0.85
	stub.embeddings["write a fibonacci function"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["implement fibonacci recursively"] = []float64{0.8, 0.2, 0.0, 0.0}

	c.Set(ctx, "write a fibonacci function", RouteLocal)

	got, ok := c.Get(ctx, "implement fibonacci recursively")
	if !ok || got != RouteLocal {
		t.Errorf("above threshold: got (%v, %v), want (RouteLocal, true)", got, ok)
	}
}

func TestSLMCache_SemanticEmbedError(t *testing.T) {
	// Embedder that always errors should fall back to no semantic match.
	errEmbed := &vectorEmbedder{err: errors.New("embedder unavailable")}
	c := NewSLMCacheWithEmbedder(time.Hour, errEmbed, 0.5)
	ctx := context.Background()

	c.Set(ctx, "write a fibonacci function", RouteLocal)

	got, ok := c.Get(ctx, "write a fibonacci function")
	// Exact match should still work.
	if !ok || got != RouteLocal {
		t.Errorf("exact match failed: got (%v, %v), want (RouteLocal, true)", got, ok)
	}

	// Semantic lookup should fail gracefully (no panic) when embedder errors.
	_, ok = c.Get(ctx, "different prompt")
	if ok {
		t.Errorf("semantic with embed error: expected miss, got hit")
	}
}

func TestSLMCache_SemanticDisabled(t *testing.T) {
	// Cache without embedder should behave like exact-match only.
	c := NewSLMCache(time.Hour) // no embedder
	ctx := context.Background()

	c.Set(ctx, "write a fibonacci function", RouteLocal)

	// Exact match works.
	got, ok := c.Get(ctx, "write a fibonacci function")
	if !ok || got != RouteLocal {
		t.Errorf("exact match: got (%v, %v), want (RouteLocal, true)", got, ok)
	}

	// Semantic should not apply — different String is a miss.
	got, ok = c.Get(ctx, "implement fibonacci recursively")
	if ok || got != "" {
		t.Errorf("semantic not enabled: got (%v, %v), want (\"\", false)", got, ok)
	}
}

func TestSLMCache_SetEmbedding(t *testing.T) {
	// SetEmbedding stores a pre-computed embedding without calling Embed.
	c := NewSLMCacheWithEmbedder(time.Hour, newStubEmbedder(), 0.5)
	ctx := context.Background()

	emb := []float64{1.0, 0.5, 0.0, 0.0}
	c.SetEmbedding("write a fibonacci function", RouteLocal, emb)

	got, ok := c.Get(ctx, "write a fibonacci function")
	if !ok || got != RouteLocal {
		t.Errorf("SetEmbedding exact match: got (%v, %v), want (RouteLocal, true)", got, ok)
	}
}

func TestSLMCache_SemanticTTLExpiry(t *testing.T) {
	// Semantic matches should respect TTL.
	stub := newStubEmbedder()
	c := NewSLMCacheWithEmbedder(50*time.Millisecond, stub, 0.5)
	ctx := context.Background()

	stub.embeddings["write a fibonacci function"] = []float64{1.0, 0.0, 0.0, 0.0}
	stub.embeddings["implement fibonacci recursively"] = []float64{0.9, 0.1, 0.0, 0.0}
	c.Set(ctx, "write a fibonacci function", RouteLocal)

	// Immediate semantic hit.
	got, ok := c.Get(ctx, "implement fibonacci recursively")
	if !ok || got != RouteLocal {
		t.Errorf("immediate semantic hit: got (%v, %v), want (RouteLocal, true)", got, ok)
	}

	// Wait for TTL to pass.
	time.Sleep(120 * time.Millisecond)

	// Both exact and semantic should be expired.
	got, ok = c.Get(ctx, "write a fibonacci function")
	if ok || got != "" {
		t.Errorf("exact expired: got (%v, %v), want (\"\", false)", got, ok)
	}

	got, ok = c.Get(ctx, "implement fibonacci recursively")
	if ok || got != "" {
		t.Errorf("semantic expired: got (%v, %v), want (\"\", false)", got, ok)
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
	c := NewSLMCacheWithEmbedder(0, emb, 0) // zero ttl and threshold

	if c == nil {
		t.Fatal("NewSLMCacheWithEmbedder(0, emb, 0) returned nil")
	}
	if c.ttl != DefaultSLMCacheTTL {
		t.Errorf("ttl = %v, want %v", c.ttl, DefaultSLMCacheTTL)
	}
	if c.semThreshold != DefaultSemanticThreshold {
		t.Errorf("semThreshold = %v, want %v", c.semThreshold, DefaultSemanticThreshold)
	}

	ctx := context.Background()
	c.Set(ctx, "k", RouteFrontier)
	if got, ok := c.Get(ctx, "k"); !ok || got != RouteFrontier {
		t.Errorf("basic operation: got (%v, %v), want (RouteFrontier, true)", got, ok)
	}
}
