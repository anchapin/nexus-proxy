package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SLMClient talks to a local Ollama /api/chat endpoint and asks the small
// model to produce a routing decision. The HTTP layer is abstracted so
// tests can substitute a deterministic stub.
type SLMClient struct {
	BaseURL string        // e.g. "http://localhost:11434"
	Model   string        // e.g. "qwen3-coder:4b"
	Timeout time.Duration // per-call timeout (default 8s)
	Client  *http.Client
}

// TaskTypeUnknown is the sentinel TaskType emitted when the SLM omits
// the field or returns an empty string. Keeping it as a package
// constant lets tests and the handler compare against it without
// re-typing the literal across files.
const TaskTypeUnknown = "unknown"

// DefaultConfidenceWhenUnspecified is the value DecideRich assigns to
// Decision.Confidence when the SLM omits the field entirely (i.e. an
// older SLM that does not yet emit the enriched shape). It represents
// "no opinion" — neutral confidence that the operator can gate on or
// ignore. Kept as a constant so the threshold comparison in the
// handler / tests stays self-documenting.
const DefaultConfidenceWhenUnspecified = 0.5

// Decision carries the enriched routing decision from the SLM (issue
// #44). Confidence is a 0.0-1.0 self-reported score the operator can
// gate on (see Config.SLMConfidenceThreshold). TaskType is a coarse
// label for downstream cost / latency analytics — the routing
// decision does NOT branch on TaskType in this issue.
//
// Both Confidence and TaskType are best-effort: an SLM that only
// emits the legacy {"route":"..."} shape still parses cleanly via
// DecideRich's graceful degradation rule. Zero-value Confidence (the
// SLM explicitly emitted 0) is preserved as the explicit "very
// unsure" signal — the operator opts into escalating those by setting
// NEXUS_SLM_CONFIDENCE_THRESHOLD > 0.
type Decision struct {
	Route      Route
	Confidence float64
	TaskType   string
}

// NewSLMClient constructs a client. Pass nil for Client to use
// http.DefaultClient.
func NewSLMClient(baseURL, model string, timeout time.Duration, client *http.Client) *SLMClient {
	if client == nil {
		client = http.DefaultClient
	}
	return &SLMClient{BaseURL: baseURL, Model: model, Timeout: timeout, Client: client}
}

// slmSystemPrompt is the static instruction we send to the routing SLM.
// Keeping it as a package var (not a config field) makes it trivial to grep
// and to snapshot in tests.
//
// Issue #44 extends the schema with `confidence` and `task_type` so the
// handler can escalate low-confidence decisions and so cost / latency
// analytics have a category per request. The legacy {"route":"..."}
// shape still parses (see DecideRich for the graceful-degradation rule),
// so an older SLM rolling out before this prompt change keeps working.
const slmSystemPrompt = `You are an intelligent routing assistant for a coding agent proxy.
    Analyze the user's prompt.
    - If it is a simple task (boilerplate, styling, small isolated functions), output {"route": "local"}.
    - If it is a complex task (deep debugging, multi-file refactoring), output {"route": "frontier"}.
    - If it requires extreme architectural deliberation and planning, output {"route": "fusion"}.
    - Also include "confidence": a number between 0.0 and 1.0 reflecting how sure you are of the route choice.
    - Also include "task_type": one of "code_generation", "debugging", "architecture", "review", "refactoring". Use "unknown" when none of the categories fit cleanly.
    Respond ONLY in valid JSON of the shape {"route":"...", "confidence":0.0-1.0, "task_type":"..."}. No explanations.`

