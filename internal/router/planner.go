package router

// planner.go is the single route-planning seam extracted from the chat
// handler (issue #82). It runs the fixed decision pipeline —
// Guardrail -> DSL -> SLM (with optional judge-guided confidence bias)
// — and returns a structured Decision. The handler interprets the
// Decision to pick the dispatch branch and populate the debug trace;
// the planner itself is pure logic with no HTTP, logging, or tracing
// concerns so it is trivially unit-testable with stubs.
//
// The pipeline order MUST NOT change: Guardrail first (VRAM protection),
// then DSL (cheap regex fast-pass), then SLM (small-language-model
// judgement). Every failure mode defaults to RouteFrontier — the safe
// choice that never silently drops a request to a non-existent local
// model.

import (
	"context"
	"regexp"

	"github.com/anchapin/nexus-proxy/internal/telemetry"
)

// DecisionSource names which stage of the planner produced the route.
// It is the structured equivalent of the handler's trace "reason"
// field, with finer granularity for the SLM branch so the planner tests
// can distinguish a clean SLM decision from an SLM-error fallback.
type DecisionSource string

const (
	// SourceGuardrail means the VRAM-aware guardrail forced frontier.
	SourceGuardrail DecisionSource = "guardrail"

	// SourceDSL means the regex fast-pass matched (architecture ->
	// fusion, formatting -> local).
	SourceDSL DecisionSource = "dsl"

	// SourceSLM means the small-language-model returned a valid
	// decision.
	SourceSLM DecisionSource = "slm"

	// SourceSLMError means the SLM call failed (transport error,
	// invalid JSON, unknown route value) and the planner fell back to
	// RouteFrontier — the safe default.
	SourceSLMError DecisionSource = "slm-error"

	// SourceEscalation means the planner escalated to frontier because
	// no SLM client was configured. This is a defensive path: the
	// handler always wires an SLM, but the planner guards against nil
	// so a misconfiguration degrades gracefully rather than panicking.
	SourceEscalation DecisionSource = "escalation"

	// SourceSLMEscalation (issue #301) means the planner overrode the
	// SLM's local/fusion decision to frontier because the SLM's own
	// confidence fell below the configured SLMConfidenceThreshold.
	// This is a hard override — the SLM made a decision but the low
	// confidence signal triggered an automatic escalation.
	SourceSLMEscalation DecisionSource = "slm-escalation"
)

// TraceReason maps a DecisionSource to the short string the handler
// stamps on the debug RouteTrace.Reason field. The handler's pre-issue-
// 82 ladder used "guardrail", "dsl", and "slm" as reason labels; the
// SLM-error and escalation paths also map to "slm" so the trace remains
// backward-compatible with any log-scraping tooling.
func (s DecisionSource) TraceReason() string {
	switch s {
	case SourceGuardrail:
		return "guardrail"
	case SourceDSL:
		return "dsl"
	default:
		return "slm"
	}
}

// Decision is the structured result of the routing planner. It carries
// every field the handler needs to dispatch the request, emit slog
// lines, and populate the debug trace — without the planner needing to
// know about HTTP, logging, or tracing.
type Decision struct {
	// Route is the chosen upstream: RouteLocal, RouteFrontier, or
	// RouteFusion.
	Route Route

	// Source names which stage of the planner produced the Route.
	Source DecisionSource

	// Reason is a short machine-readable detail for the trace. For the
	// guardrail path it is "vram"; for the SLM-error path it is the
	// error message; otherwise it is empty.
	Reason string

	// Confidence is the empirical local-confidence value the planner
	// fed to DecideWithConfidence (issue #47). It is NeutralConfidence
	// (0.5) when no ConfidenceStore is wired or when the decision came
	// from the guardrail / DSL stages (which bypass the SLM entirely).
	Confidence float64

	// TaskType is the Categorize() bucket for the prompt (issue #47).
	// It is empty for the guardrail and DSL stages which do not
	// categorize.
	TaskType string

	// EstimatedTokens is the tiktoken cl100k_base token estimate used
	// by the guardrail. Carried in the Decision so the handler can
	// stamp it on the trace without recomputing.
	EstimatedTokens int

	// BudgetSource is the label identifying where the guardrail budget
	// came from ("static-fallback", "ollama-ps", ...). Echoed from
	// PlanRequest so the handler can populate the trace.
	BudgetSource string

	// BudgetTokens is the resolved guardrail budget in tokens. Echoed
	// from PlanRequest.
	BudgetTokens int

	// SLMError is the error string from the SLM call when Source ==
	// SourceSLMError; empty otherwise. Carried as a string (not error)
	// so the Decision is a plain value with no lifetime concerns.
	SLMError string

	// CacheHit is true when the decision came from the SLM cache
	// (issue #206) — the prompt was a cache hit and the cached route
	// was returned without calling the SLM. False when the decision
	// came from the SLM (or from guardrail/DSL which never hit the
	// cache).
	CacheHit bool
}

