package router

import (
	"testing"
	"time"
)

func TestSLMCache_GetSet(t *testing.T) {
	c := NewSLMCache(100 * time.Millisecond)
	if got, ok := c.Get("hello"); ok || got != "" {
		t.Errorf("empty cache: got (%v, %v), want (\"\", false)", got, ok)
	}

	c.Set("hello", RouteLocal)
	got, ok := c.Get("hello")
	if !ok || got != RouteLocal {
		t.Errorf("after Set: got (%v, %v), want (RouteLocal, true)", got, ok)
	}

	// Different key is still empty.
	if got, ok := c.Get("other"); ok || got != "" {
		t.Errorf("different key: got (%v, %v), want (\"\", false)", got, ok)
	}
}

func TestSLMCache_TTLExpiry(t *testing.T) {
	c := NewSLMCache(50 * time.Millisecond)
	c.Set("key", RouteFrontier)

	// Should be present immediately.
	if _, ok := c.Get("key"); !ok {
		t.Fatal("key missing immediately after Set")
	}

	// Wait for TTL to pass.
	time.Sleep(120 * time.Millisecond)

	// Should be expired now.
	if got, ok := c.Get("key"); ok || got != "" {
		t.Errorf("after TTL: got (%v, %v), want (\"\", false)", got, ok)
	}
}

func TestSLMCache_Overwrite(t *testing.T) {
	c := NewSLMCache(time.Hour) // long TTL so expiry doesn't interfere
	c.Set("key", RouteLocal)
	c.Set("key", RouteFrontier)

	got, ok := c.Get("key")
	if !ok || got != RouteFrontier {
		t.Errorf("after overwrite: got (%v, %v), want (RouteFrontier, true)", got, ok)
	}
}

func TestSLMCache_Concurrent(t *testing.T) {
	c := NewSLMCache(time.Hour)
	done := make(chan struct{})
	for i := 0; i < 100; i++ {
		go func(i int) {
			key := "key"
			if i%2 == 0 {
				c.Set(key, RouteLocal)
			} else {
				c.Get(key)
			}
			select {
			case <-done:
			default:
			}
		}(i)
	}
	close(done) // signal goroutines to exit
}

func TestSLMCache_Len(t *testing.T) {
	c := NewSLMCache(time.Hour)
	if n := c.Len(); n != 0 {
		t.Errorf("empty cache Len = %d, want 0", n)
	}
	c.Set("a", RouteLocal)
	c.Set("b", RouteLocal)
	if n := c.Len(); n != 2 {
		t.Errorf("after 2 sets: Len = %d, want 2", n)
	}
}

func TestNewSLMCache_ZeroTTL(t *testing.T) {
	c := NewSLMCache(0)
	if c == nil {
		t.Fatal("NewSLMCache(0) returned nil")
	}
	// Should use default TTL (30s).
	c.Set("k", RouteFrontier)
	if got, ok := c.Get("k"); !ok || got != RouteFrontier {
		t.Errorf("with default TTL: got (%v, %v), want (RouteFrontier, true)", got, ok)
	}
}

func TestSLMCache_Stats(t *testing.T) {
	c := NewSLMCache(50 * time.Millisecond)
	c.Set("a", RouteLocal)
	c.Set("b", RouteLocal)

	stats := c.Stats()
	if stats.Entries != 2 {
		t.Errorf("Entries = %d, want 2", stats.Entries)
	}
	if stats.Expired != 0 {
		t.Errorf("Expired = %d, want 0 immediately after set", stats.Expired)
	}

	// Wait for TTL to pass — entries become expired but are not evicted.
	time.Sleep(120 * time.Millisecond)
	stats = c.Stats()
	if got, ok := c.Get("a"); ok || got != "" {
		t.Errorf("a expired: got (%v, %v), want (\"\", false)", got, ok)
	}
	// Entries count still includes expired (not evicted until next write).
	if stats.Expired != 2 {
		t.Errorf("Expired = %d, want 2 after TTL", stats.Expired)
	}
}
