package rag

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingEmbedder tracks how many times Embed is actually called,
// so tests can assert that the cache skips the inner embedder on a
// cache hit.
type countingEmbedder struct {
	mu         sync.Mutex
	calls      map[string]int
	batchCalls int
}

func newCountingEmbedder() *countingEmbedder {
	return &countingEmbedder{calls: make(map[string]int)}
}

func (c *countingEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	c.mu.Lock()
	c.calls[text]++
	c.mu.Unlock()
	// Return a deterministic vector derived from the text length so
	// different prompts get different embeddings.
	return []float64{float64(len(text)), 0, 0}, nil
}

func (c *countingEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.batchCalls++
	for _, text := range texts {
		c.calls[text]++
	}
	result := make([][]float64, len(texts))
	for i, text := range texts {
		result[i] = []float64{float64(len(text)), 0, 0}
	}
	return result, nil
}

func (c *countingEmbedder) IsHealthy(context.Context) bool { return true }
func (c *countingEmbedder) IsBreakerOpen() bool            { return false }
func (c *countingEmbedder) RecordBreakerSuccess()          {}

func (c *countingEmbedder) callCount(text string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[text]
}

func (c *countingEmbedder) totalCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, n := range c.calls {
		total += n
	}
	return total
}

func (c *countingEmbedder) batchCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.batchCalls
}

// --- tests ---

func TestCachedEmbedderSkipsInnerOnHit(t *testing.T) {
	inner := newCountingEmbedder()
	cached := NewCachedEmbedder(inner, 64)

	ctx := context.Background()
	prompt := "hello world"

	// First call: cache miss — inner embedder is invoked.
	vec1, err := cached.Embed(ctx, prompt)
	if err != nil {
		t.Fatalf("first Embed: %v", err)
	}
	if inner.callCount(prompt) != 1 {
		t.Errorf("after first Embed, inner calls = %d, want 1", inner.callCount(prompt))
	}

	// Second call: cache hit — inner embedder must NOT be invoked.
	vec2, err := cached.Embed(ctx, prompt)
	if err != nil {
		t.Fatalf("second Embed: %v", err)
	}
	if inner.callCount(prompt) != 1 {
		t.Errorf("after second Embed (cache hit), inner calls = %d, want 1", inner.callCount(prompt))
	}

	// Cached vector must equal the original.
	if len(vec1) != len(vec2) {
		t.Fatalf("vec length mismatch: %d vs %d", len(vec1), len(vec2))
	}
	for i := range vec1 {
		if vec1[i] != vec2[i] {
			t.Fatalf("vec[%d]: %v != %v", i, vec1[i], vec2[i])
		}
	}
}

// TestCachedEmbedderCacheHitReturnsCopy verifies that on a cache hit,
// CachedEmbedder.Embed returns a defensive copy so that mutating the
// returned vector does not corrupt the cache entry (issue #972).
func TestCachedEmbedderCacheHitReturnsCopy(t *testing.T) {
	inner := newCountingEmbedder()
	cached := NewCachedEmbedder(inner, 64)
	ctx := context.Background()
	prompt := "hello world"

	// First call: cache miss — populate the cache.
	vec1, err := cached.Embed(ctx, prompt)
	if err != nil {
		t.Fatalf("first Embed: %v", err)
	}
	if inner.callCount(prompt) != 1 {
		t.Errorf("after first Embed, inner calls = %d, want 1", inner.callCount(prompt))
	}
	originalFirstElement := vec1[0]

	// Second call: cache hit — returns a vector.
	vec2, err := cached.Embed(ctx, prompt)
	if err != nil {
		t.Fatalf("second Embed (cache hit): %v", err)
	}
	if inner.callCount(prompt) != 1 {
		t.Errorf("after second Embed (cache hit), inner calls = %d, want 1", inner.callCount(prompt))
	}

	// Mutate the returned vector — this must NOT affect the cache entry.
	vec2[0] = 9999

	// Third call: cache hit — the cached vector must be unchanged.
	vec3, err := cached.Embed(ctx, prompt)
	if err != nil {
		t.Fatalf("third Embed (cache hit): %v", err)
	}
	if vec3[0] != originalFirstElement {
		t.Errorf("after mutating vec2, vec3[0] = %v, want original %v — cache entry was corrupted", vec3[0], originalFirstElement)
	}
}

