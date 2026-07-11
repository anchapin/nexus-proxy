// Command nexus is the entry point for the Nexus Proxy. It loads
// configuration from the environment, constructs the chat handler with its
// collaborators (RAG store, SLM client, formatting regex, judge observer,
// telemetry recorder), and serves /v1/chat/completions.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/handlers"
	"github.com/anchapin/nexus-proxy/internal/health"
	"github.com/anchapin/nexus-proxy/internal/judge"
	"github.com/anchapin/nexus-proxy/internal/metrics"
	"github.com/anchapin/nexus-proxy/internal/probe"
	"github.com/anchapin/nexus-proxy/internal/quality"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/telemetry"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"
)

const (
	formattingRegexPattern = `(?i)\b(css|format|docstring|lint|typo|boilerplate)\b`
	bootRAGTimeout         = 30 * time.Second
	shutdownTimeout        = 10 * time.Second
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		// The structured logger is not yet wired, so use the std
		// log.Fatalf path. This is one of two unrecoverable boot
		// errors (issue #3) — every other log call below flows
		// through slog.
		log.Fatalf("config: %v", err)
	}
	logger := cfg.NewLogger()
	slog.SetDefault(logger)

	emb := rag.NewOllamaEmbedder(cfg.OllamaURL, cfg.EmbeddingModel, nil)
	store := rag.NewStore(emb, cfg.RAGThreshold)
	bootCtx, cancel := context.WithTimeout(context.Background(), bootRAGTimeout)
	defer cancel()
	if err := store.IndexDir(bootCtx, cfg.ExamplesDir); err != nil {
		slog.Warn("rag index failed", slog.Any("err", err))
	}

	slm := router.NewSLMClient(cfg.OllamaURL, cfg.RouterModel, cfg.SLMTimeout, nil)
	// Judge-guided adaptive routing (issue #47): the confidence
	// floor/ceiling bound the neutral band the SLM uses when a
	// confidence signal is supplied. Zero values fall back to the
	// router defaults, so this is safe even when the feature is off.
	slm.ConfidenceFloor = cfg.RoutingConfidenceFloor
	slm.ConfidenceCeiling = cfg.RoutingConfidenceCeiling
	re := regexp.MustCompile(formattingRegexPattern)

	// Ollama health poller (issue #8). When NEXUS_HEALTH_POLL_INTERVAL
	// is zero the handler treats Ollama as always healthy (useful for
	// containers that know Ollama is co-located and unreachable
	// states are impossible). Otherwise a background goroutine
	// pings /api/tags on the cadence and the chat handler reroutes
	// route=local/route=fusion to frontier when Ollama trips the
	// breaker. The poller's context is the boot context so a
	// fatal-level config error cancels it cleanly.
	var hpoller *health.Health
	if cfg.HealthPollInterval > 0 {
		hpoller = health.New(
			cfg.OllamaURL,
			cfg.HealthPollInterval,
			cfg.HealthBreakerThreshold,
			cfg.HealthProbeTimeout,
			http.DefaultClient,
		)
		go hpoller.Run(context.Background())
		defer func() {
			if err := hpoller.Close(); err != nil {
				slog.Warn("health poller close", slog.Any("err", err))
			}
		}()
	} else {
		slog.Info("ollama health poller disabled (NEXUS_HEALTH_POLL_INTERVAL=0)")
	}

	// Hardware-aware VRAM probe (issue #6). Replaces the static
	// NEXUS_TOKEN_GUARDRAIL with a live measurement of the loaded
	// Ollama model's context_length (via /api/ps) and the AMD GPU
	// sysfs VRAM nodes. The manager performs an initial blocking
	// probe so the first proxied request after boot already sees the
	// dynamic budget, then re-polls on the configured cadence. When
	// the probe is disabled (NEXUS_PROBE_INTERVAL=0) the manager
	// still runs the boot probe once and the chat handler falls
	// back to the static value when it produces no budget.
	probeImpl := probe.NewOllamaProbe(cfg.OllamaURL, http.DefaultClient)
	probeImpl.BytesPerToken = cfg.ProbeBytesPerToken
	probeMgr := probe.NewManager(probeImpl, cfg.ProbePollInterval, cfg.ProbeTimeout)
	go probeMgr.Run(context.Background())
	defer func() {
		if err := probeMgr.Close(); err != nil {
			slog.Warn("probe manager close", slog.Any("err", err))
		}
	}()
	if cfg.ProbePollInterval > 0 {
		slog.Info("vram probe enabled",
			slog.Duration("interval", cfg.ProbePollInterval),
			slog.Duration("timeout", cfg.ProbeTimeout),
		)
	} else {
		slog.Info("vram probe polling disabled (NEXUS_PROBE_INTERVAL=0); boot snapshot only")
	}

	// Async LLM-as-a-judge evaluator (issue #15). The handler never
	// imports internal/judge; we plug the observer in here via a
	// closure that adapts LocalCompletion to the evaluator's
	// Sample + Enqueue entry points.
	var (
		judgeEval       *judge.Evaluator
		judgeObs        handlers.JudgeObserver
		confidenceStore *router.SQLiteConfidenceStore
		confidenceObs   router.ConfidenceStore
	)
	if cfg.JudgeEnabled && cfg.JudgeAPIKey != "" {
		evalCfg := judge.Config{
			URL:         cfg.JudgeURL,
			Model:       cfg.JudgeModel,
			APIKey:      cfg.JudgeAPIKey,
			SampleRate:  cfg.JudgeSampleRate,
			Concurrency: cfg.JudgeConcurrency,
			QueueDepth:  cfg.JudgeQueueDepth,
			Timeout:     cfg.JudgeTimeout,
			CostPer1K:   cfg.JudgeCostPer1KUSD,
		}
		// Issue #16 will swap this for a SQLite-backed Storage. The
		// interface is identical so the swap is a one-line change.
		var storage judge.Storage = judge.NewMemoryStorage()

		// Judge-guided adaptive routing (issue #47). When enabled we
		// open the confidence store and wrap the judge storage in a
		// bridge that feeds each landed JudgeScore back into the store
		// as a per-category local outcome. The observer stashes the
		// prompt category at enqueue time so the bridge can resolve it
		// when the async score arrives.
		var bridge *confidenceBridge
		if cfg.RoutingConfidenceEnabled() {
			cs, cerr := router.OpenConfidenceStore(router.ConfidenceConfig{
				Path:       cfg.RoutingConfidenceDB,
				MinSamples: cfg.RoutingConfidenceMinSamples,
				Window:     cfg.RoutingConfidenceWindow,
			})
			if cerr != nil {
				slog.Error("routing confidence store open failed, adaptive routing disabled",
					slog.Any("err", cerr))
			} else {
				confidenceStore = cs
				confidenceObs = cs
				bridge = newConfidenceBridge(storage, cs)
				storage = bridge
				slog.Info("adaptive routing enabled",
					slog.String("db", cs.Path()),
					slog.Float64("floor", cfg.RoutingConfidenceFloor),
					slog.Float64("ceiling", cfg.RoutingConfidenceCeiling),
					slog.Int("min_samples", cfg.RoutingConfidenceMinSamples),
					slog.Duration("window", cfg.RoutingConfidenceWindow),
				)
			}
		}

		judgeEval = judge.NewEvaluator(evalCfg, http.DefaultClient, storage)
		judgeObs = handlers.JudgeObserverFunc(func(c handlers.LocalCompletion) {
			if !judgeEval.Sample() {
				return
			}
			if bridge != nil {
				bridge.note(c.RequestID, router.Categorize(c.Instruction))
			}
			if !judgeEval.Enqueue(judge.Sample{
				RequestID:   c.RequestID,
				Instruction: c.Instruction,
				Output:      c.Output,
				LocalModel:  c.LocalModel,
			}) {
				if bridge != nil {
					bridge.forget(c.RequestID)
				}
				slog.Warn("judge queue full, dropped request", slog.String("request_id", c.RequestID))
			}
		})
		slog.Info("judge enabled",
			slog.String("url", cfg.JudgeURL),
			slog.String("model", cfg.JudgeModel),
			slog.Float64("sample_rate", cfg.JudgeSampleRate),
			slog.Int("concurrency", cfg.JudgeConcurrency),
		)
	} else {
		slog.Info("judge disabled (sample rate <= 0 or no api key)")
	}
	// The confidence store's lifetime is tied to the judge storage
	// (the bridge delegates Close to the inner judge storage), but the
	// underlying *sql.DB is owned here. Closing it on shutdown flushes
	// WAL frames. judgeEval.Close (below, on signal) drains the queue
	// first, so any in-flight outcome is recorded before this runs.
	defer func() {
		if confidenceStore != nil {
			if err := confidenceStore.Close(); err != nil {
				slog.Warn("routing confidence store close", slog.Any("err", err))
			}
		}
	}()

	recorder := buildRecorder(cfg)
	defer func() {
		if err := recorder.Close(); err != nil {
			slog.Error("telemetry close", slog.Any("err", err))
		}
	}()

	// SQLite-backed metrics store (issue #4). When configured the
	// per-request savings events go here; the JSONL recorder above
	// is left in place so operators can still get a tail-friendly
	// log. Hand-off via MetricsObserver keeps the handlers package
	// free of the metrics import (same dependency rule as judge
	// and quality).
	metricsStore, metricsObs := buildMetrics(cfg)
	defer func() {
		if metricsStore != nil {
			if err := metricsStore.Close(); err != nil {
				slog.Error("metrics close", slog.Any("err", err))
			}
		}
	}()

	// Async AST/compiler verifier (issue #13). The handler never
	// imports internal/quality; we plug a closure in that maps the
	// handler's QualityEvent shape to the verifier's Event shape
	// and dispatches via the verifier's non-blocking Submit. The
	// verifier is dormant when QualityConcurrency is non-positive
	// — same pattern as the judge, so the handler is unaffected
	// when the operator leaves the verifier disabled.
	var (
		verifier *quality.ShellVerifier
		qualityO handlers.QualityObserver
	)
	if cfg.QualityEnabled {
		verifier = quality.NewShellVerifier(quality.Config{
			Concurrency: cfg.QualityConcurrency,
			QueueDepth:  cfg.QualityQueueDepth,
			Timeout:     cfg.QualityTimeout,
			StderrCap:   cfg.QualityStderrCap,
			Observer: quality.ObserverFunc(func(v quality.Verdict) {
				// Sink: forward to the telemetry row keyed by
				// request id (issue #16 will materialise this
				// in the SQLite schema). For now we log the
				// verdict so operators can confirm the worker
				// is doing real work.
				if v.Err != nil {
					slog.Warn("quality verdict error",
						slog.String("request_id", v.Event.RequestID),
						slog.String("path", v.Event.Path),
						slog.String("repo_root", v.RepoRoot),
						slog.Bool("pass", v.Pass),
						slog.Int("exit_code", v.ExitCode),
						slog.Any("err", v.Err),
					)
					return
				}
				slog.Info("quality verdict",
					slog.String("request_id", v.Event.RequestID),
					slog.String("path", v.Event.Path),
					slog.String("repo_root", v.RepoRoot),
					slog.String("kind", string(v.Kind)),
					slog.Bool("pass", v.Pass),
					slog.Int("exit_code", v.ExitCode),
					slog.Int64("duration_ms", v.DurationMs),
				)
			}),
		})
		qualityO = handlers.QualityObserverFunc(func(e handlers.QualityEvent) {
			if !verifier.Submit(quality.Event{
				RequestID: e.RequestID,
				Path:      e.Path,
				ToolName:  e.ToolName,
			}) {
				slog.Warn("quality queue full, dropped request",
					slog.String("request_id", e.RequestID),
					slog.String("path", e.Path),
				)
			}
		})
		slog.Info("quality verifier enabled",
			slog.Int("concurrency", cfg.QualityConcurrency),
			slog.Int("queue", cfg.QualityQueueDepth),
			slog.Duration("timeout", cfg.QualityTimeout),
		)
	} else {
		slog.Info("quality verifier disabled (concurrency <= 0)")
	}
	defer func() {
		if verifier != nil {
			if err := verifier.Close(); err != nil {
				slog.Warn("quality verifier close", slog.Any("err", err))
			}
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/v1/chat/completions", handlers.Chat(handlers.Deps{
		Config:          cfg,
		Client:          http.DefaultClient,
		RAG:             store,
		SLM:             slm,
		FormattingRegex: re,
		Confidence:      confidenceObs,
		JudgeObserver:   judgeObs,
		QualityObserver: qualityO,
		MetricsObserver: metricsObs,
		Recorder:        recorder,
		Health:          hpoller,
		BudgetObserver:  budgetObserver(probeMgr),
	}))

	// /healthz returns a small JSON document so operators can see
	// the dynamic VRAM budget without scraping logs (issue #6).
	// Status code is always 200 when the binary is alive; the body
	// carries the bootstrap state (`ollama_healthy`,
	// `budget_tokens`, `budget_source`). Compose/K8s liveness probes
	// that pipe `curl /healthz` into grep will still match the
	// `"status":"ok"` field.
	mux.HandleFunc("/healthz", healthzHandler(hpoller, probeMgr, cfg))
	slog.Info("healthz endpoint serves dynamic budget JSON",
		slog.String("ollama_url", cfg.OllamaURL),
	)

	slog.Info("starting nexus proxy",
		slog.String("addr", cfg.Addr),
		slog.String("local_model", cfg.LocalModel),
		slog.String("frontier_model", cfg.FrontierModel),
	)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown: stop accepting new connections, drain
	// in-flight requests, then drain the judge queue so we don't
	// lose pending JudgeScore records. The telemetry recorder is
	// closed via the deferred call above so it always flushes,
	// even on log.Fatalf.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("shutting down, draining judge queue and closing server")
		if judgeEval != nil {
			if err := judgeEval.Close(); err != nil {
				slog.Warn("judge close", slog.Any("err", err))
			}
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("server shutdown", slog.Any("err", err))
		}
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		// Unrecoverable boot/server error — log.Fatalf is kept
		// here per the issue #3 acceptance criteria.
		log.Fatalf("server: %v", err)
	}
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

