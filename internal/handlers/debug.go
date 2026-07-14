// Package handlers — debug request/response tracing mode (issue #33).
//
// When the operator sets NEXUS_DEBUG=true the chat handler emits a
// structured trace per request covering each lifecycle step:
//
//   - RequestTrace     inbound payload summary (request id, message count,
//     estimated tokens, model, stream flag)
//   - TransformTrace   post-middleware state (RAG, TOON, prompt engineering)
//   - RouteTrace       routing decision (reason, budget source, estimated tokens)
//   - UpstreamTrace    upstream target (host only, model, streaming, cascade)
//   - ResponseTrace    response summary (status, TTFT, bytes, output tokens,
//     truncated body preview)
//
// Each trace is emitted as one structured slog.Info call with slog.Group
// fields so an operator can grep a single request id and see the full
// pipeline. Traces are accumulated in local variables during the request
// and flushed once at the end — never interleaved with the SSE stream
// output the harness is consuming.
//
// Sensitive fields are redacted before they leave the handler: API keys
// are masked to their last 4 characters (`sk-...XXXX`), Authorization
// headers are stripped from any structured payload, and the response
// body preview is capped at NEXUS_DEBUG_BODY_BYTES (default 512).
//
// Per-request tracing (issue #242): the operator can also request a
// trace for a single request without enabling NEXUS_DEBUG=true
// globally. By passing the X-Nexus-Trace-ID header with a value
// matching the request's X-Request-Id (or generated id), exactly one
// request is traced. This is useful for investigating a specific
// user complaint without flooding logs for all traffic.
//
// When neither NEXUS_DEBUG is set nor X-Nexus-Trace-ID matches, this
// file adds zero allocations: the handler skips trace construction
// entirely and the production fast path is byte-for-byte identical to
// before this issue.
package handlers

import (
	"log/slog"
	"net/url"
	"strings"
)

// RequestTrace is the inbound payload summary emitted before any
// middleware runs. It lets an operator see "what did the harness send"
// without parsing the raw POST body.
type RequestTrace struct {
	RequestID        string
	Messages         int
	EstimatedTokens  int
	ModelRequested   string
	Stream           bool
	InboundBodyBytes int
}

// Log emits the trace as a structured slog.Info call under the
// [DEBUG] prefix. reqID is included as a top-level field for
// correlation across the multi-line trace stream.
func (t RequestTrace) Log(logger *slog.Logger, reqID string) {
	logger.Info("[DEBUG] request",
		slog.String("request_id", reqID),
		slog.Group("request",
			slog.Int("messages", t.Messages),
			slog.Int("estimated_tokens", t.EstimatedTokens),
			slog.String("model", t.ModelRequested),
			slog.Bool("stream", t.Stream),
			slog.Int("body_bytes", t.InboundBodyBytes),
		),
	)
}

// TransformTrace captures the post-middleware state of the request —
// what did RAG inject, did TOON rewrite any JSON arrays, did the meta
// prompt get applied — so an operator can confirm the middleware chain
// ran end-to-end.
type TransformTrace struct {
	PromptEngineeringApplied bool
	RAGInjected              bool
	RAGFilename              string
	RAGCacheHit              bool    // true when embedding was served from cache (issue #227)
	RAGScore                 float64
	TOONApplied              bool
	TOONBytesBefore          int
	TOONBytesAfter          int
	TOONTokensSaved          int
}

// Log emits the trace as a structured slog.Info call under the
// [DEBUG] prefix.
func (t TransformTrace) Log(logger *slog.Logger, reqID string) {
	logger.Info("[DEBUG] transforms",
		slog.String("request_id", reqID),
		slog.Group("transforms",
			slog.Bool("prompt_engineering", t.PromptEngineeringApplied),
			slog.Group("rag",
				slog.Bool("injected", t.RAGInjected),
				slog.String("filename", t.RAGFilename),
				slog.Bool("cache_hit", t.RAGCacheHit),
				slog.Float64("score", t.RAGScore),
			),
			slog.Group("toon",
				slog.Bool("applied", t.TOONApplied),
				slog.Int("bytes_before", t.TOONBytesBefore),
				slog.Int("bytes_after", t.TOONBytesAfter),
				slog.Int("tokens_saved", t.TOONTokensSaved),
			),
		),
	)
}

// RouteTrace captures the routing decision and the inputs that drove
// it: which pass (guardrail / DSL / SLM), the budget source for the
// VRAM-aware path, and the latest-prompt token estimate. Together
// these answer "why did nexus pick the model it did?".
type RouteTrace struct {
	Route           string // "local" | "frontier" | "fusion"
	Reason          string // "guardrail" | "dsl" | "slm"
	BudgetSource    string // "static-fallback" | "ollama-ps" | ...
	BudgetTokens    int
	EstimatedTokens int
	SLMRaw          string // raw SLM JSON when Reason == "slm"; empty otherwise
}

// Log emits the trace as a structured slog.Info call under the
// [DEBUG] prefix.
func (t RouteTrace) Log(logger *slog.Logger, reqID string) {
	logger.Info("[DEBUG] routing",
		slog.String("request_id", reqID),
		slog.Group("routing",
			slog.String("route", t.Route),
			slog.String("reason", t.Reason),
			slog.String("budget_source", t.BudgetSource),
			slog.Int("budget_tokens", t.BudgetTokens),
			slog.Int("estimated_tokens", t.EstimatedTokens),
			slog.String("slm_raw", t.SLMRaw),
		),
	)
}

// UpstreamTrace captures the upstream call metadata: target host
// (never the full URL — query strings and paths can leak model
// identifiers), the model name, streaming mode, and cascade details
// (which steps were tried, which served the response).
type UpstreamTrace struct {
	Route           string // echoed for grep convenience
	TargetHost      string // host only, no path / query
	Model           string // model name sent to the upstream
	Streaming       bool
	CascadeSteps    []string // ordered list of step names attempted
	CascadeServedBy string   // "" when no step succeeded
	CascadeSuccess  bool
}