// DecideRich returns the enriched routing decision for prompt (issue
// #44). Falls back to RouteFrontier on any transport / decode / parse
// failure so the request path never silently drops a request to a
// non-existent local model — the same "every failure defaults to
// frontier" safety net that Decide has always provided.
//
// Graceful degradation rules:
//
//   - A model that emits the legacy shape {"route":"local"} still
//     parses cleanly. The missing confidence is treated as "no
//     opinion" and defaulted to DefaultConfidenceWhenUnspecified
//     (0.5); the missing / empty task_type becomes TaskTypeUnknown.
//     No error is returned — adding the new fields to the system
//     prompt must not break an older SLM rollout.
//   - A model that emits an unrecognised route value (e.g.
//     "banana") maps to RouteFrontier the same way Decide did;
//     Confidence and TaskType are preserved from the payload.
//   - Confidence values outside [0, 1] are clamped into range so
//     the threshold comparison stays meaningful if the SLM ever
//     overshoots.
//
// Errors are reserved for transport / decode / parse failures —
// routing-shape failures (unknown route, missing fields) return a
// Decision with the documented defaults and a nil error.
func (c *SLMClient) DecideRich(ctx context.Context, prompt string) (Decision, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "system", "content": slmSystemPrompt},
			{"role": "user", "content": prompt},
		},
		"format":  "json",
		"stream":  false,
		"options": map[string]float64{"temperature": 0.1},
	})

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodPost,
		c.BaseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return Decision{Route: RouteFrontier, TaskType: TaskTypeUnknown}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Client.Do(req)
	if err != nil {
		return Decision{Route: RouteFrontier, TaskType: TaskTypeUnknown}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Decision{Route: RouteFrontier, TaskType: TaskTypeUnknown}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Decision{Route: RouteFrontier, TaskType: TaskTypeUnknown}, fmt.Errorf("slm: status %d: %s", resp.StatusCode, body)
	}

	var raw struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Decision{Route: RouteFrontier, TaskType: TaskTypeUnknown}, fmt.Errorf("slm: decode: %w", err)
	}
	content := strings.TrimSpace(raw.Message.Content)
	if content == "" {
		return Decision{Route: RouteFrontier, TaskType: TaskTypeUnknown}, errors.New("slm: empty content")
	}

	// Pointer fields let us distinguish "missing" from "explicitly
	// 0": a legacy SLM that only emits {"route":"..."} leaves both
	// pointers nil, and we apply the documented defaults. The
	// explicit 0 case (model said "very unsure") is preserved end
	// to end so the operator can opt into escalating it.
	var parsed struct {
		Route      string   `json:"route"`
		Confidence *float64 `json:"confidence"`
		TaskType   *string  `json:"task_type"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return Decision{Route: RouteFrontier, TaskType: TaskTypeUnknown}, fmt.Errorf("slm: parse decision %q: %w", content, err)
	}

	decision := Decision{Route: RouteFrontier, TaskType: TaskTypeUnknown}
	switch Route(strings.ToLower(parsed.Route)) {
	case RouteLocal, RouteFusion:
		decision.Route = Route(strings.ToLower(parsed.Route))
	}
	if parsed.Confidence != nil {
		confidence := *parsed.Confidence
		// Clamp into [0, 1] so a malformed payload cannot
		// produce nonsense like "1.5" or "-0.2". The behaviour
		// is documented on Decision: explicit zero stays zero.
		if confidence > 1.0 {
			confidence = 1.0
		} else if confidence < 0 {
			confidence = 0
		}
		decision.Confidence = confidence
	} else {
		decision.Confidence = DefaultConfidenceWhenUnspecified
	}
	if parsed.TaskType != nil {
		if t := strings.ToLower(strings.TrimSpace(*parsed.TaskType)); t != "" {
			decision.TaskType = t
		}
	}
	return decision, nil
}

// Decide returns the routing decision for prompt. Thin wrapper
// around DecideRich preserved for embedders (existing tests, future
// callers) that only care about the route. Behaviour is identical
// to the pre-issue-#44 Decide — the "every failure defaults to
// frontier" safety net and the route-mapping rules are unchanged —
// only the underlying decoding path was widened to capture
// `confidence` and `task_type` for callers that opt into DecideRich.
func (c *SLMClient) Decide(ctx context.Context, prompt string) (Route, error) {
	decision, err := c.DecideRich(ctx, prompt)
	return decision.Route, err
}

// HTTPPoster is the minimal interface SLMClient needs from an http.Client.
// It exists so tests can swap in fakes without depending on *http.Client.
type HTTPPoster interface {
	Do(req *http.Request) (*http.Response, error)
}
