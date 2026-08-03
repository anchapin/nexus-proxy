// Command nexus is the entry point for the Nexus Proxy. It loads
// configuration from the environment, constructs the chat handler with its
// collaborators (RAG store, SLM client, formatting regex, judge observer,
// telemetry recorder), and serves /v1/chat/completions.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/handlers"
	"github.com/anchapin/nexus-proxy/internal/health"
	"github.com/anchapin/nexus-proxy/internal/judge"
	"github.com/anchapin/nexus-proxy/internal/metrics"
	"github.com/anchapin/nexus-proxy/internal/probe"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/secrets"
	"github.com/anchapin/nexus-proxy/internal/telemetry"
	"github.com/anchapin/nexus-proxy/internal/tracing"
)

const (
	bootRAGTimeout = 30 * time.Second
)

// version is the build version. Overridden at compile time via
// -ldflags "-X main.version=v1.2.3" in the Makefile and the release
// workflow. The default "dev" lets `nexus --version` work from a
// local `make build` without any special setup.
var version = "dev"

// commit is the git commit SHA. Overridden at compile time via
// -ldflags "-X main.commit=$(git rev-parse HEAD)". Default "unknown".
var commit = "unknown"

// circuitBreakerAdapter bridges the chat handler's CircuitBreakerObserver
// calls into the observability Collector (issue #304, #886).
type circuitBreakerAdapter struct {
	recordFailure      func(string)
	recordRecovery     func(string)
	incEmbedderFailure func(string)
	incRAGCircuitTrip  func(string)
	incRAGCircuitRecov func(string)
}

func (a circuitBreakerAdapter) RecordCircuitFailure(circuit string)  { a.recordFailure(circuit) }
func (a circuitBreakerAdapter) RecordCircuitRecovery(circuit string) { a.recordRecovery(circuit) }
func (a circuitBreakerAdapter) IncEmbedderFailure(kind string)       { a.incEmbedderFailure(kind) }
func (a circuitBreakerAdapter) IncRAGCircuitTrip(kind string)        { a.incRAGCircuitTrip(kind) }
func (a circuitBreakerAdapter) IncRAGCircuitRecover(kind string)     { a.incRAGCircuitRecov(kind) }

