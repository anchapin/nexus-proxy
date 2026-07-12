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
