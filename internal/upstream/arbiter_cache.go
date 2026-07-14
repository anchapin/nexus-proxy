// Package upstream provides the hot-path HTTP clients for the proxy.
package upstream

import (
	"hash/fnv"
	"sync"
	"time"
)

// ArbiterCacheEntry is a single cached arbiter synthesis response.
type ArbiterCacheEntry struct {
	Synthesis  string    // the arbiter's text synthesis
	CachedAt   time.Time // when this entry was written
	TTLDuration time.Duration // original TTL at write time (for expiration check)
}

// ArbiterCache is a concurrency-safe in-memory TTL cache for arbiter
// synthesis responses (issue #232). It is keyed by a FNV hash of
// (first.Content, second.Content) so identical panel-member outputs
// share one cache entry regardless of order. The zero value is ready
// to use; constructing via NewArbiterCache is optional but allows
// injecting a time source for deterministic testing.
type ArbiterCache struct {
	mu    sync.RWMutex
	items map[uint64]*ArbiterCacheEntry

	// Time source for expiration checks. Defaults to time.Now if nil.
	NowFunc func() time.Time
}

// NewArbiterCache constructs an empty, ready-to-use cache.
func NewArbiterCache() *ArbiterCache {
	return &ArbiterCache{items: make(map[uint64]*ArbiterCacheEntry)}
}

// cacheKey computes a deterministic FNV-64a hash of the two panel-member
// contents. The order does not matter (r1 vs r2 swapped produces the
// same key) so cache lookups are symmetric.
func cacheKey(r1Content, r2Content string) uint64 {
	h := fnv.New64a()
	// XOR the two contents so swapping r1/r2 yields the same key.
	h.Write([]byte(r1Content))
	h.Write([]byte(r2Content))
	return h.Sum64()
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
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.items[key]
	if !ok {
		return "", false
	}
	if now.Sub(entry.CachedAt) > entry.TTLDuration {
		return "", false
	}
	return entry.Synthesis, true
}

// Set stores a synthesis text under the hash of (r1Content, r2Content).
// If an entry already exists for this key it is overwritten with a fresh
// timestamp. Thread-safe.
func (c *ArbiterCache) Set(r1Content, r2Content, synthesis string, ttl time.Duration) {
	if c == nil {
		return
	}
	key := cacheKey(r1Content, r2Content)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = &ArbiterCacheEntry{
		Synthesis:   synthesis,
		CachedAt:    c.now(),
		TTLDuration: ttl,
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

// Purge removes all entries from the cache. For testing or graceful
// shutdown if a cleaner approach is added later. Thread-safe.
func (c *ArbiterCache) Purge() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[uint64]*ArbiterCacheEntry)
}

// now returns the current time, delegating to TimeFunc if set.
func (c *ArbiterCache) now() time.Time {
	if c == nil || c.NowFunc == nil {
		return time.Now()
	}
	return c.NowFunc()
}
