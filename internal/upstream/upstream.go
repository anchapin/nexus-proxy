// Package upstream handles the proxy's outbound traffic: streaming responses
// back to the harness, and running the fusion panel (local + frontier in
// parallel + arbiter synthesis).
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is the minimal interface used by the stream and fusion helpers. The
// default http.Client satisfies it; tests can pass a stub.
type Client interface {
	Do(req *http.Request) (*http.Response, error)
}

// Stream POSTs payload to targetURL and flushes every newline-terminated
// chunk from the upstream response body straight to w. This preserves the
// harness's expected SSE framing — each `data: {…}\n\n` arrives intact.
//
// apiKey may be empty (local Ollama has no auth).
func Stream(w http.ResponseWriter, client Client, targetURL, apiKey string, payload map[string]interface{}) error {
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("upstream: marshal: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(jsonPayload))
	if err != nil {
		return fmt.Errorf("upstream: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("upstream: do: %w", err)
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("upstream: response writer does not support flushing")
	}
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return werr
			}
			flusher.Flush()
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
	}
}

// FetchPanel fetches a single non-streaming completion from targetURL and
// returns the assistant message text. Designed for the fusion panel where
// we need the full response before asking the arbiter to synthesize.
func FetchPanel(ctx context.Context, client Client, targetURL, apiKey, modelName string, body map[string]interface{}) (string, error) {
	payload := make(map[string]interface{}, len(body)+2)
	for k, v := range body {
		payload[k] = v
	}
	payload["model"] = modelName
	payload["stream"] = false

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("fusion: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(jsonPayload))
	if err != nil {
		return "", fmt.Errorf("fusion: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fusion: do: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fusion: %s status %d: %s", modelName, resp.StatusCode, respBody)
	}

	var raw struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return "", fmt.Errorf("fusion: decode: %w", err)
	}
	if len(raw.Choices) == 0 {
		return "", fmt.Errorf("fusion: %s returned empty choice", modelName)
	}
	return raw.Choices[0].Message.Content, nil
}

// PanelResult is one member's contribution to a fusion response. Members
// that errored are returned with Err set and Content empty; callers should
// surface that to the arbiter so it can choose to ignore or down-weight.
type PanelResult struct {
	Source  string // "local" or "frontier"
	Content string
	Err     error
}

// Panel runs local and frontier fetches concurrently and waits for both.
// Each member gets its own timeout (perFetchTimeout) so a slow frontier
// can't pin the local one.
//
// arbiterURL/arbiterKey/arbiterModel identify the synthesis model. The
// arbiter receives a single user message containing both candidates and
// streams the synthesized reply via Stream.
func Panel(
	w http.ResponseWriter,
	client Client,
	localBaseURL, localModel, frontierURL, frontierModel string,
	arbiterURL, arbiterKey, arbiterModel string,
	body map[string]interface{},
	latestPrompt string,
	perFetchTimeout time.Duration,
) error {
	results := make(chan PanelResult, 2)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), withDefault(perFetchTimeout))
		defer cancel()
		c, err := FetchPanel(ctx, client,
			localBaseURL+"/v1/chat/completions", "", localModel, body)
		results <- PanelResult{Source: "local", Content: c, Err: err}
	}()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), withDefault(perFetchTimeout))
		defer cancel()
		c, err := FetchPanel(ctx, client,
			frontierURL, "", frontierModel, body)
		results <- PanelResult{Source: "frontier", Content: c, Err: err}
	}()
	r1 := <-results
	r2 := <-results

	synth := SynthesisPrompt(latestPrompt, r1, r2)
	synthBody := map[string]interface{}{
		"model": arbiterModel,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a master synthesis AI. Deliver only the final synthesized response. Do not mention that you are an arbiter."},
			{"role": "user", "content": synth},
		},
		"stream": true,
	}
	return Stream(w, client, arbiterURL, arbiterKey, synthBody)
}

// SynthesisPrompt formats the arbiter prompt. Exported so the handler and
// any future CLI dashboard can render the same template.
func SynthesisPrompt(userPrompt string, local, frontier PanelResult) string {
	return fmt.Sprintf(`You are a Master Synthesis Arbiter AI. Synthesize the strongest final answer from these candidates.

User Prompt: %s

Candidate 1 (Local Model - Fast execution):
%s

Candidate 2 (Frontier Model - Deep reasoning):
%s`, userPrompt, formatCandidate(local), formatCandidate(frontier))
}

func formatCandidate(r PanelResult) string {
	if r.Err != nil {
		return fmt.Sprintf("[%s failed: %v]", r.Source, r.Err)
	}
	return r.Content
}

func withDefault(d time.Duration) time.Duration {
	if d <= 0 {
		return 120 * time.Second
	}
	return d
}
