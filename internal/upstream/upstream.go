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
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/anchapin/nexus-proxy/internal/tracing"
)

// ErrClientAbort is returned by streamPanelResultAsSSE when the client
// disconnects mid-stream (EPIPE, ECONNRESET). Callers must treat it as
// a non-error client-abort condition: log at info level and return early
// without continuing to the arbiter.
var ErrClientAbort = errors.New("client abort")

// panelPanicsTotal counts recovered panics in panel goroutines (issue #309).
// Exposed via PanelPanicsTotal for the /metrics endpoint.
var panelPanicsTotal atomic.Uint64

// IncPanelPanics increments the panel panic counter. Called from panel
// goroutines when they catch and recover from a panic.
func IncPanelPanics() { panelPanicsTotal.Add(1) }

// PanelPanicsTotal returns the cumulative panel panic count.
func PanelPanicsTotal() uint64 { return panelPanicsTotal.Load() }

// IsClientAbort reports whether err is a client-side connection error
// (EPIPE, ECONNRESET, broken pipe) that indicates the client disconnected
// mid-response. These are logged at info level rather than error level.
func IsClientAbort(err error) bool {
	if err == nil {
		return false
	}
	// errors.Is walks the chain of wrapped errors.
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, ErrClientAbort)
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
// Header names are canonicalised by net/http before they reach the map,
// so we compare against the canonical form. X-Nexus-* is matched by
// prefix so future instrumentation headers pass through untouched.
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

// Client is the minimal interface used by the stream and fusion helpers. The
// default http.Client satisfies it; tests can pass a stub.
type Client interface {
	Do(req *http.Request) (*http.Response, error)
}

// bufferResponseWriter wraps a bytes.Buffer to implement http.ResponseWriter
// for capturing buffered responses without doing any actual HTTP writing.
type bufferResponseWriter struct {
	buf *bytes.Buffer
}

func (b *bufferResponseWriter) Header() http.Header { return make(http.Header) }
func (b *bufferResponseWriter) Write(d []byte) (int, error) {
	return b.buf.Write(d)
}
func (b *bufferResponseWriter) WriteHeader(int) {}
func (b *bufferResponseWriter) Flush()          {}

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

// ErrUpstreamTruncated is returned by StreamWithContext when the
// upstream connection dropped mid-stream after at least one chunk was
// forwarded but before the upstream emitted its own `data: [DONE]`
// sentinel (issue #118). The caller MUST NOT treat this as a hard
// failure requiring a 502: the HTTP response is already committed
// (200 + flushed SSE frames) and StreamWithContext has already emitted
// a synthetic truncation event + `data: [DONE]` so the downstream
// client does not hang. The sentinel exists solely so the handler can
// record the truncation via its observability hook.
var ErrUpstreamTruncated = errors.New("upstream: stream truncated")

// sseDoneMarker is the OpenAI SSE stream terminator, recognised as a
// standalone frame so a [DONE] embedded inside a JSON content chunk
// never falsely marks the stream complete.
const sseDoneMarker = "data: [DONE]"

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
	// Propagate W3C trace context for distributed correlation (issue #299).
	if tp := tracing.TraceparentFromContext(ctx); tp != "" {
		req.Header.Set("traceparent", tp)
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
	var wroteAny bool
	var seenDone bool
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return werr
			}
			flusher.Flush()
			wroteAny = true
			if !seenDone && isSSEDoneLine(line) {
				seenDone = true
			}
		}
		if err != nil {
			// The upstream already emitted its own [DONE] sentinel,
			// so the SSE stream completed normally — any trailing
			// read error is irrelevant (the harness already saw the
			// terminator). A clean io.EOF is a graceful close: the
			// upstream finished its body (possibly without a [DONE]
			// sentinel, as Ollama does on local routes) and the
			// harness sees a normal end-of-stream. Both cases return
			// nil so the happy-path stream stays byte-for-byte
			// identical (issue #118 acceptance: a complete,
			// [DONE]-less upstream must NOT gain a synthetic
			// terminator).
			if seenDone || errors.Is(err, io.EOF) {
				return nil
			}
			// Any other read error after we forwarded at least one
			// chunk — io.ErrUnexpectedEOF from a truncated chunked
			// body (the `kill -9 ollama` reproduction), a connection
			// reset, a read timeout — is a mid-stream TCP drop. The
			// downstream SSE parser would hang waiting for the
			// [DONE] sentinel that never arrives, so emit a synthetic
			// truncation event + [DONE] and return a sentinel the
			// handler can record (issue #118).
			if wroteAny {
				return emitTruncationTerminator(w, flusher)
			}
			return err
		}
	}
}

