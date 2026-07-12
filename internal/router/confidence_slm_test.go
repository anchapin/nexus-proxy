package router

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// captureSystemPrompt returns the system-message content the SLM sent, so
// tests can assert the confidence-driven augmentation without re-parsing
// the whole payload.
func decodeSeenSystem(body string) string {
	// The payload is a JSON object; the system content is the first
	// message. We only need a cheap contains check in the callers, so
	// return the raw body — assertions use strings.Contains.
	return body
}

func TestDecideNeutralMatchesDecide(t *testing.T) {
	var decideBody, neutralBody string
	c := NewSLMClient("http://x", "m", time.Second, newClient(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		decideBody = string(b)
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	}))
	if _, err := c.Decide(context.Background(), "hello"); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	c2 := NewSLMClient("http://x", "m", time.Second, newClient(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		neutralBody = string(b)
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	}))
	if _, err := c2.DecideWithConfidence(context.Background(), "hello", NeutralConfidence); err != nil {
		t.Fatalf("DecideWithConfidence: %v", err)
	}
	if decodeSeenSystem(decideBody) != decodeSeenSystem(neutralBody) {
		t.Errorf("neutral confidence payload differs from Decide:\n Decide=%s\nNeutral=%s", decideBody, neutralBody)
	}
	if strings.Contains(neutralBody, "ADAPTIVE ROUTING CONTEXT") {
		t.Errorf("neutral payload should not contain bias note: %s", neutralBody)
	}
}

func TestDecideWithConfidenceLowInjectsNegativeBias(t *testing.T) {
	var seen string
	c := NewSLMClient("http://x", "m", time.Second, newClient(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		seen = string(b)
		return okBody(`{"message":{"content":"{\"route\":\"frontier\"}"}}`)
	}))
	if _, err := c.DecideWithConfidence(context.Background(), "debug this", 0.1); err != nil {
		t.Fatalf("DecideWithConfidence: %v", err)
	}
	if !strings.Contains(seen, "POORLY") || !strings.Contains(seen, "ADAPTIVE ROUTING CONTEXT") {
		t.Errorf("low-confidence payload missing negative bias note: %s", seen)
	}
	if strings.Contains(seen, "WELL") {
		t.Errorf("low-confidence payload should not contain positive note: %s", seen)
	}
}

func TestDecideWithConfidenceHighInjectsPositiveBias(t *testing.T) {
	var seen string
	c := NewSLMClient("http://x", "m", time.Second, newClient(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		seen = string(b)
		return okBody(`{"message":{"content":"{\"route\":\"local\"}"}}`)
	}))
	if _, err := c.DecideWithConfidence(context.Background(), "simple css", 0.95); err != nil {
		t.Fatalf("DecideWithConfidence: %v", err)
	}
	if !strings.Contains(seen, "WELL") || !strings.Contains(seen, "ADAPTIVE ROUTING CONTEXT") {
		t.Errorf("high-confidence payload missing positive bias note: %s", seen)
	}
	if strings.Contains(seen, "POORLY") {
		t.Errorf("high-confidence payload should not contain negative note: %s", seen)
	}
}

func TestSystemPromptForRespectsCustomBounds(t *testing.T) {
	c := &SLMClient{ConfidenceFloor: 0.3, ConfidenceCeiling: 0.7}
	if got := c.systemPromptFor(0.35); got != slmSystemPrompt {
		t.Error("0.35 within custom band should be unaugmented")
	}
	if got := c.systemPromptFor(0.2); !strings.Contains(got, "POORLY") {
		t.Error("0.2 below custom floor should get negative note")
	}
	if got := c.systemPromptFor(0.8); !strings.Contains(got, "WELL") {
		t.Error("0.8 above custom ceiling should get positive note")
	}
}

// TestAdaptiveRoutingIntegration is the issue #47 integration test:
// simulate 10 low-score local outcomes for "debugging", then verify the
// SLM receives the negative-bias system prompt augmentation for a new
// debugging prompt routed via the confidence store.
func TestAdaptiveRoutingIntegration(t *testing.T) {
	cs := newTestConfidenceStore(t, 5, 168*time.Hour)
	const prompt = "help me debug this failing test, I keep getting an exception"
	category := Categorize(prompt)
	if category != CategoryDebugging {
		t.Fatalf("Categorize(%q) = %q, want debugging", prompt, category)
	}
	for i := 0; i < 10; i++ {
		cs.RecordOutcome(category, RouteLocal, 1+i%2) // consistently 1..2
	}

	confidence := cs.LocalConfidence(category)
	if confidence >= DefaultConfidenceFloor {
		t.Fatalf("LocalConfidence = %v, want below floor %v", confidence, DefaultConfidenceFloor)
	}

	var seen string
	slm := NewSLMClient("http://x", "m", time.Second, newClient(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		seen = string(b)
		return okBody(`{"message":{"content":"{\"route\":\"frontier\"}"}}`)
	}))
	route, err := slm.DecideWithConfidence(context.Background(), prompt, confidence)
	if err != nil {
		t.Fatalf("DecideWithConfidence: %v", err)
	}
	if route != RouteFrontier {
		t.Errorf("route = %q, want frontier", route)
	}
	if !strings.Contains(seen, "ADAPTIVE ROUTING CONTEXT") || !strings.Contains(seen, "POORLY") {
		t.Errorf("SLM did not receive negative-bias augmentation: %s", seen)
	}
}
