// Package handlers contains the HTTP entry points for the proxy. The chat
// handler is the only public endpoint; it owns the request lifecycle:
// middleware chain -> routing decision -> upstream execution.
package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anchapin/nexus-proxy/internal/concurrencylimit"
	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/health"
	"github.com/anchapin/nexus-proxy/internal/middleware"
	"github.com/anchapin/nexus-proxy/internal/observability"
	"github.com/anchapin/nexus-proxy/internal/providers"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/telemetry"
	"github.com/anchapin/nexus-proxy/internal/tracing"
	"github.com/anchapin/nexus-proxy/internal/upstream"
)

// LocalCompletion is the event surface the chat handler exposes for
// out-of-band observers (notably the async LLM-as-a-judge evaluator,
// see internal/judge). It is intentionally tiny: only the fields a
// judge needs to score a response. The handler never imports judge —
// the observer plugs in from cmd/nexus/main.go.
type LocalCompletion struct {
	RequestID   string
	Instruction string
	Output      string
	LocalModel  string
}

// JudgeObserver is the hook the chat handler invokes when a
// RouteLocal request completes successfully. Implementations must be
// safe to call concurrently; the handler invokes them on the same
// goroutine that serves the request so the observer should not block
// long. The handler also enforces a body-size cap before invoking
// the hook so a runaway model cannot blow the observer's memory.
type JudgeObserver interface {
	Submit(LocalCompletion)
}

// JudgeObserverFunc adapts a plain function to the JudgeObserver
// interface so wiring from main.go stays a one-liner.
type JudgeObserverFunc func(LocalCompletion)

// Submit implements JudgeObserver.
func (f JudgeObserverFunc) Submit(c LocalCompletion) { f(c) }

// QualityEvent is emitted to the QualityObserver hook each time the
// handler detects a tool call in an upstream response that looks like
// a file edit (write_file, edit_file, apply_patch, ...). It mirrors
// the Event type in internal/quality — the handler does not import
// that package so the dependency direction stays one-way (cmd/nexus
// adapts between the two shapes).
type QualityEvent struct {
	RequestID string // correlates to the chat handler's request id
	Path      string // path of the edited file
	ToolName  string // write_file, edit_file, apply_patch, ...
}

// QualityObserver is the hook the chat handler invokes with detected
// edits. Like the JudgeObserver, it must be safe to call from many
// goroutines and must not block; the verifier does the heavy work
// asynchronously and only the Submit call happens on the request
// goroutine.
type QualityObserver interface {
	Submit(QualityEvent)
}

// QualityObserverFunc adapts a plain function to the QualityObserver
// interface so wiring from main.go stays a one-liner.
type QualityObserverFunc func(QualityEvent)

// Submit implements QualityObserver.
func (f QualityObserverFunc) Submit(e QualityEvent) { f(e) }

// MetricsEvent carries the per-request data needed by the savings
// dashboard (issue #4). Fields track the full Round-trip metrics:
// route/model/input-tokens are routing dimensions; TOON/RAG/cost are
// the savings dimensions the dashboard renders. The handler builds
// one of these after every proxied request (success, failure, or
// short-circuit) and dispatches it to the configured MetricsObserver.
//
// Mirrors internal/metrics.Request so a tiny adapter in main.go can
// forward directly without translation — kept here as a separate
// type so the handlers package stays free of the metrics import.
type MetricsEvent struct {
	Timestamp         time.Time
	RequestID         string
	Route             string
	Model             string
	InputTokens       int
	TOONSavingsTokens int
	RAGInjected       bool
	RAGFilename       string
	EstimatedCostUSD  float64

	OutputTokens   int
	TTFTMs         int64
	TotalLatencyMs int64
	TPS            float64
	Streaming      bool
	Error          string
}

// MetricsObserver is the hook the chat handler invokes once per
// proxied request with a MetricsEvent payload. Same invariants as
// JudgeObserver / QualityObserver:
//   - safe to call from many goroutines;
//   - must not block (the SQLite backend uses a buffered channel);
//   - may be nil (treated as "no observer"; hot path is unaffected).
type MetricsObserver interface {
	Submit(MetricsEvent)
}

// MetricsObserverFunc adapts a plain function to the MetricsObserver
// interface so wiring from main.go stays a one-liner.
type MetricsObserverFunc func(MetricsEvent)

// Submit implements MetricsObserver.
func (f MetricsObserverFunc) Submit(e MetricsEvent) { f(e) }

// ObservabilityObserver is the hook the chat handler invokes once per
// proxied request with an observability.ObservabilityEvent payload
// (issue #40). The in-process Prometheus collector
// (internal/observability.Collector) implements this interface
// directly — handlers imports that leaf package (no cycle), so main.go
// wires the collector in without a field-copy adapter. Same invariants
// as MetricsObserver:
//   - safe to call from many goroutines;
//   - must not block (the collector does only atomic increments);
//   - may be nil (treated as "no collector"; hot path is unaffected).
type ObservabilityObserver interface {
	Submit(observability.ObservabilityEvent)
}

// BudgetObserver is the dynamic VRAM-budget hook the chat handler
// consults before falling back to the static `NEXUS_TOKEN_GUARDRAIL`
// (issue #6).
//
// Implementations must be safe to call concurrently from many request
// goroutines. BudgetTokens should return the most recent probe
// measurement; when no probe is configured or the last probe was
// disabled, BudgetTokens returns <= 0 and the handler transparently
// falls back to the static value configured by the operator.
//
// BudgetSource is a short human-readable label (e.g. "ollama-ps",
// "amd-sysfs", "ollama-ps+amd-sysfs", "static-fallback") used in log
// lines and in /healthz so operators can see what is driving the
// budget at a glance.
type BudgetObserver interface {
	BudgetTokens() int
	BudgetSource() string
}

// BudgetObserverFunc adapts two plain functions to the BudgetObserver
// interface so wiring from main.go stays a one-liner. Either closure
// may be nil; nil Tokens treats the observer as "no budget", nil
// Source returns "static".
type BudgetObserverFunc struct {
	Tokens func() int
	Source func() string
}

// BudgetTokens implements BudgetObserver.
func (b BudgetObserverFunc) BudgetTokens() int {
	if b.Tokens == nil {
		return 0
	}
	return b.Tokens()
}

// BudgetSource implements BudgetObserver.
func (b BudgetObserverFunc) BudgetSource() string {
	if b.Source == nil {
		return "static"
	}
	return b.Source()
}

// SpendGuard is the rolling daily-budget hook the chat handler
// consults before dispatching a RouteFrontier request (issue #38).
// When WouldExceed returns true the handler returns HTTP 429
// "Daily frontier budget exceeded" instead of dispatching — local
// routing is never gated. After a frontier request completes
// (success or upstream error) the handler calls Record so the
// rolling 24-hour window stays accurate.
//
// Implementations must be safe to call concurrently from many
// request goroutines. A nil SpendGuard disables the gate entirely
// (the pre-issue-#38 behaviour), so callers can leave Deps.SpendGuard
// unset without any per-request nil-check overhead.
type SpendGuard interface {
	WouldExceed(amount float64) bool
	Record(amount float64)
}