func TestCachedEmbedderDifferentPrompts(t *testing.T) {
	inner := newCountingEmbedder()
	cached := NewCachedEmbedder(inner, 64)

	ctx := context.Background()

	_, _ = cached.Embed(ctx, "prompt A")
	_, _ = cached.Embed(ctx, "prompt B")
	_, _ = cached.Embed(ctx, "prompt A") // cache hit
	_, _ = cached.Embed(ctx, "prompt C")
	_, _ = cached.Embed(ctx, "prompt B") // cache hit

	if inner.totalCalls() != 3 {
		t.Errorf("total inner calls = %d, want 3 (A, B, C — each embedded once)", inner.totalCalls())
	}
}

func TestCachedEmbedderLRUEviction(t *testing.T) {
	inner := newCountingEmbedder()
	cached := NewCachedEmbedder(inner, 3) // tiny cache: 3 entries

	ctx := context.Background()

	// Fill the cache to capacity.
	_, _ = cached.Embed(ctx, "p1")
	_, _ = cached.Embed(ctx, "p2")
	_, _ = cached.Embed(ctx, "p3")
	if inner.totalCalls() != 3 {
		t.Fatalf("after filling cache: inner calls = %d, want 3", inner.totalCalls())
	}

	// Access p1 to make it most-recently-used (p3 is now LRU).
	_, _ = cached.Embed(ctx, "p1") // cache hit
	if inner.totalCalls() != 3 {
		t.Fatalf("after touching p1: inner calls = %d, want 3", inner.totalCalls())
	}

	// Insert p4 — evicts the LRU which should be p2 (p3 was inserted
	// after p2, and p1 was just touched).
	_, _ = cached.Embed(ctx, "p4")
	if inner.totalCalls() != 4 {
		t.Fatalf("after inserting p4: inner calls = %d, want 4", inner.totalCalls())
	}

	// p2 should have been evicted → re-embedding p2 hits the inner.
	_, _ = cached.Embed(ctx, "p2")
	if inner.callCount("p2") != 2 {
		t.Errorf("p2 inner calls after re-embed = %d, want 2 (evicted then re-cached)", inner.callCount("p2"))
	}

	// p1 should still be cached (it was touched before p4 was inserted).
	_, _ = cached.Embed(ctx, "p1")
	if inner.callCount("p1") != 1 {
		t.Errorf("p1 inner calls = %d, want 1 (should still be cached)", inner.callCount("p1"))
	}
}

func TestCachedEmbedderZeroSizeDefaultsTo256(t *testing.T) {
	// A size <= 0 should clamp to 256, not disable the cache.
	cached := NewCachedEmbedder(newCountingEmbedder(), 0)
	if cached.maxEntries != 256 {
		t.Errorf("maxEntries = %d, want 256 (clamped from 0)", cached.maxEntries)
	}
}

func TestCachedEmbedderConcurrent(t *testing.T) {
	// Hammer the cache from multiple goroutines with overlapping
	// prompts to surface races. Run under `go test -race`.
	inner := newCountingEmbedder()
	cached := NewCachedEmbedder(inner, 32)

	ctx := context.Background()
	var wg sync.WaitGroup
	var errors atomic.Int64

	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				prompt := promptForGoroutine(id, i%5)
				_, err := cached.Embed(ctx, prompt)
				if err != nil {
					errors.Add(1)
				}
			}
		}(g)
	}
	wg.Wait()

	if errors.Load() > 0 {
		t.Errorf("got %d errors during concurrent Embed", errors.Load())
	}
}

// promptForGoroutine generates deterministic overlapping prompts so
// multiple goroutines race on the same cache keys.
func promptForGoroutine(id, slot int) string {
	// Only 5 distinct prompts per goroutine but shared across goroutines.
	return "prompt-slot-" + string(rune('A'+slot))
}

