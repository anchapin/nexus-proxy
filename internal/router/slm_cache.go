package router

import (
	"container/heap"
	"context"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Eviction reason labels for the bounded Prometheus
// `nexus_slm_cache_evictions_total{reason=...}` series (issue #449).
// The set is intentionally closed: any new reason must be added here
// so the cardinality stays bounded and the observability surface can
// be documented exhaustively.
const (
	EvictionReasonTTL = "ttl"
	EvictionReasonLRU = "lru"
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
// Eviction counters (issue #449): TTL and LRU removals each bump an
// atomic counter that is exposed via Stats() and forwarded to a
// caller-supplied eviction observer so the proxy can distinguish
// "TTL too short" (high churn) from "cache undersized" (capacity
// pressure) in /metrics as `nexus_slm_cache_evictions_total{reason}`.
//
// Zero value is ready to use with default TTL (DefaultSLMCacheTTL).
// Construct with NewSLMCache to override TTL.
type SLMCache struct {
	ttl          time.Duration
	maxEntries   int
	embedder     Embedder
	semThreshold float64 // cosine similarity floor for semantic match (0.0..1.0)

	mu      sync.RWMutex
	entries map[string]cachedDecision
	expiry  []string // keys sorted by expiry time (earliest first)

	// ttlEvictions and lruEvictions are cumulative atomic counters
	// bumped inside the synchronized eviction paths so they are safe
	// to read concurrently with Set (issue #449). They are not
	// guarded by mu because the hot path already holds it; atomic
	// updates keep Stats() lock-free.
	ttlEvictions uint64
	lruEvictions uint64

	// onEviction, when non-nil, is invoked once per evicted entry
	// with reason = "ttl" or "lru". The callback runs AFTER the
	// cache mutex is released so it is safe to call into observability
	// or any other subsystem without risking re-entrant deadlocks.
	// The slice pointer is captured under mu; callbacks should not
	// mutate it.
	onEviction func(reason string)
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

// DefaultSLMCacheMaxEntries is the default max entries cap. 512 covers
// a typical burst of distinct prompts without excessive memory use.
const DefaultSLMCacheMaxEntries = 512

// DefaultSemanticThreshold is the default cosine similarity floor for
// semantic cache hits (issue #245). 0.85 is a conservative threshold
// that groups very similar prompts (same intent, different wording)
// without false positives.
const DefaultSemanticThreshold = 0.85

// NewSLMCache constructs a cache with the given TTL and max entries.
// Pass zero TTL to use DefaultSLMCacheTTL; zero maxEntries to use
// DefaultSLMCacheMaxEntries. The returned cache has semantic
// deduplication disabled; use NewSLMCacheWithEmbedder to enable it.
func NewSLMCache(ttl time.Duration, maxEntries int) *SLMCache {
	if ttl <= 0 {
		ttl = DefaultSLMCacheTTL
	}
	if maxEntries <= 0 {
		maxEntries = DefaultSLMCacheMaxEntries
	}
	return &SLMCache{
		ttl:          ttl,
		maxEntries:   maxEntries,
		entries:      make(map[string]cachedDecision),
		semThreshold: DefaultSemanticThreshold,
	}
}

// NewSLMCacheWithEmbedder constructs a cache with the given TTL, max
// entries, and semantic deduplication enabled. embedder is used to
// compute prompt embeddings; threshold is the cosine similarity floor
// (0.0..1.0) for two prompts to be considered semantically equivalent.
// Pass zero threshold to use DefaultSemanticThreshold.
func NewSLMCacheWithEmbedder(ttl time.Duration, maxEntries int, embedder Embedder, threshold float64) *SLMCache {
	if ttl <= 0 {
		ttl = DefaultSLMCacheTTL
	}
	if maxEntries <= 0 {
		maxEntries = DefaultSLMCacheMaxEntries
	}
	if threshold <= 0 {
		threshold = DefaultSemanticThreshold
	}
	return &SLMCache{
		ttl:          ttl,
		maxEntries:   maxEntries,
		embedder:     embedder,
		semThreshold: threshold,
		entries:      make(map[string]cachedDecision),
	}
}

// sortExpiry sorts the expiry slice by stamp (earliest first).
// This maintains the invariant that expiry[0] is the entry to evict next.
func (c *SLMCache) sortExpiry() {
	sort.Slice(c.expiry, func(i, j int) bool {
		iEntry, oki := c.entries[c.expiry[i]]
		jEntry, okj := c.entries[c.expiry[j]]
		if !oki {
			return true
		}
		if !okj {
			return false
		}
		return iEntry.stamp.Before(jEntry.stamp)
	})
}

// evictExpired removes all entries whose TTL has expired and bumps the
// TTL eviction counter once per removed entry (issue #449). It returns
// the number of entries removed so the caller can dispatch the
// eviction observer after unlocking. Caller must hold c.mu.
func (c *SLMCache) evictExpired() int {
	now := time.Now()
	var keep []string
	removed := 0
	for _, key := range c.expiry {
		entry, ok := c.entries[key]
		if !ok {
			continue // already removed
		}
		if now.Sub(entry.stamp) > c.ttl {
			delete(c.entries, key)
			removed++
		} else {
			keep = append(keep, key)
		}
	}
	c.expiry = keep
	c.sortExpiry()
	if removed > 0 {
		atomic.AddUint64(&c.ttlEvictions, uint64(removed))
	}
	return removed
}

// evictLru removes the least-recently-used (oldest by stamp) non-expired
// entry to make room for a new insertion and bumps the LRU eviction
// counter (issue #449). It returns 1 when an entry was removed, 0
// otherwise, so the caller can dispatch the eviction observer after
// unlocking. Caller must hold c.mu.
func (c *SLMCache) evictLru() int {
	if len(c.expiry) == 0 {
		return 0
	}
	// expiry is sorted by stamp, so the first element is the oldest.
	lruKey := c.expiry[0]
	delete(c.entries, lruKey)
	c.expiry = c.expiry[1:]
	atomic.AddUint64(&c.lruEvictions, 1)
	return 1
}

// CacheHitKind describes the mechanism that produced a cache hit.
// It is returned as the third value from Get.
type CacheHitKind string

const (
	// CacheHitExact means the prompt was found by exact string match.
	CacheHitExact CacheHitKind = "exact"
	// CacheHitSemantic means the prompt was found by cosine similarity
	// against stored embeddings (issue #245).
	CacheHitSemantic CacheHitKind = "semantic"
)

// Get returns the cached route for prompt and true if a non-expired
// entry exists; otherwise it returns the zero Route and false. On a
// miss the caller should invoke the SLM and then call Set to populate
// the cache.
//
// The returned string indicates how the hit was produced:
//   - CacheHitExact ("exact") for an exact string match (O(1))
//   - CacheHitSemantic ("semantic") for a cosine-similarity match
//   - "" (empty) when there is no hit
//
// When an Embedder is configured, Get first tries exact string matching
// (O(1), sub-millisecond). On an exact miss it falls back to semantic
// matching: it embeds the incoming prompt and scans stored embeddings
// for the best cosine similarity. If the best match exceeds the
// configured threshold the cached route is returned. Semantic matching
// requires an HTTP call to the embedder and may add latency.
func (c *SLMCache) Get(ctx context.Context, prompt string) (Route, bool, CacheHitKind) {
	// Fast path: exact string match. Hold RLock for the duration of the
	// map read so we don't race with Set (which holds a Mutex).
	c.mu.RLock()
	entry, ok := c.entries[prompt]
	expired := !ok || time.Since(entry.stamp) > c.ttl
	c.mu.RUnlock()
	if ok && !expired {
		return entry.Route, true, CacheHitExact
	}

	// Semantic fallback: requires embedder.
	if c.embedder == nil {
		return "", false, ""
	}
	return c.getSemantic(ctx, prompt)
}

// getSemantic embeds the prompt and finds the best cached embedding by
// cosine similarity. It returns the route of the best match if above
// the threshold, along with CacheHitSemantic; otherwise it returns
// "", false, "". Caller must not hold a lock (it releases the lock
// around the embedder call to avoid blocking Set during HTTP).
func (c *SLMCache) getSemantic(ctx context.Context, prompt string) (Route, bool, CacheHitKind) {
	emb, err := c.embedder.Embed(ctx, prompt)
	if err != nil {
		return "", false, ""
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
		return best, true, CacheHitSemantic
	}
	return "", false, ""
}

// Set records the routing decision for prompt. Subsequent calls to Get
// with the same prompt will return this decision until it expires.
// When an Embedder is configured Set also computes and stores the
// prompt embedding for semantic deduplication. Set is safe for
// concurrent use.
func (c *SLMCache) Set(ctx context.Context, prompt string, route Route) {
	var ttlRemoved, lruRemoved int
	var onEvict func(reason string)

	c.mu.Lock()

	var emb []float64
	if c.embedder != nil {
		emb, _ = c.embedder.Embed(ctx, prompt) // best-effort; embed errors are logged by caller
	}

	// If at capacity, evict expired entries first, then LRU.
	if c.maxEntries > 0 && len(c.entries) >= c.maxEntries {
		ttlRemoved = c.evictExpired()
		if len(c.entries) >= c.maxEntries {
			lruRemoved = c.evictLru()
		}
	}

	c.entries[prompt] = cachedDecision{
		Route: route,
		stamp: time.Now(),
		emb:   emb,
	}
	c.expiry = append(c.expiry, prompt)
	c.sortExpiry()

	onEvict = c.onEviction
	c.mu.Unlock()

	// Dispatch the eviction observer AFTER releasing the cache lock
	// so the observer can safely call into observability, logging,
	// or any other subsystem (issue #449).
	c.dispatchEvictions(onEvict, ttlRemoved, lruRemoved)
}

// SetEmbedding stores a routing decision with a pre-computed embedding.
// Use this when the caller has already computed the embedding to avoid
// redundant embedder calls (e.g. during cache warming).
func (c *SLMCache) SetEmbedding(prompt string, route Route, emb []float64) {
	var ttlRemoved, lruRemoved int
	var onEvict func(reason string)

	c.mu.Lock()

	if c.maxEntries > 0 && len(c.entries) >= c.maxEntries {
		ttlRemoved = c.evictExpired()
		if len(c.entries) >= c.maxEntries {
			lruRemoved = c.evictLru()
		}
	}

	c.entries[prompt] = cachedDecision{
		Route: route,
		stamp: time.Now(),
		emb:   emb,
	}
	c.expiry = append(c.expiry, prompt)
	c.sortExpiry()

	onEvict = c.onEviction
	c.mu.Unlock()

	c.dispatchEvictions(onEvict, ttlRemoved, lruRemoved)
}

// dispatchEvictions fires the eviction observer once per removed
// entry. It runs without holding the cache lock so the observer is
// free to call into other subsystems. A nil observer is a no-op so
// the common path stays cheap.
func (c *SLMCache) dispatchEvictions(onEvict func(reason string), ttlRemoved, lruRemoved int) {
	if onEvict == nil {
		return
	}
	for i := 0; i < ttlRemoved; i++ {
		onEvict(EvictionReasonTTL)
	}
	for i := 0; i < lruRemoved; i++ {
		onEvict(EvictionReasonLRU)
	}
}

// SetEvictionObserver registers a callback that is invoked once per
// evicted entry with reason = "ttl" or "lru" (issue #449). Pass nil
// to clear the observer. The callback runs on the goroutine that
// called Set/SetEmbedding, after the cache lock is released, so it
// must not be assumed to be on a dedicated worker. Callers that want
// to record into observability.RouteCounters should pass a closure
// that forwards to ObserveSLMCacheEviction; the closure will execute
// without re-entering the cache.
func (c *SLMCache) SetEvictionObserver(fn func(reason string)) {
	c.mu.Lock()
	c.onEviction = fn
	c.mu.Unlock()
}

// SLMCacheStats holds state counters for the SLM cache. It is
// returned by Stats so callers can inspect cache effectiveness.
// TTLEvictions and LRUEvictions are cumulative since cache creation
// (issue #449) and reflect removals that actually happened; entries
// past TTL but not yet evicted are reflected by the Expired counter.
type SLMCacheStats struct {
	Entries      int    // live (non-expired) entries
	Expired      int    // entries past TTL (not yet evicted)
	TTLEvictions uint64 // cumulative TTL removals
	LRUEvictions uint64 // cumulative LRU removals (capacity pressure)
}

// Stats returns a snapshot of cache entry counts and cumulative
// eviction counters (issue #449). Counters are incremented by Get;
// there is no separate increment for misses. The eviction counters
// are read atomically without holding the cache lock so concurrent
// Sets do not block Stats reads.
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
	return SLMCacheStats{
		Entries:      n,
		Expired:      expired,
		TTLEvictions: atomic.LoadUint64(&c.ttlEvictions),
		LRUEvictions: atomic.LoadUint64(&c.lruEvictions),
	}
}

// Len returns the number of entries in the cache (including expired
// entries not yet evicted). Exposed for testing.
func (c *SLMCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Enabled reports whether the cache is active (TTL > 0).
func (c *SLMCache) Enabled() bool {
	if c == nil {
		return false
	}
	return c.ttl > 0
}

// TTLSeconds returns the configured cache TTL in seconds. Returns 0 when
// the cache is disabled.
func (c *SLMCache) TTLSeconds() int {
	if c == nil {
		return 0
	}
	return int(c.ttl.Seconds())
}

// MaxEntries returns the configured max entries cap.
func (c *SLMCache) MaxEntries() int {
	if c == nil {
		return 0
	}
	return c.maxEntries
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

// expiryHeap is a placeholder to satisfy heap.Interface for future use.
// Currently we use sort.Slice for simplicity; this allows us to swap
// to a real heap later without API changes.
type expiryHeap struct{}

func (expiryHeap) Len() int           { return 0 }
func (expiryHeap) Less(i, j int) bool { return false }
func (expiryHeap) Swap(i, j int)      {}
func (h *expiryHeap) Push(x any)      {}
func (h *expiryHeap) Pop() any        { return "" }

var _ heap.Interface = (*expiryHeap)(nil)