// Deps bundles the collaborators the chat handler needs. Wiring them
// explicitly makes the handler trivial to unit-test with stubs.
type Deps struct {
	Config          config.Config
	Client          upstream.Client // http.Client satisfies this interface
	RAG             *rag.Store
	SLM             *router.SLMClient
	FormattingRegex *regexp.Regexp

	// JudgeObserver is optional. When nil, the handler does not
	// buffer the streamed response for judge sampling — every
	// request takes the fast path with zero added overhead.
	//
	// The handler never imports the judge package; main.go wires
	// a closure that adapts LocalCompletion to the judge's
	// Sample/Enqueue entry points.
	JudgeObserver JudgeObserver

	// QualityObserver is optional. When non-nil, the handler scans
	// the captured upstream body for tool-call patterns that look
	// like file edits (write_file / edit_file / apply_patch / ...)
	// and dispatches one QualityEvent per detected file path.
	// The scan runs on the request goroutine AFTER the response
	// has been fully streamed; it adds negligible overhead and
	// never blocks the response path. The handler does not import
	// the quality package; main.go wires a closure that forwards
	// to the verifier's Submit.
	QualityObserver QualityObserver

	// MetricsObserver is optional. When non-nil, the handler
	// dispatches one MetricsEvent per proxied request after the
	// upstream response flushes, with the route, model, input
	// tokens, TOON savings, RAG injection, and rough frontier
	// cost. The observer is invoked synchronously from the request
	// goroutine but does not block the response path because the
	// SQLite-backed implementation (internal/metrics) enqueues
	// onto a buffered channel and writes asynchronously. The
	// handler does not import the metrics package; main.go wires
	// a closure that forwards to metricsStore.Submit.
	MetricsObserver MetricsObserver

	// ObservabilityObserver is optional. When non-nil, the handler
	// dispatches one observability.ObservabilityEvent per proxied
	// request after the upstream response flushes, carrying the
	// routing/error/RAG/TOON/degraded flags, token sums, cost, and
	// latency dimensions the Prometheus collector (issue #40) needs.
	// The collector increments only sync/atomic primitives, so Submit
	// is non-blocking and safe to call from the request goroutine.
	// When nil the handler skips the dispatch entirely (zero overhead).
	ObservabilityObserver ObservabilityObserver

	// Recorder receives one record per proxied request. Never nil at
	// runtime: Chat() installs telemetry.Noop{} when the caller
	// passes a zero value, so downstream code can invoke Record
	// unconditionally without a nil-check.
	Recorder telemetry.Recorder

	// Health tracks the live reachability of the local Ollama
	// endpoint (issue #8). When IsLocalHealthy returns false the
	// handler short-circuits route=local to frontier and skips the
	// local panel member of route=fusion, stamping
	// X-Nexus-Degraded: true on the response. Optional: nil is
	// treated as "always healthy" so unit tests and tools that do
	// not wire the poller still work unchanged.
	Health *health.Health

	// BudgetObserver is the dynamic VRAM-budget hook (issue #6).
	// When non-nil and BudgetTokens returns a positive value, the
	// handler uses that as the guardrail budget instead of
	// Config.TokenGuardrail. BudgetTokens <=0 (or a nil observer)
	// falls back to the static NEXUS_TOKEN_GUARDRAIL value so the
	// handler behaves identically to before issue #6 when no
	// probe is wired.
	BudgetObserver BudgetObserver

	// Limiter is the VRAM-aware concurrency gate (issue #35). When
	// non-nil the handler acquires a slot before dispatching any
	// RouteLocal request (and before the local panel member of
	// RouteFusion); waiters queue-and-wait up to
	// Config.LocalQueueTimeout, after which the request is
	// fast-promoted to frontier via SkipLocal and the response
	// carries X-Nexus-Overflow: true. RouteFrontier is never
	// gated.
	//
	// Optional: a nil Limiter preserves the pre-issue-#35
	// unbounded behaviour exactly, which is what every existing
	// unit test relies on. The chat hot path consults Config
	// alongside the receiver so a misconfigured (max<=0) Limiter
	// is also treated as disabled.
	Limiter *concurrencylimit.Limiter

	// SpendGuard is the rolling daily-budget gate for RouteFrontier
	// (issue #38). When non-nil the handler checks WouldExceed before
	// dispatching to the frontier; if the check returns true the
	// handler returns 429 "Daily frontier budget exceeded" instead.
	// Local and fusion routing are never gated. After a frontier
	// request completes the handler calls Record so the 24h window
	// stays accurate. A nil SpendGuard disables the gate entirely
	// (backward compatible).
	SpendGuard SpendGuard

	// Tracer emits spans for each request phase (issue #41). The
	// exporter owns the W3C traceparent parsing, sampling decision,
	// and OTLP/JSON export; the handler just wraps each phase in
	// a child span and stamps the per-phase attributes (route,
	// model, input_tokens, ttft_ms, total_latency_ms, ...).
	//
	// Optional: a nil Tracer is treated as "tracing disabled" so
	// every method call short-circuits without producing a span or
	// allocating beyond a nil-check. Chat() does NOT substitute a
	// no-op exporter — the handler does the nil-check inline so the
	// hot path avoids an unnecessary indirection when tracing is off.
	Tracer *tracing.Exporter

	// maxObservedBytes caps the body the observer sees. The full
	// response is still streamed to the client — only the buffered
	// copy used for sampling is bounded. Zero uses DefaultObservedCap.
	maxObservedBytes int
}

// DefaultObservedCap is the upper bound on the bytes the judge hook
// buffers per request. Typical LLM completions are well under this;
// the cap exists so a runaway local model cannot OOM the proxy.
const DefaultObservedCap = 256 * 1024 // 256 KiB

