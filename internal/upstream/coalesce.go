package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// Package-level atomic counters for Prometheus exposition (issue #1155).
var (
	coalesceHitsTotal   atomic.Uint64
	coalesceMissesTotal atomic.Uint64
)

// IncCoalesceHit increments the coalesce hit counter. Exported for
// test reset via ResetCoalesceCountersForTest.
func IncCoalesceHit() { coalesceHitsTotal.Add(1) }

// IncCoalesceMiss increments the coalesce miss counter.
func IncCoalesceMiss() { coalesceMissesTotal.Add(1) }

// CoalesceHitsTotal returns the cumulative coalesce hit count.
func CoalesceHitsTotal() uint64 { return coalesceHitsTotal.Load() }

// CoalesceMissesTotal returns the cumulative coalesce miss count.
func CoalesceMissesTotal() uint64 { return coalesceMissesTotal.Load() }

// ResetCoalesceCountersForTest zeroes both counters. Test-only.
func ResetCoalesceCountersForTest() {
	coalesceHitsTotal.Store(0)
	coalesceMissesTotal.Store(0)
}

// coalesceEntry holds a cached result with its expiry time.
type coalesceEntry struct {
	result coalesceResult
	expiry time.Time
}

// coalesceResult bundles the return values of fetchCascadeStep so they
// can be shared across concurrent callers via singleflight.
type coalesceResult struct {
	msg      AssistantMessage
	model    string
	respBody []byte
	err      error
}

// Coalescer deduplicates identical concurrent non-streaming cascade
// requests using golang.org/x/sync/singleflight (issue #1155). A short
// TTL cache extends the dedup window past the in-flight period so
// bursty identical requests within the window share a single upstream
// call. An LRU cap bounds memory usage.
type Coalescer struct {
	group singleflight.Group

	mu         sync.Mutex
	cache      map[string]*coalesceEntry
	order      []string // LRU ordering: oldest at index 0
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
}

// NewCoalescer creates a Coalescer with the given TTL window and LRU
// cap. ttl <= 0 disables cross-flight caching (singleflight-only mode).
// maxEntries <= 0 disables the cap.
func NewCoalescer(ttl time.Duration, maxEntries int) *Coalescer {
	return &Coalescer{
		cache:      make(map[string]*coalesceEntry),
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
	}
}

// FlightKey computes the singleflight key as
// sha256(method + model + serializedBody).
func FlightKey(method, model string, serializedBody []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte(model))
	h.Write(serializedBody)
	return hex.EncodeToString(h.Sum(nil))
}

// Do executes fn exactly once per concurrent group of identical keys.
// If a fresh result is in the TTL cache it is returned without calling
// fn. Concurrent callers with the same key share a single fn execution
// via singleflight.Do.
func (c *Coalescer) Do(key string, fn func() (AssistantMessage, string, []byte, error)) (AssistantMessage, string, []byte, error) {
	// 1. Check TTL cache for a fresh entry.
	c.mu.Lock()
	if entry, ok := c.cache[key]; ok && c.now().Before(entry.expiry) {
		c.mu.Unlock()
		coalesceHitsTotal.Add(1)
		return entry.result.msg, entry.result.model, entry.result.respBody, entry.result.err
	}
	c.mu.Unlock()

	// 2. Singleflight dedup. isLeader is set to true only inside the
	// closure, which singleflight executes exactly once per key. All
	// other concurrent callers (waiters) keep isLeader == false.
	isLeader := false
	v, _, _ := c.group.Do(key, func() (any, error) {
		isLeader = true
		msg, model, respBody, err := fn()
		result := coalesceResult{msg: msg, model: model, respBody: respBody, err: err}

		c.mu.Lock()
		c.evictExpiredLocked()
		if c.maxEntries > 0 && len(c.cache) >= c.maxEntries {
			c.evictOldestLocked()
		}
		c.cache[key] = &coalesceEntry{result: result, expiry: c.now().Add(c.ttl)}
		c.order = append(c.order, key)
		c.mu.Unlock()

		return result, nil
	})

	if isLeader {
		coalesceMissesTotal.Add(1)
	} else {
		coalesceHitsTotal.Add(1)
	}

	cr, ok := v.(coalesceResult)
	if !ok {
		return AssistantMessage{}, "", nil, nil
	}
	return cr.msg, cr.model, cr.respBody, cr.err
}

// evictExpiredLocked removes expired entries. Caller must hold c.mu.
func (c *Coalescer) evictExpiredLocked() {
	now := c.now()
	i := 0
	for i < len(c.order) {
		key := c.order[i]
		if entry, ok := c.cache[key]; ok && !now.Before(entry.expiry) {
			delete(c.cache, key)
			c.order = append(c.order[:i], c.order[i+1:]...)
		} else {
			i++
		}
	}
}

// evictOldestLocked removes the oldest entry. Caller must hold c.mu.
func (c *Coalescer) evictOldestLocked() {
	if len(c.order) == 0 {
		return
	}
	oldest := c.order[0]
	delete(c.cache, oldest)
	c.order = c.order[1:]
}

// FlightKeyMethod is the HTTP method used in flight-key computation.
// Exported as a constant so tests can verify the key composition.
const FlightKeyMethod = http.MethodPost
