package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/observability"
)

func TestStatusHandler(t *testing.T) {
	resetAt := time.Now().Add(24 * time.Hour)
	// Setup a status handler with all deps wired.
	handler := Status(StatusDeps{
		JudgeEnabled:    func() bool { return true },
		JudgeDepth:      func() int { return 3 },
		JudgeCapacity:   func() int { return 64 },
		JudgeWorkers:    func() int { return 2 },
		QualityEnabled:  func() bool { return true },
		QualityDepth:    func() int { return 0 },
		QualityCapacity: func() int { return 64 },
		QualityWorkers:  func() int { return 2 },
		RAGHealthy: func(ctx context.Context) bool {
			return true
		},
		RAGIndexedExamples: func() int { return 12 },
		RoutingSnapshot: func() RoutingSnapshot {
			return RoutingSnapshot{
				Decisions: []observability.RouteCounterEntry{
					{Route: "local", Source: "dsl", Count: 10},
					{Route: "frontier", Source: "guardrail", Count: 5},
					{Route: "frontier", Source: "slm", Count: 3},
				},
			}
		},
		Uptime: func() time.Duration { return 1 * time.Hour },
		RateLimiterEnabled: func() bool { return true },
		RateLimiterRPM:     func() int { return 120 },
		RateLimiterBurst:   func() int { return 30 },
		BudgetEnabled:         func() bool { return true },
		BudgetDailyLimitUSD:   func() float64 { return 10.0 },
		BudgetCurrentSpendUSD: func() float64 { return 2.5 },
		BudgetResetAt:         func() time.Time { return resetAt },
		MetricsDBWritable: func() bool { return true },
		MetricsDBPath:     func() string { return "/var/lib/nexus-proxy/metrics.db" },
		SLMCacheEnabled:    func() bool { return true },
		SLMCacheTTLSeconds: func() int { return 30 },
		ArbiterCacheEnabled:    func() bool { return true },
		ArbiterCacheTTLSeconds: func() int { return 300 },
	})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var resp struct {
		Judge         JudgeStatus        `json:"judge"`
		Quality       QualityStatus      `json:"quality"`
		RAG           RAGStatus        `json:"rag"`
		Routing       RoutingSnapshot    `json:"routing"`
		Uptime        int64             `json:"uptime_ms"`
		RateLimiter   RateLimiterStatus `json:"rate_limiter"`
		Budget        BudgetStatus      `json:"budget"`
		MetricsDB     MetricsDBStatus   `json:"metrics_db"`
		SLMCache     SLMCacheStatus   `json:"slm_cache"`
		ArbiterCache  ArbiterCacheStatus `json:"arbiter_cache"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	// Judge assertions
	if !resp.Judge.Enabled {
		t.Error("judge.enabled = false, want true")
	}
	if resp.Judge.Depth != 3 {
		t.Errorf("judge.queue_depth = %d, want 3", resp.Judge.Depth)
	}
	if resp.Judge.Capacity != 64 {
		t.Errorf("judge.queue_capacity = %d, want 64", resp.Judge.Capacity)
	}
	if resp.Judge.Workers != 2 {
		t.Errorf("judge.workers = %d, want 2", resp.Judge.Workers)
	}

	// Quality assertions
	if !resp.Quality.Enabled {
		t.Error("quality.enabled = false, want true")
	}
	if resp.Quality.Depth != 0 {
		t.Errorf("quality.queue_depth = %d, want 0", resp.Quality.Depth)
	}
	if resp.Quality.Capacity != 64 {
		t.Errorf("quality.queue_capacity = %d, want 64", resp.Quality.Capacity)
	}
	if resp.Quality.Workers != 2 {
		t.Errorf("quality.workers = %d, want 2", resp.Quality.Workers)
	}

	// RAG assertions
	if !resp.RAG.Healthy {
		t.Error("rag.embedding_healthy = false, want true")
	}
	if resp.RAG.IndexedExamples != 12 {
		t.Errorf("rag.indexed_examples = %d, want 12", resp.RAG.IndexedExamples)
	}

	// Routing assertions
	if len(resp.Routing.Decisions) != 3 {
		t.Fatalf("len(routing.decisions) = %d, want 3", len(resp.Routing.Decisions))
	}
	if resp.Routing.Decisions[0].Route != "local" {
		t.Errorf("routing.decisions[0].route = %q, want %q", resp.Routing.Decisions[0].Route, "local")
	}
	if resp.Routing.Decisions[0].Count != 10 {
		t.Errorf("routing.decisions[0].count = %d, want 10", resp.Routing.Decisions[0].Count)
	}

	// Uptime assertions
	if resp.Uptime != 3600000 {
		t.Errorf("uptime_ms = %d, want 3600000", resp.Uptime)
	}

	// Rate limiter assertions
	if !resp.RateLimiter.Enabled {
		t.Error("rate_limiter.enabled = false, want true")
	}
	if resp.RateLimiter.RPM != 120 {
		t.Errorf("rate_limiter.rpm = %d, want 120", resp.RateLimiter.RPM)
	}
	if resp.RateLimiter.Burst != 30 {
		t.Errorf("rate_limiter.burst = %d, want 30", resp.RateLimiter.Burst)
	}

	// Budget assertions
	if !resp.Budget.Enabled {
		t.Error("budget.enabled = false, want true")
	}
	if resp.Budget.DailyLimitUSD != 10.0 {
		t.Errorf("budget.daily_limit_usd = %f, want 10.0", resp.Budget.DailyLimitUSD)
	}
	if resp.Budget.CurrentSpendUSD != 2.5 {
		t.Errorf("budget.current_spend_usd = %f, want 2.5", resp.Budget.CurrentSpendUSD)
	}
	if resp.Budget.ResetAt.IsZero() {
		t.Error("budget.reset_at is zero, want non-zero time")
	}

	// Metrics DB assertions
	if !resp.MetricsDB.Writable {
		t.Error("metrics_db.writable = false, want true")
	}
	if resp.MetricsDB.Path != "/var/lib/nexus-proxy/metrics.db" {
		t.Errorf("metrics_db.path = %q, want %q", resp.MetricsDB.Path, "/var/lib/nexus-proxy/metrics.db")
	}

	// SLM cache assertions
	if !resp.SLMCache.Enabled {
		t.Error("slm_cache.enabled = false, want true")
	}
	if resp.SLMCache.TTLSeconds != 30 {
		t.Errorf("slm_cache.ttl_seconds = %d, want 30", resp.SLMCache.TTLSeconds)
	}

	// Arbiter cache assertions
	if !resp.ArbiterCache.Enabled {
		t.Error("arbiter_cache.enabled = false, want true")
	}
	if resp.ArbiterCache.TTLSeconds != 300 {
		t.Errorf("arbiter_cache.ttl_seconds = %d, want 300", resp.ArbiterCache.TTLSeconds)
	}
}

func TestStatusHandlerDisabledSubsystems(t *testing.T) {
	// All subsystems disabled.
	handler := Status(StatusDeps{
		JudgeEnabled:    func() bool { return false },
		JudgeDepth:      func() int { return 0 },
		JudgeCapacity:   func() int { return 0 },
		JudgeWorkers:    func() int { return 0 },
		QualityEnabled:  func() bool { return false },
		QualityDepth:    func() int { return 0 },
		QualityCapacity: func() int { return 0 },
		QualityWorkers:  func() int { return 0 },
		RAGHealthy: func(ctx context.Context) bool {
			return false
		},
		RAGIndexedExamples: func() int { return 0 },
		RoutingSnapshot: func() RoutingSnapshot {
			return RoutingSnapshot{Decisions: nil}
		},
		Uptime:               func() time.Duration { return 0 },
		RateLimiterEnabled:   func() bool { return false },
		RateLimiterRPM:       func() int { return 0 },
		RateLimiterBurst:     func() int { return 0 },
		BudgetEnabled:         func() bool { return false },
		BudgetDailyLimitUSD:   func() float64 { return 0 },
		BudgetCurrentSpendUSD: func() float64 { return 0 },
		BudgetResetAt:         func() time.Time { return time.Time{} },
		MetricsDBWritable:     func() bool { return false },
		MetricsDBPath:         func() string { return "" },
		SLMCacheEnabled:       func() bool { return false },
		SLMCacheTTLSeconds:    func() int { return 0 },
		ArbiterCacheEnabled:    func() bool { return false },
		ArbiterCacheTTLSeconds: func() int { return 0 },
	})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var resp struct {
		Judge         JudgeStatus        `json:"judge"`
		Quality       QualityStatus      `json:"quality"`
		RAG           RAGStatus        `json:"rag"`
		Uptime        int64           `json:"uptime_ms"`
		RateLimiter   RateLimiterStatus `json:"rate_limiter"`
		Budget        BudgetStatus     `json:"budget"`
		MetricsDB     MetricsDBStatus  `json:"metrics_db"`
		SLMCache     SLMCacheStatus  `json:"slm_cache"`
		ArbiterCache  ArbiterCacheStatus `json:"arbiter_cache"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if resp.Judge.Enabled {
		t.Error("judge.enabled = true, want false")
	}
	if resp.Judge.Depth != 0 {
		t.Errorf("judge.queue_depth = %d, want 0", resp.Judge.Depth)
	}
	if resp.Quality.Enabled {
		t.Error("quality.enabled = true, want false")
	}
	if resp.RAG.Healthy {
		t.Error("rag.embedding_healthy = true, want false")
	}
	if resp.RAG.IndexedExamples != 0 {
		t.Errorf("rag.indexed_examples = %d, want 0", resp.RAG.IndexedExamples)
	}

	// Rate limiter assertions
	if resp.RateLimiter.Enabled {
		t.Error("rate_limiter.enabled = true, want false")
	}
	if resp.RateLimiter.RPM != 0 {
		t.Errorf("rate_limiter.rpm = %d, want 0", resp.RateLimiter.RPM)
	}

	// Budget assertions
	if resp.Budget.Enabled {
		t.Error("budget.enabled = true, want false")
	}

	// Metrics DB assertions
	if resp.MetricsDB.Writable {
		t.Error("metrics_db.writable = true, want false")
	}

	// SLM cache assertions
	if resp.SLMCache.Enabled {
		t.Error("slm_cache.enabled = true, want false")
	}

	// Arbiter cache assertions
	if resp.ArbiterCache.Enabled {
		t.Error("arbiter_cache.enabled = true, want false")
	}
}

func TestStatusHandlerContentType(t *testing.T) {
	handler := Status(StatusDeps{
		JudgeEnabled:       func() bool { return false },
		JudgeDepth:         func() int { return 0 },
		JudgeCapacity:      func() int { return 0 },
		JudgeWorkers:       func() int { return 0 },
		QualityEnabled:     func() bool { return false },
		QualityDepth:       func() int { return 0 },
		QualityCapacity:    func() int { return 0 },
		QualityWorkers:     func() int { return 0 },
		RAGHealthy:         func(ctx context.Context) bool { return false },
		RAGIndexedExamples: func() int { return 0 },
		RoutingSnapshot:    func() RoutingSnapshot { return RoutingSnapshot{} },
		Uptime:             func() time.Duration { return 0 },
		RateLimiterEnabled: func() bool { return false },
		RateLimiterRPM:     func() int { return 0 },
		RateLimiterBurst:   func() int { return 0 },
		BudgetEnabled:         func() bool { return false },
		BudgetDailyLimitUSD:   func() float64 { return 0 },
		BudgetCurrentSpendUSD: func() float64 { return 0 },
		BudgetResetAt:         func() time.Time { return time.Time{} },
		MetricsDBWritable:     func() bool { return false },
		MetricsDBPath:         func() string { return "" },
		SLMCacheEnabled:       func() bool { return false },
		SLMCacheTTLSeconds:    func() int { return 0 },
		ArbiterCacheEnabled:    func() bool { return false },
		ArbiterCacheTTLSeconds: func() int { return 0 },
	})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	contentType := rr.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", contentType, "application/json")
	}
}