// Chat returns an http.Handler that runs the middleware chain, picks a
// route, and streams the chosen upstream's response back to the harness.
//
// Middleware order (do not reorder casually):
//  1. applyPromptEngineering    — inject role/CoT/constraints into system
//  2. applyRetrievalAugmentation — embed latest prompt, inject best match
//  3. optimizePromptContext     — TOON-compress JSON arrays in user msgs
//  4. evaluateDSL -> getSLMRoutingDecision
//  5. dispatch:
//     local    -> Cascade (streaming) or BufferedFetch (non-streaming)
//     frontier -> Stream (streaming) or BufferedFetch (non-streaming)
//     fusion   -> Panel (honors stream flag for arbiter branch)
//
// After a successful RouteLocal stream the handler invokes
// JudgeObserver (when configured) with a LocalCompletion event. The
// observer may decide to enqueue the request for judge scoring; the
// handler does not wait on the observer.
//
// Likewise, when QualityObserver is configured, the handler scans
// the captured response body for tool-call envelopes that look like
// file edits (write_file / edit_file / apply_patch / ...) and
// forwards a QualityEvent per detected path. The verifier (cmd/nexus)
// runs `cargo check` / `npx tsc` asynchronously and dispatches the
// verdict back via its own observer hook.
//
// Every proxied request — success, failure, or short-circuit — emits
// exactly one telemetry.Record via Recorder so the issue #16 dashboard
// can answer "what fraction of traffic failed in the last hour?".
func Chat(d Deps) http.Handler {
	if d.maxObservedBytes <= 0 {
		d.maxObservedBytes = DefaultObservedCap
	}
	if d.Recorder == nil {
		d.Recorder = telemetry.Noop{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := requestID(r)
		started := time.Now()

		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		// Distributed tracing (issue #41). Honour an inbound W3C
		// `traceparent` so an upstream caller can attach the proxy
		// to its trace; otherwise mint a fresh trace id. The
		// exporter's sampler decides per-trace whether the spans
		// are actually exported, so NEXUS_TRACING_SAMPLE_RATE=0
		// transparently skips the whole subtree.
		var traceCtx tracing.Context
		if tp := r.Header.Get("traceparent"); tp != "" {
			if traceID, spanID, ok := tracing.ParseTraceparent(tp); ok {
				traceCtx = tracing.Context{TraceID: traceID, SpanID: spanID}
			}
		}
		var rootCtx tracing.Context
		var rootSpan *tracing.Span
		if d.Tracer != nil {
			rootCtx, rootSpan = d.Tracer.StartSpan(traceCtx, "nexus.chat_completions")
			defer rootSpan.End()
			rootSpan.SetAttr("request_id", reqID)
		}

		// Hard cap on request body (issue #11). Wrap with
		// http.MaxBytesReader BEFORE any allocation so an oversized
		// POST cannot exhaust proxy memory; MaxBytesReader caps
		// reads at the limit and surfaces *http.MaxBytesError on
		// overflow, which we translate to 413 below.
		maxBytes := d.Config.EffectiveMaxBodyBytes()
		r.Body = http.MaxBytesReader(w, r.Body, int64(maxBytes))

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeJSONError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
					"Request body exceeds NEXUS_MAX_BODY_BYTES (%d bytes)", maxBytes))
				slog.Warn("rejected oversized request",
					slog.String("remote", r.RemoteAddr),
					slog.Int("limit_bytes", maxBytes),
					slog.String("request_id", reqID),
				)
				return
			}
			http.Error(w, "Failed to read request", http.StatusBadRequest)
			return
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
			return
		}
		rawMessages, ok := body["messages"].([]interface{})
		if !ok {
			http.Error(w, "Invalid or missing messages array", http.StatusBadRequest)
			return
		}

		messages := middleware.ApplyPromptEngineering(rawMessages, d.Config.MetaPrompt)
		latestPrompt := middleware.ExtractLatestUserPrompt(messages)
		var ragInjected bool
		var ragFilename string
		if d.Tracer != nil {
			_, ragSpan := d.Tracer.StartSpan(rootCtx, "rag.retrieve")
			ragSpan.SetAttr("threshold", d.Config.RAGThreshold)
			if ex, score, err := d.RAG.Retrieve(r.Context(), latestPrompt); err == nil && ex != nil {
				messages = middleware.InjectRAG(messages, rag.FormatInjection(ex))
				slog.Info("rag hit",
					slog.String("filename", ex.Filename),
					slog.Float64("score", score),
					slog.String("request_id", reqID),
				)
				ragInjected = true
				ragFilename = ex.Filename
				ragSpan.SetAttr("filename", ex.Filename)
				ragSpan.SetAttr("score", score)
				ragSpan.SetAttr("injected", true)
				_ = score
			} else {
				ragSpan.SetAttr("injected", false)
			}
			ragSpan.End()
		} else {
			if ex, score, err := d.RAG.Retrieve(r.Context(), latestPrompt); err == nil && ex != nil {
				messages = middleware.InjectRAG(messages, rag.FormatInjection(ex))
				slog.Info("rag hit",
					slog.String("filename", ex.Filename),
					slog.Float64("score", score),
					slog.String("request_id", reqID),
				)
				ragInjected = true
				ragFilename = ex.Filename
				_ = score
			}
		}
		// Snapshot the JSON size BEFORE TOON compression so the
		// metrics observer can attribute tokens saved by the
		// round-trip pass. Uses the cheap "4 chars per token"
		// heuristic the rest of the project uses for telemetry
		// (see internal/telemetry.EstimateTokens).
		preCompressionChars := totalMessageChars(messages)
		var toonSpan *tracing.Span
		if d.Tracer != nil {
			_, toonSpan = d.Tracer.StartSpan(rootCtx, "toon.compress")
			toonSpan.SetAttr("pre_chars", preCompressionChars)
		}
		toonCompressed := middleware.CompressJSONBlocks(messages)
		if toonCompressed {
			messages = middleware.AppendSystemNote(messages, d.Config.TOONNotice)
			slog.Info("toon compressed messages", slog.String("request_id", reqID))
		}
		body["messages"] = messages
		latestPrompt = middleware.ExtractLatestUserPrompt(messages)
		if toonSpan != nil {
			toonSpan.SetAttr("compressed", toonCompressed)
			toonSpan.End()
		}

		var route router.Route
		// VRAM-aware guardrail (issue #6). When the
		// BudgetObserver is wired and reports a positive budget,
		// the dynamic measurement wins; otherwise we fall back to
		// the operator-configured NEXUS_TOKEN_GUARDRAIL. The source
		// label travels with the log line so operators can confirm
		// which path was taken without digging through the source.
		guardrailBudget := d.Config.TokenGuardrail
		guardrailSource := "static-fallback"
		if d.BudgetObserver != nil {
			if dyn := d.BudgetObserver.BudgetTokens(); dyn > 0 {
				guardrailBudget = dyn
				guardrailSource = d.BudgetObserver.BudgetSource()
			}
		}
		// Wrap the routing decision in a single span
		// (route.guardrail|route.dsl|route.slm) so the trace tree
		// shows which rule fired. The span name is chosen inside
		// the if/else so the collector displays the actual path
		// taken without ambiguity.
		if g, hit := router.Guardrail(latestPrompt, guardrailBudget); hit {
			slog.Info("guardrail forced frontier",
				slog.String("reason", "vram"),
				slog.String("budget_source", guardrailSource),
				slog.Int("budget_tokens", guardrailBudget),
				slog.Int("estimated_tokens", len(latestPrompt)/4),
				slog.String("request_id", reqID),
			)
			route = g
			if d.Tracer != nil {
				_, rs := d.Tracer.StartSpan(rootCtx, "route.guardrail")
				rs.SetAttr("budget_source", guardrailSource)
				rs.SetAttr("budget_tokens", guardrailBudget)
				rs.SetAttr("estimated_tokens", len(latestPrompt)/4)
				rs.SetAttr("decision", string(route))
				rs.End()
			}
		} else if r2, hit := router.DSL(latestPrompt, d.FormattingRegex); hit {
			slog.Info("dsl match",
				slog.String("route", string(r2)),
				slog.String("request_id", reqID),
			)
			route = r2
			if d.Tracer != nil {
				_, rs := d.Tracer.StartSpan(rootCtx, "route.dsl")
				rs.SetAttr("decision", string(route))
				rs.End()
			}
		} else {
			slog.Debug("dsl bypassed, asking slm", slog.String("request_id", reqID))
			dec, err := d.SLM.Decide(r.Context(), latestPrompt)
			if err != nil {
				slog.Error("slm error, defaulting to frontier",
					slog.Any("err", err),
					slog.String("request_id", reqID),
				)
				dec = router.RouteFrontier
			}
			slog.Info("slm decision",
				slog.String("route", string(dec)),
				slog.String("request_id", reqID),
			)
			route = dec
			if d.Tracer != nil {
				_, rs := d.Tracer.StartSpan(rootCtx, "route.slm")
				rs.SetAttr("decision", string(route))
				if err != nil {
					rs.RecordError(err)
				}
				rs.End()
			}
		}

		// Honor the harness's stream flag (issue #10). OpenAI treats
		// stream as advisory; a stream=false request must receive a
		// single chatCompletionResponse JSON object, not chunked SSE.
		// The local and frontier branches dispatch on this flag below
		// (Cascade/Stream for stream=true, BufferedFetch for
		// stream=false). The fusion arbiter reads body["stream"]
		// directly inside upstream.Panel.
		streaming := true
		if s, ok := body["stream"].(bool); ok && !s {
			streaming = false
		}

		// Wrap the response writer so we can capture TTFT and byte counts
		// without affecting upstream.Stream's flusher contract.
		var firstWriteAt atomic.Int64 // unix nano; 0 means "no write yet"
		obs := telemetry.NewObservingWriter(w, func(t time.Time) {
			firstWriteAt.CompareAndSwap(0, t.UnixNano())
		})

		// Graceful degradation (issue #8). When the local Ollama health
		// poller reports unhealthy we:
		//   - skip the local fetch on route=local (the cascade starts
		//     at frontier, the harness sees frontier content with
		//     X-Nexus-Degraded: true);
		//   - skip the local panel member on route=fusion (Panel is
		//     told to skipLocal, the arbiter sees a synthetic
		//     "[local failed: ...]" candidate and synthesises from
		//     frontier alone);
		//   - leave route=frontier untouched (no local involved).
		// We stamp the response header before the upstream call so it
		// lands in the SSE frame regardless of which route served
		// the request. The token guardrail (#6) is consulted first;
		// when it fires we always go to frontier, so this check
		// never overrides a guardrail-forced reroute.
		localHealthy := d.Health.IsLocalHealthy()
		skipLocal := !localHealthy && (route == router.RouteLocal || route == router.RouteFusion)
		// degraded captures the health-based skip state for the
		// observability collector (issue #40). It is snapshotted here,
		// before the concurrency-overflow path below may reassign
		// skipLocal — overflow is a distinct signal (X-Nexus-Overflow)
		// and must not inflate the degraded counter.
		degraded := skipLocal
		if skipLocal {
			w.Header().Set("X-Nexus-Degraded", "true")
			slog.Warn("ollama unhealthy; skipping local arm",
				slog.String("route", string(route)),
				slog.String("request_id", reqID),
			)
		} else {
			w.Header().Set("X-Nexus-Degraded", "false")
		}

		var model string
		var upErr error
		// Build the traceparent for outbound propagation once we
		// know the upstream span. Each route branch starts its own
		// span below, captures the resulting upCtx, and threads
		// the corresponding traceparent header into the upstream
		// call so the distributed trace stays correlated end-to-end.
		switch route {
		case router.RouteFusion:
			slog.Info("starting fusion panel", slog.String("request_id", reqID))

			// VRAM-aware concurrency gate for the local panel
			// member (issue #35). Same acquire-or-skip semantics
			// as RouteLocal above, but we cannot mutate the
			// outer skipLocal here because the subsequent
			// Switch-case uses the same variable name to drive
			// both the cascade SkipLocal and the non-streaming
			// target. A scoped variable keeps the intent
			// explicit without changing any other branch.
			fusionSkipLocal := skipLocal
			if d.Limiter != nil && d.Config.LocalMaxConcurrent > 0 {
				if !d.Limiter.Acquire(r.Context(), d.Config.LocalQueueTimeout) {
					w.Header().Set("X-Nexus-Overflow", "true")
					slog.Warn("local concurrency limit reached; skipping local panel member",
						slog.Int("max_concurrent", d.Config.LocalMaxConcurrent),
						slog.Duration("queue_timeout", d.Config.LocalQueueTimeout),
						slog.String("request_id", reqID),
					)
					fusionSkipLocal = true
				} else {
					defer d.Limiter.Release()
				}
			}

			var upCtx tracing.Context
			var upSpan *tracing.Span
			if d.Tracer != nil {
				upCtx, upSpan = d.Tracer.StartSpan(rootCtx, "upstream.fusion")
				upSpan.SetAttr("local_model", d.Config.LocalModel)
				upSpan.SetAttr("frontier_model", d.Config.FrontierModel)
				upSpan.SetAttr("skip_local", fusionSkipLocal)
			}
			upErr = upstream.Panel(
				obs, d.Client,
				d.Config.OllamaURL, d.Config.LocalModel,
				d.Config.FrontierURL, d.Config.FrontierModel,
				d.Config.FrontierURL, d.Config.FrontierKey, d.Config.FrontierModel,
				body, latestPrompt, d.Config.FusionTimeout,
				d.Config.ArbiterTimeout,
				fusionSkipLocal,
				upstream.WithTraceparent(tracing.InjectTraceparent(upCtx)),
			)
			if upErr != nil {
				slog.Error("fusion error",
					slog.Any("err", upErr),
					slog.String("request_id", reqID),
				)
				if upSpan != nil {
					upSpan.RecordError(upErr)
				}
				http.Error(w, "Upstream error", http.StatusBadGateway)
			}
			model = d.Config.FrontierModel
			if upSpan != nil {
				upSpan.End()
			}

		case router.RouteLocal:
			body["model"] = d.Config.LocalModel

			// VRAM-aware concurrency gate (issue #35). At most
			// Config.LocalMaxConcurrent RouteLocal requests may
			// dispatch local Ollama simultaneously; waiters queue
			// for up to Config.LocalQueueTimeout, after which the
			// request is fast-promoted to frontier via SkipLocal
			// and X-Nexus-Overflow: true is stamped on the
			// response. RouteFrontier is never gated.
			//
			// Two conditions must be true for the gate to fire:
			// both the Limiter receiver is non-nil and the operator
			// actually configured a positive ceiling. A limiter
			// built with New(<=0) returns nil and the config check
			// below also disables — so this entire block is a
			// no-op under NEXUS_LOCAL_MAX_CONCURRENT=0.
			if d.Limiter != nil && d.Config.LocalMaxConcurrent > 0 {
				if !d.Limiter.Acquire(r.Context(), d.Config.LocalQueueTimeout) {
					w.Header().Set("X-Nexus-Overflow", "true")
					slog.Warn("local concurrency limit reached; promoting to frontier cascade",
						slog.Int("max_concurrent", d.Config.LocalMaxConcurrent),
						slog.Duration("queue_timeout", d.Config.LocalQueueTimeout),
						slog.String("request_id", reqID),
					)
					skipLocal = true
				} else {
					// Defer is safe even for the streaming
					// branch because cas.Run is synchronous
					// and returns when the response flushes
					// (or the client disconnects, which still
					// terminates Run). A panic in Run would
					// also fire the deferred Release.
					defer d.Limiter.Release()
				}
			}

			// Cascade (issue #14): try local Ollama first, fall back to
			// configured frontier endpoints (frontier, then z.ai) on
			// retryable failures. Cascade is rebuilt per request so
			// config changes take effect without restarting the
			// process. CascadeConfig.SkipLocal (issue #8) omits the
			// local step entirely when the health poller reports
			// Ollama unreachable, so the cascade starts at frontier
			// without paying the local timeout. The concurrency
			// overflow path above re-uses the same SkipLocal knob so
			// we don't have to thread a parallel flag through the
			// cascade (issue #35). Hoisted above the stream flag
			// (issue #10) so both branches share a single configured
			// cascade; see below for the non-streaming degraded
			// override.
			//
			// Providers (issue #43): when the registry has entries
			// we hand them straight to BuildLocalCascade so the
			// fallback list reflects NEXUS_PROVIDERS rather than
			// the legacy NEXUS_FRONTIER_* + NEXUS_ZAI_* pair. The
			// legacy fields are passed alongside so callers that
			// still build CascadeConfig by hand (existing tests)
			// keep working when Providers is nil.
			cas := upstream.BuildLocalCascade(upstream.CascadeConfig{
				LocalURL:      d.Config.OllamaURL,
				LocalModel:    d.Config.LocalModel,
				Providers:     d.Config.Providers.FrontierProviders(),
				FrontierURL:   d.Config.FrontierURL,
				FrontierModel: d.Config.FrontierModel,
				FrontierKey:   d.Config.FrontierKey,
				ZAIURL:        d.Config.ZAIURL,
				ZAIModel:      d.Config.ZAIModel,
				ZAIKey:        d.Config.ZAIKey,
				Timeout:       d.Config.CascadeTimeout,
				SkipLocal:     skipLocal,
			})

			// Writer chain (outermost first):
			//   upstream.Write -> captureWriter (judge + quality tee) ->
			//   ObservingWriter (telemetry byte count + TTFT) ->
			//   underlying ResponseWriter.
			// captureWriter is only installed when at least one
			// observer is set; otherwise the dispatch writes
			// directly through obs with zero overhead.
			rw := http.ResponseWriter(obs)
			var cap *captureWriter
			if d.JudgeObserver != nil || d.QualityObserver != nil {
				cap = newCaptureWriter(obs, d.maxObservedBytes)
				rw = cap
			}
			var upCtx tracing.Context
			var upSpan *tracing.Span
			if d.Tracer != nil {
				upCtx, upSpan = d.Tracer.StartSpan(rootCtx, "upstream.local")
				upSpan.SetAttr("local_model", d.Config.LocalModel)
				upSpan.SetAttr("skip_local", skipLocal)
				upSpan.SetAttr("streaming", streaming)
			}
			if streaming {
				// Cascade (issue #14): try local Ollama first, fall
				// back to configured frontier endpoints (frontier,
				// then z.ai) on retryable failures. SkipLocal (issue
				// #8) is honoured — when the health poller reports
				// Ollama unreachable the cascade skips the local step
				// entirely and starts at frontier.
				res, err := cas.Run(rw, d.Client, body, upstream.WithTraceparent(tracing.InjectTraceparent(upCtx)))
				logCascadeTelemetry(res, err, reqID)
				if upSpan != nil {
					upSpan.SetAttr("cascade_attempted", res.RouteAttempted)
					upSpan.SetAttr("cascade_served_by", res.ServedBy)
					upSpan.SetAttr("cascade_attempts", res.Attempts)
					upSpan.SetAttr("cascade_succeeded", res.Succeeded)
				}
				if err != nil {
					slog.Error("upstream error",
						slog.Any("err", err),
						slog.String("request_id", reqID),
					)
					upErr = err
					if upSpan != nil {
						upSpan.RecordError(err)
					}
					http.Error(w, "Upstream error", http.StatusBadGateway)
					// fall through: telemetry Record still fires
					// below so the failed request shows up in the
					// dashboard.
				} else {
					model = d.Config.LocalModel
					if res.Succeeded && cap != nil {
						if d.JudgeObserver != nil {
							d.JudgeObserver.Submit(LocalCompletion{
								RequestID:   reqID,
								Instruction: latestPrompt,
								Output:      cap.Buffer(),
								LocalModel:  d.Config.LocalModel,
							})
						}
						if d.QualityObserver != nil {
							emitDetectedEdits(cap.Buffer(), reqID, d.QualityObserver)
						}
					}
				}
			} else {
				// Non-streaming (issue #10): BufferedFetch collects
				// the full upstream body and writes it as a single
				// chatCompletionResponse JSON object. Local model
				// only; the cascade's validation / fallback is
				// bypassed in this branch because the harness
				// expects a verbatim response.
				//
				// Issue #8 (graceful degradation): when skipLocal is
				// true the local URL would hang / timeout because
				// Ollama is unreachable. Route the buffered fetch to
				// the configured frontier endpoint instead so the
				// harness still gets a JSON reply promptly. The
				// response is tagged via X-Nexus-Degraded=true
				// (stamped above).
				//
				// Issue #43: the fallback target is the registry's
				// first frontier provider, matching the route=frontier
				// branch above. The legacy Config.FrontierURL/Key
				// remain the source when the registry is empty
				// (e.g. callers that build Config by hand).
				targetURL := strings.TrimRight(d.Config.OllamaURL, "/") + "/v1/chat/completions"
				apiKey := ""
				frontierModel := d.Config.FrontierModel
				if skipLocal {
					if frontiers := d.Config.Providers.FrontierProviders(); len(frontiers) > 0 {
						first := frontiers[0]
						targetURL = first.URL
						apiKey = first.APIKey
						frontierModel = first.Model
					} else {
						targetURL = d.Config.FrontierURL
						apiKey = d.Config.FrontierKey
					}
				}
				if err := upstream.BufferedFetch(rw, d.Client, targetURL, apiKey, body, upstream.WithTraceparent(tracing.InjectTraceparent(upCtx))); err != nil {
					slog.Error("upstream error",
						slog.Any("err", err),
						slog.String("request_id", reqID),
					)
					upErr = err
					if upSpan != nil {
						upSpan.RecordError(err)
					}
					http.Error(w, "Upstream error", http.StatusBadGateway)
				} else {
					model = d.Config.LocalModel
					if skipLocal {
						model = frontierModel
					}
					if upSpan != nil {
						upSpan.SetAttr("cascade_served_by", "local-buffer")
					}
					if cap != nil {
						if d.JudgeObserver != nil {
							d.JudgeObserver.Submit(LocalCompletion{
								RequestID:   reqID,
								Instruction: latestPrompt,
								Output:      cap.Buffer(),
								LocalModel:  d.Config.LocalModel,
							})
						}
						if d.QualityObserver != nil {
							emitDetectedEdits(cap.Buffer(), reqID, d.QualityObserver)
						}
					}
				}
			}
			if upSpan != nil {
				upSpan.End()
			}

		default:
			// Issue #43: pick the frontier endpoint from the
			// registry's first provider, falling back to the
			// legacy Config.FrontierURL/Model/Key when the
			// registry is empty (callers that build a Config
			// struct directly without going through Load).
			// The fallback path preserves the pre-#43 behaviour
			// for unit tests that don't populate the registry.
			frontierURL := d.Config.FrontierURL
			frontierKey := d.Config.FrontierKey
			if frontiers := d.Config.Providers.FrontierProviders(); len(frontiers) > 0 {
				first := frontiers[0]
				frontierURL = first.URL
				frontierKey = first.APIKey
				model = first.Model
			} else {
				model = d.Config.FrontierModel
			}

			// Daily frontier budget gate (issue #38). Before
			// dispatching to the frontier, check whether the
			// estimated cost would push the rolling 24-hour spend
			// past the configured cap. Local and fusion routing
			// are never gated — only pure RouteFrontier is. When
			// SpendGuard is nil (budget disabled) this is a
			// complete no-op, preserving the pre-issue-#38
			// behaviour.
			if d.SpendGuard != nil {
				estCost := frontierCostEstimate(
					string(router.RouteFrontier), model,
					telemetry.EstimateTokens(latestPrompt),
					d.Config.Providers, d.Config.JudgeCostPer1KUSD,
				)
				if d.SpendGuard.WouldExceed(estCost) {
					slog.Warn("daily frontier budget exceeded",
						slog.String("request_id", reqID),
						slog.Float64("estimated_cost", estCost),
					)
					writeJSONError(w, http.StatusTooManyRequests,
						"Daily frontier budget exceeded")
					return
				}
			}

			var upCtx tracing.Context
			var upSpan *tracing.Span
			if d.Tracer != nil {
				upCtx, upSpan = d.Tracer.StartSpan(rootCtx, "upstream.frontier")
				upSpan.SetAttr("frontier_model", model)
				upSpan.SetAttr("streaming", streaming)
			}

			// Honor the harness's stream flag (issue #10). Stream
			// preserves SSE framing for chunked deliveries;
			// BufferedFetch collects the full body and returns a
			// single chatCompletionResponse JSON object.
			if streaming {
				upErr = upstream.Stream(obs, d.Client,
					frontierURL, frontierKey, body,
					upstream.WithTraceparent(tracing.InjectTraceparent(upCtx)))
			} else {
				upErr = upstream.BufferedFetch(obs, d.Client,
					frontierURL, frontierKey, body,
					upstream.WithTraceparent(tracing.InjectTraceparent(upCtx)))
			}
			if upErr != nil {
				slog.Error("upstream error",
					slog.Any("err", upErr),
					slog.String("request_id", reqID),
				)
				if upSpan != nil {
					upSpan.RecordError(upErr)
				}
				http.Error(w, "Upstream error", http.StatusBadGateway)
			}
			if upSpan != nil {
				upSpan.End()
			}
		}

		// Record frontier spend for the rolling 24h budget (issue
		// #38). Recorded after the request completes (success or
		// upstream error) because the frontier API consumed tokens
		// either way. The budget-exceeded early return above skips
		// this recording, which is correct — the request was never
		// dispatched.
		if route == router.RouteFrontier && d.SpendGuard != nil {
			d.SpendGuard.Record(frontierCostEstimate(
				string(router.RouteFrontier), model,
				telemetry.EstimateTokens(latestPrompt),
				d.Config.Providers, d.Config.JudgeCostPer1KUSD,
			))
		}

		// Per-request recording. The metrics observer (issue #4)
		// is preferred when configured: it carries the savings
		// dimensions (TOON delta, RAG injection, cost) that
		// telemetry.Record cannot express. The legacy Recorder
		// remains for callers that only want the TTFT / latency
		// fields, or when no metrics store is wired (operator
		// opted out by leaving NEXUS_METRICS_DB empty).
		totalMs := time.Since(started).Milliseconds()
		var ttftMs int64
		if streaming && firstWriteAt.Load() > 0 {
			ttftMs = time.Unix(0, firstWriteAt.Load()).Sub(started).Milliseconds()
			if ttftMs < 0 {
				ttftMs = 0
			}
		}
		outputTokens := int(obs.BytesOut() / 4)
		rec := buildRecord(reqID, started, firstWriteAt.Load(), obs.BytesOut(), streaming, route, model, latestPrompt, upErr)
		// Hoist savings/cost so both the MetricsObserver dispatch and
		// the ObservabilityObserver dispatch (issue #40) see the same
		// values without recomputing. frontierCostEstimate is a cheap
		// multiply and totalTokenSavings is a subtract, so computing
		// them unconditionally costs negligible cycles.
		postCompressionChars := totalMessageChars(messages)
		savings := totalTokenSavings(preCompressionChars, postCompressionChars)
		cost := frontierCostEstimate(string(route), model, telemetry.EstimateTokens(latestPrompt), d.Config.Providers, d.Config.JudgeCostPer1KUSD)
		if d.MetricsObserver != nil {
			tps := telemetry.ComputeTPS(outputTokens, ttftMs, totalMs)
			d.MetricsObserver.Submit(MetricsEvent{
				Timestamp:         rec.Timestamp,
				RequestID:         reqID,
				Route:             string(route),
				Model:             model,
				InputTokens:       rec.InputTokens,
				TOONSavingsTokens: savings,
				RAGInjected:       ragInjected,
				RAGFilename:       ragFilename,
				EstimatedCostUSD:  cost,
				OutputTokens:      outputTokens,
				TTFTMs:            ttftMs,
				TotalLatencyMs:    totalMs,
				TPS:               tps,
				Streaming:         streaming,
				Error:             rec.Error,
			})
		} else {
			d.Recorder.Record(rec)
		}

		// In-process Prometheus collector (issue #40). Dispatched once
		// per proxied request with the routing/error/RAG/TOON/degraded
		// flags, token sums, cost, and latency. The collector does only
		// atomic increments, so this is non-blocking and race-free.
		if d.ObservabilityObserver != nil {
			d.ObservabilityObserver.Submit(observability.ObservabilityEvent{
				Route:             string(route),
				Error:             rec.Error,
				RAGInjected:       ragInjected,
				TOONCompressed:    toonCompressed,
				Degraded:          degraded,
				InputTokens:       rec.InputTokens,
				OutputTokens:      outputTokens,
				TOONSavingsTokens: savings,
				EstimatedCostUSD:  cost,
				TotalLatencyMs:    totalMs,
				TTFTMs:            ttftMs,
			})
		}

		// Stamp the request-level summary attributes on the root
		// span (issue #41). Operators querying the collector for
		// "show me every route=frontier request that took > 5s"
		// can filter on these without leaving the trace UI.
		if rootSpan != nil {
			rootSpan.SetAttr("route", string(route))
			rootSpan.SetAttr("model", model)
			rootSpan.SetAttr("input_tokens", rec.InputTokens)
			rootSpan.SetAttr("output_tokens", outputTokens)
			rootSpan.SetAttr("ttft_ms", ttftMs)
			rootSpan.SetAttr("total_latency_ms", totalMs)
			rootSpan.SetAttr("degraded", degraded)
			rootSpan.SetAttr("streaming", streaming)
			rootSpan.SetAttr("rag_injected", ragInjected)
			if ragInjected {
				rootSpan.SetAttr("rag_filename", ragFilename)
			}
			rootSpan.SetAttr("toon_compressed", toonCompressed)
			rootSpan.SetAttr("toon_savings_tokens", savings)
			if upErr != nil {
				rootSpan.RecordError(upErr)
			}
		}
	})
}

