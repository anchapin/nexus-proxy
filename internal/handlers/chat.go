// Package handlers contains the HTTP entry points for the proxy. The chat
// handler is the only public endpoint; it owns the request lifecycle:
// middleware chain -> routing decision -> upstream execution.
package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anchapin/nexus-proxy/internal/circuit"
	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/health"
	"github.com/anchapin/nexus-proxy/internal/middleware"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/telemetry"
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

// RouteDecisionEvent is emitted to the RouteDecisionObserver hook
// once per proxied request, immediately after the planner returns
// its Decision (issue #74). It mirrors the same fields the handler
// stamps on the X-Nexus-Route-* response headers so the observer and
// the response surface always agree. Observers use this for
// Prometheus counters, dashboards, and any other out-of-band metric
// that needs to attribute a route to its planning stage without
// re-running the planner.
type RouteDecisionEvent struct {
	RequestID  string
	Route      string
	Source     string
	Reason     string
	Confidence float64
	TaskType   string
	// CacheHit is true when the decision came from the SLM cache
	// (issue #206) — a duplicate prompt was served from cache without
	// calling the SLM.
	CacheHit bool
}

// RouteDecisionObserver is the hook invoked once per proxied request
// after the planner returns. Implementations must be safe for
// concurrent use from many goroutines; the handler invokes Observe
// on the same request goroutine so observers should not block for
// long (a few atomic increments is fine; a network call is not).
// Nil is treated as "no observer"; the hot path is unaffected.
type RouteDecisionObserver interface {
	Observe(RouteDecisionEvent)
}

// RouteDecisionObserverFunc adapts a plain function to the
// RouteDecisionObserver interface so wiring from main.go stays a
// one-liner.
type RouteDecisionObserverFunc func(RouteDecisionEvent)

// Observe implements RouteDecisionObserver.
func (f RouteDecisionObserverFunc) Observe(e RouteDecisionEvent) { f(e) }

// Rejection reason constants (issue #119). These are the label values
// that appear in the nexus_requests_rejected_total{reason} Prometheus
// family. They are exported so the rate-limit middleware (which lives
// in a separate package) and main.go's wiring can reference the same
// vocabulary without importing internal/observability — the strings
// flow through the RejectionObserver hook.
const (
	// RejectionMethod marks a 405 Method Not Allowed response.
	RejectionMethod = "method"
	// RejectionBodyTooLarge marks a 413 Payload Too Large response
	// (request body exceeded NEXUS_MAX_BODY_BYTES).
	RejectionBodyTooLarge = "body_too_large"
	// RejectionBadRequest marks a generic 400 Bad Request response
	// (invalid JSON, missing messages array, unreadable body,
	// strict-mode prompt-injection rejection).
	RejectionBadRequest = "bad_request"
	// RejectionRateLimit marks a 429 Too Many Requests response
	// emitted by the rate-limit middleware. The reason is recorded
	// by the middleware hook wired in main.go.
	RejectionRateLimit = "rate_limit"
)

// RejectionEvent carries the minimal context a rejection observer
// needs: which request was rejected and why. The handler dispatches
// one per early-return path (issue #119) so the Prometheus counter
// and any out-of-band sink can report rejection rate alongside
// nexus_requests_total.
type RejectionEvent struct {
	RequestID string
	Reason    string
}

// RejectionObserver is the hook invoked once per request that the
// proxy rejects before it reaches an upstream (405 / 413 / 400 / 429).
// Same invariants as the other observer hooks: safe for concurrent
// use, must not block, nil means "no observer". The handler does not
// import the observability package; main.go wires a closure that
// forwards to RouteCounters.ObserveRejection.
type RejectionObserver interface {
	ObserveRejection(RejectionEvent)
}

// RejectionObserverFunc adapts a plain function to the
// RejectionObserver interface.
type RejectionObserverFunc func(RejectionEvent)

// ObserveRejection implements RejectionObserver.
func (f RejectionObserverFunc) ObserveRejection(e RejectionEvent) { f(e) }

// FusionOutcomeEvent carries the outcome of a fusion panel after
// PanelStreaming returns (issue #187). It lets operators compute the
// fusion agreement rate: skipped/(skipped+invoked).
type FusionOutcomeEvent struct {
	RequestID      string
	ArbiterSkipped bool
}

// FusionOutcomeObserver is the hook invoked once per fusion request
// with the PanelStreaming outcome (issue #187). Safe for concurrent
// use, must not block, nil means "no observer". The handler does not
// import the observability package; main.go wires a closure that
// forwards to RouteCounters.ObserveFusionOutcome.
type FusionOutcomeObserver interface {
	ObserveFusionOutcome(FusionOutcomeEvent)
}

// FusionOutcomeObserverFunc adapts a plain function to the
// FusionOutcomeObserver interface.
type FusionOutcomeObserverFunc func(FusionOutcomeEvent)

// ObserveFusionOutcome implements FusionOutcomeObserver.
func (f FusionOutcomeObserverFunc) ObserveFusionOutcome(e FusionOutcomeEvent) { f(e) }

// RAGEvent carries the outcome of a single RAG retrieval attempt
// (issue #186). Hit is true when Retrieve returned a non-nil example;
// Filename is the matched snippet (meaningful only when Hit is true).
// MissReason is the miss cause when Hit is false: "empty_store" when
// the store has no indexed examples, "threshold" when the best match
// fell below the similarity floor, or "embed_error" when the embedding
// call failed.
type RAGEvent struct {
	Hit        bool
	Filename   string
	MissReason string
}

// RAGObserver is the hook invoked once per RAG retrieval attempt,
// allowing the observability layer to record hit/miss rates (issue #186).
// Safe for concurrent use; must not block. Nil is a valid no-op.
type RAGObserver interface {
	ObserveRAG(RAGEvent)
}

// RAGObserverFunc adapts a plain function to the RAGObserver interface.
type RAGObserverFunc func(RAGEvent)

// ObserveRAG implements RAGObserver.
func (f RAGObserverFunc) ObserveRAG(e RAGEvent) { f(e) }

// CascadeFallbackEvent carries the reason for a cascade fallback (issue #205).
// The handler dispatches one when a retryable step failure causes the cascade
// to fall back to the next step. The reason is one of "timeout",
// "transport_error", or "malformed_toolcall".
type CascadeFallbackEvent struct {
	RequestID string
	Reason    string // "timeout", "transport_error", or "malformed_toolcall"
}

