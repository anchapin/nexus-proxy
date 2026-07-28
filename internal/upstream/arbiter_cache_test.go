package upstream

import (
	"strings"
	"sync"
	"testing"
	"time"
)

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