// logCascadeTelemetry emits the route_attempted / attempts line for the
// cascade. Once issue #16 wires a real metrics store this becomes the
// place to publish a structured event; for now it is a structured slog
// event (issue #3).
func logCascadeTelemetry(res upstream.CascadeResult, err error, requestID string) {
	slog.Info("cascade result",
		slog.String("route_attempted", res.RouteAttempted),
		slog.Int("attempts", res.Attempts),
		slog.String("served_by", res.ServedBy),
		slog.Bool("success", res.Succeeded),
		slog.Any("err", err),
		slog.String("request_id", requestID),
	)
}

// writeJSONError writes a structured JSON error response. Used for the
// 413 body-cap overflow (issue #11) so clients get a parseable error
// rather than a plain-text body. The shape matches the OpenAI error
// envelope (`{"error":{"message":...,"type":...}}`) so existing
// OpenAI-compatible clients surface the message without changes.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"message": message,
			"type":    http.StatusText(status),
		},
	})
}

// requestID extracts (or generates) a correlation id for the judge
// hook. The handler honours an inbound X-Request-Id so a caller can
// thread its own id through; otherwise we mint a short hex token.
//
// The inbound value is sanitised (issue #39): characters outside
// [a-zA-Z0-9._:-] are stripped so a crafted header cannot inject log
// entries or break downstream JSON consumers, and the length is capped
// at requestIDMaxLen. When the value is empty after sanitization the
// handler falls through to a generated hex id.
func requestID(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" {
		if clean := sanitizeRequestID(v); clean != "" {
			return clean
		}
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail on linux; fall back to a
		// static marker so downstream logging still has *some*
		// correlation id.
		return "req-unknown"
	}
	return "req-" + hex.EncodeToString(b[:])
}