// Record resolves the category for the scored request and feeds a local
// outcome into the confidence store, then delegates to the inner storage.
// Parse-failure scores (Err set, or Score outside 1..5) are persisted by
// the inner storage but excluded from the confidence aggregate.
func (b *confidenceBridge) Record(s judge.JudgeScore) error {
	b.mu.Lock()
	cat, ok := b.cats[s.RequestID]
	delete(b.cats, s.RequestID)
	b.mu.Unlock()
	if ok && s.Err == nil && s.Score >= 1 {
		b.conf.RecordOutcome(cat, router.RouteLocal, s.Score)
	}
	return b.inner.Record(s)
}

// Close delegates to the inner judge storage. The confidence store's own
// *sql.DB is closed separately in main (it is owned there, not by the
// bridge).
func (b *confidenceBridge) Close() error { return b.inner.Close() }

// buildRecorder constructs the telemetry recorder from config. A disabled
// TelemetryPath returns a Noop so the handler can stay recorder-agnostic.
func buildRecorder(cfg config.Config) telemetry.Recorder {
	if !cfg.TelemetryEnabled() {
		slog.Info("telemetry disabled (NEXUS_TELEMETRY_PATH is empty)")
		return telemetry.Noop{}
	}
	r, err := telemetry.NewJSONLRecorder(cfg.TelemetryPath)
	if err != nil {
		slog.Error("telemetry recorder init failed, falling back to Noop", slog.Any("err", err))
		return telemetry.Noop{}
	}
	slog.Info("telemetry recording", slog.String("path", r.Path()))
	return r
}

