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

// Decide returns the routing decision for prompt. The fallback on any
// failure (transport, decode, parse, unknown value) is RouteFrontier — that
// is the safest default because it never silently drops a request to a
// non-existent local model.
func (c *SLMClient) Decide(ctx context.Context, prompt string) (Route, error) {
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
		return RouteFrontier, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Client.Do(req)
	if err != nil {
		return RouteFrontier, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
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
	var decision struct {
		Route string `json:"route"`
	}
	if err := json.Unmarshal([]byte(content), &decision); err != nil {
		return RouteFrontier, fmt.Errorf("slm: parse decision %q: %w", content, err)
	}

	switch Route(strings.ToLower(decision.Route)) {
	case RouteLocal, RouteFusion:
		return Route(strings.ToLower(decision.Route)), nil
	default:
		return RouteFrontier, nil
	}
}

// HTTPPoster is the minimal interface SLMClient needs from an http.Client.
// It exists so tests can swap in fakes without depending on *http.Client.
type HTTPPoster interface {
	Do(req *http.Request) (*http.Response, error)
}