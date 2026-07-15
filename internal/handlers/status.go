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
	Enabled   bool `json:"judge_enabled"`
	Depth     int  `json:"judge_queue_depth"`
	Capacity  int  `json:"judge_queue_capacity"`
	Workers   int  `json:"judge_workers"`
}

// QualityStatus reports the live queue state of the quality verifier.
type QualityStatus struct {
	Enabled   bool `json:"quality_enabled"`
	Depth     int  `json:"quality_queue_depth"`
	Capacity  int  `json:"quality_queue_capacity"`
	Workers   int  `json:"quality_workers"`
}

// RAGStatus reports the health of the RAG embedder.
type RAGStatus struct {
	Healthy        bool `json:"rag_embedding_healthy"`
	IndexedExamples int  `json:"rag_indexed_examples"`
}

// RoutingSnapshot is a point-in-time copy of the routing decision counters.
type RoutingSnapshot struct {
	Decisions []observability.RouteCounterEntry `json:"decisions"`
}

// StatusDeps bundles the collaborators the status handler needs.
type StatusDeps struct {
	JudgeDepth     func() int   // returns current judge queue depth (0 if disabled)
	JudgeCapacity  func() int   // returns configured judge queue capacity (0 if disabled)
	JudgeWorkers   func() int   // returns configured judge concurrency (0 if disabled)
	JudgeEnabled   func() bool  // returns true if judge is active

	QualityDepth    func() int  // returns current quality queue depth (0 if disabled)
	QualityCapacity func() int  // returns configured quality queue capacity (0 if disabled)
	QualityWorkers  func() int  // returns configured quality concurrency (0 if disabled)
	QualityEnabled  func() bool // returns true if quality verifier is active

	RAGHealthy        func(context.Context) bool // returns true if RAG embedder is reachable
	RAGIndexedExamples func() int               // returns number of indexed examples (0 if none/disabled)

	RoutingSnapshot func() RoutingSnapshot // returns a point-in-time snapshot of routing counters

	Uptime func() time.Duration // process uptime
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
//	  "uptime_ms":    3600000
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
			Judge   JudgeStatus        `json:"judge"`
			Quality QualityStatus      `json:"quality"`
			RAG     RAGStatus          `json:"rag"`
			Routing RoutingSnapshot    `json:"routing"`
			Uptime  int64             `json:"uptime_ms"`
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