// SLMDecider is the minimal interface the planner needs from the SLM
// client. *SLMClient satisfies it; tests substitute a stub that returns
// deterministic decisions without an HTTP round-trip.
type SLMDecider interface {
	Decide(ctx context.Context, prompt string) (Route, error)
	DecideWithConfidence(ctx context.Context, prompt string, confidence float64) (Route, error)
}

// Planner is the single route-planning seam (issue #82). Construct one
// per request (or reuse — it holds no mutable state) and call Plan to
// get a Decision. The planner is pure logic: it performs SLM HTTP calls
// via the SLMDecider interface but does no logging, tracing, or HTTP-
// lifecycle work. The handler interprets the Decision.
type Planner struct {
	// SLM is consulted when neither the guardrail nor the DSL has an
	// opinion. Required for the SLM stage; when nil the planner falls
	// back to RouteFrontier (SourceEscalation) rather than panicking.
	SLM SLMDecider

	// Confidence is the optional judge-guided adaptive routing store
	// (issue #47). When non-nil the planner categorizes the prompt,
	// looks up the local model's historical confidence for that
	// category, and feeds it to SLM.DecideWithConfidence so a
	// low-confidence category biases toward frontier. When nil the
	// planner uses the plain SLM.Decide path — routing is byte-for-byte
	// identical to the pre-issue-47 behaviour.
	Confidence ConfidenceStore

	// FormattingRegex is the DSL fast-pass regex(es) for formatting tasks
	// (css, lint, docstring, ...). An empty/nil slice disables the
	// formatting branch of the DSL (the fusion patterns still fire).
	FormattingRegex []*regexp.Regexp

	// FusionPatterns is the DSL fast-pass regex(es) for architecture
	// keywords that warrant running both local and frontier (fusion).
	// An empty/nil slice uses the hardcoded defaults
	// ("architectural design", "system architecture").
	FusionPatterns []*regexp.Regexp

	// LocalPatternsRegex is the DSL fast-pass regex(es) for common coding
	// task keywords (refactor, security scan, generate tests, explain this
	// code, performance analysis, ...). An empty/nil slice disables the
	// local-patterns branch of the DSL.
	LocalPatternsRegex []*regexp.Regexp

	// SLMCache is the optional time-bounded prompt→route cache
	// (issue #206). When non-nil the planner checks the cache before
	// calling the SLM; a hit returns the cached route without calling
	// the SLM. A miss calls the SLM and stores the result. When nil the
	// SLM is always called (pre-cache behaviour).
	SLMCache *SLMCache

	// ConfidenceThreshold is the hard-escalation floor for SLM decisions
	// (issue #301). When the SLM returns RouteLocal or RouteFusion with
	// confidence below this threshold, the planner overrides to
	// RouteFrontier and sets Source to SourceSLMEscalation. A zero or
	// negative value disables the hard override (the soft bias via
	// DecideWithConfidence still applies when Confidence is wired). The
	// planner uses >, not >=, so a threshold of 0.3 fires when
	// confidence is 0.29.
	ConfidenceThreshold float64
}

// PlanRequest carries the per-request inputs the planner needs. The
// handler resolves the guardrail budget from the BudgetObserver (or the
// static config fallback) and passes it here; the planner does not know
// about BudgetObserver so it stays free of handler concerns.
type PlanRequest struct {
	// Prompt is the latest user message after all middleware transforms
	// (meta-prompt, RAG injection, TOON compression). The planner reads
	// it but never mutates it.
	Prompt string

	// GuardrailBudget is the resolved VRAM-token ceiling. When <= 0 the
	// guardrail stage is disabled (never fires) — matching Guardrail's
	// own contract.
	GuardrailBudget int

	// GuardrailSource is the label identifying where the budget came
	// from ("static-fallback", "ollama-ps", ...). Echoed into the
	// Decision for tracing; the planner does not interpret it.
	GuardrailSource string

	// Context is passed through to the SLM HTTP call. The planner does
	// not derive a timeout from it — the SLMClient applies its own
	// configured timeout via context.WithTimeout internally.
	Context context.Context
}