func main() {
	startTime := time.Now()

	// Subcommand dispatch (issue #32, #1141). dispatch() handles
	// check/doctor/config/dashboard/judge/routing-preview/version/help
	// and calls os.Exit when a subcommand is matched. When no
	// subcommand is present it returns so we start the proxy.
	dispatchFromOS()

	cfg, err := config.Load()
	if err != nil {
		// The structured logger is not yet wired, so use the std
		// log.Fatalf path. This is one of two unrecoverable boot
		// errors (issue #3).
		log.Fatalf("config: %v", err)
	}

	// External secret-manager resolution (issue #1173). When a non-env
	// backend is configured, resolve API keys from Vault / AWS SM before
	// proceeding. Fail-closed: unreachable backend aborts boot.
	if cfg.SecretBackend != "" && cfg.SecretBackend != "env" {
		resolver, err := secrets.NewResolver(secrets.BackendConfig{
			Backend:     cfg.SecretBackend,
			VaultAddr:   cfg.VaultAddr,
			VaultToken:  cfg.VaultToken,
			VaultRole:   cfg.VaultRole,
			VaultPath:   cfg.VaultPath,
			AWSSMPrefix: cfg.AWSSMPrefix,
		})
		if err != nil {
			log.Fatalf("secrets: %v", err)
		}
		store := secrets.NewSecretStore(resolver)
		if err := store.Populate(); err != nil {
			log.Fatalf("secrets: %v", err)
		}
		// Override env-sourced credentials with resolver values. A resolver
		// value of "" (not found in external store, not in env) is left as-is.
		if v := store.Get("NEXUS_FRONTIER_API_KEY"); v != "" {
			cfg.FrontierKey = v
		}
		if v := store.Get("NEXUS_ZAI_API_KEY"); v != "" {
			cfg.ZAIKey = v
		}
		if v := store.Get("NEXUS_PROXY_API_KEY"); v != "" {
			cfg.ProxyAPIKey = v
		}
		if v := store.Get("NEXUS_JUDGE_API_KEY"); v != "" {
			cfg.JudgeAPIKey = v
		}
		if v := store.Get("NEXUS_COHERE_API_KEY"); v != "" {
			cfg.CohereAPIKey = v
		}
		// Start periodic refresh if configured (issue #1173 acceptance criterion).
		if cfg.SecretRefresh > 0 {
			slog.Info("secret refresh enabled",
				slog.String("component", "secrets"),
				slog.Duration("interval", cfg.SecretRefresh),
			)
			cancel := store.StartRefresh(cfg.SecretRefresh)
			defer cancel()
		}
		defer store.Close()
	}
	logger := cfg.NewLogger()
	// Wrap the slog handler so trace_id / span_id are injected into
	// every log record inside a traced request's context (issue #1169).
	// Only wraps when tracing is enabled — when disabled, zero overhead.
	if cfg.TracingEndpoint != "" && cfg.LogTraceID {
		logger = slog.New(tracing.NewLogHandler(logger.Handler()))
	}
	slog.SetDefault(logger)

	srv, parts, cleanup, err := buildServer(cfg, startTime)
	if err != nil {
		log.Fatalf("server: %v", err)
	}
	defer cleanup()

	// Graceful shutdown: stop accepting new connections, drain
	// in-flight requests, then drain the judge queue so we don't
	// lose pending JudgeScore records (issue #121).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("shutting down, draining judge queue and closing server",
			slog.Duration("drain_budget", cfg.ShutdownTimeout),
		)
		parts.drainComponents()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("server shutdown", slog.Any("err", err))
		}
	}()

	// SIGHUP-based config hot reload (issue #306).
	sighupCh := make(chan os.Signal, 1)
	signal.Notify(sighupCh, syscall.SIGHUP)
	go func() {
		for range sighupCh {
			cfg = parts.handleSIGHUP(cfg)
		}
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		// Unrecoverable boot/server error — log.Fatalf is kept
		// here per the issue #3 acceptance criteria.
		log.Fatalf("server: %v", err)
	}
}

// printVersion writes the build version to w. Extracted from main()
// so it can be unit-tested without os.Exec.
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "nexus %s\n", version)
}

// confidenceBridge adapts the judge's Storage seam to the router's
// ConfidenceStore (issue #47). The judge worker pool calls Record with a
// JudgeScore that carries no task category, so the bridge remembers the
// category per request id (stashed by the observer at enqueue time) and
// resolves it when the score lands. It delegates to an inner Storage (the
// in-memory judge log) so existing judge behaviour is preserved.
//
// This adapter lives in main.go — not internal/judge or internal/router —
// so neither package imports the other (the AGENTS.md dependency rule).
type confidenceBridge struct {
	inner judge.Storage
	conf  router.ConfidenceStore
	mu    sync.Mutex
	cats  map[string]string // request id -> category
}

func newConfidenceBridge(inner judge.Storage, conf router.ConfidenceStore) *confidenceBridge {
	return &confidenceBridge{inner: inner, conf: conf, cats: make(map[string]string)}
}

// note stashes the category for a request id before it is enqueued so the
// async Record can resolve it once the judge score lands.
func (b *confidenceBridge) note(requestID, category string) {
	b.mu.Lock()
	b.cats[requestID] = category
	b.mu.Unlock()
}

// forget drops a stashed category so the map does not leak when an enqueue
// is rejected (queue full) and no score will ever arrive.
func (b *confidenceBridge) forget(requestID string) {
	b.mu.Lock()
	delete(b.cats, requestID)
	b.mu.Unlock()
}