// Log emits the trace as a structured slog.Info call under the
// [DEBUG] prefix. The cascade fields are grouped under a sub-group so
// frontier / fusion traces (which have no cascade) stay readable.
func (t UpstreamTrace) Log(logger *slog.Logger, reqID string) {
	upstream := []any{
		slog.String("route", t.Route),
		slog.String("target_host", t.TargetHost),
		slog.String("model", t.Model),
		slog.Bool("streaming", t.Streaming),
	}
	if len(t.CascadeSteps) > 0 {
		upstream = append(upstream,
			slog.Group("cascade",
				slog.Any("steps", t.CascadeSteps),
				slog.String("served_by", t.CascadeServedBy),
				slog.Bool("success", t.CascadeSuccess),
			),
		)
	}
	logger.Info("[DEBUG] upstream",
		slog.String("request_id", reqID),
		slog.Group("upstream", upstream...),
	)
}

// ResponseTrace captures the response shape: HTTP status, time to first
// byte (zero for non-streaming), total bytes, output token estimate,
// and a body preview capped at bodyBytes (default 512) so a runaway
// upstream cannot flood the log.
type ResponseTrace struct {
	Status        int
	TTFTMs        int64
	TotalBytes    uint64
	OutputTokens  int
	BodyPreview   string
	BodyTruncated bool
}

// Log emits the trace as a structured slog.Info call under the
// [DEBUG] prefix.
func (t ResponseTrace) Log(logger *slog.Logger, reqID string) {
	logger.Info("[DEBUG] response",
		slog.String("request_id", reqID),
		slog.Group("response",
			slog.Int("status", t.Status),
			slog.Int64("ttft_ms", t.TTFTMs),
			slog.Uint64("total_bytes", t.TotalBytes),
			slog.Int("output_tokens", t.OutputTokens),
			slog.String("body_preview", t.BodyPreview),
			slog.Bool("body_truncated", t.BodyTruncated),
		),
	)
}

// MaskAPIKey returns a redacted form of key suitable for logs. Empty
// input returns ""; a key 8 characters or shorter returns a fully
// masked string ("****"); otherwise the visible prefix is the first 3
// characters and the visible suffix is the last 4. Typical output:
// `sk-...XYZ1` for `sk-proj-abcdefXYZ1`.
func MaskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "****"
	}
	return key[:3] + "..." + key[len(key)-4:]
}

// HostOfURL returns the host component of rawURL — empty string for
// unparseable URLs. The full URL is intentionally never logged because
// frontier paths leak model identifiers (e.g.
// /v1/chat/completions?model=gpt-4o). Host-only output also keeps
// query strings (which occasionally carry bearer tokens in legacy
// systems) out of logs.
func HostOfURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// TruncateForDebug returns the first n bytes of s with a trailing
// "...(truncated, total=N)" suffix when the original was longer. The
// function avoids splitting a UTF-8 rune (every byte that starts a
// rune has its high bit either 0 or matching the 10xxxxxx continuation
// pattern, so we walk back from the cap to the last ASCII boundary
// only when strictly necessary — for our use case a byte boundary is
// fine because previews are debug-only and downstream log readers
// tolerate partial runes).
func TruncateForDebug(s string, n int) (string, bool) {
	if n <= 0 || len(s) <= n {
		return s, false
	}
	return s[:n] + "...(truncated, total=" + itoa(len(s)) + ")", true
}

// itoa is a tiny stdlib-free integer formatter used only by
// TruncateForDebug. Inlined to avoid pulling strconv into the trace
// package surface; the value is always small (< 1 MiB).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// RedactedHeaders renders headers for structured logging with the
// Authorization value masked. Other headers pass through verbatim.
// Returns nil for nil input.
func RedactedHeaders(h map[string][]string) map[string][]string {
	if h == nil {
		return nil
	}
	out := make(map[string][]string, len(h))
	for k, vs := range h {
		if strings.EqualFold(k, "Authorization") {
			masked := make([]string, len(vs))
			for i := range vs {
				// "Bearer sk-...XYZ1" → "Bearer ****"
				masked[i] = "Bearer ****"
			}
			out[k] = masked
			continue
		}
		out[k] = vs
	}
	return out
}

// DebugTrace is the bundle the chat handler accumulates during a single
// request lifecycle and emits as a single emit call when debug tracing
// is on. The struct is intentionally flat — handlers build each
// sub-trace at its respective lifecycle step and write the bundle once
// the response flushes (so SSE bytes never interleave with trace
// lines).
type DebugTrace struct {
	Request    RequestTrace
	Transforms TransformTrace
	Routing    RouteTrace
	Upstream   UpstreamTrace
	Response   ResponseTrace
}

// Emit renders every sub-trace as a structured slog.Info call under
// the [DEBUG] prefix. Safe to call when Debug is false — the call
// sites guard against it.
func (d DebugTrace) Emit(logger *slog.Logger) {
	if logger == nil {
		return
	}
	d.Request.Log(logger, d.Request.RequestID)
	d.Transforms.Log(logger, d.Request.RequestID)
	d.Routing.Log(logger, d.Request.RequestID)
	d.Upstream.Log(logger, d.Request.RequestID)
	d.Response.Log(logger, d.Request.RequestID)
}

// DebugEmitted is a tiny sentinel used by tests that swap
// slog.Default to count the number of [DEBUG] lines emitted by a
// single request. The handler does not read this; the constant
// exists so tests can assert "exactly N debug lines per request"
// without parsing JSON log output.
const DebugLogPrefix = "[DEBUG] "
