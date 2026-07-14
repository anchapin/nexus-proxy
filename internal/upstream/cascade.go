package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// CascadeStep is one member of a Cascade: a single model endpoint the
// runner will try in order. Name is a short identifier used in logs and
// the telemetry route_attempted field (e.g. "local", "frontier", "zai").
type CascadeStep struct {
	Name   string
	URL    string
	APIKey string
	Model  string
}

// Cascade runs an ordered list of steps and falls back to the next one on
// retryable failures: transport errors, HTTP 5xx/408/429, timeouts, and
// malformed upstream responses (unparseable JSON or malformed tool_calls).
//
// Cascade is stateless; build a fresh one per request so it picks up env
// changes without restarting the process (issue #14 acceptance criteria).
type Cascade struct {
	Steps   []CascadeStep
	Timeout time.Duration // per-attempt; <=0 falls back to cascadeDefaultTimeout
}

// CascadeResult is the per-request outcome suitable for telemetry.
type CascadeResult struct {
	// RouteAttempted is a "->"-joined sequence of step Names attempted,
	// regardless of whether the final one succeeded. e.g. "local",
	// "local->frontier", "local->frontier->zai". Empty when no steps ran.
	RouteAttempted string
	// Attempts is the number of steps that were tried.
	Attempts int
	// ServedBy is the Name of the step whose response was streamed to the
	// client. Empty when every step failed.
	ServedBy string
	// Succeeded is true if a step returned a usable response.
	Succeeded bool
	// LocalStepFailed is true when the "local" step was attempted but
	// failed with a retryable error and a later step served the request
	// (or the cascade exhausted every step). The chat handler uses this
	// to arm the local-route cooldown (issue #80) so subsequent requests
	// within the cooldown window skip local and go directly to the
	// fallback, avoiding repeated slow local timeouts until the health
	// poller catches up.
	LocalStepFailed bool
	// ToolCalls carries the OpenAI-compatible tool_calls from the step
	// that served the request (issue #72). Empty for content-only
	// responses. The chat handler reads this to populate telemetry
	// (tool_call_count) and to feed the quality observer.
	ToolCalls []ToolCall
	// FallbackReason is the reason label for the cascade_fallback_total
	// metric (issue #205). It is set whenever a retryable step failure
	// causes the cascade to fall back to the next step. The value is one
	// of "timeout", "transport_error", or "malformed_toolcall". Empty
	// when no fallback occurred (cascade succeeded on first step or all
	// steps failed without retryable errors).
	FallbackReason string
}

// cascadeDefaultTimeout is the per-attempt timeout used when Cascade.Timeout
// is <= 0. Mirrors the issue default ("configurable, default 30s").
const cascadeDefaultTimeout = 30 * time.Second

// ErrSSEPartialWrite is returned by writeSSEResponse when an SSE body write
// fails after HTTP headers have already been committed (WriteHeader called).
// The caller must not call http.Error after receiving this error; the
// connection should be closed instead to avoid corrupting the SSE stream.
// See issue #241.
var ErrSSEPartialWrite = errors.New("cascade: SSE partial write after headers committed")

// cascadeErr tags a per-step failure so the runner knows whether to fall
// back (retry=true) or surface the error immediately (retry=false — e.g.
// upstream returned 401, retrying won't help). The reason field carries
// one of three values used for cascade_fallback_total{reason} metrics:
// "timeout", "transport_error", or "malformed_toolcall".
type cascadeErr struct {
	retry  bool
	reason string // "" when non-retryable
	msg    string
}

func (e *cascadeErr) Error() string { return e.msg }

// newCascadeErr creates a cascadeErr. reason is the label for the
// cascade_fallback_total metric: "timeout", "transport_error",
// "malformed_toolcall", or "" for non-retryable errors.
func newCascadeErr(retry bool, reason, format string, args ...interface{}) error {
	return &cascadeErr{retry: retry, reason: reason, msg: fmt.Sprintf(format, args...)}
}

