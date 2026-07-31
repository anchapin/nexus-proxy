package upstream

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCacheKeyCollisionResistance verifies that distinct (r1, r2) pairs
// produce distinct cache keys, ensuring no collision from the combination
// method. This guards against XOR's non-collision-free property where
// different pairs could theoretically produce the same combined hash.
func TestCacheKeyCollisionResistance(t *testing.T) {
	seen := make(map[[32]byte]string)

	addPair := func(r1, r2 string) {
		k := cacheKey(r1, r2)
		// Canonicalize description for symmetric pairs
		if r1 < r2 {
			seen[k] = r1 + "|" + r2
		} else {
			seen[k] = r2 + "|" + r1
		}
	}

	// Add pairs - all symmetric variants will map to same key (expected)
	addPair("local output A", "frontier output B")
	addPair("alpha", "beta")
	addPair("response one", "response two")
	addPair("aaaa", "aaab")
	addPair(strings.Repeat("x", 100), strings.Repeat("y", 100))
	addPair("", "non-empty")
	addPair("🎱", "🔮")

	// Check that each pair's key is unique
	keys := make([][32]byte, 0, len(seen))
	descs := make([]string, 0, len(seen))
	for k, desc := range seen {
		keys = append(keys, k)
		descs = append(descs, desc)
	}

	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] == keys[j] {
				t.Errorf("collision: %q and %q produced same key %x", descs[i], descs[j], keys[i])
			}
		}
	}

	// Verify symmetry is preserved
	for _, pair := range [][2]string{
		{"local", "frontier"}, {"alpha", "beta"}, {"", "x"}, {"🎉", "text"},
	} {
		r1, r2 := pair[0], pair[1]
		k1 := cacheKey(r1, r2)
		k2 := cacheKey(r2, r1)
		if k1 != k2 {
			t.Errorf("cacheKey not symmetric: (%q, %q) != (%q, %q)", r1, r2, r2, r1)
		}
	}
}

func TestCacheKeySymmetric(t *testing.T) {
	cases := [][2]string{
		{"local output", "frontier output"},
		{"alpha", "beta"},
		{"", ""},
		{"same", "same"},
		{"a", "b"},
		{"", "non-empty"},
		{"order", "ORDER"},
		{"🎉 emoji", "text"},
		{strings.Repeat("x", 1000), strings.Repeat("y", 1000)},
		{"frontier first", "local second"},
	}

	for _, tc := range cases {
		k1 := cacheKey(tc[0], tc[1])
		k2 := cacheKey(tc[1], tc[0])
		if k1 != k2 {
			t.Errorf("cacheKey(%q, %q) = 0x%016x\n\tcacheKey(%q, %q) = 0x%016x\n\twant same key",
				tc[0], tc[1], k1, tc[1], tc[0], k2)
		}
	}
}

// TestCacheKeyIdenticalContentNotZero verifies that cacheKey(a, a) does not
// produce an all-zeros key, which was the zero-key collision bug in issue #966.
// All perfect-agreement responses must have distinct cache keys so they do not
// share an LRU slot.
func TestCacheKeyIdenticalContentNotZero(t *testing.T) {
	var zeroKey [32]byte

	// sha256("x|SAME") is not all zeros
	k := cacheKey("x", "x")
	if k == zeroKey {
		t.Error("cacheKey(\"x\", \"x\") = all-zeros, want non-zero key")
	}

	// Different identical-content pairs must have different keys
	k1 := cacheKey("alpha", "alpha")
	k2 := cacheKey("beta", "beta")
	if k1 == k2 {
		t.Error("cacheKey(\"alpha\", \"alpha\") = cacheKey(\"beta\", \"beta\"), want distinct keys")
	}
	if k1 == zeroKey || k2 == zeroKey {
		t.Error("identical-content key is all-zeros, want non-zero key")
	}

	// Empty string identical content
	k3 := cacheKey("", "")
	if k3 == zeroKey {
		t.Error("cacheKey(\"\", \"\") = all-zeros, want non-zero key")
	}
}

