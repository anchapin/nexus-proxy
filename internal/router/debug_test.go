package router

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestDebugCache(t *testing.T) {
	callCount := 0
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		callCount++
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client).WithCache(2, 5*time.Minute)

	// Fill cache with two entries
	t.Logf("Before A: callCount=%d", callCount)
	if _, err := c.Decide(context.Background(), "prompt A"); err != nil {
		t.Fatalf("A: %v", err)
	}
	t.Logf("After A: callCount=%d", callCount)
	if _, err := c.Decide(context.Background(), "prompt B"); err != nil {
		t.Fatalf("B: %v", err)
	}
	t.Logf("After B: callCount=%d", callCount)

	// Access A again to make it MRU (B is now LRU)
	if _, err := c.Decide(context.Background(), "prompt A"); err != nil {
		t.Fatalf("A again: %v", err)
	}
	t.Logf("After A again: callCount=%d", callCount)

	// Add C, should evict B (LRU)
	if _, err := c.Decide(context.Background(), "prompt C"); err != nil {
		t.Fatalf("C: %v", err)
	}
	t.Logf("After C: callCount=%d", callCount)

	// B should now be a miss
	if _, err := c.Decide(context.Background(), "prompt B"); err != nil {
		t.Fatalf("B again: %v", err)
	}
	t.Logf("After B again: callCount=%d", callCount)

	// A should still be a hit
	if _, err := c.Decide(context.Background(), "prompt A"); err != nil {
		t.Fatalf("A again: %v", err)
	}
	t.Logf("After A again: callCount=%d", callCount)
}