// buildMetrics opens the SQLite metrics store when NEXUS_METRICS_DB is
// set. Returns a nil store (and a nil observer) when the operator
// opted out, which lets the handler take the no-metrics fast path.
//
// The observer is a tiny adapter from handlers.MetricsEvent to
// metrics.Request — same pattern as the judge/quality observers.
func buildMetrics(cfg config.Config) (metrics.Store, handlers.MetricsObserver) {
	if !cfg.MetricsEnabled() {
		slog.Info("metrics disabled (NEXUS_METRICS_DB is empty)")
		return nil, nil
	}
	store, err := metrics.Open(cfg.MetricsDBPath)
	if err != nil {
		slog.Error("metrics open failed, metrics disabled", slog.Any("err", err))
		return nil, nil
	}
	if ss, ok := store.(*metrics.SQLiteStore); ok {
		slog.Info("metrics recording", slog.String("path", ss.Path()))
	}
	obs := handlers.MetricsObserverFunc(func(e handlers.MetricsEvent) {
		// The adapter does its own error handling — RecordRequest
		// never blocks the caller, but we still swallow the
		// (currently always-nil) error so the handler stays
		// caller-agnostic.
		_ = store.RecordRequest(metrics.Request{
			Timestamp:         e.Timestamp,
			RequestID:         e.RequestID,
			Route:             e.Route,
			Model:             e.Model,
			InputTokens:       e.InputTokens,
			TOONSavingsTokens: e.TOONSavingsTokens,
			RAGInjected:       e.RAGInjected,
			RAGFilename:       e.RAGFilename,
			EstimatedCostUSD:  e.EstimatedCostUSD,
			OutputTokens:      e.OutputTokens,
			TTFTMs:            e.TTFTMs,
			TotalLatencyMs:    e.TotalLatencyMs,
			TPS:               e.TPS,
			Streaming:         e.Streaming,
			Error:             e.Error,
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
// the operator configured, and whether the local Ollama poller
// considers Ollama healthy (nil hpoller -> true, matches the
// health.Health nil-safe contract).
func healthzHandler(hpoller *health.Health, mgr *probe.Manager, cfg config.Config) http.HandlerFunc {
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
		resp := struct {
			Status         string `json:"status"`
			OllamaHealthy  bool   `json:"ollama_healthy"`
			BudgetTokens   int    `json:"budget_tokens"`
			BudgetSource   string `json:"budget_source"`
			FreeVRAMBytes  int64  `json:"free_vram_bytes,omitempty"`
			ModelContext   int    `json:"model_context,omitempty"`
			StaticFallback int    `json:"static_fallback_tokens"`
		}{
			Status:         "ok",
			OllamaHealthy:  hpoller == nil || hpoller.IsLocalHealthy(),
			BudgetTokens:   displayTokens,
			BudgetSource:   source,
			FreeVRAMBytes:  budget.FreeVRAMBytes,
			ModelContext:   budget.ModelContext,
			StaticFallback: cfg.TokenGuardrail,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}
