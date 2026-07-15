// Package handlers — health.go contains the orchestrator-facing
// health endpoints (issue #42). The file is deliberately a leaf of
// the package: it depends only on stdlib, the existing config /
// health imports already used by chat.go, and tiny read-only
// interfaces for the per-package state it surfaces (/status). The
// chat hot path stays free of judge / quality imports per the
// AGENTS.md dependency rule; main.go wires those via the adapters
// declared here.
//
// Three Kubernetes-style endpoints are exposed in addition to the
// legacy /healthz alias:
//
//	GET /livez   — liveness. Always 200 when the process is alive;
//	               no dependency checks. The kubelet's livenessProbe
//	               target — a failed probe should restart the pod.
//
//	GET /readyz  — readiness. 200 when the proxy can serve traffic,
//	               503 when it cannot. Mode is configurable via
//	               NEXUS_READINESS_MODE (strict|degraded; default
//	               degraded). In degraded mode the endpoint always
//	               returns 200 so an orchestrator never evicts the
//	               pod over a transient dependency outage; the
//	               "degraded" field still surfaces the real state.
//	               The kubelet's readinessProbe target.
//
//	GET /status  — detailed JSON view of every backing subsystem.
//	               Intended for operator dashboards / on-call
//	               debugging, not for probe wiring.
//
// /healthz is preserved verbatim as a backward-compatibility alias
// for the pre-#42 envelope (status/budget/ollama fields) so existing
// Docker healthchecks and operator scripts do not break.
package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/health"
)

// Readiness mode constants. The mode is selected via
// NEXUS_READINESS_MODE (see internal/config). Unknown values fall
// back to ReadinessModeDegraded so a typo in an env var never flips
// the pod into a state where a single dependency outage triggers an
// eviction storm.
const (
	// ReadinessModeStrict: /readyz returns 200 iff the proxy can
	// actually serve traffic (Ollama healthy OR frontier key
	// configured). When both are unavailable /readyz returns 503
	// so the orchestrator stops sending the pod new requests.
	ReadinessModeStrict = "strict"

	// ReadinessModeDegraded (default): /readyz always returns 200
	// when the process is alive. The "degraded" boolean in the
	// response body surfaces the actual subsystem state for
	// observability without triggering an eviction. This is the
	// safer default for local dev and for operators who would
	// rather handle dependency outages in code than via pod churn.
	ReadinessModeDegraded = "degraded"
)

// NormalizeReadinessMode canonicalises the configured mode and
// falls back to ReadinessModeDegraded on unknown / empty values.
// Exposed so cmd/nexus can derive a single source of truth at boot
// rather than repeating the case-fold at every read site.
func NormalizeReadinessMode(mode string) string {
	switch mode {
	case ReadinessModeStrict:
		return ReadinessModeStrict
	case "", ReadinessModeDegraded:
		return ReadinessModeDegraded
	default:
		return ReadinessModeDegraded
	}
}

// JudgeStats is the read-only surface the /status handler reads from
// the LLM-as-a-judge evaluator (issue #15). Declared as an interface
// here so the handlers package stays free of the judge import (see
// the AGENTS.md dependency rule); main.go wires a closure that calls
// judge.Evaluator directly. The judge evaluator satisfies the
// interface structurally; a nil evaluator is wrapped by main.go into
// the all-zero JudgeStatsFunc below.
//
// All methods must be safe to call concurrently — /status is hit by
// every probe + dashboard refresh.
type JudgeStats interface {
	Enabled() bool
	QueueDepth() int
	Concurrency() int
}

// QualityStats is the read-only surface the /status handler reads
// from the AST/compiler verifier (issue #13). Same rationale as
// JudgeStats: declared as an interface so handlers does not import
// internal/quality. main.go wires the closure.
type QualityStats interface {
	Enabled() bool
	QueueDepth() int
	Concurrency() int
	Dropped() uint64
}

// ProbeStats is the read-only surface the /status and /healthz
// handlers read from the dynamic VRAM probe (issue #6). Same
// rationale as JudgeStats / QualityStats: keeps handlers free of
// the probe import. main.go wires a closure that calls
// probe.Manager.Get() and projects the Budget into this shape.
type ProbeStats interface {
	BudgetTokens() int
	BudgetSource() string
	FreeVRAMBytes() int64
	ModelContext() int
}

