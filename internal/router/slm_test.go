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

// TestSLMDecideRichNewFormat exercises the enriched schema
// ({"route":..., "confidence":..., "task_type":...}). All three fields
// must round-trip unchanged from the SLM to Decision (issue #44).
func TestSLMDecideRichNewFormat(t *testing.T) {
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		return okBody(`{"message":{"content":"{\"route\":\"local\",\"confidence\":0.8,\"task_type\":\"debugging\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	d, err := c.DecideRich(context.Background(), "x")
	if err != nil {
		t.Fatalf("DecideRich: %v", err)
	}
	if d.Route != RouteLocal {
		t.Errorf("Route = %q, want local", d.Route)
	}
	if d.Confidence != 0.8 {
		t.Errorf("Confidence = %v, want 0.8", d.Confidence)
	}
	if d.TaskType != "debugging" {
		t.Errorf("TaskType = %q, want debugging", d.TaskType)
	}
}

// TestSLMDecideRichOldFormatGracefulDegradation is the regression
// guard for backward compatibility (issue #44 AC). An SLM emitting
// only the legacy {"route":"..."} shape must parse cleanly (no
// error), default Confidence to 0.5 and TaskType to "unknown", and
// preserve the chosen route. Without this defaulting, an older SLM
// rollout would regress to "every failure defaults to frontier".
func TestSLMDecideRichOldFormatGracefulDegradation(t *testing.T) {
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	d, err := c.DecideRich(context.Background(), "x")
	if err != nil {
		t.Fatalf("DecideRich must not error on legacy shape: %v", err)
	}
	if d.Route != RouteLocal {
		t.Errorf("Route = %q, want local (legacy field is preserved)", d.Route)
	}
	if d.Confidence != DefaultConfidenceWhenUnspecified {
		t.Errorf("Confidence = %v, want %v (no opinion default)", d.Confidence, DefaultConfidenceWhenUnspecified)
	}
	if d.TaskType != TaskTypeUnknown {
		t.Errorf("TaskType = %q, want %q", d.TaskType, TaskTypeUnknown)
	}
}

// TestSLMDecideRichExplicitZeroConfidence keeps the "very unsure"
// signal distinct from "no opinion". A model that explicitly emits
// confidence=0 must NOT be promoted to the default 0.5 — the
// operator may have configured NEXUS_SLM_CONFIDENCE_THRESHOLD > 0
// to escalate those requests to frontier.
func TestSLMDecideRichExplicitZeroConfidence(t *testing.T) {
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		return okBody(`{"message":{"content":"{\"route\":\"local\",\"confidence\":0,\"task_type\":\"\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	d, err := c.DecideRich(context.Background(), "x")
	if err != nil {
		t.Fatalf("DecideRich: %v", err)
	}
	if d.Route != RouteLocal {
		t.Errorf("Route = %q, want local", d.Route)
	}
	if d.Confidence != 0 {
		t.Errorf("Confidence = %v, want 0 (explicit zero is preserved)", d.Confidence)
	}
	if d.TaskType != TaskTypeUnknown {
		t.Errorf("TaskType = %q, want %q (empty string falls back)", d.TaskType, TaskTypeUnknown)
	}
}

// TestSLMDecideRichClampsOvershootConfidence keeps the threshold
// comparison meaningful: a confidence of 1.5 must not be treated as
// "more confident than 1.0" — that would let a malformed SLM payload
// defeat the escalation gate.
func TestSLMDecideRichClampsOvershootConfidence(t *testing.T) {
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		return okBody(`{"message":{"content":"{\"route\":\"local\",\"confidence\":1.5,\"task_type\":\"debugging\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	d, _ := c.DecideRich(context.Background(), "x")
	if d.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want clamped to 1.0", d.Confidence)
	}
}

// TestSLMDecideRichClampsNegativeConfidence is the symmetric guard
// for under-range confidence values.
func TestSLMDecideRichClampsNegativeConfidence(t *testing.T) {
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		return okBody(`{"message":{"content":"{\"route\":\"local\",\"confidence\":-0.5,\"task_type\":\"debugging\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	d, _ := c.DecideRich(context.Background(), "x")
	if d.Confidence != 0 {
		t.Errorf("Confidence = %v, want clamped to 0", d.Confidence)
	}
}

// TestSLMDecideRichUppercasesTaskType confirms the parser
// normalises case so downstream code can compare against the
// documented taxonomy without re-implementing ToLower.
func TestSLMDecideRichNormalisesTaskType(t *testing.T) {
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		return okBody(`{"message":{"content":"{\"route\":\"local\",\"confidence\":0.8,\"task_type\":\"  Debugging  \"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	d, err := c.DecideRich(context.Background(), "x")
	if err != nil {
		t.Fatalf("DecideRich: %v", err)
	}
	if d.TaskType != "debugging" {
		t.Errorf("TaskType = %q, want normalised to \"debugging\"", d.TaskType)
	}
}

// TestSLMDecideRichUnknownRouteKeepsConfidence confirms that the
// "unknown route falls back to frontier" rule (preserved from
// Decide) does NOT reset the confidence / task_type fields. The
// chat handler only escalates local / fusion on low confidence, so
// the gate must see the original confidence even when the route
// default-shifted to frontier.
func TestSLMDecideRichUnknownRouteKeepsConfidence(t *testing.T) {
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		return okBody(`{"message":{"content":"{\"route\":\"banana\",\"confidence\":0.4,\"task_type\":\"review\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	d, err := c.DecideRich(context.Background(), "x")
	if err != nil {
		t.Fatalf("DecideRich: %v", err)
	}
	if d.Route != RouteFrontier {
		t.Errorf("Route = %q, want frontier (default for unknown value)", d.Route)
	}
	if d.Confidence != 0.4 {
		t.Errorf("Confidence = %v, want 0.4 (preserved across route fallback)", d.Confidence)
	}
	if d.TaskType != "review" {
		t.Errorf("TaskType = %q, want review", d.TaskType)
	}
}

// TestDecideStillReturnsRouteOnly locks the backward-compat contract
// of the legacy wrapper: callers (existing tests, future embedders)
// that only read the route get the same value Decide always returned.
func TestDecideStillReturnsRouteOnly(t *testing.T) {
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		return okBody(`{"message":{"content":"{\"route\":\"local\",\"confidence\":0.8,\"task_type\":\"debugging\"}"}}`)
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	r, err := c.Decide(context.Background(), "x")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if r != RouteLocal {
		t.Errorf("Decide returned %q, want local", r)
	}
}

// TestSLMDecideRichTransportErrorFallsBackToFrontier confirms the
// safety net still fires for enriched decoding — a transport
// failure produces Decision{Route: RouteFrontier, TaskType:
// TaskTypeUnknown} + a non-nil error so callers can log it.
func TestSLMDecideRichTransportErrorFallsBackToFrontier(t *testing.T) {
	client := newClient(func(_ *http.Request) (*http.Response, error) {
		return nil, errNet("dial tcp: connection refused")
	})
	c := NewSLMClient("http://x", "m", time.Second, client)
	d, err := c.DecideRich(context.Background(), "x")
	if err == nil {
		t.Fatal("DecideRich must surface transport errors")
	}
	if d.Route != RouteFrontier {
		t.Errorf("Route = %q, want frontier", d.Route)
	}
	if d.TaskType != TaskTypeUnknown {
		t.Errorf("TaskType = %q, want %q", d.TaskType, TaskTypeUnknown)
	}
}

type errNet string

func (e errNet) Error() string { return string(e) }