// isSSEDoneLine reports whether line is the OpenAI SSE terminator
// `data: [DONE]` (with any trailing newline / carriage return).
// Matching on the trimmed token means a partial buffer without the
// newline still classifies correctly, and an upstream that frames
// with `\r\n` does not cause a miss. A [DONE] embedded inside a JSON
// content chunk never matches because content frames carry the JSON
// payload after `data: `, not the bare token.
func isSSEDoneLine(line []byte) bool {
	return strings.TrimSpace(string(line)) == sseDoneMarker
}

// emitTruncationTerminator writes the SSE signal the downstream
// harness needs when the upstream dropped mid-stream (issue #118).
// The response is already committed (200 + flushed SSE frames), so
// the status code cannot be changed at this point; the authoritative
// client-facing signal is the in-band error event followed by the
// `data: [DONE]` sentinel the harness's SSE parser is waiting on.
//
// X-Nexus-Truncated is set best-effort on the header map: it reaches
// the client only in the rare case where the drop is detected before
// the first body flush; once SSE frames are on the wire the header no
// longer travels, but the value is still observable through /status,
// debug traces, and httptest recorders (and through the handler,
// which also stamps it on detection). The in-band truncation event is
// the reliable client signal regardless.
//
// Returns ErrUpstreamTruncated so the caller records the truncation
// without treating it as a hard 502.
func emitTruncationTerminator(w http.ResponseWriter, flusher http.Flusher) error {
	w.Header().Set("X-Nexus-Truncated", "true")
	if _, err := io.WriteString(w, "data: {\"error\":{\"message\":\"upstream stream truncated\",\"type\":\"upstream_truncated\"}}\n\n"); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return ErrUpstreamTruncated
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
	// Propagate W3C trace context for distributed correlation (issue #299).
	if tp := tracing.TraceparentFromContext(ctx); tp != "" {
		req.Header.Set("traceparent", tp)
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

	// Forward only allowlisted upstream headers (issue #39), then force
	// Content-Type to application/json so the harness sees a plain JSON
	// envelope regardless of what the upstream declared.
	copyAllowedHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, werr := w.Write(respBody)
	return werr
}

// FetchPanel fetches a single non-streaming completion from targetURL and
// returns the assistant message (content + tool_calls). Designed for the
// fusion panel where we need the full response before asking the arbiter
// to synthesize. Tool calls are preserved (issue #72) so the progressive
// streaming path can emit them as delta.tool_calls when the panel member
// is the speculative winner.
func FetchPanel(ctx context.Context, client Client, targetURL, apiKey, modelName string, body map[string]interface{}) (AssistantMessage, error) {
	payload := make(map[string]interface{}, len(body)+2)
	for k, v := range body {
		payload[k] = v
	}
	payload["model"] = modelName
	payload["stream"] = false

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return AssistantMessage{}, fmt.Errorf("fusion: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(jsonPayload))
	if err != nil {
		return AssistantMessage{}, fmt.Errorf("fusion: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	// Propagate W3C trace context for distributed correlation (issue #299).
	if tp := tracing.TraceparentFromContext(ctx); tp != "" {
		req.Header.Set("traceparent", tp)
	}

	resp, err := client.Do(req)
	if err != nil {
		return AssistantMessage{}, fmt.Errorf("fusion: do: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return AssistantMessage{}, fmt.Errorf("fusion: %s status %d: %s", modelName, resp.StatusCode, respBody)
	}

	var raw struct {
		Choices []struct {
			Message struct {
				Content   string     `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls,omitempty"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return AssistantMessage{}, fmt.Errorf("fusion: decode: %w", err)
	}
	if len(raw.Choices) == 0 {
		return AssistantMessage{}, fmt.Errorf("fusion: %s returned empty choice", modelName)
	}
	return AssistantMessage{
		Content:   raw.Choices[0].Message.Content,
		ToolCalls: raw.Choices[0].Message.ToolCalls,
	}, nil
}

// PanelResult is one member's contribution to a fusion response. Members
// that errored are returned with Err set and Content empty; callers should
// surface that to the arbiter so it can choose to ignore or down-weight.
//
// ToolCalls carries any OpenAI-compatible tool_calls the member returned
// (issue #72). When the member is streamed speculatively as the winner,
// streamPanelResultAsSSE emits these as delta.tool_calls. The arbiter
// synthesis path is text-only — tool calls from a disagreeing member are
// not merged into the arbiter output. This is intentional: tool-call
// arbitration (picking the "better" set of tool calls from two members)
// is a separate concern left for a future change.
type PanelResult struct {
	Source    string // "local" or "frontier"
	Content   string
	ToolCalls []ToolCall
	Err       error
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
//
// arbiterCache/arbiterCacheTTL implement optional caching of arbiter
// synthesis responses (issue #232). When arbiterCache is non-nil and
// arbiterCacheTTL > 0, synthesis responses are cached keyed by a hash
// of (first.Content, second.Content). Cache hits serve the cached text
// without calling the arbiter. When nil or TTL=0 the cache is bypassed
// and the arbiter is called on every disagreement. Returns true if the
// response was served from cache (so the caller can record metrics).
//
// The ctx parameter is the request context (typically r.Context() from the
// HTTP handler). When the client disconnects, ctx is cancelled and the
// in-flight upstream calls are cancelled within 1 second rather than
// waiting for their individual timeouts (issue #297).
func Panel(
	ctx context.Context,
	w http.ResponseWriter,
	client Client,
	localBaseURL, localModel, frontierURL, frontierModel string,
	arbiterURL, arbiterKey, arbiterModel string,
	body map[string]interface{},
	latestPrompt string,
	perFetchTimeout time.Duration,
	arbiterTimeout time.Duration,
	skipLocal bool,
	arbiterCache *ArbiterCache,
	arbiterCacheTTL time.Duration,
) (cacheHit bool, _ error) {
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
			defer func() {
				if r := recover(); r != nil {
					IncPanelPanics()
					results <- PanelResult{Source: "local", Err: fmt.Errorf("panic: %v", r)}
				}
			}()
			ctx, cancel := context.WithTimeout(ctx, withDefault(perFetchTimeout))
			defer cancel()
			msg, err := FetchPanel(ctx, client,
				localBaseURL+"/v1/chat/completions", "", localModel, body)
			results <- PanelResult{Source: "local", Content: msg.Content, ToolCalls: msg.ToolCalls, Err: err}
		}()
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				IncPanelPanics()
				results <- PanelResult{Source: "frontier", Err: fmt.Errorf("panic: %v", r)}
			}
		}()
		ctx, cancel := context.WithTimeout(ctx, withDefault(perFetchTimeout))
		defer cancel()
		msg, err := FetchPanel(ctx, client,
			frontierURL, "", frontierModel, body)
		results <- PanelResult{Source: "frontier", Content: msg.Content, ToolCalls: msg.ToolCalls, Err: err}
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
	arbiterCtx, cancelArbiter := context.WithTimeout(ctx, withDefaultArbiterTimeout(arbiterTimeout))
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

	// Issue #232: check arbiter cache before calling the arbiter endpoint.
	// Cache is only populated for stream=false (BufferedFetch) since stream=true
	// (StreamWithContext) passes SSE through directly and doesn't give us
	// content to cache.
	if arbiterCache != nil && arbiterCacheTTL > 0 {
		if cached, ok := arbiterCache.Get(r1.Content, r2.Content); ok {
			slog.Info("fusion arbiter cache hit",
				slog.String("r1_source", r1.Source),
				slog.String("r2_source", r2.Source),
			)
			cacheHit = true
			if stream {
				return true, streamCachedArbiterSynthesis(w, cached)
			}
			return true, writeCachedArbiterJSON(w, cached, arbiterModel)
		}
	}

	// When stream=true, use the original StreamWithContext to pass SSE through
	// directly (no caching possible, but preserves passthrough behavior).
	// When stream=false, use BufferedFetchWithContext with stream=false to get
	// JSON, then format the response (can cache for future requests).
	var synthesis string
	var fetchErr error
	if stream {
		// stream=true: SSE passthrough, no caching
		fetchErr = StreamWithContext(arbiterCtx, w, client, arbiterURL, arbiterKey, synthBody)
		if fetchErr != nil {
			return false, fmt.Errorf("fusion: arbiter stream: %w", fetchErr)
		}
		if err := writeSSEDone(w); err != nil {
			return false, err
		}
		return false, nil
	}
	// stream=false: buffered fetch, can cache
	{
		bufBody := synthBody
		bufBody["stream"] = false
		var buf bytes.Buffer
		bufW := &bufferResponseWriter{buf: &buf}
		fetchErr = BufferedFetchWithContext(arbiterCtx, bufW, client, arbiterURL, arbiterKey, bufBody)
		if fetchErr == nil {
			var raw struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(buf.Bytes(), &raw); err == nil {
				if len(raw.Choices) > 0 {
					synthesis = raw.Choices[0].Message.Content
				}
			}
		}
	}
	if fetchErr != nil {
		return false, fmt.Errorf("fusion: arbiter fetch: %w", fetchErr)
	}

	// Cache the synthesis for future identical panel members (issue #232).
	if arbiterCache != nil && arbiterCacheTTL > 0 && synthesis != "" {
		arbiterCache.Set(r1.Content, r2.Content, synthesis, arbiterCacheTTL)
	}

	return false, writeCachedArbiterJSON(w, synthesis, arbiterModel)
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

// PanelOutcome describes the runtime path PanelStreaming took. The chat
// handler reads it to record telemetry (issue #48 acceptance: the
// dashboard must be able to report what fraction of fusion requests
// achieved agreement and skipped the arbiter).
type PanelOutcome struct {
	// ArbiterSkipped is true when the two panel members agreed
	// (SimilarityRatio >= agreementThreshold) OR when only one
	// member returned content (the other errored, or skipLocal
	// ran the frontier-only path). In both cases the speculative
	// answer was streamed to the user without invoking the arbiter.
	ArbiterSkipped bool
	// Source identifies the panel member that streamed first:
	// "local", "frontier", or "" when no member returned content.
	// "frontier" in the skipLocal degraded path (issue #8).
	Source string
	// Similarity is the Jaccard ratio between the two panel
	// members' contents. 0 when fewer than two members returned
	// content.
	Similarity float64
	// ArbiterCacheHit is true when the arbiter synthesis was served
	// from the cache (issue #232) without calling the arbiter.
	// When false and the arbiter was invoked, the synthesis was
	// fetched from the arbiter and cached for future requests.
	ArbiterCacheHit bool
}

// PanelStreaming runs the fusion panel with progressive delivery
// (issue #48). It launches both panel members in parallel — identical
// to Panel — but the first member to return is streamed to the user
// immediately as a speculative OpenAI-compatible SSE chunk tagged with
// the source name in the chunk metadata. The second member then
// arrives:
//
//   - Agreement (SimilarityRatio >= agreementThreshold): the response
//     terminates with `data: [DONE]\n\n`. The arbiter is NOT invoked.
//     The user sees the faster member's output and the proxy pays no
//     arbiter cost.
//   - Disagreement: the arbiter runs as today and its synthesis is
//     streamed as ADDITIONAL SSE chunks after the speculative one,
//     then `data: [DONE]\n\n`. This is the "append" disagreement mode
//     documented in the issue; "replace" / "diff" modes are out of
//     scope for this change and would require a separate spec.
//   - One member errored (or skipLocal is true): the successful
//     member's content is streamed and the response terminates. No
//     arbiter is invoked — the speculative answer IS the answer.
//
// For non-streaming harness requests (body["stream"] == false) the
// call is delegated to Panel so the existing single
// chatCompletionResponse JSON-object shape is preserved (issue #10).
//
// agreementThreshold is in [0,1]; values < 0 are clamped to 0 (every
// disagreement runs the arbiter) and values > 1 are clamped to 1
// (the arbiter is always skipped when both members succeed).
//
// arbiterCache/arbiterCacheTTL implement optional caching of arbiter
// synthesis responses (issue #232). When arbiterCache is non-nil and
// arbiterCacheTTL > 0, synthesis responses are cached keyed by a hash
// of (first.Content, second.Content). Cache hits stream the cached
// text without calling the arbiter. When nil or TTL=0 the cache is
// bypassed and the arbiter is called on every disagreement.
//
// The ctx parameter is the request context (typically r.Context() from the
// HTTP handler). When the client disconnects, ctx is cancelled and the
// in-flight upstream calls are cancelled within 1 second rather than
// waiting for their individual timeouts (issue #297).
func PanelStreaming(
	ctx context.Context,
	w http.ResponseWriter,
	client Client,
	localBaseURL, localModel, frontierURL, frontierModel string,
	arbiterURL, arbiterKey, arbiterModel string,
	body map[string]interface{},
	latestPrompt string,
	perFetchTimeout time.Duration,
	arbiterTimeout time.Duration,
	skipLocal bool,
	agreementThreshold float64,
	requestID string,
	arbiterCache *ArbiterCache,
	arbiterCacheTTL time.Duration,
) (PanelOutcome, error) {
	var outcome PanelOutcome

	// Non-streaming fallback. The handler should already have routed
	// stream=false to Panel directly, but we double-check here so the
	// contract is enforced at the function boundary: a caller that
	// hands PanelStreaming a stream=false body gets the existing
	// JSON-object response shape (issue #10).
	if s, ok := body["stream"].(bool); ok && !s {
		cacheHit, err := Panel(ctx, w, client,
			localBaseURL, localModel, frontierURL, frontierModel,
			arbiterURL, arbiterKey, arbiterModel,
			body, latestPrompt, perFetchTimeout, arbiterTimeout,
			skipLocal, arbiterCache, arbiterCacheTTL)
		if err != nil {
			return outcome, err
		}
		outcome.ArbiterCacheHit = cacheHit
		return outcome, nil
	}

	// Clamp the threshold into [0, 1] so a misconfigured operator
	// can't accidentally disable the agreement-skip path entirely
	// (negative) or always skip it regardless of similarity (>1).
	if agreementThreshold < 0 {
		agreementThreshold = 0
	} else if agreementThreshold > 1 {
		agreementThreshold = 1
	}

	// SSE response headers must be set before the first Write. We
	// commit them now so the speculative chunk goes out with the
	// correct Content-Type regardless of which member wins.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Nexus-Fusion-Progressive", "true")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	results := make(chan PanelResult, 2)
	var cancelLocal, cancelFrontier context.CancelFunc
	if skipLocal {
		// Issue #8: synthetic local failure so the arbiter-style
		// code paths below degrade cleanly. The handler sets
		// X-Nexus-Degraded=true; we don't duplicate the header here.
		results <- PanelResult{
			Source: "local",
			Err:    errors.New("ollama unavailable (degraded)"),
		}
	} else {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					IncPanelPanics()
					results <- PanelResult{Source: "local", Err: fmt.Errorf("panic: %v", r)}
				}
			}()
			ctxLocal, cancel := context.WithTimeout(ctx, withDefault(perFetchTimeout))
			cancelLocal = cancel
			defer cancel()
			msg, err := FetchPanel(ctxLocal, client,
				localBaseURL+"/v1/chat/completions", "", localModel, body)
			results <- PanelResult{Source: "local", Content: msg.Content, ToolCalls: msg.ToolCalls, Err: err}
		}()
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				IncPanelPanics()
				results <- PanelResult{Source: "frontier", Err: fmt.Errorf("panic: %v", r)}
			}
		}()
		ctxFrontier, cancel := context.WithTimeout(ctx, withDefault(perFetchTimeout))
		cancelFrontier = cancel
		defer cancel()
		msg, err := FetchPanel(ctxFrontier, client,
			frontierURL, "", frontierModel, body)
		results <- PanelResult{Source: "frontier", Content: msg.Content, ToolCalls: msg.ToolCalls, Err: err}
	}()
	first := <-results
	second := <-results

	// Both members errored — there's nothing speculative to deliver.
	// Surface the upstream errors so the handler renders a 502 with
	// context. The legacy Panel path tolerates one failed member by
	// passing the error through to the arbiter; in progressive mode
	// the only sensible fallback is to fail the request.
	if first.Err != nil && second.Err != nil {
		return outcome, fmt.Errorf("fusion: both members failed: local=%w; frontier=%w",
			first.Err, second.Err)
	}

	// Pick the member that produced content. If one errored the other
	// wins outright; if both succeeded, prefer the one carrying
	// tool_calls (issue #72): tool calls are the primary deliverable
	// for coding agents and the arbiter cannot synthesize them, so a
	// member that returned structured tool calls should be the
	// speculative winner even if it arrived second.
	winner := first
	winnerFromSecond := false
	if first.Err != nil {
		winner = second
		winnerFromSecond = true
	} else if len(second.ToolCalls) > 0 && len(first.ToolCalls) == 0 {
		winner = second
		winnerFromSecond = true
	}
	outcome.Source = winner.Source

	if err := streamPanelResultAsSSE(w, winner); err != nil {
		if errors.Is(err, ErrClientAbort) {
			// Client disconnected mid-stream. The speculative chunk
			// never made it; do not invoke the arbiter — there is
			// nobody to receive its output. Log at info level so
			// operators can distinguish "client dropped" from "slow
			// arbiter" in the access log (issue #167).
			slog.Info("fusion speculative write: client aborted",
				slog.String("source", outcome.Source),
			)
			return outcome, nil
		}
		return outcome, fmt.Errorf("fusion: stream speculative: %w", err)
	}

	// Tool-call responses bypass the arbiter (issue #72). The arbiter
	// synthesizes text — it cannot merge or choose between two sets of
	// structured tool calls. When the speculative winner carries tool
	// calls we terminate the response immediately after streaming them.
	// This is the documented "route tool-call requests away from fusion
	// arbitration" path: the request still goes through fusion (both
	// panel members ran), but the arbitration step is skipped. A future
	// change may add tool-call-aware arbitration.
	if len(winner.ToolCalls) > 0 {
		outcome.ArbiterSkipped = true
		slog.Info("fusion tool-call winner, arbiter skipped",
			slog.String("request_id", requestID),
			slog.String("source", outcome.Source),
			slog.Int("tool_calls", len(winner.ToolCalls)),
		)
		// Cancel the slow member's goroutine to stop in-flight work.
		// The slow member's result was already consumed in the main
		// flow (second := <-results), so no drain is needed.
		if winnerFromSecond {
			// winner is second; first (local) is slow
			if cancelLocal != nil {
				cancelLocal()
			}
		} else {
			// winner is first; second (frontier) is slow
			if cancelFrontier != nil {
				cancelFrontier()
			}
		}
		if err := writeSSEDone(w); err != nil {
			return outcome, err
		}
		return outcome, nil
	}

	// One-member case (the other errored, or skipLocal ran frontier
	// alone). The speculative answer IS the answer; no arbiter.
	// The slow member's result was already consumed in the main flow.
	if first.Err != nil || second.Err != nil {
		outcome.ArbiterSkipped = true
		// Cancel the slow member's goroutine to stop in-flight work.
		if winnerFromSecond {
			// winner is second; first (local) is slow
			if cancelLocal != nil {
				cancelLocal()
			}
		} else {
			// winner is first; second (frontier) is slow
			if cancelFrontier != nil {
				cancelFrontier()
			}
		}
		if err := writeSSEDone(w); err != nil {
			return outcome, err
		}
		return outcome, nil
	}

	// Both members succeeded: compare and decide on the arbiter.
	outcome.Similarity = SimilarityRatio(first.Content, second.Content)
	if outcome.Similarity >= agreementThreshold {
		outcome.ArbiterSkipped = true
		slog.Info("fusion agreement, arbiter skipped",
			slog.String("request_id", requestID),
			slog.String("source", outcome.Source),
			slog.Float64("similarity", outcome.Similarity),
			slog.Float64("threshold", agreementThreshold),
		)
		// Cancel the slow member's goroutine to stop in-flight work.
		// Both results are already consumed from the channel (lines 499-500),
		// so no drain is needed; cancelling aborts the slow HTTP request.
		if winnerFromSecond {
			// winner is second; first (local) is slow
			if cancelLocal != nil {
				cancelLocal()
			}
		} else {
			// winner is first; second (frontier) is slow
			if cancelFrontier != nil {
				cancelFrontier()
			}
		}
		if err := writeSSEDone(w); err != nil {
			return outcome, err
		}
		return outcome, nil
	}

	// Disagreement — run the arbiter and stream its synthesis as
	// additional SSE chunks after the speculative one. This is the
	// "append" mode documented in the issue; OpenAI-compatible
	// clients concatenate delta.content across chunks, so the
	// harness sees `speculative_text + arbiter_text`. The arbiter
	// text is the authoritative final answer in the operator's
	// mental model.
	slog.Info("fusion disagreement, invoking arbiter",
		slog.String("request_id", requestID),
		slog.String("first_source", outcome.Source),
		slog.Float64("similarity", outcome.Similarity),
		slog.Float64("threshold", agreementThreshold),
	)
	synth := SynthesisPrompt(latestPrompt, first, second)
	synthBody := map[string]interface{}{
		"model": arbiterModel,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a master synthesis AI. Deliver only the final synthesized response. Do not mention that you are an arbiter."},
			{"role": "user", "content": synth},
		},
		"stream": true,
	}

	// Issue #232: check arbiter cache before calling the arbiter endpoint.
	// Cache hit: stream the cached synthesis as SSE.
	// Cache miss: use StreamWithContext for SSE passthrough (original behavior)
	// since progressive delivery prioritizes low latency over caching gains.
	if arbiterCache != nil && arbiterCacheTTL > 0 {
		if cached, ok := arbiterCache.Get(first.Content, second.Content); ok {
			slog.Info("fusion arbiter cache hit (streaming)",
				slog.String("first_source", first.Source),
				slog.String("second_source", second.Source),
			)
			outcome.ArbiterCacheHit = true
			if err := streamCachedArbiterSynthesis(w, cached); err != nil {
				return outcome, err
			}
			return outcome, nil
		}
	}

	arbiterCtx, cancelArbiter := context.WithTimeout(context.Background(), withDefaultArbiterTimeout(arbiterTimeout))
	defer cancelArbiter()

	// Use StreamWithContext for SSE passthrough. This preserves the original
	// behavior where the arbiter's SSE is passed through directly. Caching
	// synthesis content is only useful for the non-streaming Panel path.
	if err := StreamWithContext(arbiterCtx, w, client, arbiterURL, arbiterKey, synthBody); err != nil {
		return outcome, fmt.Errorf("fusion: arbiter stream: %w", err)
	}
	// Cancel the slow member's goroutine — its result was already consumed
	// (second := <-results) but the goroutine may still be trying to send
	// on the full channel, which would deadlock if we didn't cancel.
	if winnerFromSecond {
		// winner is second; first (local) is slow
		if cancelLocal != nil {
			cancelLocal()
		}
	} else {
		// winner is first; second (frontier) is slow
		if cancelFrontier != nil {
			cancelFrontier()
		}
	}
	if err := writeSSEDone(w); err != nil {
		return outcome, err
	}
	return outcome, nil
}

// streamPanelResultAsSSE writes a single OpenAI-compatible SSE chunk
// that carries the panel member's content as a delta. The chunk
// embeds the source ("local" / "frontier") in a "nexus" metadata
// field so the harness / log scraper can identify which model was
// streamed speculatively. Err-flagged results are silently skipped —
// the caller is responsible for picking a winner before invoking.
//
// When the winner carries tool_calls (issue #72), the delta emits
// delta.tool_calls (with per-call index) instead of delta.content,
// and finish_reason is "tool_calls". The arbiter synthesis path is
// text-only; if the speculative winner had tool calls the response
// terminates after the speculative chunk — there is no text to
// append from an arbiter.
//
// Headers must already be committed (WriteHeader called) when this
// runs, so the chunk lands with the response Content-Type the caller
// set.
func streamPanelResultAsSSE(w http.ResponseWriter, r PanelResult) error {
	if r.Err != nil {
		return nil
	}
	var delta map[string]interface{}
	var finishReason string
	if len(r.ToolCalls) > 0 {
		tc := make([]map[string]interface{}, len(r.ToolCalls))
		for i, call := range r.ToolCalls {
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
		if r.Content != "" {
			delta["content"] = r.Content
		}
		finishReason = "tool_calls"
	} else {
		delta = map[string]interface{}{"content": r.Content}
		finishReason = "stop"
	}
	chunk := map[string]interface{}{
		"object": "chat.completion.chunk",
		"nexus":  map[string]string{"source": r.Source},
		"choices": []map[string]interface{}{
			{"index": 0, "delta": delta, "finish_reason": finishReason},
		},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("fusion: marshal speculative chunk: %w", err)
	}
	if _, err := w.Write([]byte("data: ")); err != nil {
		if IsClientAbort(err) {
			return ErrClientAbort
		}
		return err
	}
	if _, err := w.Write(b); err != nil {
		if IsClientAbort(err) {
			return ErrClientAbort
		}
		return err
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		if IsClientAbort(err) {
			return ErrClientAbort
		}
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// streamCachedArbiterSynthesis streams a cached arbiter synthesis text as
// SSE chunks (issue #232). This mimics the output of StreamWithContext
// for the arbiter, but serves from cache instead. The synthesis is
// streamed as a single delta chunk followed by [DONE]. Headers must
// already be committed (WriteHeader called) when this runs.
func streamCachedArbiterSynthesis(w http.ResponseWriter, synthesis string) error {
	chunk := map[string]interface{}{
		"object": "chat.completion.chunk",
		"nexus":  map[string]string{"source": "arbiter-cached"},
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]interface{}{"content": synthesis}, "finish_reason": "stop"},
		},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("fusion: marshal cached arbiter chunk: %w", err)
	}
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return writeSSEDone(w)
}

// writeCachedArbiterJSON writes a cached arbiter synthesis as a
// non-streaming JSON chat completion response (issue #232). This mimics
// the output of BufferedFetchWithContext for the arbiter, but serves
// from cache instead. The model name is passed as modelName so the
// response has the correct model field.
func writeCachedArbiterJSON(w http.ResponseWriter, synthesis, modelName string) error {
	resp := map[string]interface{}{
		"object": "chat.completion",
		"model":  modelName,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]string{
					"role":    "assistant",
					"content": synthesis,
				},
				"finish_reason": "stop",
			},
		},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("fusion: marshal cached arbiter JSON: %w", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, werr := w.Write(b)
	return werr
}

// writeSSEDone emits the OpenAI streaming terminator and flushes. SSE
// clients (and the harness's chat-completion consumers) treat
// `data: [DONE]\n\n` as "no more chunks"; the proxy must write it
// after every successful progressive-fusion response — agreement
// (no arbiter), one-member (no arbiter), or disagreement (arbiter
// stream completed).
func writeSSEDone(w http.ResponseWriter) error {
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}