// TestArbiterCacheIdenticalContentDistinctEntries verifies that perfect-agreement
// responses (r1Content == r2Content) are cached as distinct entries and do not
// collide at the zero key slot (issue #966).
func TestArbiterCacheIdenticalContentDistinctEntries(t *testing.T) {
	cache := NewArbiterCache(5*time.Minute, 0)
	ttl := 5 * time.Minute

	// Set two distinct identical-content pairs
	cache.Set("alpha", "alpha", "synthesis alpha", ttl)
	cache.Set("beta", "beta", "synthesis beta", ttl)

	if cache.Len() != 2 {
		t.Errorf("cache.Len() = %d, want 2 distinct entries", cache.Len())
	}

	// Both should be retrievable independently
	got, ok := cache.Get("alpha", "alpha")
	if !ok {
		t.Error("cache.Get(\"alpha\", \"alpha\") = miss, want hit")
	}
	if got != "synthesis alpha" {
		t.Errorf("cache.Get(\"alpha\", \"alpha\") = %q, want %q", got, "synthesis alpha")
	}

	got, ok = cache.Get("beta", "beta")
	if !ok {
		t.Error("cache.Get(\"beta\", \"beta\") = miss, want hit")
	}
	if got != "synthesis beta" {
		t.Errorf("cache.Get(\"beta\", \"beta\") = %q, want %q", got, "synthesis beta")
	}

	// Overwriting one identical-content entry must not affect the other
	cache.Set("alpha", "alpha", "synthesis alpha v2", ttl)

	got, ok = cache.Get("alpha", "alpha")
	if !ok {
		t.Error("cache.Get(\"alpha\", \"alpha\") = miss after overwrite, want hit")
	}
	if got != "synthesis alpha v2" {
		t.Errorf("cache.Get(\"alpha\", \"alpha\") = %q, want %q", got, "synthesis alpha v2")
	}

	// beta should be unaffected
	got, ok = cache.Get("beta", "beta")
	if !ok {
		t.Error("cache.Get(\"beta\", \"beta\") = miss after alpha overwrite, want hit")
	}
	if got != "synthesis beta" {
		t.Errorf("cache.Get(\"beta\", \"beta\") = %q, want %q", got, "synthesis beta")
	}
}

func TestArbiterCacheGetSetOrderIndependent(t *testing.T) {
	cache := NewArbiterCache(5*time.Minute, 0)
	ttl := 5 * time.Minute

	cache.Set("alpha", "beta", "synthesis result", ttl)

	got, ok := cache.Get("beta", "alpha")
	if !ok {
		t.Error("cache.Get(\"beta\", \"alpha\") = (_, false), want (\"synthesis result\", true)")
	}
	if got != "synthesis result" {
		t.Errorf("cache.Get(\"beta\", \"alpha\") = %q, want %q", got, "synthesis result")
	}
}

func TestArbiterCacheGetSetBasic(t *testing.T) {
	cache := NewArbiterCache(5*time.Minute, 0)
	ttl := 5 * time.Minute

	cache.Set("local", "frontier", "arbiter synthesis", ttl)

	got, ok := cache.Get("local", "frontier")
	if !ok {
		t.Error("cache.Get miss, want hit")
	}
	if got != "arbiter synthesis" {
		t.Errorf("cache.Get = %q, want %q", got, "arbiter synthesis")
	}
}

func TestArbiterCacheMiss(t *testing.T) {
	cache := NewArbiterCache(5*time.Minute, 0)

	_, ok := cache.Get("nonexistent", "key")
	if ok {
		t.Error("cache.Get for nonexistent key = (true), want (false)")
	}
}

func TestArbiterCacheExpiry(t *testing.T) {
	now := time.Now().UTC()
	cache := NewArbiterCache(100*time.Millisecond, 0)
	cache.NowFunc = func() time.Time { return now }

	cache.Set("a", "b", "expired synthesis", 50*time.Millisecond)

	_, ok := cache.Get("a", "b")
	if !ok {
		t.Error("cache.Get miss before expiry, want hit")
	}

	cache.NowFunc = func() time.Time { return now.Add(200 * time.Millisecond) }

	_, ok = cache.Get("a", "b")
	if ok {
		t.Error("cache.Get hit after expiry, want miss")
	}
}

func TestArbiterCacheOverwrite(t *testing.T) {
	cache := NewArbiterCache(5*time.Minute, 0)
	ttl := 5 * time.Minute

	cache.Set("a", "b", "first", ttl)
	cache.Set("a", "b", "second", ttl)

	got, ok := cache.Get("a", "b")
	if !ok {
		t.Error("cache.Get miss after overwrite, want hit")
	}
	if got != "second" {
		t.Errorf("cache.Get = %q, want %q (latest overwrite)", got, "second")
	}
}