func TestCachedEmbedderCacheStatsNoEmbedCache(t *testing.T) {
	// When inner is NOT an *EmbedCache, CacheStats and EmbedHitCount must
	// return zeros and NOT panic (issue #794).
	inner := newCountingEmbedder()
	cached := NewCachedEmbedder(inner, 64)

	hits, misses := cached.CacheStats()
	if hits != 0 || misses != 0 {
		t.Errorf("CacheStats = (%d, %d), want (0, 0)", hits, misses)
	}
	if count := cached.EmbedHitCount(); count != 0 {
		t.Errorf("EmbedHitCount = %d, want 0", count)
	}
}

func TestCachedEmbedderEmbedBatch_AllCached(t *testing.T) {
	inner := newCountingEmbedder()
	cached := NewCachedEmbedder(inner, 64)

	ctx := context.Background()
	texts := []string{"alpha", "beta", "gamma"}

	for _, text := range texts {
		_, _ = cached.Embed(ctx, text)
	}
	if inner.totalCalls() != 3 {
		t.Fatalf("after seeding cache: inner calls = %d, want 3", inner.totalCalls())
	}

	result, err := cached.EmbedBatch(ctx, texts)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if inner.batchCallCount() != 0 {
		t.Errorf("after all-cached EmbedBatch: batch calls = %d, want 0", inner.batchCallCount())
	}
	if len(result) != len(texts) {
		t.Fatalf("result length = %d, want %d", len(result), len(texts))
	}
	for i, text := range texts {
		if result[i] == nil {
			t.Errorf("result[%d] is nil, want non-nil", i)
			continue
		}
		expected := []float64{float64(len(text)), 0, 0}
		if len(result[i]) != len(expected) {
			t.Errorf("result[%d] len = %d, want %d", i, len(result[i]), len(expected))
			continue
		}
		for j, v := range expected {
			if result[i][j] != v {
				t.Errorf("result[%d][%d] = %v, want %v", i, j, result[i][j], v)
			}
		}
	}
}

// TestCachedEmbedderEmbedBatchCacheHitReturnsCopy verifies that on a cache hit
// within EmbedBatch, the returned vectors are defensive copies so that
// mutating them does not corrupt the cache entry (issue #972).
func TestCachedEmbedderEmbedBatchCacheHitReturnsCopy(t *testing.T) {
	inner := newCountingEmbedder()
	cached := NewCachedEmbedder(inner, 64)
	ctx := context.Background()
	texts := []string{"alpha", "beta", "gamma"}

	// Seed the cache first.
	for _, text := range texts {
		_, _ = cached.Embed(ctx, text)
	}
	if inner.totalCalls() != 3 {
		t.Fatalf("after seeding cache: inner calls = %d, want 3", inner.totalCalls())
	}

	// First EmbedBatch call — all hits, returns copies.
	result1, err := cached.EmbedBatch(ctx, texts)
	if err != nil {
		t.Fatalf("first EmbedBatch: %v", err)
	}
	if inner.batchCallCount() != 0 {
		t.Errorf("after first EmbedBatch: batch calls = %d, want 0", inner.batchCallCount())
	}

	// Mutate all returned vectors.
	for i := range result1 {
		result1[i][0] = 9999
	}

	// Second EmbedBatch call — all hits, must return original values.
	result2, err := cached.EmbedBatch(ctx, texts)
	if err != nil {
		t.Fatalf("second EmbedBatch: %v", err)
	}
	for i, text := range texts {
		expected := float64(len(text))
		if result2[i][0] != expected {
			t.Errorf("after mutating result1, result2[%d][0] = %v, want original %v — cache entry was corrupted", i, result2[i][0], expected)
		}
	}
}