// Plan runs the routing pipeline and returns a Decision.
//
// Pipeline order (DO NOT REORDER):
//  1. Guardrail — VRAM protection, forces RouteFrontier when the prompt
//     exceeds the budget.
//  2. DSL — cheap regex fast-pass, returns RouteLocal or RouteFusion
//     for obvious cases.
//  3. SLM — small-language-model judgement, with optional confidence
//     bias from the judge-guided adaptive routing store.
//
// Every failure mode (SLM error, nil SLM client) defaults to
// RouteFrontier — the safe choice.
func (p *Planner) Plan(req PlanRequest) Decision {
	estimatedTokens := telemetry.EstimateTokens(req.Prompt)

	// Stage 1: VRAM-aware guardrail.
	if g, hit := Guardrail(req.Prompt, req.GuardrailBudget); hit {
		return Decision{
			Route:           g,
			Source:          SourceGuardrail,
			Reason:          "vram",
			Confidence:      NeutralConfidence,
			EstimatedTokens: estimatedTokens,
			BudgetSource:    req.GuardrailSource,
			BudgetTokens:    req.GuardrailBudget,
		}
	}

	// Stage 2: DSL fast-pass. Use default patterns when config fields are nil.
	fusionPatterns := p.FusionPatterns
	if len(fusionPatterns) == 0 {
		fusionPatterns = DefaultFusionPatterns
	}
	formattingPatterns := p.FormattingRegex
	if len(formattingPatterns) == 0 {
		formattingPatterns = DefaultFormattingPatterns
	}
	localPatterns := p.LocalPatternsRegex
	if len(localPatterns) == 0 {
		localPatterns = DefaultLocalPatterns
	}
	if r, hit := DSL(req.Prompt, fusionPatterns, formattingPatterns, localPatterns); hit {
		return Decision{
			Route:           r,
			Source:          SourceDSL,
			Confidence:      NeutralConfidence,
			EstimatedTokens: estimatedTokens,
			BudgetSource:    req.GuardrailSource,
			BudgetTokens:    req.GuardrailBudget,
		}
	}

	// Stage 3: SLM (with optional confidence bias).
	//
	// When a ConfidenceStore is wired we categorize the prompt and feed
	// the local model's historical confidence for that category into
	// the SLM, biasing toward frontier for categories the local model
	// has handled poorly. When no store is wired (judge disabled) we
	// take the plain neutral Decide path so routing is unchanged.
	//
	// When SLMCache is wired (issue #206), we check the cache first.
	// A cache hit returns the cached decision immediately (CacheHit=true);
	// a cache miss calls the SLM and stores the result (CacheHit=false).
	var (
		dec        Route
		err        error
		confidence float64 = NeutralConfidence
		category   string
	)

	// Check cache first if enabled.
	if p.SLMCache != nil {
		if cached, hit := p.SLMCache.Get(req.Context, req.Prompt); hit {
			// We still categorize for observability even on cache hit,
			// but we use the cached route directly.
			if p.Confidence != nil {
				category = Categorize(req.Prompt)
				confidence = p.Confidence.LocalConfidence(category)
			}
			// Hard override: same check as the miss path — a cached
			// local/fusion decision with low confidence still escalates.
			if p.ConfidenceThreshold > 0 && (cached == RouteLocal || cached == RouteFusion) && confidence < p.ConfidenceThreshold {
				return Decision{
					Route:           RouteFrontier,
					Source:          SourceSLMEscalation,
					Reason:          "low_confidence",
					Confidence:      confidence,
					TaskType:        category,
					EstimatedTokens: estimatedTokens,
					BudgetSource:    req.GuardrailSource,
					BudgetTokens:    req.GuardrailBudget,
					CacheHit:        true,
				}
			}
			return Decision{
				Route:           cached,
				Source:          SourceSLM,
				Confidence:      confidence,
				TaskType:        category,
				EstimatedTokens: estimatedTokens,
				BudgetSource:    req.GuardrailSource,
				BudgetTokens:    req.GuardrailBudget,
				CacheHit:        true,
			}
		}
	}

	if p.Confidence != nil {
		category = Categorize(req.Prompt)
		confidence = p.Confidence.LocalConfidence(category)
		dec, err = p.SLM.DecideWithConfidence(req.Context, req.Prompt, confidence)
	} else {
		dec, err = p.SLM.Decide(req.Context, req.Prompt)
	}
	if err != nil {
		return Decision{
			Route:           RouteFrontier,
			Source:          SourceSLMError,
			Reason:          err.Error(),
			Confidence:      confidence,
			TaskType:        category,
			EstimatedTokens: estimatedTokens,
			BudgetSource:    req.GuardrailSource,
			BudgetTokens:    req.GuardrailBudget,
			SLMError:        err.Error(),
			CacheHit:        false,
		}
	}

	// Hard override: if the SLM returned local/fusion but confidence
	// is below the threshold, escalate to frontier (issue #301).
	// The check uses > so threshold 0.3 fires on 0.29. A zero or
	// negative threshold disables the override.
	if p.ConfidenceThreshold > 0 && (dec == RouteLocal || dec == RouteFusion) && confidence < p.ConfidenceThreshold {
		category = Categorize(req.Prompt)
		return Decision{
			Route:           RouteFrontier,
			Source:          SourceSLMEscalation,
			Reason:          "low_confidence",
			Confidence:      confidence,
			TaskType:        category,
			EstimatedTokens: estimatedTokens,
			BudgetSource:    req.GuardrailSource,
			BudgetTokens:    req.GuardrailBudget,
			CacheHit:        false,
		}
	}

	// Cache the successful decision for future identical prompts.
	if p.SLMCache != nil {
		p.SLMCache.Set(req.Context, req.Prompt, dec)
	}
	return Decision{
		Route:           dec,
		Source:          SourceSLM,
		Confidence:      confidence,
		TaskType:        category,
		EstimatedTokens: estimatedTokens,
		BudgetSource:    req.GuardrailSource,
		BudgetTokens:    req.GuardrailBudget,
		CacheHit:        false,
	}
}

// Compile-time assertion: *SLMClient satisfies SLMDecider so the handler
// can pass it directly to the Planner without an adapter.
var _ SLMDecider = (*SLMClient)(nil)
