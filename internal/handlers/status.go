// Package handlers contains the HTTP entry points for the proxy.
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/anchapin/nexus-proxy/internal/observability"
)

// JudgeStatus reports the live queue state of the judge evaluator.
type JudgeStatus struct {
	Enabled  bool `json:"judge_enabled"`
	Depth    int  `json:"judge_queue_depth"`
	Capacity int  `json:"judge_queue_capacity"`
	Workers  int  `json:"judge_workers"`
}

// QualityStatus reports the live queue state of the quality verifier.
type QualityStatus struct {
	Enabled  bool `json:"quality_enabled"`
	Depth    int  `json:"quality_queue_depth"`
	Capacity int  `json:"quality_queue_capacity"`
	Workers  int  `json:"quality_workers"`
}

// RAGStatus reports the health of the RAG embedder.
type RAGStatus struct {
	Healthy         bool `json:"rag_embedding_healthy"`
	IndexedExamples int  `json:"rag_indexed_examples"`
}

// RoutingSnapshot is a point-in-time copy of the routing decision counters.
type RoutingSnapshot struct {
	Decisions []observability.RouteCounterEntry `json:"decisions"`
}

// RateLimiterStatus reports the live state of the rate limiter.
type RateLimiterStatus struct {
	Enabled bool `json:"enabled"`
	RPM     int  `json:"rpm"`
	Burst   int  `json:"burst"`
}

// BudgetStatus reports the rolling 24h spend guard state.
type BudgetStatus struct {
	Enabled         bool      `json:"enabled"`
	DailyLimitUSD   float64   `json:"daily_limit_usd"`
	CurrentSpendUSD float64   `json:"current_spend_usd"`
	ResetAt         time.Time `json:"reset_at"`
}

// MetricsDBStatus reports the SQLite metrics store writability.
type MetricsDBStatus struct {
	Writable bool   `json:"writable"`
	Path     string `json:"path"`
}

// SLMCacheStatus reports the SLM routing decision cache state.
type SLMCacheStatus struct {
	Enabled    bool `json:"enabled"`
	TTLSeconds int  `json:"ttl_seconds"`
}

// ArbiterCacheStatus reports the fusion arbiter synthesis cache state.
type ArbiterCacheStatus struct {
	Enabled    bool `json:"enabled"`
	TTLSeconds int  `json:"ttl_seconds"`
}

// StatusDeps bundles the collaborators the status handler needs.
type StatusDeps struct {
	JudgeDepth    func() int  // returns current judge queue depth (0 if disabled)
	JudgeCapacity func() int  // returns configured judge queue capacity (0 if disabled)
	JudgeWorkers  func() int  // returns configured judge concurrency (0 if disabled)
	JudgeEnabled  func() bool // returns true if judge is active

	QualityDepth    func() int  // returns current quality queue depth (0 if disabled)
	QualityCapacity func() int  // returns configured quality queue capacity (0 if disabled)
	QualityWorkers  func() int  // returns configured quality concurrency (0 if disabled)
	QualityEnabled  func() bool // returns true if quality verifier is active

	RAGHealthy         func(context.Context) bool // returns true if RAG embedder is reachable
	RAGIndexedExamples func() int                 // returns number of indexed examples (0 if none/disabled)

	RoutingSnapshot func() RoutingSnapshot // returns a point-in-time snapshot of routing counters

	Uptime func() time.Duration // process uptime

	// RateLimiter reports the rate limiter state.
	RateLimiterEnabled func() bool
	RateLimiterRPM     func() int
	RateLimiterBurst   func() int

	// Budget reports the budget guard state.
	BudgetEnabled         func() bool
	BudgetDailyLimitUSD   func() float64
	BudgetCurrentSpendUSD func() float64
	BudgetResetAt         func() time.Time

	// MetricsDB reports the SQLite metrics store state.
	MetricsDBWritable func() bool
	MetricsDBPath     func() string

	// SLMCache reports the SLM routing decision cache state.
	SLMCacheEnabled    func() bool
	SLMCacheTTLSeconds func() int

	// ArbiterCache reports the fusion arbiter synthesis cache state.
	ArbiterCacheEnabled    func() bool
	ArbiterCacheTTLSeconds func() int
}