// JudgeStatsFunc adapts a set of plain functions to the JudgeStats
// interface. A nil function on any field is treated as the zero
// value (false / 0) so wiring sites can leave a disabled subsystem
// as the zero JudgeStatsFunc{} without per-call nil checks.
//
// Method values bind to the original receiver, so passing
// judgeEval.Enabled as a method value captures the non-nil pointer
// at wiring time — see cmd/nexus/main.go for the canonical pattern.
// Field names carry an Fn suffix to avoid colliding with the
// interface methods of the same name.
type JudgeStatsFunc struct {
	EnabledFn     func() bool
	QueueDepthFn  func() int
	ConcurrencyFn func() int
}

// Enabled implements JudgeStats.
func (f JudgeStatsFunc) Enabled() bool {
	if f.EnabledFn == nil {
		return false
	}
	return f.EnabledFn()
}

// QueueDepth implements JudgeStats.
func (f JudgeStatsFunc) QueueDepth() int {
	if f.QueueDepthFn == nil {
		return 0
	}
	return f.QueueDepthFn()
}

// Concurrency implements JudgeStats.
func (f JudgeStatsFunc) Concurrency() int {
	if f.ConcurrencyFn == nil {
		return 0
	}
	return f.ConcurrencyFn()
}

// QualityStatsFunc adapts a set of plain functions to the
// QualityStats interface. Same nil-safe contract as JudgeStatsFunc.
type QualityStatsFunc struct {
	EnabledFn     func() bool
	QueueDepthFn  func() int
	ConcurrencyFn func() int
	DroppedFn     func() uint64
}

// Enabled implements QualityStats.
func (f QualityStatsFunc) Enabled() bool {
	if f.EnabledFn == nil {
		return false
	}
	return f.EnabledFn()
}

// QueueDepth implements QualityStats.
func (f QualityStatsFunc) QueueDepth() int {
	if f.QueueDepthFn == nil {
		return 0
	}
	return f.QueueDepthFn()
}

// Concurrency implements QualityStats.
func (f QualityStatsFunc) Concurrency() int {
	if f.ConcurrencyFn == nil {
		return 0
	}
	return f.ConcurrencyFn()
}

// Dropped implements QualityStats.
func (f QualityStatsFunc) Dropped() uint64 {
	if f.DroppedFn == nil {
		return 0
	}
	return f.DroppedFn()
}

// ProbeStatsFunc adapts a set of plain functions to the ProbeStats
// interface. Same nil-safe contract as the other adapters; a nil
// Source returns "static" so the response is never ambiguous about
// where the budget came from.
type ProbeStatsFunc struct {
	TokensFn   func() int
	SourceFn   func() string
	FreeVRAMFn func() int64
	ContextFn  func() int
}

// BudgetTokens implements ProbeStats.
func (f ProbeStatsFunc) BudgetTokens() int {
	if f.TokensFn == nil {
		return 0
	}
	return f.TokensFn()
}

// BudgetSource implements ProbeStats.
func (f ProbeStatsFunc) BudgetSource() string {
	if f.SourceFn == nil {
		return "static"
	}
	return f.SourceFn()
}

// FreeVRAMBytes implements ProbeStats.
func (f ProbeStatsFunc) FreeVRAMBytes() int64 {
	if f.FreeVRAMFn == nil {
		return 0
	}
	return f.FreeVRAMFn()
}

// ModelContext implements ProbeStats.
func (f ProbeStatsFunc) ModelContext() int {
	if f.ContextFn == nil {
		return 0
	}
	return f.ContextFn()
}

// LivezHandler returns the /livez handler. Always 200 when the
// process is alive; the body is a tiny JSON envelope so a curl-based
// diagnostic still surfaces useful output. No dependency checks —
// the kubelet's livenessProbe must not flap on transient Ollama or
// frontier outages (those are /readyz's job).
func LivezHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
	}
}

