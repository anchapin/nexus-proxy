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
	"strings"
	"time"
)

// Client is the minimal interface used by the stream and fusion helpers. The
// default http.Client satisfies it; tests can pass a stub.
type Client interface {
	Do(req *http.Request) (*http.Response, error)
}

// allowedHeaders is the allowlist of upstream response headers the proxy
// forwards to clients (issue #39). Headers NOT in this set — Server,
// Set-Cookie, Via, upstream X-RateLimit-*, X-Powered-By, ... — are dropped
// so the proxy does not leak upstream identity or forward session state
// into the harness's response stream.
//
// X-Nexus-* is matched by prefix (see headerAllowed) so the proxy's own
// instrumentation headers (X-Nexus-Degraded, X-Nexus-Overflow,
// X-Nexus-Cascade-Served-By, X-Nexus-RateLimit-*) pass through regardless
// of which subsystem set them.
var allowedHeaders = map[string]struct{}{
	"Content-Type":  {},
	"Cache-Control": {},
}

// headerAllowed reports whether name should be forwarded to the client.
// Header names are canonicalised by net/http (CanonicalMIMEHeaderKey)
// before they reach the map, so we compare against the canonical form.
// X-Nexus-* is matched by prefix (case-insensitive, redundant given
// canonicalisation but defensive) so future instrumentation headers pass
// through without touching the allowlist.
func headerAllowed(name string) bool {
	if _, ok := allowedHeaders[name]; ok {
		return true
	}
	return strings.HasPrefix(strings.ToLower(name), "x-nexus-")
}