// CascadeFallbackObserver is the hook invoked when the cascade falls back
// to a later step due to a retryable error (issue #205). Implementations
// must be safe for concurrent use and must not block. Nil means "no
// observer" — the hot path is unaffected. The handler does not import
// the observability package; main.go wires a closure that forwards to
// RouteCounters.ObserveCascadeFallback.
type CascadeFallbackObserver interface {
	ObserveCascadeFallback(CascadeFallbackEvent)
}

// CascadeFallbackObserverFunc adapts a plain function to the
// CascadeFallbackObserver interface.
type CascadeFallbackObserverFunc func(CascadeFallbackEvent)

// ObserveCascadeFallback implements CascadeFallbackObserver.
func (f CascadeFallbackObserverFunc) ObserveCascadeFallback(e CascadeFallbackEvent) { f(e) }

// MetricsEvent carries the per-request data needed by the savings
// dashboard (issue #4). Fields track the full round-trip metrics:
// route/model/input-tokens are routing dimensions; TOON/RAG/cost are
// the savings dimensions the dashboard renders. The handler builds
// one of these after every proxied request (success, failure, or
// short-circuit) and dispatches it to the configured MetricsObserver.
//
// Mirrors internal/metrics.Request so a tiny adapter in main.go can
// forward directly without translation — kept here as a separate
// type so the handlers package stays free of the metrics import.
//
// FusionArbiterSkipped (issue #48) is true only for route=fusion
// requests that streamed the speculative panel-member answer and
// terminated without invoking the arbiter (panel agreement, or one
// member failed). False in every other case so the dashboard can
// report the "fusion agreement rate" as a single ratio.
//
// RouteSource / RouteReason / SLMConfidence / SLMTaskType (issue #74)
// carry the planner's Decision metadata. RouteSource is one of
// guardrail / dsl / slm / slm-error / escalation; RouteReason is the
// short machine-readable detail; SLMConfidence is the [0,1]
// confidence value the SLM was called with (0.5 neutral, 0 for
// non-SLM stages); SLMTaskType is the Categorize() bucket. The
// dashboard joins these to answer "what fraction of frontier traffic
// came from a low-confidence SLM escalation?".
type MetricsEvent struct {
	Timestamp             time.Time
	RequestID             string
	Route                 string
	Model                 string
	InputTokens           int
	TOONSavingsTokens     int
	TOONCompressionMethod string // issue #247: "fenced", "nested", or "" (no compression)

	RAGInjected      bool
	RAGFilename      string
	EstimatedCostUSD float64

	// BaselineCostUSD is what the request would have cost at the
	// configured frontier baseline rate (issue #73). SavingsUSD is
	// max(BaselineCostUSD - EstimatedCostUSD, 0).
	BaselineCostUSD float64
	SavingsUSD      float64

	OutputTokens            int
	TTFTMs                  int64
	TotalLatencyMs          float64
	TPS                     float64
	Streaming               bool
	FusionArbiterSkipped    bool
	FusionJaccardSimilarity float64
	// FusionArbiterCostUSD (issue #239): estimated cost of the arbiter
	// call when it ran. 0 when the arbiter was skipped or for non-fusion routes.
	FusionArbiterCostUSD    float64
	ToolCallCount          int
	Error                   string

	RouteSource   string
	RouteReason   string
	SLMConfidence float64
	SLMTaskType   string
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

// LocalLimiter bounds the number of concurrent local-route requests
// the proxy issues against the local Ollama instance (issue #81).
// Acquire blocks until a slot is available or ctx is cancelled; on
// success it returns a release function the caller MUST invoke exactly
// once when the local upstream work is done. On cancellation it returns
// a non-nil error and a nil release so the caller skips the dispatch.
//
// Implementations must be safe for concurrent use by many goroutines
// (the chat handler is itself concurrent). The concrete implementation
// lives in internal/concurrencylimit; main.go wires it so the handlers
// package never imports that package (same dependency direction as the
// judge / quality / probe observers).
//
// A nil LocalLimiter means "no limit" — the pre-#81 unlimited behaviour
// — so unit tests and deployments that leave NEXUS_LOCAL_MAX_CONCURRENT
// unset are byte-for-byte identical.
type LocalLimiter interface {
	Acquire(ctx context.Context) (release func(), err error)
}

// Deps bundles the collaborators the chat handler needs. Wiring them
// explicitly makes the handler trivial to unit-test with stubs.
type Deps struct {
	Config          config.Config
	Client          upstream.Client // http.Client satisfies this interface
	RAG             rag.RAGStore
	SLM             *router.SLMClient
	FormattingRegex *regexp.Regexp

	// Confidence is the optional judge-guided adaptive routing store
	// (issue #47). When non-nil the handler categorizes the prompt,
	// looks up the local model's historical confidence for that
	// category, and passes it to the SLM via DecideWithConfidence so a
	// low-confidence category biases toward frontier. When nil (the
	// default, and always when the judge is disabled) the handler uses
	// the plain neutral Decide path — routing is byte-for-byte identical
	// to the pre-issue-47 behaviour. The handler talks to router, never
	// judge; main.go bridges JudgeScore -> RecordOutcome.
	Confidence router.ConfidenceStore

	// SLMCache is the optional time-bounded prompt→route cache
	// (issue #206). When non-nil the planner checks the cache before
	// calling the SLM; a cache hit returns the cached route without
	// calling the SLM. When nil the SLM is always called
	// (pre-cache behaviour). The cache TTL is configured at cache
	// construction time via NewSLMCache.
	SLMCache *router.SLMCache

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

	// LocalLimiter bounds concurrent local-route requests (issue
	// #81). When non-nil the handler Acquires a slot before issuing
	// the local upstream dispatch and Releases it once the dispatch
	// returns. The effective slot count is min(ceiling, freeVRAM/
	// bytesPerSlot) recomputed from the latest probe snapshot on
	// every Acquire, so a thermal-throttle or model-swap event that
	// drops free VRAM throttles the next request automatically. Nil
	// means "no limit" (pre-#81 unlimited behaviour); the field is
	// left nil by Chat() when the operator did not set
	// NEXUS_LOCAL_MAX_CONCURRENT so the hot path is unchanged.
	LocalLimiter LocalLimiter

	// LocalCooldown arms a short cooldown after the cascade detects
	// a local (Ollama) failure (issue #80). While active, subsequent
	// route=local and route=fusion requests skip the local step
	// entirely (same path as the health-unhealthy skip) and are
	// served by the fallback route. The response is stamped with
	// X-Nexus-Local-Cooldown: true so clients can detect the
	// degradation. Nil means the circuit is disabled
	// (NEXUS_LOCAL_COOLDOWN=0); the hot path is byte-for-byte
	// identical to the pre-#80 behaviour.
	LocalCooldown *circuit.Cooldown

	// RouteDecisionObserver is invoked once per proxied request
	// with the planner's Decision metadata (issue #74). Implementations
	// must be safe for concurrent use; the handler invokes the
	// observer on the request goroutine right after planner.Plan
	// returns, so the observer should not block for long. Nil is
	// treated as "no observer"; the hot path is unaffected. The
	// handler does not import the observability package — main.go
	// wires a closure that adapts the event to the in-process
	// route counter.
	RouteDecisionObserver RouteDecisionObserver

	// RejectionObserver is invoked once per request that the proxy
	// rejects before it reaches an upstream (issue #119): 405 method,
	// 413 body-too-large, 400 bad-request, and strict-mode
	// prompt-injection rejections. The rate-limit middleware's 429
	// path is wired separately in main.go because it fires before
	// the chat handler. Implementations must be safe for concurrent
	// use and must not block. Nil is treated as "no observer"; the
	// hot path is unaffected. The handler does not import the
	// observability package — main.go wires a closure that adapts
	// the event to the in-process rejection counter.
	RejectionObserver RejectionObserver

	// FusionOutcomeObserver is invoked once per fusion request after
	// PanelStreaming returns, with the arbiter outcome (issue #187).
	// Safe for concurrent use, must not block, nil is "no observer".
	// The handler does not import observability; main.go wires a
	// closure that forwards to RouteCounters.ObserveFusionOutcome.
	FusionOutcomeObserver FusionOutcomeObserver

	// RAGObserver is invoked once per RAG retrieval attempt (issue #186)
	// with the hit/miss outcome so the observability layer can record
	// RAG effectiveness metrics. Implementations must be safe for
	// concurrent use and must not block. Nil is treated as "no
	// observer"; the hot path is unaffected. The handler does not
	// import the observability package — main.go wires a closure
	// that adapts the event to the in-process RAG counter.
	RAGObserver RAGObserver

	// CascadeFallbackObserver is invoked when the cascade falls back to a
	// later step due to a retryable error (issue #205). The handler
	// dispatches exactly one event per request when FallbackReason is
	// non-empty (i.e., the cascade fell back at least once). The
	// reason is one of "timeout", "transport_error", or
	// "malformed_toolcall". Implementations must be safe for concurrent
	// use and must not block. Nil means "no observer"; the hot path is
	// unaffected. The handler does not import the observability package —
	// main.go wires a closure that forwards to
	// RouteCounters.ObserveCascadeFallback.
	CascadeFallbackObserver CascadeFallbackObserver

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
//
// Issue #119: requests the proxy REJECTS before routing (405 method,
// 413 body-too-large, 400 bad-request, strict-mode prompt-injection)
// also emit exactly one record (route="rejected") AND fire the
// RejectionObserver so the nexus_requests_rejected_total{reason}
// Prometheus counter stays in sync with the response surface. The
// rate-limit middleware's 429 path is instrumented separately (it fires
// before this handler) via a hook wired in main.go.
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

		// Debug trace scaffolding (issue #33). Allocated up front so
		// each lifecycle step can populate its sub-trace; emitted as
		// a single batch at the very end. The trace variable is a
		// single DebugTrace value — not a pointer — so the hot path
		// (Debug=false) never allocates at all. The conditional at
		// emit time keeps the production cost at zero.
		var trace DebugTrace
		trace.Request.RequestID = reqID
		trace.Request.Stream = true // overridden after parse if harness says stream=false
		if d.Config.Debug {
			slog.Debug("[DEBUG] handler entered",
				slog.String("request_id", reqID),
			)
		}

		// recordRejection (issue #119) emits the terminal signal for
		// every early-return path: one telemetry Record (route=
		// "rejected") OR one MetricsEvent when the metrics store is
		// wired, plus one RejectionObserver dispatch so the
		// nexus_requests_rejected_total{reason} counter increments.
		// Centralising the recording here means every `return` below
		// that calls this closure honours the doc-comment promise
		// ("every request emits exactly one record"). The closure
		// captures started/reqID so callers only pass the reason.
		recordRejection := func(reason string) {
			totalMs := float64(time.Since(started).Microseconds()) / 1000.0
			ts := time.Now().UTC()
			if d.MetricsObserver != nil {
				d.MetricsObserver.Submit(MetricsEvent{
					Timestamp:      ts,
					RequestID:      reqID,
					Route:          "rejected",
					Error:          reason,
					TotalLatencyMs: totalMs,
				})
			} else {
				d.Recorder.Record(telemetry.Record{
					Timestamp:      ts,
					RequestID:      reqID,
					Route:          "rejected",
					TotalLatencyMs: totalMs,
					Error:          reason,
				})
			}
			if d.RejectionObserver != nil {
				d.RejectionObserver.ObserveRejection(RejectionEvent{
					RequestID: reqID,
					Reason:    reason,
				})
			}
		}

		if r.Method != http.MethodPost {
			recordRejection(RejectionMethod)
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
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
				recordRejection(RejectionBodyTooLarge)
				writeJSONError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
					"Request body exceeds NEXUS_MAX_BODY_BYTES (%d bytes)", maxBytes))
				slog.Warn("rejected oversized request",
					slog.String("remote", r.RemoteAddr),
					slog.Int("limit_bytes", maxBytes),
					slog.String("request_id", reqID),
				)
				return
			}
			recordRejection(RejectionBadRequest)
			http.Error(w, "Failed to read request", http.StatusBadRequest)
			return
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			recordRejection(RejectionBadRequest)
			http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
			return
		}
		rawMessages, ok := body["messages"].([]interface{})
		if !ok {
			recordRejection(RejectionBadRequest)
			http.Error(w, "Invalid or missing messages array", http.StatusBadRequest)
			return
		}

		// Capture the inbound shape for the debug trace BEFORE any
		// middleware mutates messages. Populating these fields is
		// cheap (a few assignments) so we do it unconditionally; the
		// DebugTrace.Emit call is the only path that actually reads
		// them, and that's gated by d.Config.Debug below.
		trace.Request.Messages = len(rawMessages)
		trace.Request.InboundBodyBytes = len(bodyBytes)
		if m, ok := body["model"].(string); ok {
			trace.Request.ModelRequested = m
		}
		if s, ok := body["stream"].(bool); ok {
			trace.Request.Stream = s
		}

		// Prompt-injection hardening (issue #76). In warn and strict
		// modes the proxy scans the user-supplied system messages for
		// suspicious override patterns BEFORE applying its own policy.
		// Strict mode rejects the request with a 400 OpenAI-style
		// error; warn mode logs and continues. Off mode (default)
		// skips detection entirely for zero overhead.
		if d.Config.PromptInjectionIsolated() {
			hits := middleware.DetectSuspiciousSystem(rawMessages)
			if len(hits) > 0 {
				if d.Config.PromptInjectionMode == middleware.InjectionModeStrict {
					recordRejection(RejectionBadRequest)
					writeJSONError(w, http.StatusBadRequest,
						"Request rejected: suspicious prompt-injection pattern detected in system message")
					slog.Warn("strict mode rejected suspicious system message",
						slog.Int("patterns", len(hits)),
						slog.String("request_id", reqID),
					)
					return
				}
				middleware.LogSuspicious(hits, reqID)
			}
		}

		// Apply prompt engineering. In off mode (default) the legacy
		// append path is used — byte-for-byte backward compatible.
		// In warn/strict modes the isolated variant inserts a dedicated
		// leading system message wrapped in proxy-policy delimiters so
		// trusted text always precedes user-supplied system content.
		var messages []interface{}
		if d.Config.PromptInjectionIsolated() {
			messages = middleware.ApplyPromptEngineeringIsolated(rawMessages, d.Config.MetaPrompt)
		} else {
			messages = middleware.ApplyPromptEngineering(rawMessages, d.Config.MetaPrompt)
		}
		latestPrompt := middleware.ExtractLatestUserPrompt(messages)
		// Record whether the meta-prompt actually appended to a
		// system slot — "applied" only when there is now a system
		// message containing the operator-configured enhancement.
		trace.Transforms.PromptEngineeringApplied =
			d.Config.MetaPrompt != "" && len(messages) > 0 &&
				containsSystemWith(messages, d.Config.MetaPrompt)
		var ragInjected bool
		var ragFilename string
		var ragScore float64
		ragEx, ragScore, ragErr := d.RAG.Retrieve(r.Context(), latestPrompt)
		switch {
		case ragErr != nil:
			slog.Info("rag miss",
				slog.String("reason", "embed_error"),
				slog.String("request_id", reqID),
			)
			if d.RAGObserver != nil {
				d.RAGObserver.ObserveRAG(RAGEvent{Hit: false, MissReason: "embed_error"})
			}
		case ragEx != nil:
			messages = middleware.InjectRAG(messages, rag.FormatInjection(ragEx))
			slog.Info("rag hit",
				slog.String("filename", ragEx.Filename),
				slog.Float64("score", ragScore),
				slog.String("request_id", reqID),
			)
			ragInjected = true
			ragFilename = ragEx.Filename
			if d.RAGObserver != nil {
				d.RAGObserver.ObserveRAG(RAGEvent{Hit: true, Filename: ragEx.Filename})
			}
		case d.RAG.Size() == 0:
			slog.Info("rag miss",
				slog.String("reason", "empty_store"),
				slog.String("request_id", reqID),
			)
			if d.RAGObserver != nil {
				d.RAGObserver.ObserveRAG(RAGEvent{Hit: false, MissReason: "empty_store"})
			}
		default:
			slog.Info("rag miss",
				slog.String("reason", "threshold"),
				slog.Float64("score", ragScore),
				slog.String("request_id", reqID),
			)
			if d.RAGObserver != nil {
				d.RAGObserver.ObserveRAG(RAGEvent{Hit: false, MissReason: "threshold"})
			}
		}
		trace.Transforms.RAGInjected = ragInjected
		trace.Transforms.RAGFilename = ragFilename
		trace.Transforms.RAGScore = ragScore
		// Snapshot the JSON size BEFORE TOON compression so the
		// metrics observer can attribute tokens saved by the
		// round-trip pass. Uses the cheap "4 chars per token"
		// heuristic the rest of the project uses for telemetry
		// (see internal/telemetry.EstimateTokens).
		preCompressionChars := totalMessageChars(messages)
		trace.Transforms.TOONBytesBefore = preCompressionChars
		toonCompressionMethod := middleware.CompressJSONBlocks(messages)
		if toonCompressionMethod != "" {
			if d.Config.PromptInjectionIsolated() {
				messages = middleware.AppendSystemNoteIsolated(messages, d.Config.TOONNotice)
			} else {
				messages = middleware.AppendSystemNote(messages, d.Config.TOONNotice)
			}
			slog.Info("toon compressed messages",
				slog.String("request_id", reqID),
				slog.String("method", string(toonCompressionMethod)))
		}
		postCompressionChars := totalMessageChars(messages)
		trace.Transforms.TOONApplied = postCompressionChars != preCompressionChars
		trace.Transforms.TOONBytesAfter = postCompressionChars
		trace.Transforms.TOONTokensSaved = totalTokenSavings(preCompressionChars, postCompressionChars)
		body["messages"] = messages
		latestPrompt = middleware.ExtractLatestUserPrompt(messages)
		// Promote the post-middleware prompt length into the
		// request trace so operators can see what the SLM/router
		// actually consumed. The pre-middleware count lives on the
		// TransformTrace; the post-middleware count lives on the
		// RequestTrace because it is what every later stage sees.
		trace.Request.EstimatedTokens = len(latestPrompt) / 4

		// --- routing decision (issue #82) ---------------------------------
		//
		// The full decision ladder (Guardrail -> DSL -> SLM, with the
		// judge-guided confidence bias) now lives in router.Planner.
		// The handler resolves the VRAM budget from the BudgetObserver
		// (or the static config fallback), delegates the decision to
		// the planner, and interprets the structured Decision to emit
		// slog lines and populate the debug trace. This keeps the
		// planner pure (no HTTP, no logging) and gives the handler a
		// single call site for routing changes.

		// VRAM-aware guardrail budget (issue #6). When the
		// BudgetObserver is wired and reports a positive budget, the
		// dynamic measurement wins; otherwise we fall back to the
		// operator-configured NEXUS_TOKEN_GUARDRAIL. The source label
		// travels with the log line so operators can confirm which
		// path was taken without digging through the source.
		guardrailBudget := d.Config.TokenGuardrail
		guardrailSource := "static-fallback"
		if d.BudgetObserver != nil {
			if dyn := d.BudgetObserver.BudgetTokens(); dyn > 0 {
				guardrailBudget = dyn
				guardrailSource = d.BudgetObserver.BudgetSource()
			}
		}

		planner := &router.Planner{
			SLM:             d.SLM,
			Confidence:      d.Confidence,
			FormattingRegex: d.FormattingRegex,
			SLMCache:        d.SLMCache,
		}
		decision := planner.Plan(router.PlanRequest{
			Prompt:          latestPrompt,
			GuardrailBudget: guardrailBudget,
			GuardrailSource: guardrailSource,
			Context:         r.Context(),
		})
		route := decision.Route

		// Surface route-decision metadata on the response and via the
		// observer hook (issue #74). The four X-Nexus-Route-* headers
		// let clients and intermediate proxies reason about routing
		// without scraping logs. Each value is sanitized against
		// header-injection and bounded by MaxHeaderValue so the SLM
		// error string (which can echo attacker-influenced text) is
		// safe to include. The RouteDecisionObserver hook keeps the
		// in-process counters (Prometheus text exposition in
		// internal/observability) in sync with the response surface
		// — the source/escalation story the issue calls out is only
		// complete when both ship together.
		routeEvent := RouteDecisionEvent{
			RequestID:  reqID,
			Route:      string(decision.Route),
			Source:     string(decision.Source),
			Reason:     decision.Reason,
			Confidence: decision.Confidence,
			TaskType:   decision.TaskType,
			CacheHit:   decision.CacheHit,
		}
		w.Header().Set("X-Nexus-Route", SanitizeHeaderValue(routeEvent.Route))
		w.Header().Set("X-Nexus-Route-Source", SanitizeHeaderValue(routeEvent.Source))
		w.Header().Set("X-Nexus-Route-Reason", SanitizeHeaderValue(routeEvent.Reason))
		w.Header().Set("X-Nexus-Route-Confidence", SanitizeHeaderValue(formatConfidence(routeEvent.Confidence)))
		if d.RouteDecisionObserver != nil {
			d.RouteDecisionObserver.Observe(routeEvent)
		}

		// Emit the slog lines the pre-extraction handler produced, so
		// existing log-scraping tests and operator dashboards keep
		// working unchanged. The planner is log-free; the handler owns
		// the observability surface.
		switch decision.Source {
		case router.SourceGuardrail:
			slog.Info("guardrail forced frontier",
				slog.String("reason", "vram"),
				slog.String("budget_source", decision.BudgetSource),
				slog.Int("budget_tokens", decision.BudgetTokens),
				slog.Int("estimated_tokens", decision.EstimatedTokens),
				slog.String("request_id", reqID),
			)
		case router.SourceDSL:
			slog.Info("dsl match",
				slog.String("route", string(decision.Route)),
				slog.String("request_id", reqID),
			)
		default:
			// SLM path (success, error, or escalation). The pre-
			// extraction handler emitted these debug lines before the
			// SLM call; we emit them after the planner returns, which
			// is temporally equivalent for log purposes.
			slog.Debug("dsl bypassed, asking slm", slog.String("request_id", reqID))
			if d.Confidence != nil {
				slog.Debug("adaptive routing confidence",
					slog.String("category", decision.TaskType),
					slog.Float64("confidence", decision.Confidence),
					slog.String("request_id", reqID),
				)
			}
			if decision.Source == router.SourceSLMError {
				slog.Error("slm error, defaulting to frontier",
					slog.Any("err", errors.New(decision.SLMError)),
					slog.String("request_id", reqID),
				)
			}
			slog.Info("slm decision",
				slog.String("route", string(decision.Route)),
				slog.String("request_id", reqID),
			)
		}

		// Populate the debug trace from the Decision. The trace reason
		// uses the planner's source-to-reason mapping so the labels
		// ("guardrail", "dsl", "slm") are backward-compatible with the
		// pre-extraction handler.
		trace.Routing.Route = string(decision.Route)
		trace.Routing.Reason = decision.Source.TraceReason()
		trace.Routing.BudgetSource = decision.BudgetSource
		trace.Routing.BudgetTokens = decision.BudgetTokens
		trace.Routing.EstimatedTokens = decision.EstimatedTokens

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
		// Refresh the request trace's stream flag with the resolved
		// value (defaulted to true when the harness omitted it) so
		// the debug log reflects what the handler actually used.
		trace.Request.Stream = streaming

		// Wrap the response writer so we can capture TTFT and byte counts
		// without affecting upstream.Stream's flusher contract.
		var firstWriteAt atomic.Int64 // unix nano; 0 means "no write yet"
		obs := telemetry.NewObservingWriter(w, func(t time.Time) {
			firstWriteAt.CompareAndSwap(0, t.UnixNano())
		})

		// Graceful degradation (issue #8) + local-route cooldown
		// (issue #80). When the local Ollama health poller reports
		// unhealthy OR the cooldown circuit is active (armed by a
		// recent cascade-detected local failure) we:
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
		//
		// Issue #80: the cooldown closes the window between the
		// cascade observing a local failure and the health poller
		// tripping its breaker. Without it, every request in that
		// window re-attempts the dead local endpoint and pays the
		// full upstream timeout before falling back.
		localHealthy := d.Health.IsLocalHealthy()
		cooldownActive := d.LocalCooldown != nil && d.LocalCooldown.Active()
		skipLocal := (!localHealthy || cooldownActive) && (route == router.RouteLocal || route == router.RouteFusion)
		if skipLocal {
			w.Header().Set("X-Nexus-Degraded", "true")
			if cooldownActive {
				w.Header().Set("X-Nexus-Local-Cooldown", "true")
				slog.Warn("local-route cooldown active; skipping local arm",
					slog.String("route", string(route)),
					slog.String("request_id", reqID),
				)
			} else {
				slog.Warn("ollama unhealthy; skipping local arm",
					slog.String("route", string(route)),
					slog.String("request_id", reqID),
				)
			}
		} else {
			w.Header().Set("X-Nexus-Degraded", "false")
		}

		var model string
		var upErr error
		var fusionArbiterSkipped bool
		var fusionJaccardSimilarity float64
		// toolCallCount is populated from the cascade result on the
		// local streaming route (issue #72) and forwarded to telemetry
		// + metrics so the dashboard can report how many tool calls
		// were preserved. Zero on all other paths.
		var toolCallCount int
		// capw is the response-body captureWriter installed for the
		// local cascade branch when judge/quality observers OR
		// debug tracing are configured (issue #33). Hoisted out of
		// the RouteLocal case so the debug emit block at the end of
		// the handler can read the buffered body preview regardless
		// of which branch served the request. For route=frontier
		// the value is nil and the debug trace records an empty
		// body preview — the operator can still see the request id
		// and decide whether to enable captureWriter globally in a
		// follow-up.
		var capw *captureWriter
		switch route {
		case router.RouteFusion:
			slog.Info("starting fusion panel", slog.String("request_id", reqID))
			// Issue #48: progressive delivery. When the operator
			// has not opted out and the harness asked for SSE
			// (streaming=true), dispatch to PanelStreaming instead
			// of the legacy Panel. The streaming path races the
			// two panel members, streams the first to complete as a
			// speculative OpenAI-compatible SSE chunk, and only
			// invokes the arbiter when the second member
			// disagrees. The legacy Panel path remains unchanged
			// for stream=false / NEXUS_FUSION_PROGRESSIVE=false.
			//
			// Debug trace (issue #33): fusion dispatches to two
			// distinct upstreams (local + frontier) plus a
			// synthesis arbiter. The trace records the frontier
			// host as the "primary" target and notes both
			// panel members via the cascade-free routing — fusion
			// has no cascade, both panel members run in parallel.
			trace.Upstream.Route = string(route)
			trace.Upstream.Streaming = streaming
			trace.Upstream.Model = d.Config.FrontierModel
			trace.Upstream.TargetHost = HostOfURL(d.Config.FrontierURL)
			if streaming && d.Config.FusionProgressiveDelivery {
				var outcome upstream.PanelOutcome
				outcome, upErr = upstream.PanelStreaming(
					obs, d.Client,
					d.Config.OllamaURL, d.Config.LocalModel,
					d.Config.FrontierURL, d.Config.FrontierModel,
					d.Config.FrontierURL, d.Config.FrontierKey, d.Config.FrontierModel,
					body, latestPrompt, d.Config.FusionTimeout,
					d.Config.ArbiterTimeout,
					skipLocal,
					d.Config.FusionAgreementThreshold,
					reqID,
				)
				fusionArbiterSkipped = outcome.ArbiterSkipped
				fusionJaccardSimilarity = outcome.Similarity
				if outcome.ArbiterSkipped {
					slog.Info("fusion arbiter skipped",
						slog.String("source", outcome.Source),
						slog.Float64("similarity", outcome.Similarity),
						slog.String("request_id", reqID),
					)
				}
				if d.FusionOutcomeObserver != nil {
					d.FusionOutcomeObserver.ObserveFusionOutcome(FusionOutcomeEvent{
						RequestID:      reqID,
						ArbiterSkipped: outcome.ArbiterSkipped,
					})
				}
			} else {
				upErr = upstream.Panel(
					obs, d.Client,
					d.Config.OllamaURL, d.Config.LocalModel,
					d.Config.FrontierURL, d.Config.FrontierModel,
					d.Config.FrontierURL, d.Config.FrontierKey, d.Config.FrontierModel,
					body, latestPrompt, d.Config.FusionTimeout,
					d.Config.ArbiterTimeout,
					skipLocal,
				)
			}
			if upErr != nil {
				slog.Error("fusion error",
					slog.Any("err", upErr),
					slog.String("request_id", reqID),
				)
				http.Error(w, "Upstream error", http.StatusBadGateway)
			}
			model = d.Config.FrontierModel

		case router.RouteLocal:
			// VRAM-aware concurrency ceiling (issue #81). Bound the
			// number of in-flight local-route requests so a small GPU
			// does not OOM under bursty load. The limiter is optional;
			// nil means "no limit" (pre-#81 unlimited behaviour). On
			// acquire failure (ctx cancelled while queued) we surface a
			// 503 and skip the dispatch — the telemetry Record below
			// still fires so the rejected request is visible. A
			// successful acquire registers a deferred release so the
			// slot is freed once the local dispatch (streaming or
			// buffered) has fully drained.
			if d.LocalLimiter != nil {
				release, lerr := d.LocalLimiter.Acquire(r.Context())
				if lerr != nil {
					slog.Warn("local concurrency acquire rejected",
						slog.Any("err", lerr),
						slog.String("request_id", reqID),
					)
					model = d.Config.LocalModel
					upErr = lerr
					trace.Upstream.Route = string(route)
					trace.Upstream.Streaming = streaming
					trace.Upstream.Model = model
					trace.Upstream.TargetHost = HostOfURL(d.Config.OllamaURL)
					http.Error(w, "Local route busy", http.StatusServiceUnavailable)
					break
				}
				defer release()
			}
			body["model"] = d.Config.LocalModel

			// Cascade (issue #14): try local Ollama first, fall back to
			// configured frontier endpoints (frontier, then z.ai) on
			// retryable failures. Cascade is rebuilt per request so
			// config changes take effect without restarting the
			// process. CascadeConfig.SkipLocal (issue #8) omits the
			// local step entirely when the health poller reports
			// Ollama unreachable, so the cascade starts at frontier
			// without paying the local timeout. Hoisted above the
			// stream flag (issue #10) so both branches share a single
			// configured cascade; see below for the non-streaming
			// degraded override.
			cas := upstream.BuildLocalCascade(upstream.CascadeConfig{
				LocalURL:      d.Config.OllamaURL,
				LocalModel:    d.Config.LocalModel,
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
			//   upstream.Write -> captureWriter (judge + quality tee OR debug) ->
			//   ObservingWriter (telemetry byte count + TTFT + status) ->
			//   underlying ResponseWriter.
			// captureWriter is installed when at least one observer
			// is set OR debug tracing is on (issue #33); otherwise
			// the dispatch writes directly through obs with zero
			// overhead.
			rw := http.ResponseWriter(obs)
			if d.JudgeObserver != nil || d.QualityObserver != nil || d.Config.Debug {
				capw = newCaptureWriter(obs, d.maxObservedBytes)
				rw = capw
			}
			if streaming {
				// Cascade (issue #14): try local Ollama first, fall
				// back to configured frontier endpoints (frontier,
				// then z.ai) on retryable failures. SkipLocal (issue
				// #8) is honoured — when the health poller reports
				// Ollama unreachable the cascade skips the local step
				// entirely and starts at frontier.
				res, err := cas.Run(rw, d.Client, body)
				logCascadeTelemetry(res, err, reqID)
				// Issue #205: record cascade fallback metric when a retryable
				// step failure caused the cascade to fall back to the next
				// step. The observer is nil-safe; nil means no observability
				// wiring so the hot path is unaffected.
				if res.FallbackReason != "" && d.CascadeFallbackObserver != nil {
					d.CascadeFallbackObserver.ObserveCascadeFallback(CascadeFallbackEvent{
						RequestID: reqID,
						Reason:    res.FallbackReason,
					})
				}
				// Issue #80: arm the local-route cooldown when the
				// cascade reports the local step failed before a
				// fallback served the request. Subsequent requests
				// within the cooldown window skip local and go
				// directly to the fallback, avoiding repeated slow
				// local timeouts until the health poller catches up.
				if res.LocalStepFailed && d.LocalCooldown != nil {
					d.LocalCooldown.RecordFailure()
					slog.Warn("local-route cooldown armed after cascade local failure",
						slog.String("route_attempted", res.RouteAttempted),
						slog.String("served_by", res.ServedBy),
						slog.String("request_id", reqID),
					)
				}
				if err != nil {
					slog.Error("upstream error",
						slog.Any("err", err),
						slog.String("request_id", reqID),
					)
					upErr = err
					http.Error(w, "Upstream error", http.StatusBadGateway)
					// fall through: telemetry Record still fires
					// below so the failed request shows up in the
					// dashboard.
				} else {
					model = d.Config.LocalModel
					toolCallCount = len(res.ToolCalls)
					if res.Succeeded && capw != nil {
						if d.JudgeObserver != nil {
							d.JudgeObserver.Submit(LocalCompletion{
								RequestID:   reqID,
								Instruction: latestPrompt,
								Output:      capw.Buffer(),
								LocalModel:  d.Config.LocalModel,
							})
						}
						if d.QualityObserver != nil {
							emitDetectedEdits(capw.Buffer(), reqID, d.QualityObserver)
						}
					}
				}
				// Debug trace (issue #33): populate cascade details
				// once the cascade result is known. Steps come from
				// the joined RouteAttempted; the runner already
				// separated them with "->". The frontier host is the
				// canonical "target" because local has no host
				// exposed in the URL — operators reading the trace
				// get the most-actionable piece.
				trace.Upstream.Route = string(route)
				trace.Upstream.Streaming = streaming
				trace.Upstream.Model = model
				trace.Upstream.TargetHost = HostOfURL(d.Config.FrontierURL)
				if res.RouteAttempted != "" {
					trace.Upstream.CascadeSteps = strings.Split(res.RouteAttempted, "->")
				} else {
					trace.Upstream.CascadeSteps = stepNames(cas.Steps)
				}
				trace.Upstream.CascadeServedBy = res.ServedBy
				trace.Upstream.CascadeSuccess = res.Succeeded
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
				targetURL := strings.TrimRight(d.Config.OllamaURL, "/") + "/v1/chat/completions"
				apiKey := ""
				if skipLocal {
					targetURL = d.Config.FrontierURL
					apiKey = d.Config.FrontierKey
				}
				if err := upstream.BufferedFetch(rw, d.Client, targetURL, apiKey, body); err != nil {
					slog.Error("upstream error",
						slog.Any("err", err),
						slog.String("request_id", reqID),
					)
					upErr = err
					http.Error(w, "Upstream error", http.StatusBadGateway)
					// Issue #80: a non-streaming local fetch failure is
					// also a local failure — arm the cooldown so the next
					// request skips local and goes to the fallback.
					if !skipLocal && d.LocalCooldown != nil {
						d.LocalCooldown.RecordFailure()
					}
				} else {
					model = d.Config.LocalModel
					if skipLocal {
						model = d.Config.FrontierModel
					}
					if capw != nil {
						if d.JudgeObserver != nil {
							d.JudgeObserver.Submit(LocalCompletion{
								RequestID:   reqID,
								Instruction: latestPrompt,
								Output:      capw.Buffer(),
								LocalModel:  d.Config.LocalModel,
							})
						}
						if d.QualityObserver != nil {
							emitDetectedEdits(capw.Buffer(), reqID, d.QualityObserver)
						}
					}
				}
				// Debug trace (issue #33): non-streaming local path
				// bypasses the cascade so there are no per-step
				// attempts to report; we just record the single
				// upstream target we hit.
				trace.Upstream.Route = string(route)
				trace.Upstream.Streaming = false
				trace.Upstream.Model = model
				trace.Upstream.TargetHost = HostOfURL(targetURL)
				trace.Upstream.CascadeSteps = nil
				trace.Upstream.CascadeServedBy = ""
				trace.Upstream.CascadeSuccess = upErr == nil
			}

		default:
			model = d.Config.FrontierModel
			// Honor the harness's stream flag (issue #10). Stream
			// preserves SSE framing for chunked deliveries;
			// BufferedFetch collects the full body and returns a
			// single chatCompletionResponse JSON object.
			if streaming {
				upErr = upstream.Stream(obs, d.Client,
					d.Config.FrontierURL, d.Config.FrontierKey, body)
			} else {
				upErr = upstream.BufferedFetch(obs, d.Client,
					d.Config.FrontierURL, d.Config.FrontierKey, body)
			}
			if upErr != nil {
				slog.Error("upstream error",
					slog.Any("err", upErr),
					slog.String("request_id", reqID),
				)
				http.Error(w, "Upstream error", http.StatusBadGateway)
			}
			// Debug trace (issue #33): route=frontier is a single
			// endpoint with no cascade — populate the trace with
			// just the target host and model.
			trace.Upstream.Route = string(route)
			trace.Upstream.Streaming = streaming
			trace.Upstream.Model = model
			trace.Upstream.TargetHost = HostOfURL(d.Config.FrontierURL)
		}

		// Per-request recording. The metrics observer (issue #4)
		// is preferred when configured: it carries the savings
		// dimensions (TOON delta, RAG injection, cost) that
		// telemetry.Record cannot express. The legacy Recorder
		// remains for callers that only want the TTFT / latency
		// fields, or when no metrics store is wired (operator
		// opted out by leaving NEXUS_METRICS_DB empty).
		//
		// totalMs is float64 ms (issue #68) so a sub-millisecond
		// handler run still records a non-zero latency; the int64
		// truncation used to flip to 0 on fast hardware and
		// exposed a write race against the recorder's reader.
		totalMs := float64(time.Since(started).Microseconds()) / 1000.0
		var ttftMs int64
		if streaming && firstWriteAt.Load() > 0 {
			ttftMs = time.Unix(0, firstWriteAt.Load()).Sub(started).Milliseconds()
			if ttftMs < 0 {
				ttftMs = 0
			}
		}
		outputTokens := int(obs.BytesOut() / 4)
		rec := buildRecord(reqID, started, firstWriteAt.Load(), obs.BytesOut(), streaming, route, model, latestPrompt, upErr, fusionArbiterSkipped, fusionJaccardSimilarity, toolCallCount, decision)
		if d.MetricsObserver != nil {
			postCompressionChars := totalMessageChars(messages)
			savings := totalTokenSavings(preCompressionChars, postCompressionChars)
			inputTokens := telemetry.EstimateTokens(latestPrompt)
			cost := frontierCostEstimate(string(route), model, inputTokens, d.Config.JudgeCostPer1KUSD)
			baselineCost := baselineCostEstimate(inputTokens+outputTokens, d.Config.CostBaselineRatePer1K)
			savingsCost := baselineCost - cost
			if savingsCost < 0 {
				savingsCost = 0
			}
			tps := telemetry.ComputeTPS(outputTokens, ttftMs, totalMs)
			d.MetricsObserver.Submit(MetricsEvent{
				Timestamp:               rec.Timestamp,
				RequestID:               reqID,
				Route:                   string(route),
				Model:                   model,
				InputTokens:             rec.InputTokens,
				TOONSavingsTokens:       savings,
				TOONCompressionMethod:   string(toonCompressionMethod),
				RAGInjected:             ragInjected,
				RAGFilename:             ragFilename,
				EstimatedCostUSD:        cost,
				BaselineCostUSD:         baselineCost,
				SavingsUSD:              savingsCost,
				OutputTokens:            outputTokens,
				TTFTMs:                  ttftMs,
				TotalLatencyMs:          totalMs,
				TPS:                     tps,
				Streaming:               streaming,
				FusionArbiterSkipped:    fusionArbiterSkipped,
				FusionJaccardSimilarity: fusionJaccardSimilarity,
				ToolCallCount:           toolCallCount,
				Error:                   rec.Error,
				RouteSource:             string(decision.Source),
				RouteReason:             decision.Reason,
				SLMConfidence:           decision.Confidence,
				SLMTaskType:             decision.TaskType,
			})
		} else {
			d.Recorder.Record(rec)
		}

		// Debug trace emission (issue #33). When Debug is on we
		// flush a single batch of structured slog lines that
		// describes the full request lifecycle. Emission happens
		// AFTER the metrics dispatch so the trace can include the
		// final response status (which http.Error may have set
		// before we got here). The trace is gated on the
		// pre-existing flag, so the production path pays zero
		// allocations when NEXUS_DEBUG is unset.
		if d.Config.Debug {
			trace.Response.Status = obs.StatusCode()
			trace.Response.TTFTMs = ttftMs
			trace.Response.TotalBytes = obs.BytesOut()
			trace.Response.OutputTokens = outputTokens
			if capw != nil {
				preview, truncated := TruncateForDebug(capw.Buffer(), d.Config.EffectiveDebugBodyBytes())
				trace.Response.BodyPreview = preview
				trace.Response.BodyTruncated = truncated
			} else {
				// captureWriter was not installed (debug turned
				// on mid-request, race-style — should not happen
				// because the flag is checked at every code path
				// but be defensive). Surface the situation rather
				// than crash on a nil deref.
				trace.Response.BodyPreview = ""
				trace.Response.BodyTruncated = false
			}
			trace.Emit(slog.Default())
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
func requestID(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" {
		return v
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

// containsSystemWith reports whether messages already contained a
// system role whose content includes needle. Used by the debug trace
// to confirm the meta-prompt pass actually appended the operator-
// configured enhancement rather than leaving messages untouched (e.g.
// when MetaPrompt is empty or the harness sent zero messages).
func containsSystemWith(messages []interface{}, needle string) bool {
	if needle == "" {
		return false
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "system" {
			continue
		}
		if content, _ := msg["content"].(string); strings.Contains(content, needle) {
			return true
		}
	}
	return false
}

// stepNames returns the human-friendly names of cascade steps in
// declaration order. Used by the debug trace (issue #33) as a
// fallback when the cascade runner's RouteAttempted is empty (e.g.
// every step errored before recording). Keeps the trace complete
// even on full failure paths.
func stepNames(steps []upstream.CascadeStep) []string {
	if len(steps) == 0 {
		return nil
	}
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

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
	fusionArbiterSkipped bool,
	fusionJaccardSimilarity float64,
	toolCallCount int,
	decision router.Decision,
) telemetry.Record {
	totalMs := float64(time.Since(started).Microseconds()) / 1000.0
	var ttftMs int64
	if streaming && firstWriteNano > 0 {
		ttftMs = time.Unix(0, firstWriteNano).Sub(started).Milliseconds()
		if ttftMs < 0 {
			ttftMs = 0
		}
	}
	outputTokens := int(bytesOut / 4)
	rec := telemetry.Record{
		Timestamp:               time.Now().UTC(),
		RequestID:               requestID,
		Model:                   model,
		Route:                   string(route),
		InputTokens:             telemetry.EstimateTokens(latestPrompt),
		OutputTokens:            outputTokens,
		TTFTMs:                  ttftMs,
		TotalLatencyMs:          totalMs,
		Streaming:               streaming,
		FusionArbiterSkipped:    fusionArbiterSkipped,
		FusionJaccardSimilarity: fusionJaccardSimilarity,
		ToolCallCount:           toolCallCount,
		RouteSource:             string(decision.Source),
		RouteReason:             decision.Reason,
		SLMConfidence:           decision.Confidence,
		SLMTaskType:             decision.TaskType,
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

// frontierCostEstimate multiplies input tokens by the configured
// cost-per-1k. Returns zero for non-frontier routes so local +
// fusion-trail rows count as zero cost in the dashboard. The
// JudgeCostPer1KUSD knob is reused here for lack of a separate
// frontier-rate env; the PRD says one is enough at this stage.
func frontierCostEstimate(route, model string, inputTokens int, costPer1KUSD float64) float64 {
	// model is reserved for future per-model pricing tables —
	// the bundle stays cheap by sharing JudgeCostPer1KUSD for now.
	_ = model
	if route != string(router.RouteFrontier) {
		return 0
	}
	if costPer1KUSD <= 0 || inputTokens <= 0 {
		return 0
	}
	return float64(inputTokens) * costPer1KUSD / 1000.0
}

// formatConfidence renders a [0,1] confidence as the X-Nexus-Route-
// Confidence response header value (issue #74). Two-decimal fixed
// precision keeps the value directly comparable to the
// telemetry.Record.SLMConfidence field without ambiguity; values
// outside the [0,1] range are clamped to the nearest bound so a
// runaway SLM cannot produce an unwieldy header. Non-SLM stages
// (guardrail, DSL) emit 0.50 — the router's NeutralConfidence floor —
// so log scrapers can distinguish "no confidence" from "really zero".
func formatConfidence(c float64) string {
	if c < 0 {
		c = 0
	}
	if c > 1 {
		c = 1
	}
	if c == 0 {
		return "0.00"
	}
	return strconv.FormatFloat(c, 'f', 2, 64)
}

// baselineCostEstimate computes what a request would have cost if
// sent to the frontier baseline provider, regardless of the actual
// route (issue #73). It uses the total token count (input + output)
// at the baseline rate, so local/fusion requests with zero actual
// cost show a positive baseline and savings. Returns zero when the
// rate is unavailable (pricing data missing) so the request is
// recorded without failing.
func baselineCostEstimate(totalTokens int, baselineRatePer1K float64) float64 {
	if baselineRatePer1K <= 0 || totalTokens <= 0 {
		return 0
	}
	return float64(totalTokens) * baselineRatePer1K / 1000.0
}