// requestIDMaxLen caps the length of an inbound X-Request-Id. Longer
// values are truncated so a malicious client cannot bloat logs or the
// telemetry row with an arbitrarily long correlation id (issue #39).
const requestIDMaxLen = 128

// requestIDDisallowedRe matches characters NOT permitted in a sanitized
// request id. The allowed set is [a-zA-Z0-9._:-]; any other character
// (newlines, control bytes, quotes, angle brackets, whitespace, ...) is
// stripped so a crafted X-Request-Id cannot inject log entries or break
// downstream JSON consumers (issue #39).
var requestIDDisallowedRe = regexp.MustCompile(`[^a-zA-Z0-9._:-]`)

// sanitizeRequestID strips characters outside [a-zA-Z0-9._:-] and caps
// the length at requestIDMaxLen. Returns "" when the input is empty
// after sanitization, so the caller falls through to a generated hex id.
func sanitizeRequestID(s string) string {
	s = requestIDDisallowedRe.ReplaceAllString(s, "")
	if len(s) > requestIDMaxLen {
		s = s[:requestIDMaxLen]
	}
	return s
}

// captureWriter is an http.ResponseWriter that tees every Write into
// a bounded in-memory buffer while passing through to the underlying
// writer. It implements http.Flusher so upstream.Stream's flush
// behaviour is preserved. The buffer cap is a hard ceiling: writes
// past the cap are still forwarded to the client but the buffer
// silently stops growing.
type captureWriter struct {
	w        http.ResponseWriter
	cap      int
	buf      strings.Builder
	overflow bool
}

