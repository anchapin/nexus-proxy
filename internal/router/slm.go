package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anchapin/nexus-proxy/internal/observability"
)

// defaultMaxResponseBytes is the default cap on upstream response bodies.
// It is used by ReadAllLimited to prevent memory exhaustion (issue #365).
const defaultMaxResponseBytes = 64 << 20 // 64 MiB

// SLMClient talks to a local Ollama /api/chat endpoint and asks the small
// model to produce a routing decision. The HTTP layer is abstracted so
// tests can substitute a deterministic stub.
type SLMClient struct {
	BaseURL string        // e.g. "http://localhost:11434"
	Model   string        // e.g. "qwen3-coder:4b"
	Timeout time.Duration // per-call timeout (default 8s)
	Client  *http.Client

	// ConfidenceFloor / ConfidenceCeiling bound the neutral band for
	// judge-guided adaptive routing (issue #47). When the empirical
	// local confidence passed to DecideWithConfidence is below the floor
	// the system prompt is augmented with a frontier bias; above the
	// ceiling it gets a local bias; inside the band the request is
	// byte-for-byte identical to the pre-issue-47 path. Zero values fall
	// back to DefaultConfidenceFloor / DefaultConfidenceCeiling.
	ConfidenceFloor   float64
	ConfidenceCeiling float64
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
const slmSystemPrompt = `You are an intelligent routing assistant for a coding agent proxy. 
    Analyze the user's prompt. 
    - If it is a simple task (boilerplate, styling, small isolated functions), output {"route": "local"}. 
    - If it is a complex task (deep debugging, multi-file refactoring), output {"route": "frontier"}. 
    - If it requires extreme architectural deliberation and planning, output {"route": "fusion"}.
	Respond ONLY in valid JSON. No explanations.`

// negativeBiasNote is appended to slmSystemPrompt when empirical local
// confidence for the task category is below the floor. It nudges the SLM
// toward frontier without hard-overriding its judgement — the SLM may
// still pick local for a trivially simple prompt.
const negativeBiasNote = `

ADAPTIVE ROUTING CONTEXT: Historical quality evaluations show the LOCAL model has performed POORLY on tasks similar to this one. Strongly prefer {"route": "frontier"} unless the task is trivially simple.`

// positiveBiasNote is appended when empirical local confidence is above the
// ceiling: the local model has a strong track record on this kind of task,
// so favour it when the request is not clearly complex.
const positiveBiasNote = `

ADAPTIVE ROUTING CONTEXT: Historical quality evaluations show the LOCAL model handles tasks similar to this one WELL. Prefer {"route": "local"} when the task is not clearly complex.`

// Decide returns the routing decision for prompt. It is the neutral-path
// entry point: equivalent to DecideWithConfidence with NeutralConfidence,
// so the SLM request is byte-for-byte identical to the pre-issue-47
// behaviour. The fallback on any failure (transport, decode, parse,
// unknown value) is RouteFrontier — that is the safest default because it
// never silently drops a request to a non-existent local model.
func (c *SLMClient) Decide(ctx context.Context, prompt string) (Route, error) {
	return c.DecideWithConfidence(ctx, prompt, NeutralConfidence)
}

// DecideWithConfidence is Decide augmented with the empirical local
// confidence signal (issue #47). confidence is a 0.0..1.0 estimate of how
// well the local model performs on prompts like this one, derived from
// historical judge scores (see ConfidenceStore). Below the floor the
// system prompt gains a frontier bias; above the ceiling a local bias;
// inside the neutral band the request is unchanged from Decide.
func (c *SLMClient) DecideWithConfidence(ctx context.Context, prompt string, confidence float64) (Route, error) {
	return c.decide(ctx, prompt, c.systemPromptFor(confidence))
}

// systemPromptFor returns the SLM system prompt for the given confidence,
// applying the floor/ceiling bias notes. It is separated out so tests can
// assert the exact augmentation without an HTTP round-trip.
func (c *SLMClient) systemPromptFor(confidence float64) string {
	floor := c.ConfidenceFloor
	if floor <= 0 {
		floor = DefaultConfidenceFloor
	}
	ceiling := c.ConfidenceCeiling
	if ceiling <= 0 {
		ceiling = DefaultConfidenceCeiling
	}
	switch {
	case confidence < floor:
		return slmSystemPrompt + negativeBiasNote
	case confidence > ceiling:
		return slmSystemPrompt + positiveBiasNote
	default:
		return slmSystemPrompt
	}
}

// decide performs the HTTP round-trip with the supplied system prompt.
func (c *SLMClient) decide(ctx context.Context, prompt, systemPrompt string) (Route, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
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
		return RouteFrontier, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Client.Do(req)
	if err != nil {
		return RouteFrontier, err
	}
	defer resp.Body.Close()

	body, err := observability.ReadAllLimited(nil, resp.Body, defaultMaxResponseBytes)
	if err != nil {
		return RouteFrontier, err
	}
	if resp.StatusCode != http.StatusOK {
		return RouteFrontier, fmt.Errorf("slm: status %d: %s", resp.StatusCode, body)
	}

	var raw struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return RouteFrontier, fmt.Errorf("slm: decode: %w", err)
	}
	content := strings.TrimSpace(raw.Message.Content)
	if content == "" {
		return RouteFrontier, errors.New("slm: empty content")
	}
	decision, err := parseSLMDecision(content)
	if err != nil {
		return RouteFrontier, err
	}

	switch Route(strings.ToLower(decision.Route)) {
	case RouteLocal, RouteFusion:
		return Route(strings.ToLower(decision.Route)), nil
	default:
		return RouteFrontier, nil
	}
}

