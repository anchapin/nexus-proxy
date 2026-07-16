// cache.go — LRU-backed embedding cache (issue #115).
//
// Embeddings are deterministic for a given model+text pair, so every
// repeat prompt re-embeds identical bytes through an Ollama HTTP call
// that costs 15–80 ms. CachedEmbedder wraps any Embedder with a
// bounded LRU keyed on the raw prompt text. On a cache hit the
// round-trip is skipped entirely.
//
// The cache stores only the embedding vector — not the retrieval
// result — so newly indexed examples still re-score against cached
// prompt vectors. It is in-memory only and deliberately not persisted
// to disk (that overlaps the SQLite persistence layer from #46).

package rag

import (
	"container/list"
	"context"
	"sync"
)

// CachedEmbedder wraps an Embedder with a bounded LRU cache.
type CachedEmbedder struct {
	inner Embedder

	mu         sync.Mutex
	maxEntries int
	cache      map[string]*list.Element
	ll         *list.List
}

type cacheEntry struct {
	key string
	vec []float64
}

// NewCachedEmbedder returns an Embedder whose results are memoized in
// a least-recently-used cache capped at maxEntries. A size <= 0 is
// clamped to 256 so a misconfiguration never disables the cache
// silently.
func NewCachedEmbedder(inner Embedder, maxEntries int) *CachedEmbedder {
	if maxEntries <= 0 {
		maxEntries = 256
	}
	return &CachedEmbedder{
		inner:      inner,
		maxEntries: maxEntries,
		cache:      make(map[string]*list.Element),
		ll:         list.New(),
	}
}

// Embed returns the cached vector for text if present, otherwise
// delegates to the inner Embedder and stores the result. Concurrent
// calls for different texts proceed in parallel; the lock is held
// only for map/list bookkeeping, never across the inner HTTP call.
func (c *CachedEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	// Fast path: cache hit under a short-lived lock.
	c.mu.Lock()
	if el, ok := c.cache[text]; ok {
		c.ll.MoveToFront(el)
		vec := el.Value.(*cacheEntry).vec
		c.mu.Unlock()
		return vec, nil
	}
	c.mu.Unlock()

	// Slow path: delegate to the real embedder (no lock held).
	vec, err := c.inner.Embed(ctx, text)
	if err != nil {
		return nil, err
	}

	// Insert under the lock. Double-check in case another goroutine
	// already populated this key while we were embedding.
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.cache[text]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*cacheEntry).vec, nil
	}
	entry := &cacheEntry{key: text, vec: vec}
	el := c.ll.PushFront(entry)
	c.cache[text] = el
	if c.ll.Len() > c.maxEntries {
		if oldest := c.ll.Back(); oldest != nil {
			c.ll.Remove(oldest)
			delete(c.cache, oldest.Value.(*cacheEntry).key)
		}
	}
	return vec, nil
}

func (c *CachedEmbedder) IsHealthy(ctx context.Context) bool {
	return c.inner.IsHealthy(ctx)
}