// Record resolves the category for the scored request and feeds the
// outcome into the confidence store, then delegates to the inner storage.
// Parse-failure scores (Err set, Score outside 1..5, or Score==0 with no Err)
// are persisted by the inner storage but excluded from the confidence aggregate.
//
// NOTE(issue #1017): A previous version of this function had an "else if"
// branch that called RecordOutcome with a hardcoded RouteLocal before the
// route-aware call, causing duplicate RecordOutcome invocations for in-range
// scores (1-5). The fix consolidates into a single RecordOutcome call
// inside the else block, with empty s.Route defaulting to RouteLocal.
func (b *confidenceBridge) Record(s judge.JudgeScore) error {
	b.mu.Lock()
	cat, ok := b.cats[s.RequestID]
	delete(b.cats, s.RequestID)
	b.mu.Unlock()
	if ok && s.Err == nil {
		if s.Score < 1 || s.Score > 5 {
			slog.Warn("confidence: judge score out of range, dropped",
				slog.String("request_id", s.RequestID),
				slog.Int("score", s.Score),
			)
		} else if s.Score == 0 {
			slog.Debug("confidence: score=0 with no error, treating as parse failure")
		} else {
			route := s.Route
			if route == "" {
				route = string(router.RouteLocal) // safety default; empty Route should not occur
			}
			if err := b.conf.RecordOutcome(cat, router.Route(route), s.Score); err != nil {
				slog.Warn("confidence: record outcome rejected",
					slog.String("request_id", s.RequestID),
					slog.Any("err", err),
				)
			}
		}
	}
	return b.inner.Record(s)
}

// Close delegates to the inner judge storage. The confidence store's own
// *sql.DB is closed separately in main (it is owned there, not by the
// bridge).
func (b *confidenceBridge) Close() error { return b.inner.Close() }

// buildRAGStore constructs the RAG store (issue #46). Returns:
//   - store: the RAGStore the chat handler is wired to (PersistentStore
//     or in-memory Store, both satisfy the interface);
//   - persistentStore: non-nil only when persistence is enabled, so the
//     caller knows whether to schedule a Close() on shutdown;
//   - watcher: non-nil only when the background file watcher is
//     running, so the caller can Stop() it before closing the DB.
//
// Boot path:
//   - When NEXUS_RAG_DB is set: open PersistentStore, call
//     LoadOrIndex (Load if the DB has rows, otherwise IndexDir which
//     embeds AND persists each file). On any error during open we log
//     and fall back to the legacy in-memory Store so the proxy still
//     serves traffic — persistence is an optimisation, not a
//     correctness requirement.
//   - When NEXUS_RAG_DB is empty: construct a plain in-memory Store
//     and run the original IndexDir path; this is byte-for-byte
//     identical to the pre-issue-46 behaviour.
//
// The watcher is started only when persistence is enabled AND
// NEXUS_RAG_POLL_INTERVAL > 0; an interval of zero leaves
// persistence on but disables runtime updates (boot-only load).
func buildRAGStore(cfg config.Config, emb rag.Embedder, bootCtx context.Context) (rag.RAGStore, *rag.PersistentStore, *rag.Watcher, rag.Embedder) {
	// emb is already wrapped with EmbedCache by the caller (issue #115, #303)
	cachedEmb := emb
	// Build the file extension/pattern filter (issue #1148). Nil when
	// both vars are empty → backward-compatible (index all files).
	fileFilter := rag.NewFileFilter(cfg.RAGFileExtensions, cfg.RAGExcludePatterns)
	if !cfg.RAGPersistentEnabled() {
		slog.Info("rag persistent store disabled (NEXUS_RAG_DB is empty); using in-memory store")
		store := rag.NewStore(cachedEmb, cfg.RAGThreshold,
			rag.WithBatchSize(cfg.RAGBatchSize),
			rag.WithChunkTokens(cfg.RAGChunkTokens),
			rag.WithRecursive(cfg.RAGRecursive),
			rag.WithFileFilter(fileFilter),
			rag.WithDedupThreshold(cfg.RAGDedupThreshold),
			rag.WithDedupCrossDir(cfg.RAGDedupCrossDir))
		if err := store.IndexDir(bootCtx, cfg.ExamplesDir); err != nil {
			slog.Warn("rag index failed", slog.Any("err", err))
		}
		return store, nil, nil, cachedEmb
	}

	ps, err := rag.OpenPersistentStore(cfg.RAGDBPath, cachedEmb, cfg.RAGThreshold,
		rag.WithBatchSize(cfg.RAGBatchSize),
		rag.WithChunkTokens(cfg.RAGChunkTokens),
		rag.WithRecursive(cfg.RAGRecursive),
		rag.WithFileFilter(fileFilter),
		rag.WithDedupThreshold(cfg.RAGDedupThreshold),
		rag.WithDedupCrossDir(cfg.RAGDedupCrossDir))
	if err != nil {
		// Persistence is a best-effort optimisation. Fall back to
		// the in-memory store so the proxy still serves traffic —
		// an operator with a broken cache should not blackhole
		// requests.
		slog.Error("rag persistent store open failed, falling back to in-memory store",
			slog.String("path", cfg.RAGDBPath),
			slog.Any("err", err),
		)
		store := rag.NewStore(cachedEmb, cfg.RAGThreshold,
			rag.WithBatchSize(cfg.RAGBatchSize),
			rag.WithChunkTokens(cfg.RAGChunkTokens),
			rag.WithRecursive(cfg.RAGRecursive),
			rag.WithFileFilter(fileFilter),
			rag.WithDedupThreshold(cfg.RAGDedupThreshold),
			rag.WithDedupCrossDir(cfg.RAGDedupCrossDir))
		if err := store.IndexDir(bootCtx, cfg.ExamplesDir); err != nil {
			slog.Warn("rag index failed", slog.Any("err", err))
		}
		return store, nil, nil, cachedEmb
	}

	n, err := ps.LoadOrIndex(bootCtx, cfg.ExamplesDir)
	if err != nil {
		// If we can't read the DB AND can't re-index, fall back to
		// a fresh in-memory store so the chat hot path still
		// works. The persistent DB stays closed but unused.
		slog.Error("rag load/index failed, falling back to in-memory store",
			slog.String("path", cfg.RAGDBPath),
			slog.Any("err", err),
		)
		_ = ps.Close()
		store := rag.NewStore(cachedEmb, cfg.RAGThreshold,
			rag.WithRecursive(cfg.RAGRecursive))
		if err := store.IndexDir(bootCtx, cfg.ExamplesDir); err != nil {
			slog.Warn("rag index failed", slog.Any("err", err))
		}
		return store, nil, nil, cachedEmb
	}
	slog.Info("rag persistent store ready",
		slog.String("path", cfg.RAGDBPath),
		slog.Int("examples", n),
	)

	var watcher *rag.Watcher
	if cfg.RAGWatcherEnabled() {
		watcher = rag.NewWatcher(ps, cfg.ExamplesDir, cfg.RAGPollInterval)
		watcher.SetRecursive(cfg.RAGRecursive)
		watcher.Start(context.Background())
		slog.Info("rag file watcher enabled",
			slog.String("dir", cfg.ExamplesDir),
			slog.Duration("interval", cfg.RAGPollInterval),
		)
	} else {
		slog.Info("rag file watcher disabled (NEXUS_RAG_POLL_INTERVAL=0); boot-time load only")
	}

	return ps, ps, watcher, cachedEmb
}