// copyAllowedHeaders copies only allowlisted headers from src to dst.
// Non-allowed headers (Server, Set-Cookie, Via, ...) are dropped so the
// proxy does not leak upstream identity or session state to the client.
func copyAllowedHeaders(dst, src http.Header) {
	for k, vs := range src {
		if !headerAllowed(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// Stream POSTs payload to targetURL and flushes every newline-terminated
// chunk from the upstream response body straight to w. This preserves the
// harness's expected SSE framing — each `data: {…}\n\n` arrives intact.
//
// apiKey may be empty (local Ollama has no auth).
//
// Stream is a thin wrapper around StreamWithContext that uses a fresh
// context.Background(). Callers that need a timeout (issue #12: the
// fusion arbiter) should use StreamWithContext directly.
func Stream(w http.ResponseWriter, client Client, targetURL, apiKey string, payload map[string]interface{}) error {
	return StreamWithContext(context.Background(), w, client, targetURL, apiKey, payload)
}

// StreamWithContext is Stream plus an explicit request context. The
// context is bound to the upstream POST via http.NewRequestWithContext,
// so cancellation (e.g. via context.WithTimeout) propagates both
// client-side (cancels the in-flight request) and server-side
// (cancels the handler's r.Context()). Use this from callers that
// need to bound an upstream call — issue #12 added NEXUS_ARBITER_TIMEOUT
// for exactly this purpose on the fusion arbiter path.
func StreamWithContext(ctx context.Context, w http.ResponseWriter, client Client, targetURL, apiKey string, payload map[string]interface{}) error {
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("upstream: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(jsonPayload))
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

	// Forward only allowlisted upstream headers so the proxy does not
	// leak upstream identity (Server), session state (Set-Cookie), or
	// routing metadata (Via) to the client (issue #39).
	copyAllowedHeaders(w.Header(), resp.Header)
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

// BufferedFetch POSTs payload to targetURL, buffers the entire upstream
// response, validates it as a single JSON object, and writes it back to
// w as one chatCompletionResponse. The body is forced to stream=false
// on the wire so the upstream returns JSON (not SSE). This is the
// harness's expected shape when body["stream"]=false — OpenAI treats
// the flag as advisory; non-stream responses come back as JSON. Issue
// #10.
//
// apiKey may be empty (local Ollama has no auth).
//
// BufferedFetch is a thin wrapper around BufferedFetchWithContext that
// uses a fresh context.Background(). Callers that need a timeout
// should use BufferedFetchWithContext directly — same contract as
// StreamWithContext.
func BufferedFetch(w http.ResponseWriter, client Client, targetURL, apiKey string, payload map[string]interface{}) error {
	return BufferedFetchWithContext(context.Background(), w, client, targetURL, apiKey, payload)
}

// BufferedFetchWithContext is BufferedFetch plus an explicit request
// context. Cancellation via context.WithTimeout propagates both
// client-side (cancels the in-flight request) and server-side (cancels
// the handler's r.Context()).
func BufferedFetchWithContext(ctx context.Context, w http.ResponseWriter, client Client, targetURL, apiKey string, payload map[string]interface{}) error {
	body := make(map[string]interface{}, len(payload)+1)
	for k, v := range payload {
		body[k] = v
	}
	body["stream"] = false

	jsonPayload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("upstream: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(jsonPayload))
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
	respBody, _ := io.ReadAll(resp.Body)

	// Validate the upstream body is a single JSON object. A misbehaving
	// upstream returning HTML or plain text would otherwise propagate
	// through to the harness and confuse its JSON parser.
	var probe map[string]interface{}
	if err := json.Unmarshal(respBody, &probe); err != nil {
		return fmt.Errorf("upstream: invalid JSON in response (status %d): %w", resp.StatusCode, err)
	}

	// Forward only allowlisted upstream headers (issue #39). Content-Type
	// is then always re-asserted to application/json so the harness sees a
	// plain JSON envelope regardless of what the upstream declared.
	copyAllowedHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, werr := w.Write(respBody)
	return werr
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
// When skipLocal is true the local Ollama fetch is omitted (issue #8
// graceful-degradation path). The local slot in the arbiter prompt is
// populated with a synthetic PanelResult whose Err is set to a sentinel
// error, which formatCandidate renders as
// "[local failed: ollama unavailable (degraded)]". The arbiter's
// "synthesize the strongest answer" instruction already copes with one
// candidate being unavailable, so the synthesis stream still produces a
// useful reply using only the frontier member.
//
// arbiterURL/arbiterKey/arbiterModel identify the synthesis model. The
// arbiter receives a single user message containing both candidates and
// streams the synthesized reply via Stream. The arbiter call is bounded
// by arbiterTimeout (issue #12, NEXUS_ARBITER_TIMEOUT, default 60s) via
// StreamWithContext so a slow synthesis endpoint cannot block the
// handler indefinitely — without this the arbiter inherits the shared
// http.DefaultClient which has no timeout.
func Panel(
	w http.ResponseWriter,
	client Client,
	localBaseURL, localModel, frontierURL, frontierModel string,
	arbiterURL, arbiterKey, arbiterModel string,
	body map[string]interface{},
	latestPrompt string,
	perFetchTimeout time.Duration,
	arbiterTimeout time.Duration,
	skipLocal bool,
) error {
	results := make(chan PanelResult, 2)
	if skipLocal {
		// Synthetic local failure so the arbiter prompt shape stays
		// stable. formatCandidate will emit "[local failed: ...]".
		results <- PanelResult{
			Source: "local",
			Err:    errors.New("ollama unavailable (degraded)"),
		}
	} else {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), withDefault(perFetchTimeout))
			defer cancel()
			c, err := FetchPanel(ctx, client,
				localBaseURL+"/v1/chat/completions", "", localModel, body)
			results <- PanelResult{Source: "local", Content: c, Err: err}
		}()
	}
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
	// Bound the arbiter call (issue #12). The two panel-member fetches
	// above already enforce perFetchTimeout via FetchPanel's context,
	// so we leave them alone and only the arbiter stream picks up the
	// new arbiterTimeout knob.
	arbiterCtx, cancelArbiter := context.WithTimeout(context.Background(), withDefaultArbiterTimeout(arbiterTimeout))
	defer cancelArbiter()
	// Honor the harness's stream flag (issue #10). Panel members
	// already force stream=false on the wire (FetchPanel needs the
	// full body to feed the arbiter), so only the arbiter dispatch
	// itself needs to branch. BufferedFetchWithContext re-asserts
	// stream=false on the arbiter wire so the synthesis endpoint
	// returns a single chatCompletionResponse JSON object.
	stream := true
	if s, ok := body["stream"].(bool); ok && !s {
		stream = false
	}
	if stream {
		return StreamWithContext(arbiterCtx, w, client, arbiterURL, arbiterKey, synthBody)
	}
	return BufferedFetchWithContext(arbiterCtx, w, client, arbiterURL, arbiterKey, synthBody)
}

// arbiterDefaultTimeout is the per-call arbiter timeout used when
// Panel's arbiterTimeout argument is <= 0. Mirrors the issue default
// ("configurable, default 60s").
const arbiterDefaultTimeout = 60 * time.Second

func withDefaultArbiterTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return arbiterDefaultTimeout
	}
	return d
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
