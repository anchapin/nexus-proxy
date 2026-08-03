// Package upstream provides the hot-path HTTP clients for the proxy.
package upstream

import (
	"bytes"
	"crypto/sha256"
	"sync"
	"time"
)

// DefaultArbiterCacheMaxEntries is the default max entries cap. 512 covers
// a typical burst of distinct panel disagreements without excessive memory
// use (issue #773).
const DefaultArbiterCacheMaxEntries = 512

// ArbiterCacheEntry is a single cached arbiter synthesis response.
type ArbiterCacheEntry struct {
	Synthesis   string        // the arbiter's text synthesis
	CachedAt    time.Time     // when this entry was written
	TTLDuration time.Duration // original TTL at write time (for expiration check)
}

// ArbiterCache is a concurrency-safe in-memory TTL cache for arbiter
// synthesis responses (issue #232). It is keyed by a SHA-256 hash of
// (first.Content, second.Content) so identical panel disagreements
// share one cache entry regardless of order. The zero value is ready
// to use; constructing via NewArbiterCache is optional but allows
// injecting a time source for deterministic testing.
//
// When maxEntries > 0, the cache enforces an LRU eviction policy: the
// least-recently-used entry is removed when Set would exceed the cap
// (issue #773).
type ArbiterCache struct {
	mu    sync.RWMutex
	items map[[32]byte]*ArbiterCacheEntry

	// Time source for expiration checks. Defaults to time.Now if nil.
	NowFunc func() time.Time

	// ttl is the configured TTL for cache entries. Enabled() returns
	// true when ttl > 0.
	ttl time.Duration

	// maxEntries caps the number of cached entries. 0 means unlimited.
	// When reached, Set evicts the least-recently-used entry (issue #773).
	maxEntries int

	// lru tracks access order for LRU eviction: older entries are at the
	// front. The slice is rebuilt on each eviction to stay in sync with
	// the items map.
	lru [][32]byte

	// EvictionObserver, when non-nil, is invoked once per evicted entry
	// with reason = "lru". The callback runs after the cache lock is
	// released so it is safe to call into observability or any other
	// subsystem. The pointer is captured under mu; callbacks should not
	// mutate it.
	EvictionObserver func(reason string)
}

// NewArbiterCache constructs an empty, ready-to-use cache with the
// given TTL and max entries. Pass ttl > 0 to enable caching; ttl <= 0
// creates a cache where Enabled() returns false. Pass maxEntries > 0 to
// cap memory at a fixed entry count with LRU eviction; pass 0 to use
// DefaultArbiterCacheMaxEntries (512). The NowFunc is defaulted to
// time.Now.
func NewArbiterCache(ttl time.Duration, maxEntries int) *ArbiterCache {
	if maxEntries <= 0 {
		maxEntries = DefaultArbiterCacheMaxEntries
	}
	return &ArbiterCache{
		items:      make(map[[32]byte]*ArbiterCacheEntry),
		NowFunc:    time.Now,
		ttl:        ttl,
		maxEntries: maxEntries,
		lru:        make([][32]byte, 0, maxEntries),
	}
}

// CacheKey computes the deterministic SHA-256 hash of two panel-member
// contents. Exported so callers (e.g. the metrics recorder) can compute
// the same key the cache uses, enabling pre-warming from historical data
// (issue #1176). See cacheKey for the full algorithm.
func CacheKey(r1Content, r2Content string) [32]byte {
	return cacheKey(r1Content, r2Content)
}

// cacheKey computes a deterministic SHA-256 hash of the two panel-member
// contents. Each content is hashed independently and the two 32-byte
// hashes are concatenated in sorted order and re-hashed to produce a
// collision-resistant key while preserving order-independence:
// cacheKey(a, b) == cacheKey(b, a). This is critical because panel
// members write to a shared channel in non-deterministic goroutine-arrival
// order.
//
// When r1Content == r2Content (perfect agreement), the sentinel prefix
// avoids any key collision by ensuring distinct entries per content even
// when both panels agree perfectly.
func cacheKey(r1Content, r2Content string) [32]byte {
	if r1Content == r2Content {
		h := sha256.Sum256([]byte(r1Content + "|SAME|"))
		return h
	}
	h1 := sha256.Sum256([]byte(r1Content))
	h2 := sha256.Sum256([]byte(r2Content))
	combined := canonicalize(&h1, &h2)
	return sha256.Sum256(combined[:])
}

// canonicalize returns the two 32-byte hashes in a fixed byte order so
// that cacheKey(a, b) == cacheKey(b, a) regardless of arrival order.
func canonicalize(h1, h2 *[32]byte) [64]byte {
	var a, b [32]byte
	if bytes.Compare(h1[:], h2[:]) < 0 {
		a, b = *h1, *h2
	} else {
		a, b = *h2, *h1
	}
	var combined [64]byte
	copy(combined[:32], a[:])
	copy(combined[32:], b[:])
	return combined
}

// touch moves the given key to the end of the LRU list (most recently used).
// Caller must hold c.mu.
func (c *ArbiterCache) touch(key [32]byte) {
	if _, exists := c.items[key]; !exists {
		return
	}
	for i, k := range c.lru {
		if k == key {
			c.lru = append(c.lru[:i], c.lru[i+1:]...)
			c.lru = append(c.lru, key)
			return
		}
	}
	c.lru = append(c.lru, key)
}

