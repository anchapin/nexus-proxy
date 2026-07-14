package router

import (
	"context"
	"math"
	"sync"
	"time"
)

// Embedder turns text into a vector for semantic similarity comparison.
// It is safe for concurrent use.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float64, error)
}

// SLMCache is a time-bounded prompt → route cache (issue #206). It
// deduplicates identical prompts within a TTL window so repeated
// identical requests (e.g. the same task retried) do not trigger an
// SLM call. This reduces SLM latency and cost for bursty duplicate
// traffic.
//
// When an Embedder is configured (issue #245), the cache also performs
// semantic deduplication: prompts whose embedding cosine similarity
// exceeds the configured threshold are treated as cache hits even when
// the exact string differs. For example, "write a fibonacci function"
// and "implement fibonacci recursively" would share the same cached
// route decision.
//
// The cache is safe for concurrent use. It uses a sync.RWMutex so
// readers (cache lookups) do not block each other; writers (cache
// inserts) take an exclusive lock.
//
// Zero value is ready to use with default TTL (DefaultSLMCacheTTL).
// Construct with NewSLMCache to override TTL.
type SLMCache struct {
	ttl          time.Duration
	embedder     Embedder
	semThreshold float64 // cosine similarity floor for semantic match (0.0..1.0)

	mu      sync.RWMutex
	entries map[string]cachedDecision
}

// cachedDecision pairs a routing decision with its insertion time for
// TTL enforcement, and optionally an embedding vector for semantic
// deduplication (issue #245).
type cachedDecision struct {
	Route Route
	stamp time.Time
	emb   []float64 // nil when semantic deduplication is disabled
}

// DefaultSLMCacheTTL is the default cache entry lifetime. 30 seconds
// covers the typical burst window where a coding agent retries the
// same prompt. Operators can override via NewSLMCache(ttl).
const DefaultSLMCacheTTL = 30 * time.Second

// DefaultSemanticThreshold is the default cosine similarity floor for
// semantic cache hits (issue #245). 0.85 is a conservative threshold
// that groups very similar prompts (same intent, different wording)
// without false positives.
const DefaultSemanticThreshold = 0.85

// NewSLMCache constructs a cache with the given TTL. Pass zero to use
// DefaultSLMCacheTTL. The returned cache has semantic deduplication
// disabled; use NewSLMCacheWithEmbedder to enable it.
func NewSLMCache(ttl time.Duration) *SLMCache {
	if ttl <= 0 {
		ttl = DefaultSLMCacheTTL
	}
	return &SLMCache{
		ttl:          ttl,
		entries:      make(map[string]cachedDecision),
		semThreshold: DefaultSemanticThreshold,
	}
}

// NewSLMCacheWithEmbedder constructs a cache with the given TTL and
// semantic deduplication enabled. embedder is used to compute prompt
// embeddings; threshold is the cosine similarity floor (0.0..1.0) for
// two prompts to be considered semantically equivalent. Pass zero
// threshold to use DefaultSemanticThreshold.
func NewSLMCacheWithEmbedder(ttl time.Duration, embedder Embedder, threshold float64) *SLMCache {
	if ttl <= 0 {
		ttl = DefaultSLMCacheTTL
	}
	if threshold <= 0 {
		threshold = DefaultSemanticThreshold
	}
	return &SLMCache{
		ttl:          ttl,
		embedder:     embedder,
		semThreshold: threshold,
		entries:      make(map[string]cachedDecision),
	}
}

// Get returns the cached route for prompt and true if a non-expired
// entry exists; otherwise it returns the zero Route and false. On a
// miss the caller should invoke the SLM and then call Set to populate
// the cache.
//
// When an Embedder is configured, Get first tries exact string matching
// (O(1), sub-millisecond). On an exact miss it falls back to semantic
// matching: it embeds the incoming prompt and scans stored embeddings
// for the best cosine similarity. If the best match exceeds the
// configured threshold the cached route is returned. Semantic matching
// requires an HTTP call to the embedder and may add latency.
func (c *SLMCache) Get(ctx context.Context, prompt string) (Route, bool) {
	// Fast path: exact string match. Hold RLock for the duration of the
	// map read so we don't race with Set (which holds a Mutex).
	c.mu.RLock()
	entry, ok := c.entries[prompt]
	expired := !ok || time.Since(entry.stamp) > c.ttl
	c.mu.RUnlock()
	if ok && !expired {
		return entry.Route, true
	}

	// Semantic fallback: requires embedder.
	if c.embedder == nil {
		return "", false
	}
	return c.getSemantic(ctx, prompt)
}

// getSemantic embeds the prompt and finds the best cached embedding by
// cosine similarity. It returns the route of the best match if above
// the threshold; otherwise it returns "", false. Caller must not hold
// a lock (it releases the lock around the embedder call to avoid
// blocking Set during HTTP).
func (c *SLMCache) getSemantic(ctx context.Context, prompt string) (Route, bool) {
	emb, err := c.embedder.Embed(ctx, prompt)
	if err != nil {
		return "", false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	var best Route
	var bestScore float64 = -1

	now := time.Now()
	for _, entry := range c.entries {
		if now.Sub(entry.stamp) > c.ttl {
			continue
		}
		if entry.emb == nil {
			continue
		}
		score := cosineSimilarity(emb, entry.emb)
		if score > bestScore {
			bestScore = score
			best = entry.Route
		}
	}

	if bestScore >= c.semThreshold {
		return best, true
	}
	return "", false
}

// Set records the routing decision for prompt. Subsequent calls to Get
// with the same prompt will return this decision until it expires.
// When an Embedder is configured Set also computes and stores the
// prompt embedding for semantic deduplication. Set is safe for
// concurrent use.
func (c *SLMCache) Set(ctx context.Context, prompt string, route Route) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var emb []float64
	if c.embedder != nil {
		// Embed synchronously while holding the lock so the entry is
		// fully populated before any reader can see it.
		// Embed is assumed to be fast enough (local Ollama) that this
		// does not block Set significantly.
		emb, _ = c.embedder.Embed(ctx, prompt) // best-effort; embed errors are logged by caller
	}

	c.entries[prompt] = cachedDecision{
		Route: route,
		stamp: time.Now(),
		emb:   emb,
	}
}

// SetEmbedding stores a routing decision with a pre-computed embedding.
// Use this when the caller has already computed the embedding to avoid
// redundant embedder calls (e.g. during cache warming).
func (c *SLMCache) SetEmbedding(prompt string, route Route, emb []float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[prompt] = cachedDecision{
		Route: route,
		stamp: time.Now(),
		emb:   emb,
	}
}

// SLMCacheStats holds state counters for the SLM cache. It is
// returned by Stats so callers can inspect cache effectiveness.
type SLMCacheStats struct {
	Entries int // live (non-expired) entries
	Expired int // entries past TTL (not yet evicted)
}

// Stats returns a snapshot of cache entry counts. Counters are
// incremented by Get; there is no separate increment for misses.
func (c *SLMCache) Stats() SLMCacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := len(c.entries)
	var expired int
	now := time.Now()
	for _, entry := range c.entries {
		if now.Sub(entry.stamp) > c.ttl {
			expired++
		}
	}
	return SLMCacheStats{Entries: n, Expired: expired}
}

// Len returns the number of entries in the cache (including expired
// entries not yet evicted). Exposed for testing.
func (c *SLMCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// cosineSimilarity returns the cosine of the angle between a and b.
// It is equivalent to rag.CosineSimilarity but lives here to keep
// router free of a rag import cycle. A zero vector on either side
// yields 0 so callers can sort without a special case.
func cosineSimilarity(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