// buildRecorder constructs the telemetry recorder from config. A disabled
// TelemetryPath returns a Noop so the handler can stay recorder-agnostic.
func buildRecorder(cfg config.Config) telemetry.Recorder {
	if !cfg.TelemetryEnabled() {
		slog.Info("telemetry disabled (NEXUS_TELEMETRY_PATH is empty)")
		return telemetry.Noop{}
	}
	r, err := telemetry.NewJSONLRecorder(cfg.TelemetryPath, int64(cfg.TelemetryMaxBytes), cfg.TelemetryMaxFiles, cfg.TelemetryBufferSize, cfg.TelemetryFlushInterval)
	if err != nil {
		slog.Error("telemetry recorder init failed, falling back to Noop", slog.Any("err", err))
		return telemetry.Noop{}
	}
	if cfg.TelemetryMaxBytes > 0 {
		slog.Info("telemetry rotation enabled",
			slog.Int("max_bytes", cfg.TelemetryMaxBytes),
			slog.Int("max_files", cfg.TelemetryMaxFiles),
			slog.Int("buffer_size", cfg.TelemetryBufferSize),
			slog.Duration("flush_interval", cfg.TelemetryFlushInterval),
			slog.String("path", r.Path()),
		)
	} else {
		slog.Info("telemetry recording",
			slog.Int("buffer_size", cfg.TelemetryBufferSize),
			slog.Duration("flush_interval", cfg.TelemetryFlushInterval),
			slog.String("path", r.Path()),
		)
	}
	return r
}