// ShouldRetry reports whether the given status+err should trigger a fallback.
// statusCode is ignored when err != nil. HTTP 5xx, 408, and 429 are retryable;
// all other 4xx responses are surfaced to the caller without retry.
func ShouldRetry(statusCode int, err error) bool {
	if err != nil {
		return true
	}
	switch statusCode {
	case http.StatusRequestTimeout, // 408
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// Run executes the cascade. The first step whose response passes all
// validation has its content streamed to w as a single OpenAI-compatible
// SSE chunk followed by [DONE]. If every step fails, Run returns the last
// error and writes nothing to w.
//
// Validation happens before any byte is sent to the client, so the harness
// never sees a malformed response (issue requirement).
func (c *Cascade) Run(w http.ResponseWriter, client Client, payload map[string]interface{}) (CascadeResult, error) {
	if len(c.Steps) == 0 {
		return CascadeResult{}, errors.New("cascade: no steps configured")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = cascadeDefaultTimeout
	}

	res := CascadeResult{}
	var lastErr error
	for i, step := range c.Steps {
		res.Attempts = i + 1
		res.RouteAttempted = joinStepNames(c.Steps[:i+1])

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		msg, servedModel, err := fetchCascadeStep(ctx, client, step, payload)
		cancel()
		if err == nil {
			slog.Info("cascade served",
				slog.String("step", step.Name),
				slog.Int("attempt", i+1),
				slog.Int("total", len(c.Steps)),
			)
			res.Succeeded = true
			res.ServedBy = step.Name
			res.ToolCalls = msg.ToolCalls
			if werr := writeSSEResponse(w, step.Name, servedModel, msg); werr != nil {
				return res, werr
			}
			return res, nil
		}
		lastErr = err
		retry := classifyFailure(err)
		// Issue #80: expose whether the local step was the one that
		// failed so the chat handler can arm the local-route cooldown.
		// Only set when the step is named "local" AND the error is
		// retryable — a non-retryable failure (e.g. 401) surfaces to
		// the caller immediately and does not indicate a transient
		// local problem worth cooling down.
		if step.Name == "local" && retry {
			res.LocalStepFailed = true
		}
		// Issue #205: capture the fallback reason whenever a retryable
		// step failure causes a fallback to the next step. The reason
		// is used for the cascade_fallback_total{reason} Prometheus
		// counter so operators can observe cascade fallback rates.
		if retry {
			res.FallbackReason = CascadeFallbackReason(err)
		}
		slog.Warn("cascade step failed",
			slog.String("step", step.Name),
			slog.Int("attempt", i+1),
			slog.Int("total", len(c.Steps)),
			slog.Bool("retry", retry),
			slog.Any("err", err),
		)
		if !retry {
			// Non-retryable (e.g. upstream returned 401/403): stop and surface.
			return res, err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("cascade: no steps attempted")
	}
	return res, fmt.Errorf("cascade: all %d steps failed; last error: %w", len(c.Steps), lastErr)
}

// classifyFailure reports whether err was tagged as retryable. Unknown
// errors default to retry=true (safer — keeps falling back).
func classifyFailure(err error) bool {
	var cf *cascadeErr
	if errors.As(err, &cf) {
		return cf.retry
	}
	return true
}

// CascadeFallbackReason extracts the reason label from err if it is a
// cascadeErr with a non-empty reason field. The returned string is one
// of "timeout", "transport_error", or "malformed_toolcall". Empty string
// is returned when err is nil or the error carries no fallback reason.
func CascadeFallbackReason(err error) string {
	if err == nil {
		return ""
	}
	var cf *cascadeErr
	if errors.As(err, &cf) {
		return cf.reason
	}
	return ""
}

func joinStepNames(steps []CascadeStep) string {
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.Name
	}
	return strings.Join(names, "->")
}

// fetchCascadeStep does a single non-streaming POST to step.URL, validates
// the response, and returns the assistant message + the model name echoed
// back by the upstream (used in the SSE response). All returned errors are
// tagged via newCascadeErr so the runner knows whether to fall back.
func fetchCascadeStep(ctx context.Context, client Client, step CascadeStep, payload map[string]interface{}) (AssistantMessage, string, error) {
	body := make(map[string]interface{}, len(payload)+2)
	for k, v := range payload {
		body[k] = v
	}
	body["model"] = step.Model
	body["stream"] = false

	jsonPayload, mErr := json.Marshal(body)
	if mErr != nil {
		return AssistantMessage{}, "", newCascadeErr(false, "", "marshal: %v", mErr)
	}
	req, rErr := http.NewRequestWithContext(ctx, http.MethodPost, step.URL, bytes.NewReader(jsonPayload))
	if rErr != nil {
		return AssistantMessage{}, "", newCascadeErr(false, "", "build request: %v", rErr)
	}
	req.Header.Set("Content-Type", "application/json")
	if step.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+step.APIKey)
	}

	resp, dErr := client.Do(req)
	if dErr != nil {
		// Transport error / ctx timeout — always retry.
		reason := "transport_error"
		if errors.Is(dErr, context.DeadlineExceeded) {
			reason = "timeout"
		}
		return AssistantMessage{}, "", newCascadeErr(true, reason, "transport: %v", dErr)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if ShouldRetry(resp.StatusCode, nil) {
		return AssistantMessage{}, "", newCascadeErr(true, "transport_error", "status %d: %s", resp.StatusCode, truncateForLog(respBody, 200))
	}
	if resp.StatusCode != http.StatusOK {
		// Non-retryable 4xx (auth, bad request, etc.). Surface to caller.
		return AssistantMessage{}, "", newCascadeErr(false, "", "status %d: %s", resp.StatusCode, truncateForLog(respBody, 200))
	}

	msg, model, vErr := extractAssistantMessage(respBody)
	if vErr != nil {
		return AssistantMessage{}, "", vErr
	}
	return msg, model, nil
}

// assistantResponse is the slice of the OpenAI-compatible chat-completion
// schema we need to validate before streaming to the harness.
type assistantResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
	} `json:"choices"`
}

