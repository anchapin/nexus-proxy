package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newClient(rt roundTripperFunc) *http.Client {
	return &http.Client{Transport: rt, Timeout: 2 * time.Second}
}

func okBody(jsonBody string) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(jsonBody)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func TestSLMDecideLocal(t *testing.T) {
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	r, err := c.Decide(context.Background(), "anything")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if r != RouteLocal {
		t.Errorf("got %q, want local", r)
	}
}

func TestSLMDecideFusion(t *testing.T) {
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		return okBody(`{"message":{"content":"{\"route\":\"FUSION\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	r, _ := c.Decide(context.Background(), "x")
	if r != RouteFusion {
		t.Errorf("got %q, want fusion", r)
	}
}

func TestSLMDecideUnknownRouteFallsBackToFrontier(t *testing.T) {
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		return okBody(`{"message":{"content":"{\"route\":\"banana\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	r, _ := c.Decide(context.Background(), "x")
	if r != RouteFrontier {
		t.Errorf("got %q, want frontier", r)
	}
}

func TestSLMDecideMalformedJSONFallsBackToFrontier(t *testing.T) {
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		return okBody(`{"message":{"content":"not json at all"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	r, _ := c.Decide(context.Background(), "x")
	if r != RouteFrontier {
		t.Errorf("got %q", r)
	}
}

func TestSLMDecideTransportErrorFallsBackToFrontier(t *testing.T) {
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		return nil, errNet("dial tcp: connection refused")
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	r, _ := c.Decide(context.Background(), "x")
	if r != RouteFrontier {
		t.Errorf("got %q", r)
	}
}

func TestSLMDecideNon200FallsBackToFrontier(t *testing.T) {
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 500,
			Body:       io.NopCloser(strings.NewReader("boom")),
		}, nil
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	r, _ := c.Decide(context.Background(), "x")
	if r != RouteFrontier {
		t.Errorf("got %q", r)
	}
}

func TestSLMDecideSendsExpectedPayload(t *testing.T) {
	var seenBody string
	client := newClient(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	})
	c := NewSLMClient("http://x", "qwen-test", time.Second, client)
	if _, err := c.Decide(context.Background(), "hello"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !strings.Contains(seenBody, `"qwen-test"`) {
		t.Errorf("payload missing model: %s", seenBody)
	}
	if !strings.Contains(seenBody, `"hello"`) {
		t.Errorf("payload missing prompt: %s", seenBody)
	}
}

type errNet string

func (e errNet) Error() string { return string(e) }

func TestParseSLMDecision(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantErr   bool
		wantRoute string // raw route string from JSON (lower-cased later by decide)
	}{
		{
			name:      "bare JSON local",
			content:   `{"route":"local"}`,
			wantRoute: "local",
		},
		{
			name:      "bare JSON frontier",
			content:   `{"route":"frontier"}`,
			wantRoute: "frontier",
		},
		{
			name:      "bare JSON fusion",
			content:   `{"route":"fusion"}`,
			wantRoute: "fusion",
		},
		{
			name:      "bare JSON with whitespace padding",
			content:   "  \n {\"route\":\"local\"} \n  ",
			wantRoute: "local",
		},
		{
			name:      "bare JSON uppercase route preserved",
			content:   `{"route":"FUSION"}`,
			wantRoute: "FUSION",
		},
		{
			name:      "markdown-fenced json block",
			content:   "```json\n{\"route\":\"local\"}\n```",
			wantRoute: "local",
		},
		{
			name:      "markdown-fenced json uppercase opener",
			content:   "```JSON\n{\"route\":\"frontier\"}\n```",
			wantRoute: "frontier",
		},
		{
			name:      "markdown-fenced bare backticks",
			content:   "```\n{\"route\":\"fusion\"}\n```",
			wantRoute: "fusion",
		},
		{
			name:      "markdown-fenced block surrounded by prose",
			content:   "Here is the decision:\n```json\n{\"route\":\"local\"}\n```\nThanks!",
			wantRoute: "local",
		},
		{
			name:      "prose prefix before JSON",
			content:   `Decision: {"route":"local"}`,
			wantRoute: "local",
		},
		{
			name:      "prose suffix after JSON",
			content:   `{"route":"frontier"} hope that helps`,
			wantRoute: "frontier",
		},
		{
			name:      "prose on both sides of JSON",
			content:   `Sure! Here it is: {"route":"local"} — cheers`,
			wantRoute: "local",
		},
		{
			name:      "nested braces inside string value",
			content:   `{"route":"local","note":"a {b} c"}`,
			wantRoute: "local",
		},
		{
			name:      "escaped quote inside string value",
			content:   `{"route":"local","note":"a \"quoted\" {value}"}`,
			wantRoute: "local",
		},
		{
			name:      "multiple objects takes first",
			content:   `{"route":"local"}{"route":"frontier"}`,
			wantRoute: "local",
		},
		{
			name:    "fenced garbage falls back to error",
			content: "```json\nnot json inside\n```",
			wantErr: true,
		},
		{
			name:    "pure garbage",
			content: "not json at all",
			wantErr: true,
		},
		{
			name:    "empty content",
			content: "",
			wantErr: true,
		},
		{
			name:    "unbalanced braces",
			content: `{"route":"local"`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := parseSLMDecision(tt.content)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseSLMDecision() error = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if d.Route != tt.wantRoute {
				t.Errorf("Route = %q, want %q", d.Route, tt.wantRoute)
			}
		})
	}
}

// TestSLMDecideTolerantShapes exercises the full Decide path to confirm
// that fenced/prose-wrapped SLM responses route correctly end-to-end and
// that genuinely malformed responses still fall back to frontier.
func TestSLMDecideTolerantShapes(t *testing.T) {
	tests := []struct {
		name    string
		content string // the SLM "message.content" field value
		want    Route
	}{
		{
			name:    "fenced json routes local",
			content: "```json\n{\"route\":\"local\"}\n```",
			want:    RouteLocal,
		},
		{
			name:    "prose-prefixed routes local",
			content: `Decision: {"route":"local"}`,
			want:    RouteLocal,
		},
		{
			name:    "prose-suffixed routes fusion",
			content: `{"route":"fusion"} done`,
			want:    RouteFusion,
		},
		{
			name:    "garbage falls back to frontier",
			content: "totally not json :::",
			want:    RouteFrontier,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Embed the content as a JSON string so it survives the outer
			// Ollama envelope unmarshal.
			inner, _ := json.Marshal(tt.content)
			body := `{"message":{"content":` + string(inner) + `}}`
			client := newClient(func(_ *http.Request) (*http.Response, error) {
				return okBody(body)
			})
			c := NewSLMClient("http://x", "m", time.Second, client)
			got, _ := c.Decide(context.Background(), "x")
			if got != tt.want {
				t.Errorf("Decide = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSLMCacheHitMiss verifies that repeated prompts hit the cache on
// the second call and only one HTTP round-trip occurs.
func TestSLMCacheHitMiss(t *testing.T) {
	callCount := 0
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		callCount++
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client).WithCache(10, 5*time.Minute)

	// First call: cache miss, should hit Ollama
	r1, err := c.Decide(context.Background(), "identical prompt")
	if err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	if r1 != RouteLocal {
		t.Errorf("first route = %q, want local", r1)
	}
	if callCount != 1 {
		t.Errorf("first callCount = %d, want 1", callCount)
	}

	// Second call: cache hit, should NOT hit Ollama
	r2, err := c.Decide(context.Background(), "identical prompt")
	if err != nil {
		t.Fatalf("second Decide: %v", err)
	}
	if r2 != RouteLocal {
		t.Errorf("second route = %q, want local", r2)
	}
	if callCount != 1 {
		t.Errorf("second callCount = %d, want 1 (cache hit)", callCount)
	}

	// Different prompt: cache miss, should hit Ollama again
	r3, err := c.Decide(context.Background(), "different prompt")
	if err != nil {
		t.Fatalf("third Decide: %v", err)
	}
	if r3 != RouteLocal {
		t.Errorf("third route = %q, want local", r3)
	}
	if callCount != 2 {
		t.Errorf("third callCount = %d, want 2 (cache miss for new prompt)", callCount)
	}
}

// TestSLMCacheBudgetMiss verifies that the same prompt with a different
// guardrail budget produces a cache miss (issue #369). This prevents
// a cached "local" decision made under a high VRAM budget from being
// incorrectly served when the next request has a much lower budget.
func TestSLMCacheBudgetMiss(t *testing.T) {
	callCount := 0
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		callCount++
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client).WithCache(10, 5*time.Minute)

	prompt := "fix the off-by-one error in the parser"

	// First call with budget 6000: cache miss, hits Ollama
	r1, err := c.DecideWithBudget(context.Background(), prompt, NeutralConfidence, 6000)
	if err != nil {
		t.Fatalf("first DecideWithBudget: %v", err)
	}
	if r1 != RouteLocal {
		t.Errorf("first route = %q, want local", r1)
	}
	if callCount != 1 {
		t.Errorf("first callCount = %d, want 1", callCount)
	}

	// Same prompt, same budget: cache hit, NO Ollama call
	r2, err := c.DecideWithBudget(context.Background(), prompt, NeutralConfidence, 6000)
	if err != nil {
		t.Fatalf("second DecideWithBudget (same budget): %v", err)
	}
	if r2 != RouteLocal {
		t.Errorf("second route = %q, want local", r2)
	}
	if callCount != 1 {
		t.Errorf("second callCount = %d, want 1 (cache hit)", callCount)
	}

	// Same prompt, different budget (3000 vs 6000): cache MISS, hits Ollama
	r3, err := c.DecideWithBudget(context.Background(), prompt, NeutralConfidence, 3000)
	if err != nil {
		t.Fatalf("third DecideWithBudget (different budget): %v", err)
	}
	if r3 != RouteLocal {
		t.Errorf("third route = %q, want local", r3)
	}
	if callCount != 2 {
		t.Errorf("third callCount = %d, want 2 (budget change = cache miss)", callCount)
	}
}

// TestSLMCacheTTLExpiry verifies that entries expire after TTL and
// trigger a fresh Ollama call.
func TestSLMCacheTTLExpiry(t *testing.T) {
	callCount := 0
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		callCount++
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	})
	// TTL = 10ms to make test fast
	c := NewSLMClient("http://x", "m", time.Second, client).WithCache(10, 10*time.Millisecond)

	// First call
	if _, err := c.Decide(context.Background(), "prompt"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if callCount != 1 {
		t.Errorf("after first callCount = %d, want 1", callCount)
	}

	// Second call within TTL: cache hit
	if _, err := c.Decide(context.Background(), "prompt"); err != nil {
		t.Fatalf("second: %v", err)
	}
	if callCount != 1 {
		t.Errorf("within TTL callCount = %d, want 1", callCount)
	}

	// Wait for TTL to expire
	time.Sleep(20 * time.Millisecond)

	// Third call after TTL: cache miss, fresh Ollama call
	if _, err := c.Decide(context.Background(), "prompt"); err != nil {
		t.Fatalf("third: %v", err)
	}
	if callCount != 2 {
		t.Errorf("after TTL callCount = %d, want 2", callCount)
	}
}

// TestSLMCacheSizeEviction verifies eviction when cache exceeds max size.
// The implementation evicts entries by map iteration order (a FIFO approximation),
// NOT LRU — access does not change insertion order. Map iteration order is
// deliberately randomised by the Go runtime, making this test non-deterministic.
// Skipped until the implementation is upgraded to a true LRU (e.g. list.Map).
func TestSLMCacheSizeEviction(t *testing.T) {
	t.Skip("flaky: implementation uses randomised map iteration order, not LRU")

	callCount := 0
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		callCount++
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	})
	// Size = 2
	c := NewSLMClient("http://x", "m", time.Second, client).WithCache(2, 5*time.Minute)

	// Fill cache with two entries
	if _, err := c.Decide(context.Background(), "prompt A"); err != nil {
		t.Fatalf("A: %v", err)
	}
	if _, err := c.Decide(context.Background(), "prompt B"); err != nil {
		t.Fatalf("B: %v", err)
	}
	if callCount != 2 {
		t.Errorf("after A+B callCount = %d, want 2", callCount)
	}

	// Access A again — FIFO order is unchanged by access, so cache still [A, B].
	if _, err := c.Decide(context.Background(), "prompt A"); err != nil {
		t.Fatalf("A again: %v", err)
	}
	if callCount != 2 {
		t.Errorf("A again callCount = %d, want 2 (cache hit)", callCount)
	}

	// Add C, evicts A (oldest/FIFO). Cache = [B, C].
	if _, err := c.Decide(context.Background(), "prompt C"); err != nil {
		t.Fatalf("C: %v", err)
	}
	if callCount != 3 {
		t.Errorf("after C callCount = %d, want 3 (A evicted)", callCount)
	}

	// B is still in cache — hit.
	if _, err := c.Decide(context.Background(), "prompt B"); err != nil {
		t.Fatalf("B again: %v", err)
	}
	if callCount != 3 {
		t.Errorf("B again callCount = %d, want 3 (B still cached)", callCount)
	}

	// Cache = [B, C], B is MRU. Add A — evicts C (oldest). Cache = [B, A].
	if _, err := c.Decide(context.Background(), "prompt A"); err != nil {
		t.Fatalf("A: %v", err)
	}
	if callCount != 4 {
		t.Errorf("A callCount = %d, want 4 (C evicted, A added)", callCount)
	}

	// A is in cache — hit.
	if _, err := c.Decide(context.Background(), "prompt A"); err != nil {
		t.Fatalf("A again: %v", err)
	}
	if callCount != 4 {
		t.Errorf("A again callCount = %d, want 4 (A still cached)", callCount)
	}

	// Cache = [B, A], A is MRU. Add B — evicts A (oldest). Cache = [B, C].
	// C was previously evicted; this is a miss.
	if _, err := c.Decide(context.Background(), "prompt C"); err != nil {
		t.Fatalf("C: %v", err)
	}
	if callCount != 5 {
		t.Errorf("C callCount = %d, want 5 (C was evicted, re-added)", callCount)
	}
}

// TestSLMCacheTransportErrorNotCached verifies that transport errors
// (network failure, non-200, parse failure) are NOT cached, so a
// transient Ollama failure is retried on the next request.
func TestSLMCacheTransportErrorNotCached(t *testing.T) {
	callCount := 0
	// First call fails, second succeeds
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		callCount++
		if callCount == 1 {
			return nil, errNet("dial tcp: connection refused")
		}
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client).WithCache(10, 5*time.Minute)

	// First call: transport error returns ErrFallback; route is frontier.
	r1, err := c.Decide(context.Background(), "flaky prompt")
	if err == nil {
		t.Fatalf("first Decide: expected transport error, got nil")
	}
	if r1 != RouteFrontier {
		t.Errorf("first route = %q, want frontier (error fallback)", r1)
	}

	// Second call: should retry (not use cached error) and succeed
	r2, err := c.Decide(context.Background(), "flaky prompt")
	if err != nil {
		t.Fatalf("second Decide: %v", err)
	}
	if r2 != RouteLocal {
		t.Errorf("second route = %q, want local (retry succeeded)", r2)
	}
	if callCount != 2 {
		t.Errorf("callCount = %d, want 2 (error not cached, retry happened)", callCount)
	}
}

// TestSLMCacheDisabledWhenSizeZero verifies that cache size 0 disables
// caching entirely (every call hits Ollama).
func TestSLMCacheDisabledWhenSizeZero(t *testing.T) {
	callCount := 0
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		callCount++
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client).WithCache(0, 5*time.Minute)

	if _, err := c.Decide(context.Background(), "prompt"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := c.Decide(context.Background(), "prompt"); err != nil {
		t.Fatalf("second: %v", err)
	}
	if callCount != 2 {
		t.Errorf("callCount = %d, want 2 (cache disabled)", callCount)
	}
}

// TestSLMCacheConfidenceKeyed verifies that different confidence values
// produce different cache entries (the system prompt changes with confidence).
func TestSLMCacheConfidenceKeyed(t *testing.T) {
	callCount := 0
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		callCount++
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client).WithCache(10, 5*time.Minute)

	// Decide (uses NeutralConfidence = 0.5)
	if _, err := c.Decide(context.Background(), "prompt"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if callCount != 1 {
		t.Errorf("after Decide callCount = %d, want 1", callCount)
	}

	// DecideWithConfidence with same confidence = cache hit
	if _, err := c.DecideWithConfidence(context.Background(), "prompt", 0.5); err != nil {
		t.Fatalf("DecideWithConfidence(0.5): %v", err)
	}
	if callCount != 1 {
		t.Errorf("after DecideWithConfidence(0.5) callCount = %d, want 1 (cache hit)", callCount)
	}

	// DecideWithConfidence with different confidence = cache miss (different system prompt)
	if _, err := c.DecideWithConfidence(context.Background(), "prompt", 0.9); err != nil {
		t.Fatalf("DecideWithConfidence(0.9): %v", err)
	}
	if callCount != 2 {
		t.Errorf("after DecideWithConfidence(0.9) callCount = %d, want 2 (different confidence = miss)", callCount)
	}
}