// buildMetrics opens the SQLite metrics store when NEXUS_METRICS_DB is
// set. Returns a nil store (and a nil observer) when the operator
// opted out, which lets the handler take the no-metrics fast path.
//
// The observer is a tiny adapter from handlers.MetricsEvent to
// metrics.Request — same pattern as the judge/quality observers.
// batchCallback is invoked after every committed SQLite transaction
// in the drain goroutine (issue #1234); pass nil if no callback needed.
func buildMetrics(cfg config.Config, batchCallback func()) (metrics.Store, handlers.MetricsObserver) {
	if !cfg.MetricsEnabled() {
		slog.Info("metrics disabled (NEXUS_METRICS_DB is empty)")
		return nil, nil
	}
	batchCfg := metrics.BatchConfig{
		Size:     cfg.MetricsBatchSize,
		Timeout:  cfg.MetricsBatchTimeout,
		Callback: batchCallback,
	}
	store, err := metrics.OpenWithRetention(cfg.MetricsDBPath, cfg.MetricsRetentionDays, nil, batchCfg)
	if err != nil {
		slog.Error("metrics open failed, metrics disabled", slog.Any("err", err))
		return nil, nil
	}
	if ss, ok := store.(*metrics.SQLiteStore); ok {
		slog.Info("metrics recording",
			slog.String("path", ss.Path()),
			slog.Int("retention_days", cfg.MetricsRetentionDays),
			slog.Int("batch_size", cfg.MetricsBatchSize),
		)
	}
	obs := handlers.MetricsObserverFunc(func(e handlers.MetricsEvent) {
		// The adapter does its own error handling — RecordRequest
		// never blocks the caller, but we still swallow the
		// (currently always-nil) error so the handler stays
		// caller-agnostic.
		_ = store.RecordRequest(metrics.Request{
			Timestamp:          e.Timestamp,
			RequestID:          e.RequestID,
			Route:              e.Route,
			Model:              e.Model,
			InputTokens:        e.InputTokens,
			TOONSavingsTokens:  e.TOONSavingsTokens,
			RAGInjected:        e.RAGInjected,
			RAGFilename:        e.RAGFilename,
			EstimatedCostUSD:   e.EstimatedCostUSD,
			BaselineCostUSD:    e.BaselineCostUSD,
			SavingsUSD:         e.SavingsUSD,
			OutputTokens:       e.OutputTokens,
			TTFTMs:             e.TTFTMs,
			TotalLatencyMs:     e.TotalLatencyMs,
			TPS:                e.TPS,
			Streaming:          e.Streaming,
			Error:              e.Error,
			RouteSource:        e.RouteSource,
			RouteReason:        e.RouteReason,
			SLMConfidence:      e.SLMConfidence,
			SLMTaskType:        e.SLMTaskType,
			ArbiterCacheKeyHex: e.ArbiterCacheKeyHex,
			ArbiterSynthesis:   e.ArbiterSynthesis,
			Tenant:             e.Tenant,
		})
	})
	return store, obs
}

// budgetObserver adapts the probe.Manager atomic snapshot into the
// handler-facing BudgetObserver (issue #6). Keeping the adapter here
// — rather than importing probe from handlers — preserves the
// dependency direction: handlers stays free of the probe import;
// only main.go knows both sides.
//
// When the manager is nil (defensive — NewManager panics on nil
// probe but a wiring mistake should never panic the binary) the
// adapter returns 0 / "static-fallback" so the handler falls back
// to the operator-configured NEXUS_TOKEN_GUARDRAIL.
func budgetObserver(mgr *probe.Manager) handlers.BudgetObserver {
	if mgr == nil {
		return handlers.BudgetObserverFunc{
			Tokens: func() int { return 0 },
			Source: func() string { return string(probe.SourceStatic) },
		}
	}
	return handlers.BudgetObserverFunc{
		Tokens: func() int { return mgr.Get().Tokens },
		Source: func() string {
			src := mgr.Get().Source
			if src == "" {
				return string(probe.SourceStatic)
			}
			return string(src)
		},
	}
}

