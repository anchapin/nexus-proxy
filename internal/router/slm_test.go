package router

import (
	"context"
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