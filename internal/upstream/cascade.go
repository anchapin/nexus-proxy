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
}

// cascadeDefaultTimeout is the per-attempt timeout used when Cascade.Timeout
// is <= 0. Mirrors the issue default ("configurable, default 30s").
const cascadeDefaultTimeout = 30 * time.Second

// cascadeErr tags a per-step failure so the runner knows whether to fall
// back (retry=true) or surface the error immediately (retry=false — e.g.
// upstream returned 401, retrying won't help).
type cascadeErr struct {
	retry bool
	msg   string
}

func (e *cascadeErr) Error() string { return e.msg }

func newCascadeErr(retry bool, format string, args ...interface{}) error {
	return &cascadeErr{retry: retry, msg: fmt.Sprintf(format, args...)}
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
		content, servedModel, err := fetchCascadeStep(ctx, client, step, payload)
		cancel()
		if err == nil {
			slog.Info("cascade served",
				slog.String("step", step.Name),
				slog.Int("attempt", i+1),
				slog.Int("total", len(c.Steps)),
			)
			res.Succeeded = true
			res.ServedBy = step.Name
			if werr := writeSSEResponse(w, step.Name, servedModel, content); werr != nil {
				return res, werr
			}
			return res, nil
		}
		lastErr = err
		retry := classifyFailure(err)
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

func joinStepNames(steps []CascadeStep) string {
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.Name
	}
	return strings.Join(names, "->")
}

// fetchCascadeStep does a single non-streaming POST to step.URL, validates
// the response, and returns the assistant content + the model name echoed
// back by the upstream (used in the SSE response). All returned errors are
// tagged via newCascadeErr so the runner knows whether to fall back.
func fetchCascadeStep(ctx context.Context, client Client, step CascadeStep, payload map[string]interface{}) (content, servedModel string, err error) {
	body := make(map[string]interface{}, len(payload)+2)
	for k, v := range payload {
		body[k] = v
	}
	body["model"] = step.Model
	body["stream"] = false

	jsonPayload, mErr := json.Marshal(body)
	if mErr != nil {
		return "", "", newCascadeErr(false, "marshal: %v", mErr)
	}
	req, rErr := http.NewRequestWithContext(ctx, http.MethodPost, step.URL, bytes.NewReader(jsonPayload))
	if rErr != nil {
		return "", "", newCascadeErr(false, "build request: %v", rErr)
	}
	req.Header.Set("Content-Type", "application/json")
	if step.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+step.APIKey)
	}

	resp, dErr := client.Do(req)
	if dErr != nil {
		// Transport error / ctx timeout — always retry.
		return "", "", newCascadeErr(true, "transport: %v", dErr)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if ShouldRetry(resp.StatusCode, nil) {
		return "", "", newCascadeErr(true, "status %d: %s", resp.StatusCode, truncateForLog(respBody, 200))
	}
	if resp.StatusCode != http.StatusOK {
		// Non-retryable 4xx (auth, bad request, etc.). Surface to caller.
		return "", "", newCascadeErr(false, "status %d: %s", resp.StatusCode, truncateForLog(respBody, 200))
	}

	content, model, vErr := extractAssistantContent(respBody)
	if vErr != nil {
		return "", "", vErr
	}
	return content, model, nil
}

// assistantResponse is the slice of the OpenAI-compatible chat-completion
// schema we need to validate before streaming to the harness.
type assistantResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []toolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
	} `json:"choices"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// extractAssistantContent parses the upstream body and returns the assistant
// text + model name. It triggers fallback (retry=true) on:
//   - JSON decode failure
//   - empty choices
//   - tool_calls with missing required fields
//   - tool_calls whose function.arguments isn't valid JSON (small models
//     hallucinate these — that's the whole point of the cascade).
func extractAssistantContent(body []byte) (content, model string, err error) {
	var raw assistantResponse
	if uErr := json.Unmarshal(body, &raw); uErr != nil {
		return "", "", newCascadeErr(true, "decode: %v", uErr)
	}
	if len(raw.Choices) == 0 {
		return "", "", newCascadeErr(true, "empty choices")
	}
	msg := raw.Choices[0].Message
	for i, tc := range msg.ToolCalls {
		if tc.ID == "" || tc.Type == "" || tc.Function.Name == "" {
			return "", "", newCascadeErr(true, "tool_call[%d] missing required fields", i)
		}
		if tc.Function.Arguments != "" {
			var probe json.RawMessage
			if pErr := json.Unmarshal([]byte(tc.Function.Arguments), &probe); pErr != nil {
				return "", "", newCascadeErr(true, "tool_call[%d].arguments not valid JSON: %v", i, pErr)
			}
		}
	}
	return msg.Content, raw.Model, nil
}

// writeSSEResponse emits a single OpenAI-compatible chat-completion chunk
// containing the buffered content, followed by the [DONE] sentinel. Headers
// are flushed if the writer supports it.
func writeSSEResponse(w http.ResponseWriter, stepName, servedModel, content string) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Nexus-Cascade-Served-By", stepName)
	w.WriteHeader(http.StatusOK)

	if servedModel == "" {
		servedModel = stepName
	}
	chunk := map[string]interface{}{
		"id":      "chatcmpl-cascade-" + stepName,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   servedModel,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]string{"content": content},
				"finish_reason": "stop",
			},
		},
	}
	chunkBytes, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", chunkBytes); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
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