// healthzHandler returns the /healthz handler. Status code is
// always 200 when the binary is alive; the JSON body carries the
// per-request VRAM budget, the source label, the fallback value
// the operator configured, whether the local Ollama poller
// considers Ollama healthy (nil hpoller -> true, matches the
// health.Health nil-safe contract), and the per-frontier-provider
// circuit state (issue #1158).
func healthzHandler(hpoller *health.Health, fhpoller *health.FrontierHealth, mgr *probe.Manager, cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		budget := probe.Budget{Source: probe.SourceStatic}
		if mgr != nil {
			budget = mgr.Get()
		}
		// When the probe has no budget to offer (still booting,
		// disabled, or every signal unavailable) we echo the
		// operator-configured TokenGuardrail so /healthz always
		// reports a concrete number operators can grep against.
		displayTokens := budget.Tokens
		source := string(budget.Source)
		if displayTokens <= 0 {
			displayTokens = cfg.TokenGuardrail
			source = string(probe.SourceStatic)
		}

		// Per-frontier-provider circuit state (issue #1158). When
		// the poller is nil the map is empty so the field is omitted
		// from the JSON (omitempty).
		type providerHealth struct {
			Name         string `json:"name"`
			Healthy      bool   `json:"healthy"`
			FailureCount int32  `json:"failure_count"`
		}
		var frontierProviders []providerHealth
		for _, st := range fhpoller.States() {
			frontierProviders = append(frontierProviders, providerHealth{
				Name:         st.Name,
				Healthy:      st.Healthy,
				FailureCount: st.FailureCount,
			})
		}

		resp := struct {
			Status            string           `json:"status"`
			OllamaHealthy     bool             `json:"ollama_healthy"`
			BudgetTokens      int              `json:"budget_tokens"`
			BudgetSource      string           `json:"budget_source"`
			FreeVRAMBytes     int64            `json:"free_vram_bytes,omitempty"`
			ModelContext      int              `json:"model_context,omitempty"`
			StaticFallback    int              `json:"static_fallback_tokens"`
			FrontierProviders []providerHealth `json:"frontier_providers,omitempty"`
		}{
			Status:            "ok",
			OllamaHealthy:     hpoller == nil || hpoller.IsLocalHealthy(),
			BudgetTokens:      displayTokens,
			BudgetSource:      source,
			FreeVRAMBytes:     budget.FreeVRAMBytes,
			ModelContext:      budget.ModelContext,
			StaticFallback:    cfg.TokenGuardrail,
			FrontierProviders: frontierProviders,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// publicPathExempt returns true for paths that must bypass the
// inbound auth gate (issue #109). /healthz and /metrics are always
// exempt so K8s probes and Prometheus scrapers work without
// credentials. /status is exempt only when NEXUS_STATUS_PUBLIC=true
// (default false) — the diagnostics surface (frontier configured,
// judge enabled, VRAM state) is reconnaissance-grade and should be
// gated by default. The web dashboard path (issue #1182, default
// /dashboard) is exempt only when NEXUS_DASHBOARD_PUBLIC=true, mirroring
// the /status posture. /debug/* is always exempt because the pprof/expvar
// subtree carries its own independent gate (DebugPprofGate, issue
// #1150); double-gating would require operators to pass the proxy API
// key before the debug key, adding friction without security gain.
func publicPathExempt(cfg config.Config) func(*http.Request) bool {
	dashPath := cfg.DashboardEndpoint
	if dashPath == "" {
		dashPath = "/dashboard"
	}
	return func(r *http.Request) bool {
		switch r.URL.Path {
		case "/healthz", "/metrics", "/readyz":
			return true
		case "/status":
			return cfg.StatusPublic
		case dashPath:
			return cfg.DashboardPublic
		default:
			// Debug pprof/expvar subtree (issue #1150). The
			// /debug/ prefix is exempt so the debug gate
			// (API key or loopback) is the sole access control.
			if strings.HasPrefix(r.URL.Path, "/debug/") {
				return true
			}
			return false
		}
	}
}