func TestStatusHandlerNilFunctions(t *testing.T) {
	// Verify that nil function fields don't panic.
	handler := Status(StatusDeps{
		JudgeEnabled:           nil,
		JudgeDepth:             nil,
		JudgeCapacity:          nil,
		JudgeWorkers:           nil,
		QualityEnabled:         nil,
		QualityDepth:           nil,
		QualityCapacity:        nil,
		QualityWorkers:         nil,
		RAGHealthy:             nil,
		RAGIndexedExamples:     nil,
		RoutingSnapshot:        nil,
		Uptime:                 nil,
		RateLimiterEnabled:     nil,
		RateLimiterRPM:         nil,
		RateLimiterBurst:       nil,
		BudgetEnabled:          nil,
		BudgetDailyLimitUSD:    nil,
		BudgetCurrentSpendUSD:  nil,
		BudgetResetAt:          nil,
		MetricsDBWritable:      nil,
		MetricsDBPath:          nil,
		SLMCacheEnabled:        nil,
		SLMCacheTTLSeconds:     nil,
		ArbiterCacheEnabled:    nil,
		ArbiterCacheTTLSeconds: nil,
	})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rr := httptest.NewRecorder()

	// Should not panic.
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	// Verify all zero values.
	var resp struct {
		Judge         JudgeStatus        `json:"judge"`
		Quality       QualityStatus      `json:"quality"`
		RAG           RAGStatus        `json:"rag"`
		Uptime        int64           `json:"uptime_ms"`
		RateLimiter   RateLimiterStatus `json:"rate_limiter"`
		Budget        BudgetStatus     `json:"budget"`
		MetricsDB     MetricsDBStatus  `json:"metrics_db"`
		SLMCache     SLMCacheStatus  `json:"slm_cache"`
		ArbiterCache  ArbiterCacheStatus `json:"arbiter_cache"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if resp.Judge.Enabled {
		t.Error("judge.enabled = true, want false")
	}
	if resp.Judge.Depth != 0 {
		t.Errorf("judge.queue_depth = %d, want 0", resp.Judge.Depth)
	}
}