func TestArbiterCacheNil(t *testing.T) {
	var nilCache *ArbiterCache

	_, ok := nilCache.Get("a", "b")
	if ok {
		t.Error("nilCache.Get returned hit, want miss")
	}

	nilCache.Set("a", "b", "val", time.Minute)

	_, ok = nilCache.Get("a", "b")
	if ok {
		t.Error("nilCache.Get returned hit after Set, want miss")
	}

	nilCache.Delete("a", "b")
}

func TestArbiterCacheEnabled(t *testing.T) {
	cacheDisabled := NewArbiterCache(0, 0)
	if cacheDisabled.Enabled() {
		t.Error("cache with ttl=0 is enabled, want disabled")
	}

	cacheEnabled := NewArbiterCache(time.Minute, 0)
	if !cacheEnabled.Enabled() {
		t.Error("cache with ttl>0 is disabled, want enabled")
	}
}

func TestArbiterCacheDelete(t *testing.T) {
	cache := NewArbiterCache(5*time.Minute, 0)
	ttl := 5 * time.Minute

	cache.Set("a", "b", "value", ttl)

	got, ok := cache.Get("a", "b")
	if !ok || got != "value" {
		t.Fatal("setup failed: cache miss before delete")
	}

	cache.Delete("a", "b")

	_, ok = cache.Get("a", "b")
	if ok {
		t.Error("cache.Get hit after Delete, want miss")
	}
}

func TestArbiterCacheLen(t *testing.T) {
	cache := NewArbiterCache(5*time.Minute, 0)
	ttl := 5 * time.Minute

	if cache.Len() != 0 {
		t.Errorf("cache.Len() = %d, want 0", cache.Len())
	}

	cache.Set("a", "b", "v1", ttl)
	if cache.Len() != 1 {
		t.Errorf("cache.Len() = %d, want 1", cache.Len())
	}

	cache.Set("c", "d", "v2", ttl)
	if cache.Len() != 2 {
		t.Errorf("cache.Len() = %d, want 2", cache.Len())
	}

	cache.Delete("a", "b")
	if cache.Len() != 1 {
		t.Errorf("cache.Len() = %d after delete, want 1", cache.Len())
	}
}

func TestArbiterCachePurge(t *testing.T) {
	cache := NewArbiterCache(5*time.Minute, 0)
	ttl := 5 * time.Minute

	cache.Set("a", "b", "v1", ttl)
	cache.Set("c", "d", "v2", ttl)
	cache.Set("e", "f", "v3", ttl)

	if cache.Len() != 3 {
		t.Fatalf("setup: cache.Len() = %d, want 3", cache.Len())
	}

	cache.Purge()

	if cache.Len() != 0 {
		t.Errorf("cache.Len() after Purge = %d, want 0", cache.Len())
	}

	_, ok := cache.Get("a", "b")
	if ok {
		t.Error("cache.Get hit after Purge, want miss")
	}
}

func TestArbiterCacheTTLSeconds(t *testing.T) {
	cache := NewArbiterCache(300*time.Second, 0)

	if cache.TTLSeconds() != 300 {
		t.Errorf("cache.TTLSeconds() = %d, want 300", cache.TTLSeconds())
	}

	nilCache := (*ArbiterCache)(nil)
	if nilCache.TTLSeconds() != 0 {
		t.Error("nilCache.TTLSeconds() = nonzero, want 0")
	}
}

func TestArbiterCacheMaxEntriesDefault(t *testing.T) {
	cache := NewArbiterCache(time.Hour, 0)
	if cache.maxEntries != DefaultArbiterCacheMaxEntries {
		t.Errorf("maxEntries = %d, want %d", cache.maxEntries, DefaultArbiterCacheMaxEntries)
	}
	if cache.MaxEntries() != DefaultArbiterCacheMaxEntries {
		t.Errorf("MaxEntries() = %d, want %d", cache.MaxEntries(), DefaultArbiterCacheMaxEntries)
	}
}

