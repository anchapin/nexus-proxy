package router

import (
	"sync"
	"time"
)

// SLMCache is a time-bounded prompt → route cache (issue #206). It
// deduplicates identical prompts within a TTL window so repeated
// identical requests (e.g. the same task retried) do not trigger an
// SLM call. This reduces SLM latency and cost for bursty duplicate
// traffic.
//
// The cache is safe for concurrent use. It uses a sync.RWMutex so
// readers (cache lookups) do not block each other; writers (cache
// inserts) take an exclusive lock.
//
// Zero value is ready to use with default TTL (DefaultSLMCacheTTL).
// Construct with NewSLMCache to override TTL.
type SLMCache struct {
	ttl time.Duration

	mu      sync.RWMutex
	entries map[string]cachedDecision
}

// cachedDecision pairs a routing decision with its insertion time for
// TTL enforcement.
type cachedDecision struct {
	Route Route
stamp  time.Time
}

// DefaultSLMCacheTTL is the default cache entry lifetime. 30 seconds
// covers the typical burst window where a coding agent retries the
// same prompt. Operators can override via NewSLMCache(ttl).
const DefaultSLMCacheTTL = 30 * time.Second

// NewSLMCache constructs a cache with the given TTL. Pass zero to use
// DefaultSLMCacheTTL.
func NewSLMCache(ttl time.Duration) *SLMCache {
	if ttl <= 0 {
		ttl = DefaultSLMCacheTTL
	}
	return &SLMCache{
		ttl:     ttl,
		entries: make(map[string]cachedDecision),
	}
}

// Get returns the cached route for prompt and true if a non-expired
// entry exists; otherwise it returns the zero Route and false. On a
// miss the caller should invoke the SLM and then call Set to populate
// the cache.
func (c *SLMCache) Get(prompt string) (Route, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[prompt]
	if !ok {
		return "", false
	}
	if time.Since(entry.stamp) > c.ttl {
		return "", false
	}
	return entry.Route, true
}

// Set records the routing decision for prompt. Subsequent calls to Get
// with the same prompt will return this decision until it expires.
// Set is safe for concurrent use.
func (c *SLMCache) Set(prompt string, route Route) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[prompt] = cachedDecision{
		Route:  route,
		stamp:  time.Now(),
	}
}

// SLMCacheStats holds state counters for the SLM cache. It is
// returned by Stats so callers can inspect cache effectiveness.
type SLMCacheStats struct {
	Entries int // live (non-expired) entries
	Expired int // entries past TTL (not yet evicted)
}

// Stats returns a snapshot of cache hit/miss counters. Counters are
// incremented by Get; there is no separate increment for misses.
func (c *SLMCache) Stats() SLMCacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := len(c.entries)
	// Count expired entries so callers can gauge eviction pressure.
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