// ToolCall is the OpenAI-compatible tool call structure. It is exported
// so CascadeResult and PanelResult can carry structured tool calls to
// the chat handler (issue #72) without the handler re-parsing JSON.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// AssistantMessage bundles the assistant content and any tool calls
// extracted from an upstream response. Used by the cascade runner and
// the fusion panel so both code paths carry the same structured shape
// (issue #72).
type AssistantMessage struct {
	Content   string
	ToolCalls []ToolCall
}

// HasToolCalls reports whether the message carries at least one valid
// tool call. Convenience helper for branching at call sites that only
// care about the "should we emit tool-call SSE chunks?" decision.
func (m AssistantMessage) HasToolCalls() bool { return len(m.ToolCalls) > 0 }

// extractAssistantMessage parses the upstream body and returns the
// assistant message (content + tool_calls) and the model name. It
// triggers fallback (retry=true) on:
//   - JSON decode failure
//   - empty choices
//   - tool_calls with missing required fields
//   - tool_calls whose function.arguments isn't valid JSON (small models
//     hallucinate these — that's the whole point of the cascade).
//
// Empty content alongside valid tool_calls is accepted (issue #72): a
// coding agent's tool-call turn often has no textual content.
func extractAssistantMessage(body []byte) (AssistantMessage, string, error) {
	var raw assistantResponse
	if uErr := json.Unmarshal(body, &raw); uErr != nil {
		return AssistantMessage{}, "", newCascadeErr(true, "transport_error", "decode: %v", uErr)
	}
	if len(raw.Choices) == 0 {
		return AssistantMessage{}, "", newCascadeErr(true, "transport_error", "empty choices")
	}
	msg := raw.Choices[0].Message
	for i, tc := range msg.ToolCalls {
		if tc.ID == "" || tc.Type == "" || tc.Function.Name == "" {
			return AssistantMessage{}, "", newCascadeErr(true, "malformed_toolcall", "tool_call[%d] missing required fields", i)
		}
		if tc.Function.Arguments != "" {
			var probe json.RawMessage
			if pErr := json.Unmarshal([]byte(tc.Function.Arguments), &probe); pErr != nil {
				return AssistantMessage{}, "", newCascadeErr(true, "malformed_toolcall", "tool_call[%d].arguments not valid JSON: %v", i, pErr)
			}
		}
	}
	return AssistantMessage{Content: msg.Content, ToolCalls: msg.ToolCalls}, raw.Model, nil
}

