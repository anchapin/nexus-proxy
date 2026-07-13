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

// TestSLMClient_CacheHit verifies that a second identical Decide call
// hits the cache and does not make a second HTTP round-trip (issue #162).
func TestSLMClient_CacheHit(t *testing.T) {
	callCount := 0
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		callCount++
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	c.CacheMaxEntries = 512

	prompt := "write a hello world function"

	// First call should hit the HTTP endpoint.
	_, _ = c.Decide(context.Background(), prompt)
	if callCount != 1 {
		t.Fatalf("first call: expected 1 HTTP call, got %d", callCount)
	}

	// Second call with identical prompt should hit cache.
	_, _ = c.Decide(context.Background(), prompt)
	if callCount != 1 {
		t.Fatalf("second call: expected 1 HTTP call (cache hit), got %d", callCount)
	}

	hits, misses := c.CacheStats()
	if hits != 1 {
		t.Errorf("hits: got %d, want 1", hits)
	}
	if misses != 1 {
		t.Errorf("misses: got %d, want 1", misses)
	}
}

// TestSLMClient_CacheTTL verifies that an expired cache entry is not
// served and a new HTTP call is made (issue #162).
func TestSLMClient_CacheTTL(t *testing.T) {
	callCount := 0
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		callCount++
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	c.CacheMaxEntries = 512
	c.CacheTTL = 10 * time.Millisecond // very short TTL for testing

	prompt := "write a hello world function"

	// First call: cache miss, HTTP call, entry cached.
	_, _ = c.Decide(context.Background(), prompt)
	if callCount != 1 {
		t.Fatalf("first call: expected 1 HTTP call, got %d", callCount)
	}

	// Second call immediately (within TTL): should hit cache.
	_, _ = c.Decide(context.Background(), prompt)
	if callCount != 1 {
		t.Fatalf("second call within TTL: expected 1 HTTP call (cache hit), got %d", callCount)
	}

	hits, misses := c.CacheStats()
	if hits != 1 {
		t.Errorf("hits after second call: got %d, want 1", hits)
	}
	if misses != 1 {
		t.Errorf("misses after second call: got %d, want 1", misses)
	}

	// Wait for the cache entry to expire.
	time.Sleep(20 * time.Millisecond)

	// Third call after TTL expiry: should miss cache and make new HTTP call.
	_, _ = c.Decide(context.Background(), prompt)
	if callCount != 2 {
		t.Fatalf("after TTL expiry: expected 2 HTTP calls, got %d", callCount)
	}

	hits2, misses2 := c.CacheStats()
	if hits2 != 1 {
		t.Errorf("hits after TTL expiry: got %d, want 1", hits2)
	}
	// misses should now be 2 (first call + third call after expiry)
	if misses2 != 2 {
		t.Errorf("misses after TTL expiry: got %d, want 2", misses2)
	}
}

// TestSLMClient_CacheDisabled verifies that zero CacheMaxEntries
// disables the cache entirely (every call makes HTTP).
func TestSLMClient_CacheDisabled(t *testing.T) {
	callCount := 0
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		callCount++
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	c.CacheMaxEntries = 0 // disabled

	prompt := "write a hello world function"

	_, _ = c.Decide(context.Background(), prompt)
	_, _ = c.Decide(context.Background(), prompt)

	if callCount != 2 {
		t.Fatalf("with cache disabled: expected 2 HTTP calls, got %d", callCount)
	}
}

// TestSLMClient_CacheMaxEntries enforces the entry limit by evicting
// the oldest entry when the cap is exceeded.
func TestSLMClient_CacheMaxEntries(t *testing.T) {
	callCount := 0
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		callCount++
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	c.CacheMaxEntries = 3
	c.CacheTTL = time.Hour // long enough that TTL won't fire during test

	// Fill the cache with 3 distinct prompts.
	for i := 0; i < 3; i++ {
		_, _ = c.Decide(context.Background(), "prompt "+string(rune('a'+i)))
	}
	if callCount != 3 {
		t.Fatalf("fill: expected 3 HTTP calls, got %d", callCount)
	}

	// A new distinct prompt evicts the oldest entry.
	_, _ = c.Decide(context.Background(), "prompt d")

	// Now asking for "prompt a" (evicted) should make a new HTTP call.
	// First reset call count.
	callCount = 0
	_, _ = c.Decide(context.Background(), "prompt a")
	if callCount != 1 {
		t.Fatalf("after eviction: expected 1 HTTP call for re-request, got %d", callCount)
	}
}