// Status returns an http.Handler that serves a JSON diagnostic snapshot of
// all async subsystems. It is the primary interface for Kubernetes readiness
// probes and on-call engineers debugging performance regressions.
//
// Response shape:
//
//	{
//	  "judge":        { "judge_enabled": true, "judge_queue_depth": 3, ... },
//	  "quality":      { "quality_enabled": true, "quality_queue_depth": 0, ... },
//	  "rag":          { "rag_embedding_healthy": true, "rag_indexed_examples": 12 },
//	  "routing":      { "decisions": [{"route":"local","source":"dsl","count":42},...] },
//	  "uptime_ms":    3600000,
//	  "rate_limiter": { "enabled": true, "rpm": 120, "burst": 30 },
//	  "budget":       { "enabled": true, "daily_limit_usd": 10.0, "current_spend_usd": 2.5, "reset_at": "..." },
//	  "metrics_db":   { "writable": true, "path": "/var/lib/nexus-proxy/metrics.db" },
//	  "slm_cache":    { "enabled": true, "ttl_seconds": 30 },
//	  "arbiter_cache": { "enabled": true, "ttl_seconds": 300 }
//	}
//
// All fields are always present; zero values mean the subsystem is disabled
// or has not yet been measured.
func Status(d StatusDeps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		ragHealthy := false
		if d.RAGHealthy != nil {
			ragHealthy = d.RAGHealthy(ctx)
		}

		snapshot := RoutingSnapshot{}
		if d.RoutingSnapshot != nil {
			snapshot = d.RoutingSnapshot()
		}

		uptimeMs := int64(0)
		if d.Uptime != nil {
			uptimeMs = d.Uptime().Milliseconds()
		}

		resp := struct {
			Judge        JudgeStatus        `json:"judge"`
			Quality      QualityStatus      `json:"quality"`
			RAG          RAGStatus          `json:"rag"`
			Routing      RoutingSnapshot    `json:"routing"`
			Uptime       int64              `json:"uptime_ms"`
			RateLimiter  RateLimiterStatus  `json:"rate_limiter"`
			Budget       BudgetStatus       `json:"budget"`
			MetricsDB    MetricsDBStatus    `json:"metrics_db"`
			SLMCache     SLMCacheStatus     `json:"slm_cache"`
			ArbiterCache ArbiterCacheStatus `json:"arbiter_cache"`
		}{
			Judge: JudgeStatus{
				Enabled:  d.JudgeEnabled != nil && d.JudgeEnabled(),
				Depth:    intOrZero(d.JudgeDepth),
				Capacity: intOrZero(d.JudgeCapacity),
				Workers:  intOrZero(d.JudgeWorkers),
			},
			Quality: QualityStatus{
				Enabled:  d.QualityEnabled != nil && d.QualityEnabled(),
				Depth:    intOrZero(d.QualityDepth),
				Capacity: intOrZero(d.QualityCapacity),
				Workers:  intOrZero(d.QualityWorkers),
			},
			RAG: RAGStatus{
				Healthy:         ragHealthy,
				IndexedExamples: intOrZero(d.RAGIndexedExamples),
			},
			Routing: snapshot,
			Uptime:  uptimeMs,
			RateLimiter: RateLimiterStatus{
				Enabled: d.RateLimiterEnabled != nil && d.RateLimiterEnabled(),
				RPM:     intOrZero(d.RateLimiterRPM),
				Burst:   intOrZero(d.RateLimiterBurst),
			},
			Budget: BudgetStatus{
				Enabled:         d.BudgetEnabled != nil && d.BudgetEnabled(),
				DailyLimitUSD:   float64OrZero(d.BudgetDailyLimitUSD),
				CurrentSpendUSD: float64OrZero(d.BudgetCurrentSpendUSD),
				ResetAt:         timeOrZero(d.BudgetResetAt),
			},
			MetricsDB: MetricsDBStatus{
				Writable: d.MetricsDBWritable != nil && d.MetricsDBWritable(),
				Path:     stringOrZero(d.MetricsDBPath),
			},
			SLMCache: SLMCacheStatus{
				Enabled:    d.SLMCacheEnabled != nil && d.SLMCacheEnabled(),
				TTLSeconds: intOrZero(d.SLMCacheTTLSeconds),
			},
			ArbiterCache: ArbiterCacheStatus{
				Enabled:    d.ArbiterCacheEnabled != nil && d.ArbiterCacheEnabled(),
				TTLSeconds: intOrZero(d.ArbiterCacheTTLSeconds),
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})
}

func intOrZero(fn func() int) int {
	if fn == nil {
		return 0
	}
	return fn()
}

func float64OrZero(fn func() float64) float64 {
	if fn == nil {
		return 0
	}
	return fn()
}

func timeOrZero(fn func() time.Time) time.Time {
	if fn == nil {
		return time.Time{}
	}
	return fn()
}

func stringOrZero(fn func() string) string {
	if fn == nil {
		return ""
	}
	return fn()
}