func TestCachedEmbedderEmbedBatch_AllUncached(t *testing.T) {
	inner := newCountingEmbedder()
	cached := NewCachedEmbedder(inner, 64)

	ctx := context.Background()
	texts := []string{"alpha", "beta", "gamma"}

	result, err := cached.EmbedBatch(ctx, texts)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if inner.batchCallCount() != 1 {
		t.Errorf("after all-uncached EmbedBatch: batch calls = %d, want 1", inner.batchCallCount())
	}
	if len(result) != len(texts) {
		t.Fatalf("result length = %d, want %d", len(result), len(texts))
	}
	for i, text := range texts {
		expected := []float64{float64(len(text)), 0, 0}
		if len(result[i]) != len(expected) {
			t.Errorf("result[%d] len = %d, want %d", i, len(result[i]), len(expected))
			continue
		}
		for j, v := range expected {
			if result[i][j] != v {
				t.Errorf("result[%d][%d] = %v, want %v", i, j, result[i][j], v)
			}
		}
	}

	_, _ = cached.EmbedBatch(ctx, texts)
	if inner.batchCallCount() != 1 {
		t.Errorf("after second EmbedBatch (all cached): batch calls = %d, want 1", inner.batchCallCount())
	}
}

func TestCachedEmbedderEmbedBatch_PartialHit(t *testing.T) {
	inner := newCountingEmbedder()
	cached := NewCachedEmbedder(inner, 64)

	ctx := context.Background()

	_, _ = cached.Embed(ctx, "alpha")

	texts := []string{"alpha", "beta", "gamma", "alpha"}

	result, err := cached.EmbedBatch(ctx, texts)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if inner.batchCallCount() != 1 {
		t.Errorf("after partial-hit EmbedBatch: batch calls = %d, want 1 (only beta,gamma fetched)", inner.batchCallCount())
	}
	if len(result) != len(texts) {
		t.Fatalf("result length = %d, want %d", len(result), len(texts))
	}

	for i, text := range texts {
		if result[i] == nil {
			t.Errorf("result[%d] is nil, want non-nil", i)
			continue
		}
		expected := []float64{float64(len(text)), 0, 0}
		if len(result[i]) != len(expected) {
			t.Errorf("result[%d] len = %d, want %d", i, len(result[i]), len(expected))
			continue
		}
		for j, v := range expected {
			if result[i][j] != v {
				t.Errorf("result[%d][%d] = %v, want %v", i, j, result[i][j], v)
			}
		}
	}
}

func TestCachedEmbedderCacheStatsWithEmbedCache(t *testing.T) {
	// When inner IS an *EmbedCache, CacheStats and EmbedHitCount must
	// forward to the wrapped cache (issue #794).
	inner := &stubEmbedder{vecs: map[string][]float64{
		"prompt": {1, 0, 0},
	}}
	cache := NewEmbedCache(inner, 100, 5*time.Minute, 5*time.Second)
	cached := NewCachedEmbedder(cache, 64)

	ctx := context.Background()

	// First call: cache miss.
	if _, err := cached.Embed(ctx, "prompt"); err != nil {
		t.Fatalf("first Embed: %v", err)
	}

	hits, misses := cached.CacheStats()
	if hits != 0 {
		t.Errorf("after first Embed, hits = %d, want 0 (miss)", hits)
	}
	if misses != 1 {
		t.Errorf("after first Embed, misses = %d, want 1", misses)
	}

	// Second call: CachedEmbedder LRU hits — the call never reaches
	// EmbedCache, so EmbedCache stats are unchanged (0 hits, 1 miss).
	if _, err := cached.Embed(ctx, "prompt"); err != nil {
		t.Fatalf("second Embed: %v", err)
	}

	hits, misses = cached.CacheStats()
	if hits != 0 {
		t.Errorf("after second Embed (LRU hit), hits = %d, want 0 (EmbedCache was not called)", hits)
	}
	if misses != 1 {
		t.Errorf("after second Embed, misses = %d, want 1", misses)
	}

	// EmbedCache.HitCount() increments only on actual cache hits within
	// EmbedCache.Embed — the second call never reached EmbedCache.
	if count := cached.EmbedHitCount(); count != 0 {
		t.Errorf("EmbedHitCount = %d, want 0 (EmbedCache not called on second request)", count)
	}
}