// newCaptureWriter wires a captureWriter around w with the given
// hard byte cap (use 0 to default).
func newCaptureWriter(w http.ResponseWriter, capBytes int) *captureWriter {
	if capBytes <= 0 {
		capBytes = DefaultObservedCap
	}
	return &captureWriter{w: w, cap: capBytes}
}

// Header implements http.ResponseWriter.
func (c *captureWriter) Header() http.Header { return c.w.Header() }

// Write implements http.ResponseWriter. The bytes are forwarded to
// the client unconditionally; they are also buffered up to the cap.
func (c *captureWriter) Write(b []byte) (int, error) {
	if !c.overflow && c.buf.Len()+len(b) <= c.cap {
		c.buf.Write(b)
	} else if !c.overflow {
		// Approaching the cap — stop buffering but keep passing
		// through. The observer will see a truncated body; that's
		// better than OOMing the proxy.
		c.overflow = true
	}
	return c.w.Write(b)
}

// WriteHeader implements http.ResponseWriter.
func (c *captureWriter) WriteHeader(s int) { c.w.WriteHeader(s) }

// Flush forwards to the underlying writer if it supports flushing.
// The chat handler relies on flushes for SSE; the judge capture must
// not regress that behaviour.
func (c *captureWriter) Flush() {
	if f, ok := c.w.(http.Flusher); ok {
		f.Flush()
	}
}