// evictLru removes the least-recently-used entry from the cache.
// It skips entries that have already been removed via Delete (stale lru entries).
// Caller must hold c.mu.
func (c *ArbiterCache) evictLru() {
	if len(c.lru) == 0 {
		return
	}
	key := c.lru[0]
	c.lru = c.lru[1:]
	delete(c.items, key)
}

// SetEvictionObserver registers a callback that is invoked once per
// evicted entry with reason = "lru" (issue #798). Pass nil to clear
// the observer. The callback runs after the cache lock is released so
// it is safe to call into observability or logging. Callers that want
// to record into observability.RouteCounters should pass a closure
// that forwards to ObserveArbiterCacheEviction.
func (c *ArbiterCache) SetEvictionObserver(fn func(reason string)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.EvictionObserver = fn
	c.mu.Unlock()
}

// Get returns the cached synthesis text and true if the entry exists
// and has not expired. Returns ("", false) if the entry is missing
// or expired. Thread-safe.
func (c *ArbiterCache) Get(r1Content, r2Content string) (string, bool) {
	if c == nil {
		return "", false
	}
	now := c.now()
	key := cacheKey(r1Content, r2Content)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.items[key]
	if !ok {
		return "", false
	}
	if now.Sub(entry.CachedAt) > entry.TTLDuration {
		return "", false
	}
	c.touch(key)
	return entry.Synthesis, true
}

// Set stores a synthesis text under the hash of (r1Content, r2Content).
// If an entry already exists for this key it is overwritten with a fresh
// timestamp. When the cache is at maxEntries capacity, the least-recently-
// used entry is evicted to make room. Thread-safe.
func (c *ArbiterCache) Set(r1Content, r2Content, synthesis string, ttl time.Duration) {
	if c == nil {
		return
	}
	key := cacheKey(r1Content, r2Content)
	c.mu.Lock()
	evicted := false
	if c.maxEntries > 0 && len(c.items) >= c.maxEntries {
		c.evictLru()
		evicted = true
	}

	c.items[key] = &ArbiterCacheEntry{
		Synthesis:   synthesis,
		CachedAt:    c.now(),
		TTLDuration: ttl,
	}
	c.touch(key)
	onEvict := c.EvictionObserver
	c.mu.Unlock()

	if evicted && onEvict != nil {
		onEvict("lru")
	}
}

// Delete removes a cache entry by key. Used for cache invalidation.
// Thread-safe.
func (c *ArbiterCache) Delete(r1Content, r2Content string) {
	if c == nil {
		return
	}
	key := cacheKey(r1Content, r2Content)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
	for i, k := range c.lru {
		if k == key {
			c.lru = append(c.lru[:i], c.lru[i+1:]...)
			return
		}
	}
}

// Len returns the number of entries in the cache. For testing/monitoring.
func (c *ArbiterCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// Enabled reports whether the cache is active (TTL > 0 at construction).
func (c *ArbiterCache) Enabled() bool {
	if c == nil {
		return false
	}
	return c.ttl > 0
}

// TTLSeconds returns the configured cache TTL in seconds. Returns 0 when
// the cache is disabled.
func (c *ArbiterCache) TTLSeconds() int {
	if c == nil {
		return 0
	}
	return int(c.ttl.Seconds())
}

// Purge removes all entries from the cache. For testing or graceful
// shutdown if a cleaner approach is added later. Thread-safe.
func (c *ArbiterCache) Purge() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[[32]byte]*ArbiterCacheEntry)
	c.lru = c.lru[:0]
}

// now returns the current time, delegating to TimeFunc if set.
func (c *ArbiterCache) now() time.Time {
	if c == nil || c.NowFunc == nil {
		return time.Now()
	}
	return c.NowFunc()
}

// MaxEntries returns the configured max entries capacity. Returns 0 when
// the cache is nil or was constructed with unlimited capacity.
func (c *ArbiterCache) MaxEntries() int {
	if c == nil {
		return 0
	}
	return c.maxEntries
}

// ArbiterCacheWarmEntry is one row for cache pre-warming (issue #1176).
// Key is a pre-computed CacheKey hash so the warmer does not need access
// to the original panel-member contents. WrittenAt is the original cache
// write timestamp; entries already older than the TTL are skipped.
type ArbiterCacheWarmEntry struct {
	Key       [32]byte
	Synthesis string
	WrittenAt time.Time
}

// Warm bulk-loads historical arbiter synthesis entries into the cache
// (issue #1176). Each entry's WrittenAt is checked against the cache
// TTL: entries already expired are skipped and counted in skippedStale.
// Loaded entries are inserted with their original WrittenAt timestamp
// and the cache's configured TTL, so the normal expiry semantics apply
// on subsequent Get calls. LRU eviction is honoured: if the cache is at
// capacity, the least-recently-used entry is evicted per insert.
//
// Returns (loaded, skippedStale). A nil cache is a no-op.
func (c *ArbiterCache) Warm(entries []ArbiterCacheWarmEntry) (loaded, skippedStale int) {
	if c == nil || len(entries) == 0 {
		return 0, 0
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range entries {
		if now.Sub(e.WrittenAt) > c.ttl {
			skippedStale++
			continue
		}
		if c.maxEntries > 0 && len(c.items) >= c.maxEntries {
			c.evictLru()
		}
		c.items[e.Key] = &ArbiterCacheEntry{
			Synthesis:   e.Synthesis,
			CachedAt:    e.WrittenAt,
			TTLDuration: c.ttl,
		}
		c.touch(e.Key)
		loaded++
	}
	return loaded, skippedStale
}