// ReadyzDeps bundles the collaborators the /readyz handler reads to
// decide whether the proxy can serve traffic. Keeping it a struct
// rather than positional args means future readiness inputs (e.g. a
// downstream queue depth, a custom operator-defined predicate) can
// be added without breaking every call site.
type ReadyzDeps struct {
	// Health is the live Ollama reachability poller (issue #8). A
	// nil receiver is treated as "healthy" so unit tests / minimal
	// wiring never trip a 503 over an absent health checker.
	Health *health.Health

	// FrontierConfigured reports whether a frontier API key is set.
	// /readyz is ready if either Ollama is healthy OR the frontier
	// is configured — frontier routing covers the "Ollama down"
	// case at the cost of API spend, which the operator has
	// explicitly opted into by setting the key.
	FrontierConfigured bool

	// Mode is the canonical readiness mode (see NormalizeReadinessMode).
	// Defaults to ReadinessModeDegraded when empty.
	Mode string
}

// ReadyzHandler returns the /readyz handler. The handler is cheap:
// one method call on Health and one bool comparison in the worst
// case, so a probe storm (kubelet, load balancer, dashboard refresh)
// never contends for I/O. Output shape:
//
//	{
//	  "status":   "ready" | "not_ready",
//	  "reason":   "<human readable>"   // only when not_ready
//	  "degraded": <bool>               // true iff a dependency is down
//	}
//
// In degraded mode the status is always "ready" and degraded
// surfaces the real state. In strict mode a 503 is returned when
// neither Ollama nor frontier can serve the request.
func ReadyzHandler(deps ReadyzDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		mode := NormalizeReadinessMode(deps.Mode)
		healthy := deps.Health == nil || deps.Health.IsLocalHealthy()

		// Decide per mode. The decision is local — no I/O, no
		// allocations on the hot path beyond a small map literal.
		switch mode {
		case ReadinessModeStrict:
			if healthy || deps.FrontierConfigured {
				writeJSON(w, http.StatusOK, readyzResponse{
					Status:   "ready",
					Degraded: !healthy,
				})
				return
			}
			writeJSON(w, http.StatusServiceUnavailable, readyzResponse{
				Status:   "not_ready",
				Reason:   "ollama_unreachable_and_no_frontier_key",
				Degraded: true,
			})
		default:
			// Degraded mode: always 200, surface actual state so
			// dashboards / alerting can react without evicting
			// the pod. This is the safe default — a temporary
			// dependency outage should not churn the proxy out
			// of the service mesh.
			writeJSON(w, http.StatusOK, readyzResponse{
				Status:   "ready",
				Degraded: !healthy && !deps.FrontierConfigured,
			})
		}
	}
}

// readyzResponse is the JSON envelope /readyz writes. Field order is
// pinned via struct ordering so the output is deterministic across
// Go versions (map encoding would randomise keys).
type readyzResponse struct {
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Degraded bool   `json:"degraded"`
}

// DetailedStatusDeps bundles the collaborators the /status handler reads
// when serialising the detailed JSON view. Splitting it from
// ReadyzDeps keeps each handler's signature small and self-evident.
type DetailedStatusDeps struct {
	Health        *health.Health
	Probe         ProbeStats
	Judge         JudgeStats
	Quality       QualityStats
	Config        config.Config
	ReadinessMode string
	StartTime     time.Time
}