// slmDecision is the parsed route-decision object returned by the routing
// SLM. The model is instructed to emit {"route":"local|frontier|fusion"},
// but it frequently wraps that in markdown fences or prose (issue #79).
// parseSLMDecision below tolerates those shapes so a usable decision is
// not discarded just because of formatting noise.
type slmDecision struct {
	Route string `json:"route"`
}

// parseSLMDecision extracts the route-decision JSON object from the SLM's
// raw response content. It tolerates three common SLM output shapes:
//  1. Bare JSON object: {"route":"local"}
//  2. Markdown-fenced JSON: ```json\n{"route":"local"}\n```
//  3. Prose before/after the first JSON object: Here: {"route":"local"} !
//
// It returns an error only when no valid JSON object can be found, so the
// caller (decide) can fall back to RouteFrontier — the safe default. The
// first balanced JSON object wins: if there are multiple objects only the
// first is considered, so ambiguous/conflicting responses do not silently
// pick a later object (issue #79).
//
// Defaults preserved for the caller: an unspecified or unknown route still
// resolves to RouteFrontier unless the object explicitly names local or
// fusion; confidence clamping and task-type defaults live in the planner
// and are untouched here.
func parseSLMDecision(content string) (slmDecision, error) {
	var d slmDecision
	content = strings.TrimSpace(content)

	// 1. Fast path: the whole content is a bare JSON object.
	if err := json.Unmarshal([]byte(content), &d); err == nil {
		return d, nil
	}

	// 2. Markdown-fenced JSON block (```json ... ``` or ``` ... ```).
	if fenced := extractFencedJSON(content); fenced != "" {
		if err := json.Unmarshal([]byte(fenced), &d); err == nil {
			return d, nil
		}
	}

	// 3. First balanced {...} substring, tolerating prose wrappers and
	// braces that appear inside JSON string literals.
	if obj := extractFirstJSONObject(content); obj != "" {
		if err := json.Unmarshal([]byte(obj), &d); err == nil {
			return d, nil
		}
	}

	return slmDecision{}, fmt.Errorf("slm: no parseable JSON decision in %q", content)
}

// extractFencedJSON returns the trimmed contents of the first markdown
// fenced code block in content, preferring a ```json opener and falling
// back to a bare ``` opener. It returns "" when no complete fenced block
// is present. Matching on the opener is case-insensitive so ```JSON and
// ```Json also work.
func extractFencedJSON(content string) string {
	lower := strings.ToLower(content)
	for _, opener := range []string{"```json", "```"} {
		idx := strings.Index(lower, opener)
		if idx < 0 {
			continue
		}
		start := idx + len(opener)
		// Skip a single trailing newline immediately after the opener so
		// it is not treated as part of the JSON payload.
		if rest := content[start:]; strings.HasPrefix(rest, "\r\n") {
			start += 2
		} else if strings.HasPrefix(rest, "\n") {
			start++
		}
		if start > len(content) {
			continue
		}
		end := strings.Index(content[start:], "```")
		if end < 0 {
			continue
		}
		return strings.TrimSpace(content[start : start+end])
	}
	return ""
}

// extractFirstJSONObject returns the first brace-balanced JSON object
// substring of content, starting at the first '{'. It is string-literal
// aware so braces that appear inside JSON string values do not affect
// balancing. It returns "" when content has no '{' or the braces never
// balance to zero.
func extractFirstJSONObject(content string) string {
	start := strings.IndexByte(content, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(content); i++ {
		c := content[i]
		if escaped {
			escaped = false
			continue
		}
		if inString {
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return content[start : i+1]
			}
		}
	}
	return ""
}

// HTTPPoster is the minimal interface SLMClient needs from an http.Client.
// It exists so tests can swap in fakes without depending on *http.Client.
type HTTPPoster interface {
	Do(req *http.Request) (*http.Response, error)
}