// Buffer returns the bytes captured so far. Safe to call after the
// upstream Stream has returned.
func (c *captureWriter) Buffer() string { return c.buf.String() }

// Compile-time assertion: captureWriter satisfies http.ResponseWriter
// and the optional Flusher interface upstream.Stream requires.
var (
	_ http.ResponseWriter = (*captureWriter)(nil)
	_ http.Flusher        = (*captureWriter)(nil)
)

// qualityEditNames is the set of tool names the handler treats as
// "this might have written a file". Mirrors internal/quality so the
// two stay in sync; the handler intentionally does not import the
// quality package (matching the AGENTS.md dependency rule for judge).
//
// Detection is liberal on purpose: the verifier's project-detection
// step filters false positives (e.g. a write_file on a path inside a
// non-project directory scores 0 but does not fail).
var qualityEditNames = []string{
	"write_file",
	"edit_file",
	"apply_patch",
	"create_file",
	"patch_file",
	"update_file",
	"write",
	"edit",
	"patch",
}

// qualityEditNameRe anchors on a known edit tool name inside a JSON
// payload. Operates on the post-stripJSONEscapes window, so the
// regex itself is plain — escape tolerance isn't necessary.
var qualityEditNameRe = regexp.MustCompile(
	`"name"\s*:\s*"(?:` + joinBars(qualityEditNames) + `)"`,
)