// StatusHandler returns the /status handler. The handler returns a
// detailed JSON snapshot of every backing subsystem — see
// statusResponse below for the exact shape. All reads are
// non-blocking (atomic loads on the hot path, method calls that
// read the latest buffered-channel state in microseconds) so a
// probe storm does not contend with the chat path.
func StatusHandler(deps DetailedStatusDeps) http.HandlerFunc {
	start := deps.StartTime
	if start.IsZero() {
		// Defensive: a wiring mistake that forgets StartTime would
		// otherwise emit uptime=0 forever. Treat as "process
		// start" by anchoring to now so the field stays meaningful.
		start = time.Now()
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		healthy := deps.Health == nil || deps.Health.IsLocalHealthy()
		failures := 0
		if deps.Health != nil {
			failures = deps.Health.FailureCount()
		}

		uptime := int64(0)
		if !start.IsZero() {
			uptime = int64(time.Since(start).Seconds())
			if uptime < 0 {
				// Defensive: clock skew should never produce a
				// negative uptime field. Clamp at zero.
				uptime = 0
			}
		}

		resp := statusResponse{
			Ollama: statusOllama{
				Healthy:      healthy,
				FailureCount: failures,
			},
			Frontier: statusFrontier{
				Configured: deps.Config.FrontierKey != "",
			},
			VRAMProbe: statusProbe{
				Tokens:        deps.Probe.BudgetTokens(),
				Source:        deps.Probe.BudgetSource(),
				FreeVRAMBytes: deps.Probe.FreeVRAMBytes(),
				ModelContext:  deps.Probe.ModelContext(),
			},
			Judge: statusJudge{
				Enabled:     deps.Judge.Enabled(),
				QueueDepth:  deps.Judge.QueueDepth(),
				Concurrency: deps.Judge.Concurrency(),
			},
			Quality: statusQuality{
				Enabled:     deps.Quality.Enabled(),
				QueueDepth:  deps.Quality.QueueDepth(),
				Concurrency: deps.Quality.Concurrency(),
				Dropped:     deps.Quality.Dropped(),
			},
			UptimeSeconds: uptime,
			ReadinessMode: NormalizeReadinessMode(deps.ReadinessMode),
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// statusResponse is the JSON envelope /status writes. Top-level
// keys match the issue body verbatim. Nested structs use omitempty
// on fields that would be noisy when absent (e.g. dropped == 0
// for a verifier that never overflowed its queue).
type statusResponse struct {
	Ollama        statusOllama   `json:"ollama"`
	Frontier      statusFrontier `json:"frontier"`
	VRAMProbe     statusProbe    `json:"vram_probe"`
	Judge         statusJudge    `json:"judge"`
	Quality       statusQuality  `json:"quality"`
	UptimeSeconds int64          `json:"uptime_seconds"`
	ReadinessMode string         `json:"readiness_mode"`
}

type statusOllama struct {
	Healthy      bool `json:"healthy"`
	FailureCount int  `json:"failure_count"`
}

type statusFrontier struct {
	Configured bool `json:"configured"`
}

type statusProbe struct {
	Tokens        int    `json:"tokens"`
	Source        string `json:"source"`
	FreeVRAMBytes int64  `json:"free_vram_bytes"`
	ModelContext  int    `json:"model_context"`
}

type statusJudge struct {
	Enabled     bool `json:"enabled"`
	QueueDepth  int  `json:"queue_depth"`
	Concurrency int  `json:"concurrency"`
}

type statusQuality struct {
	Enabled     bool   `json:"enabled"`
	QueueDepth  int    `json:"queue_depth"`
	Concurrency int    `json:"concurrency"`
	Dropped     uint64 `json:"dropped"`
}

// HealthzHandler returns the /healthz handler, preserved verbatim as
// a backwards-compatibility alias for the pre-issue-#42 envelope
// (status, ollama_healthy, budget_tokens, ...). The JSON shape
// matches the existing handler in cmd/nexus/main.go exactly so
// existing Docker healthchecks, operator scripts, and the
// Prometheus scrape-config comments continue to work unchanged.
//
// Always 200 — /healthz is a liveness alias, not a readiness signal.
// Operators who want the strict mode should rewire their probe to
// /readyz (see issue #42 acceptance criteria).
//
// The probe argument is the same ProbeStats interface the /status
// handler reads — defined here so handlers does not import the
// probe package (AGENTS.md dependency rule). main.go wires a single
// adapter and passes it to both handlers.
func HealthzHandler(hpoller *health.Health, probe ProbeStats, cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		healthy := hpoller == nil || hpoller.IsLocalHealthy()

		displayTokens := probe.BudgetTokens()
		source := probe.BudgetSource()
		if displayTokens <= 0 {
			// When the probe has no budget to offer (still
			// booting, disabled, or every signal unavailable)
			// echo the operator-configured TokenGuardrail so
			// /healthz always reports a concrete number
			// operators can grep against.
			displayTokens = cfg.TokenGuardrail
			source = "static-fallback"
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":                 "ok",
			"ollama_healthy":         healthy,
			"budget_tokens":          displayTokens,
			"budget_source":          source,
			"free_vram_bytes":        probe.FreeVRAMBytes(),
			"model_context":          probe.ModelContext(),
			"static_fallback_tokens": cfg.TokenGuardrail,
		})
	}
}

// writeJSON serialises body as JSON and writes it with the supplied
// status code and Content-Type. Centralised so every health handler
// uses the same envelope conventions and the same charset hint.
func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