// writeSSEResponse emits a single OpenAI-compatible chat-completion chunk
// containing the buffered assistant message, followed by the [DONE]
// sentinel. When the message carries tool calls (issue #72) the chunk's
// delta carries delta.tool_calls (with the per-call "index" field OpenAI
// streaming requires) and finish_reason is "tool_calls". Content-only
// responses use the legacy delta.content + finish_reason "stop" shape —
// byte-for-byte backward compatible. Headers are flushed if the writer
// supports it.
func writeSSEResponse(w http.ResponseWriter, stepName, servedModel string, msg AssistantMessage) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Nexus-Cascade-Served-By", stepName)
	w.WriteHeader(http.StatusOK)

	if servedModel == "" {
		servedModel = stepName
	}

	var delta map[string]interface{}
	var finishReason string
	if msg.HasToolCalls() {
		// OpenAI streaming tool-call deltas carry an "index" so the
		// client can assemble fragments across chunks. We emit the full
		// set in one chunk (the upstream was non-streaming), so each
		// entry is complete with id/type/function.
		tc := make([]map[string]interface{}, len(msg.ToolCalls))
		for i, call := range msg.ToolCalls {
			tc[i] = map[string]interface{}{
				"index": i,
				"id":    call.ID,
				"type":  call.Type,
				"function": map[string]string{
					"name":      call.Function.Name,
					"arguments": call.Function.Arguments,
				},
			}
		}
		delta = map[string]interface{}{"tool_calls": tc}
		// When the response is purely tool calls with no text content,
		// OpenAI clients expect content to be absent (not an empty
		// string). We only include content when non-empty so the delta
		// stays minimal and matches what real OpenAI streams emit.
		if msg.Content != "" {
			delta["content"] = msg.Content
		}
		finishReason = "tool_calls"
	} else {
		delta = map[string]interface{}{"content": msg.Content}
		finishReason = "stop"
	}

	chunk := map[string]interface{}{
		"id":      "chatcmpl-cascade-" + stepName,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   servedModel,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			},
		},
	}
	chunkBytes, err := json.Marshal(chunk)
	if err != nil {
		// Marshal failure before headers are committed — safe to return as-is.
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", chunkBytes); err != nil {
		// Body write after headers committed — return sentinel so the caller
		// closes the connection instead of calling http.Error (issue #241).
		return ErrSSEPartialWrite
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		// Body write after headers committed — sentinel (issue #241).
		return ErrSSEPartialWrite
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// CascadeConfig is the input to BuildLocalCascade. Defined here (rather
// than re-using config.Config) so upstream stays decoupled from config.
type CascadeConfig struct {
	LocalURL      string
	LocalModel    string
	FrontierURL   string
	FrontierModel string
	FrontierKey   string
	ZAIURL        string
	ZAIModel      string
	ZAIKey        string
	Timeout       time.Duration

	// SkipLocal removes the local Ollama step from the cascade.
	// The chat handler sets this when internal/health reports
	// Ollama is unreachable (issue #8): callers still get the
	// frontier (and z.ai) answer, just without paying the local
	// timeout. When true, the cascade starts at frontier; when no
	// frontier key is configured the cascade is empty and Run
	// returns an error.
	SkipLocal bool
}

// BuildLocalCascade returns a cascade whose primary is local Ollama and
// whose fallbacks are frontier and/or Z.ai endpoints that are configured
// (their key is non-empty). Order is the issue's required declaration
// order: [local, frontier, zai]. When CascadeConfig.SkipLocal is true
// the local step is omitted (issue #8 graceful-degradation path).
// When no fallback key is set the cascade has a single step — Run
// still validates the response before streaming.
func BuildLocalCascade(cfg CascadeConfig) *Cascade {
	steps := []CascadeStep{}
	if !cfg.SkipLocal {
		steps = append(steps, CascadeStep{
			Name:   "local",
			URL:    strings.TrimRight(cfg.LocalURL, "/") + "/v1/chat/completions",
			Model:  cfg.LocalModel,
			APIKey: "",
		})
	}
	if cfg.FrontierKey != "" {
		steps = append(steps, CascadeStep{
			Name:   "frontier",
			URL:    cfg.FrontierURL,
			APIKey: cfg.FrontierKey,
			Model:  cfg.FrontierModel,
		})
	}
	if cfg.ZAIKey != "" {
		steps = append(steps, CascadeStep{
			Name:   "zai",
			URL:    cfg.ZAIURL,
			APIKey: cfg.ZAIKey,
			Model:  cfg.ZAIModel,
		})
	}
	return &Cascade{Steps: steps, Timeout: cfg.Timeout}
}

// truncateForLog clamps a response body for log/error messages. Bodies
// from upstream providers can include full chat dumps; 200 bytes is enough
// to identify the failure mode without spamming logs.
func truncateForLog(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}