func TestArbiterCacheMaxEntriesEviction(t *testing.T) {
	cache := NewArbiterCache(time.Hour, 3)
	ttl := time.Hour

	cache.Set("a", "b", "v1", ttl)
	cache.Set("c", "d", "v2", ttl)
	cache.Set("e", "f", "v3", ttl)

	if cache.Len() != 3 {
		t.Fatalf("setup: cache.Len() = %d, want 3", cache.Len())
	}

	_, ok := cache.Get("a", "b")
	if !ok {
		t.Fatal("setup: cache miss for 'a', 'b' before eviction")
	}

	cache.Set("g", "h", "v4", ttl)

	if cache.Len() != 3 {
		t.Errorf("cache.Len() after eviction = %d, want 3", cache.Len())
	}

	_, ok = cache.Get("a", "b")
	if !ok {
		t.Error("cache.Get miss for 'a', 'b' after Set g,h, want hit (a,b was touched via Get, c,d is LRU)")
	}

	_, ok = cache.Get("c", "d")
	if ok {
		t.Error("cache.Get hit for 'c', 'd' after LRU eviction, want miss")
	}

	_, ok = cache.Get("e", "f")
	if !ok {
		t.Error("cache.Get miss for 'e', 'f', want hit")
	}

	_, ok = cache.Get("g", "h")
	if !ok {
		t.Error("cache.Get miss for 'g', 'h' after insertion, want hit")
	}
}

func TestArbiterCacheEvictionEmitsMetric(t *testing.T) {
	cache := NewArbiterCache(time.Hour, 2)
	ttl := time.Hour

	var mu sync.Mutex
	var evictions []string
	cache.SetEvictionObserver(func(reason string) {
		mu.Lock()
		evictions = append(evictions, reason)
		mu.Unlock()
	})

	cache.Set("a", "b", "v1", ttl)
	cache.Set("c", "d", "v2", ttl)
	if len(evictions) != 0 {
		t.Fatalf("evictions after setup: %d, want 0", len(evictions))
	}

	cache.Set("e", "f", "v3", ttl)

	mu.Lock()
	defer mu.Unlock()
	if len(evictions) != 1 {
		t.Fatalf("evictions after third Set: %d, want 1; evictions=%v", len(evictions), evictions)
	}
	if evictions[0] != "lru" {
		t.Errorf("evictions[0] = %q, want %q", evictions[0], "lru")
	}
}

func TestArbiterCacheEvictionObserver_NilSafe(t *testing.T) {
	cache := NewArbiterCache(time.Hour, 1)
	ttl := time.Hour

	cache.Set("a", "b", "v1", ttl)
	cache.Set("c", "d", "v2", ttl)

	if cache.Len() != 1 {
		t.Errorf("cache.Len() = %d, want 1", cache.Len())
	}
}

func TestArbiterCacheDeleteRemovesFromLRU(t *testing.T) {
	cache := NewArbiterCache(time.Hour, 3)
	ttl := time.Hour

	cache.Set("a", "b", "v1", ttl)
	cache.Set("c", "d", "v2", ttl)
	cache.Set("e", "f", "v3", ttl)

	cache.Delete("c", "d")

	// Delete removes from items, so after Set g,h (len=2 < maxEntries=3, no eviction)
	// items = {e,f,g,h} with lru = [e,f,g,h]
	cache.Set("g", "h", "v4", ttl)

	_, ok := cache.Get("a", "b")
	if !ok {
		t.Error("cache.Get miss for 'a', 'b', want hit")
	}

	// c,d was deleted, should be miss
	_, ok = cache.Get("c", "d")
	if ok {
		t.Error("cache.Get hit for 'c', 'd' after Delete, want miss")
	}

	// e,f was never evicted
	_, ok = cache.Get("e", "f")
	if !ok {
		t.Error("cache.Get miss for 'e', 'f', want hit")
	}

	// g,h was just added
	_, ok = cache.Get("g", "h")
	if !ok {
		t.Error("cache.Get miss for 'g', 'h', want hit")
	}
}

func BenchmarkCacheKeySameContent(b *testing.B) {
	content := "identical panel output that could appear in both local and frontier responses during fusion"
	for i := 0; i < b.N; i++ {
		cacheKey(content, content)
	}
}

func BenchmarkCacheKeyDifferentContent(b *testing.B) {
	content1 := "local model output with specific reasoning trace"
	content2 := "frontier model output with different reasoning approach"
	for i := 0; i < b.N; i++ {
		cacheKey(content1, content2)
	}
}