// qualityPathRe matches the first path-shaped field in a window.
// Captures the bare value with JSON-quote termination.
var qualityPathRe = regexp.MustCompile(
	`"(?:path|filePath|file_path|filepath)"\s*:\s*"([^"]+)"`,
)

// qualityPathWindowBytes bounds how far past a tool-name match we
// look for the path field. 4 KiB is comfortably larger than any real
// OpenCode tool call.
const qualityPathWindowBytes = 4 * 1024

// emitDetectedEdits scans body for tool-call envelopes whose name
// matches qualityEditNames and forwards one QualityEvent per unique
// path via obs. Body is the captured upstream response (already
// bounded by maxObservedBytes); obs must be safe to call from many
// goroutines — the verifier does its own queueing.
//
// The function is O(len(body)) and best-effort: malformed JSON,
// missing fields, or windows that don't contain a path field are
// silently skipped. Deduplication is per-call, by path.
func emitDetectedEdits(body, reqID string, obs QualityObserver) {
	if obs == nil || body == "" {
		return
	}
	cleaned := stripJSONEscapes(body)
	seen := make(map[string]bool, 4)
	for _, idxs := range qualityEditNameRe.FindAllStringIndex(cleaned, -1) {
		start := idxs[0]
		end := start + qualityPathWindowBytes
		if end > len(cleaned) {
			end = len(cleaned)
		}
		window := cleaned[start:end]
		path := firstPath(window)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		obs.Submit(QualityEvent{
			RequestID: reqID,
			Path:      path,
			ToolName:  "edit", // best-effort label; verifier doesn't depend on the exact name
		})
	}
}

// stripJSONEscapes reverses the small subset of escape sequences a
// tool-call envelope might carry after JSON encoding. We don't pull
// in encoding/json because that would force the handler to
// round-trip the whole response body to find a handful of patterns.
// File paths essentially never contain the other escape targets (\n,
// \t, etc.) — the few edge cases that slip through are caught at
// project-detection time and score 0.
func stripJSONEscapes(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '\\' && i+1 < len(s) {
			next := s[i+1]
			if next == '"' || next == '\\' || next == '/' {
				b.WriteByte(next)
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// firstPath pulls the first plausible path-looking field out of
// window. Exported as a small package-level helper so chat_test.go
// can call it without re-implementing the regex.
func firstPath(window string) string {
	if m := qualityPathRe.FindStringSubmatch(window); len(m) >= 2 && m[1] != "" {
		return m[1]
	}
	return ""
}

// joinBars concatenates names with "|" for use inside a regex
// alternation. stdlib does not export the equivalent helper from
// regexp so we keep the trivial inline version rather than pull in
// strings.Join (which would still need a trim on the trailing bar).
func joinBars(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "|"
		}
		out += p
	}
	return out
}

func buildRecord(
	requestID string,
	started time.Time,
	firstWriteNano int64,
	bytesOut uint64,
	streaming bool,
	route router.Route,
	model, latestPrompt string,
	upErr error,
) telemetry.Record {
	totalMs := time.Since(started).Milliseconds()
	var ttftMs int64
	if streaming && firstWriteNano > 0 {
		ttftMs = time.Unix(0, firstWriteNano).Sub(started).Milliseconds()
		if ttftMs < 0 {
			ttftMs = 0
		}
	}
	outputTokens := int(bytesOut / 4)
	rec := telemetry.Record{
		Timestamp:      time.Now().UTC(),
		RequestID:      requestID,
		Model:          model,
		Route:          string(route),
		InputTokens:    telemetry.EstimateTokens(latestPrompt),
		OutputTokens:   outputTokens,
		TTFTMs:         ttftMs,
		TotalLatencyMs: totalMs,
		Streaming:      streaming,
	}
	rec.TPS = telemetry.ComputeTPS(outputTokens, ttftMs, totalMs)
	if upErr != nil {
		rec.Error = upErr.Error()
	}
	return rec
}

// --- metrics (issue #4) helpers ----------------------------------------

// totalMessageChars marshals messages and returns the JSON byte
// length. Used to compute the TOON savings token estimate. Mirrors
// json.Marshal's own overflow behaviour (returns roughly the same
// bytes the proxy would emit across the wire).
func totalMessageChars(messages []interface{}) int {
	b, err := json.Marshal(messages)
	if err != nil {
		return 0
	}
	return len(b)
}

// totalTokenSavings returns the tokens saved by the TOON
// compression pass (pre - post character count, /4), clamped to
// zero in case the rewrite expanded the message (which can happen
// when the schema header outweighs the value rows for tiny inputs).
func totalTokenSavings(preChars, postChars int) int {
	if preChars <= postChars {
		return 0
	}
	return (preChars - postChars) / 4
}

// frontierCostEstimate multiplies input tokens by the per-provider
// cost-per-1k (issue #43) when the model matches a configured
// provider, otherwise by the fallback cost-per-1k. Returns zero for
// non-frontier routes so local + fusion-trail rows count as zero
// cost in the dashboard.
//
// The fallback knob is Config.JudgeCostPer1KUSD — re-used for lack
// of a separate env var until cost-aware routing (issue #44) lands.
// When a provider has its own InputCostPer1K > 0 (set via
// NEXUS_PROVIDER_<NAME>_INPUT_COST_PER_1K), that rate wins so each
// frontier's contribution to the rolling 24h spend is honest.
func frontierCostEstimate(route, model string, inputTokens int, reg providers.Registry, fallbackCostPer1KUSD float64) float64 {
	if route != string(router.RouteFrontier) {
		return 0
	}
	if inputTokens <= 0 {
		return 0
	}
	costPer1K := fallbackCostPer1KUSD
	if p, ok := reg.ByModel(model); ok && p.InputCostPer1K > 0 {
		costPer1K = p.InputCostPer1K
	}
	if costPer1K <= 0 {
		return 0
	}
	return float64(inputTokens) * costPer1K / 1000.0
}
