// server.go — extracted from main() so the wiring layer (config →
// middleware → handlers → HTTP server) can be integration-tested
// without os.Exit or ListenAndServe (issue #1141).
//
// buildServer returns a fully-configured *http.Server plus a cleanup
// function that releases every resource created during setup. The
// caller (main or a test) is responsible for signal handling and
// calling ListenAndServe (or httptest.NewServer for tests).
package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/anchapin/nexus-proxy/internal/audit"
	"github.com/anchapin/nexus-proxy/internal/auth"
	"github.com/anchapin/nexus-proxy/internal/budget"
	"github.com/anchapin/nexus-proxy/internal/circuit"
	"github.com/anchapin/nexus-proxy/internal/concurrencylimit"
	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/handlers"
	"github.com/anchapin/nexus-proxy/internal/health"
	"github.com/anchapin/nexus-proxy/internal/ioutils"
	"github.com/anchapin/nexus-proxy/internal/judge"
	"github.com/anchapin/nexus-proxy/internal/metrics"
	"github.com/anchapin/nexus-proxy/internal/middleware"
	"github.com/anchapin/nexus-proxy/internal/observability"
	"github.com/anchapin/nexus-proxy/internal/probe"
	"github.com/anchapin/nexus-proxy/internal/providers"
	"github.com/anchapin/nexus-proxy/internal/quality"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/ratelimit"
	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/tracing"
	"github.com/anchapin/nexus-proxy/internal/transport"
	"github.com/anchapin/nexus-proxy/internal/upstream"
)

// serverParts holds the components that the signal handler in main()
// needs to drain on graceful shutdown, and that the SIGHUP handler
// needs to hot-reload. It is returned by buildServer so main() can
// wire the handlers without re-creating the collaborators.
type serverParts struct {
	judgeEval      *judge.Evaluator
	rateLimiter    *ratelimit.Middleware
	authLimiter    *ratelimit.AuthLimiter
	authMiddleware *auth.Middleware // multi-key auth (issue #1154); nil for single-key
	exporterCloser func() error
	ipResolver     *ratelimit.ClientIPResolver
}

// buildServer constructs the fully-wired HTTP server from cfg.
// It returns the server, parts for signal handling, a cleanup function
// (which must be called on shutdown to release resources), and any
// boot error. The cleanup function closes resources in reverse order.
func buildServer(cfg config.Config, startTime time.Time) (*http.Server, *serverParts, func(), error) {
	parts := &serverParts{}

	// Configure the response-body buffer pool retention cap (issue #1177).
	// This is a global setting because sync.Pool is package-level in
	// ioutils. Values <= 0 disable pooling — GetBuffer still allocates
	// but PutBuffer discards instead of returning to the pool.
	ioutils.SetPoolBufferMaxBytes(cfg.PoolBufferMaxBytes)

	// Root context for background goroutines (probe manager, health
	// poller). Cancelled during cleanup so those goroutines exit before
	// their Close() methods are called, avoiding data races under -race.
	bgCtx, bgCancel := context.WithCancel(context.Background())

	// Collect deferred cleanups so we can return them to the caller.
	// Each entry is a cleanup function; they are invoked in reverse
	// order (LIFO) exactly like Go's defer semantics.
	var cleanups []func()
	addCleanup := func(f func()) { cleanups = append(cleanups, f) }
	cleanup := func() {
		// Cancel background goroutines first, then wait briefly for
		// them to observe the cancellation before Close() is called.
		bgCancel()
		time.Sleep(50 * time.Millisecond)
		for i := len(cleanups) - 1; i >= 0; i-- {
			func() {
				defer func() { _ = recover() }()
				cleanups[i]()
			}()
		}
	}

	// OTLP/JSON tracing exporter (issue #787).
	if cfg.TracingEndpoint != "" {
		exporter := tracing.NewExporter(tracing.ExporterConfig{
			Endpoint:  cfg.TracingEndpoint,
			Timeout:   cfg.TracingTimeout,
			QueueSize: cfg.TracingQueueSize,
			BatchSize: cfg.TracingBatchSize,
			Sampler:   tracing.NewProbabilitySampler(cfg.TracingSampleRate),
		})
		tracing.RegisterExporter(exporter)
		parts.exporterCloser = exporter.Close
		slog.Info("tracing exporter wired",
			slog.String("endpoint", cfg.TracingEndpoint),
			slog.Duration("timeout", cfg.TracingTimeout),
			slog.Int("queue_size", cfg.TracingQueueSize),
			slog.Int("batch_size", cfg.TracingBatchSize),
			slog.Float64("sample_rate", cfg.TracingSampleRate),
		)
	}
	addCleanup(func() {
		if parts.exporterCloser != nil {
			_ = parts.exporterCloser()
		}
	})

	httpClient := transport.NewFromEnv()

	emb, err := rag.NewEmbedder(cfg.EmbedderType, cfg.EmbedderBaseURL, cfg.EmbeddingModel, cfg.FrontierKey, httpClient,
		rag.BreakerConfig{Threshold: cfg.RAGCircuitBreakerThreshold, Cooldown: cfg.RAGCircuitBreakerCooldown})
	if err != nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("rag embedder: %w", err)
	}
	slog.Info("rag embedder configured",
		slog.String("type", string(cfg.EmbedderType)),
		slog.String("model", cfg.EmbeddingModel),
	)
	bootCtx, cancel := context.WithTimeout(context.Background(), bootRAGTimeout)
	addCleanup(cancel)

	var ragEmbedder rag.Embedder = emb
	if cfg.RAGEmbedCacheSize > 0 && cfg.RAGEmbedCacheTTL > 0 {
		ragEmbedder = rag.NewEmbedCache(emb, cfg.RAGEmbedCacheSize, cfg.RAGEmbedCacheTTL, cfg.RAGEmbedCacheWaitTimeout)
		slog.Info("rag embedding cache enabled",
			slog.Int("max_entries", cfg.RAGEmbedCacheSize),
			slog.Duration("ttl", cfg.RAGEmbedCacheTTL),
			slog.Duration("wait_timeout", cfg.RAGEmbedCacheWaitTimeout),
		)
	}

	store, persistentStore, ragWatcher, ragEmbed := buildRAGStore(cfg, ragEmbedder, bootCtx)

	slm := router.NewSLMClient(cfg.OllamaURL, cfg.RouterModel, cfg.SLMTimeout, httpClient)
	slm.ConfidenceFloor = cfg.RoutingConfidenceFloor
	slm.ConfidenceCeiling = cfg.RoutingConfidenceCeiling

	var hpoller *health.Health
	if cfg.HealthPollInterval > 0 {
		hpoller = health.New(
			cfg.OllamaURL,
			cfg.LocalModel,
			cfg.HealthPollInterval,
			cfg.HealthBreakerThreshold,
			cfg.HealthProbeTimeout,
			httpClient,
		)
		go hpoller.Run(bgCtx)
		addCleanup(func() {
			if err := hpoller.Close(); err != nil {
				slog.Warn("health poller close", slog.Any("err", err))
			}
		})
	} else {
		slog.Info("ollama health poller disabled (NEXUS_HEALTH_POLL_INTERVAL=0)")
	}

	probeImpl := probe.NewOllamaProbe(cfg.OllamaURL, httpClient)
	probeImpl.BytesPerToken = cfg.ProbeBytesPerToken
	probeImpl.ThermalThreshold = cfg.ProbeThermalThreshold
	probeImpl.ChatModel = cfg.LocalModel
	if cfg.LocalModel != "" {
		slog.Info("vram probe scoped to chat model",
			slog.String("chat_model", cfg.LocalModel),
		)
	}
	probeMgr := probe.NewManager(probeImpl, cfg.ProbePollInterval, cfg.ProbeTimeout)
	if cfg.ProbeNVIDIAInterval > 0 {
		probeMgr.EnableNVIDIARefresh(cfg.ProbeNVIDIAInterval)
	}
	go probeMgr.Run(bgCtx)
	addCleanup(func() {
		if err := probeMgr.Close(); err != nil {
			slog.Warn("probe manager close", slog.Any("err", err))
		}
	})
	if cfg.ProbePollInterval > 0 {
		slog.Info("vram probe enabled",
			slog.Duration("interval", cfg.ProbePollInterval),
			slog.Duration("timeout", cfg.ProbeTimeout),
		)
	} else {
		slog.Info("vram probe polling disabled (NEXUS_PROBE_INTERVAL=0); boot snapshot only")
	}

	var localLimiter handlers.LocalLimiter
	var localConcLimiter *concurrencylimit.Limiter
	if cfg.LocalMaxConcurrent > 0 {
		localConcLimiter = concurrencylimit.New(
			cfg.LocalMaxConcurrent,
			cfg.LocalVRAMBytesPerSlot,
			func() int64 { return probeMgr.Get().FreeVRAMBytes },
		)
		localLimiter = localConcLimiter
		slog.Info("local-route concurrency limiter enabled",
			slog.Int("ceiling", cfg.LocalMaxConcurrent),
			slog.Int64("bytes_per_slot", cfg.LocalVRAMBytesPerSlot),
		)
	} else {
		slog.Info("local-route concurrency limiter disabled (NEXUS_LOCAL_MAX_CONCURRENT<=0)")
	}

	var localCooldown *circuit.Cooldown
	if cfg.LocalCooldown > 0 {
		localCooldown = circuit.New(cfg.LocalCooldown)
		slog.Info("local-route cooldown enabled",
			slog.Duration("cooldown", cfg.LocalCooldown),
		)
	} else {
		slog.Info("local-route cooldown disabled (NEXUS_LOCAL_COOLDOWN<=0)")
	}

	parts.ipResolver = ratelimit.NewClientIPResolver(cfg.TrustedProxies)

	if cfg.RateLimitEnabled() && !cfg.IsLoopbackBind() && !cfg.TrustedProxiesConfigured() {
		slog.Warn("rate limit enabled on a non-loopback bind with NEXUS_TRUSTED_PROXIES unset: "+
			"all clients behind a NAT/share-IP will share a single bucket. "+
			"Set NEXUS_TRUSTED_PROXIES to the reverse-proxy CIDR, or bind to 127.0.0.1",
			slog.String("addr", cfg.Addr),
			slog.Int("rate_limit_rpm", cfg.RateLimitRPM),
		)
	}

	var rateLimiter *ratelimit.Middleware
	if cfg.RateLimitEnabled() {
		var keyFn func(*http.Request) string
		if cfg.RateLimitByAPIKey {
			keyFn = func(r *http.Request) string {
				ip := parts.ipResolver.Resolve(r)
				return ratelimit.APIKeyAwareKeyFunc(ip, r)
			}
		}
		rateLimiter = ratelimit.NewMiddleware(cfg.RateLimitRPM, cfg.RateLimitBurst, parts.ipResolver, keyFn)
		slog.Info("rate limiter enabled",
			slog.Int("rpm", cfg.RateLimitRPM),
			slog.Int("burst", cfg.RateLimitBurst),
			slog.Bool("trusted_proxies", cfg.TrustedProxiesConfigured()),
			slog.Bool("by_api_key", cfg.RateLimitByAPIKey),
		)
	} else {
		slog.Info("rate limiter disabled (NEXUS_RATE_LIMIT_RPM<=0)")
	}
	parts.rateLimiter = rateLimiter

	if cfg.ReadTimeout > 0 && cfg.ShutdownTimeout < cfg.ReadTimeout {
		slog.Warn("shutdown drain shorter than read timeout: in-flight uploads may be truncated mid-read",
			slog.Duration("shutdown_timeout", cfg.ShutdownTimeout),
			slog.Duration("read_timeout", cfg.ReadTimeout),
			slog.String("hint", "set NEXUS_SHUTDOWN_TIMEOUT >= NEXUS_SERVER_READ_TIMEOUT"),
		)
	}
	slog.Info("graceful shutdown drain configured",
		slog.Duration("shutdown_timeout", cfg.ShutdownTimeout),
	)

	var (
		judgeEval       *judge.Evaluator
		judgeObs        handlers.JudgeObserver
		confidenceStore *router.SQLiteConfidenceStore
		confidenceObs   router.ConfidenceStore
	)

	var budgetGuard *budget.Guard
	if cfg.BudgetEnabled() {
		budgetGuard = budget.NewGuard(cfg.BudgetDailyLimit)
		if cfg.BudgetAlertEnabled {
			alerter := budget.NewPrometheusAlerter(slog.Default())
			budgetGuard.SetAlerter(alerter)
			slog.Info("budget alerting enabled",
				slog.Float64("limit_usd", cfg.BudgetDailyLimit),
			)
		}
	}

	routeCounters := observability.NewRouteCounters()

	if localCooldown != nil {
		localCooldown.SetFailureObserver(func() {
			routeCounters.IncLocalCooldownTriggers()
		})
	}

	circuitCollector := observability.NewCollector()

	// Frontier provider health poller (issue #1158). Builds a probe
	// target per configured frontier provider and starts a background
	// poller that probes GET <BaseURL>/models. The per-provider circuit
	// state is surfaced in /healthz and /metrics; the router selector
	// consults it to skip providers whose circuit is open.
	var frontierHealthPoller *health.FrontierHealth
	if cfg.FrontierHealthPollInterval > 0 && cfg.FrontierEnabled() {
		var targets []health.FrontierProbeTarget
		for _, fp := range cfg.FrontierProviders() {
			targets = append(targets, health.FrontierProbeTarget{
				Name:    fp.Name,
				BaseURL: fp.URL,
				APIKey:  fp.APIKey,
			})
		}
		if len(targets) > 0 {
			frontierHealthPoller = health.NewFrontierHealth(
				targets,
				cfg.FrontierHealthPollInterval,
				cfg.FrontierHealthBreakerThreshold,
				cfg.FrontierHealthTimeout,
				httpClient,
			)
			frontierHealthPoller.SetProbeCallback(func(provider, result string) {
				circuitCollector.IncFrontierProbe(provider, result)
			})
			frontierHealthPoller.SetTripCallback(func(provider string) {
				circuitCollector.IncFrontierCircuitOpen(provider)
			})
			go frontierHealthPoller.Run(bgCtx)
			addCleanup(func() {
				if err := frontierHealthPoller.Close(); err != nil {
					slog.Warn("frontier health poller close", slog.Any("err", err))
				}
			})
			slog.Info("frontier health poller enabled",
				slog.Int("providers", len(targets)),
				slog.Duration("poll_interval", cfg.FrontierHealthPollInterval),
				slog.Int("breaker_threshold", cfg.FrontierHealthBreakerThreshold),
				slog.Duration("probe_timeout", cfg.FrontierHealthTimeout),
			)
		}
	} else {
		if cfg.FrontierHealthPollInterval <= 0 {
			slog.Info("frontier health poller disabled (NEXUS_FRONTIER_HEALTH_POLL_INTERVAL=0)")
		}
	}

	stageCollector := observability.NewCollector()
	if cfg.JudgeEnabled && cfg.JudgeAPIKey != "" {
		evalCfg := judge.Config{
			URL:                cfg.JudgeURL,
			Model:              cfg.JudgeModel,
			APIKey:             cfg.JudgeAPIKey,
			SampleRate:         cfg.JudgeSampleRate,
			FrontierSampleRate: cfg.JudgeFrontierSampleRate,
			Concurrency:        cfg.JudgeConcurrency,
			QueueDepth:         cfg.JudgeQueueDepth,
			Timeout:            cfg.JudgeTimeout,
			CostPer1K:          cfg.JudgeCostPer1KUSD,
			BudgetGuard:        budgetGuard,
		}
		var storage judge.Storage
		if cfg.JudgeDBEnabled() {
			jstore, err := judge.OpenSQLiteStore(cfg.JudgeDBPath)
			if err != nil {
				slog.Error("judge SQLite store open failed, falling back to in-memory",
					slog.String("path", cfg.JudgeDBPath),
					slog.Any("err", err),
				)
				storage = judge.NewMemoryStorage(0)
			} else {
				storage = jstore
				slog.Info("judge SQLite store opened",
					slog.String("path", jstore.Path()),
				)
			}
		} else {
			storage = judge.NewMemoryStorage(0)
			slog.Info("judge SQLite store disabled (NEXUS_JUDGE_DB is empty); using in-memory store")
		}

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

		judgeEval = judge.NewEvaluator(evalCfg, httpClient, storage)
		// Wire the score callback so RAG-vs-quality correlation metrics
		// are updated on the worker goroutine after each judge attempt
		// (issue #1167). Only valid scores (1..5) feed the correlation
		// counters; parse failures (Score==0 / Err set) are skipped by
		// ObserveJudgeScore.
		judgeEval.SetScoreCallback(func(s judge.JudgeScore) {
			circuitCollector.ObserveJudgeScore(s.RAGInjected, s.Score)
		})
		judgeObs = handlers.JudgeObserverFunc(func(c handlers.LocalCompletion) bool {
			// Issue #1162: use the frontier sample rate for frontier
			// completions so the judge builds a frontier quality
			// baseline. Local/fusion completions use the standard rate.
			if c.Route == string(router.RouteFrontier) {
				if !judgeEval.SampleFrontier() {
					return false
				}
			} else {
				if !judgeEval.Sample() {
					return false
				}
			}
			if bridge != nil {
				bridge.note(c.RequestID, router.Categorize(c.Instruction))
			}
			if !judgeEval.Enqueue(judge.Sample{
				RequestID:     c.RequestID,
				Instruction:   c.Instruction,
				Output:        c.Output,
				LocalModel:    c.LocalModel,
				Route:         c.Route,
				TraceParent:   c.TraceParent,
				TraceState:    c.TraceState,
				RAGInjected:   c.RAGInjected,
				RAGSimilarity: c.RAGSimilarity,
			}) {
				if bridge != nil {
					bridge.forget(c.RequestID)
				}
				routeCounters.ObserveJudgeQueueOverflow()
				slog.Warn("judge queue full, dropped request", slog.String("request_id", c.RequestID))
				return false
			}
			return true
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
	parts.judgeEval = judgeEval
	addCleanup(func() {
		if confidenceStore != nil {
			if err := confidenceStore.Close(); err != nil {
				slog.Warn("routing confidence store close", slog.Any("err", err))
			}
		}
	})

	recorder := buildRecorder(cfg)
	addCleanup(func() {
		if err := recorder.Close(); err != nil {
			slog.Error("telemetry close", slog.Any("err", err))
		}
	})

	if endpoint := os.Getenv("NEXUS_TRACING_ENDPOINT"); endpoint != "" {
		exp := tracing.NewExporter(tracing.ExporterConfig{
			Endpoint:  endpoint,
			Timeout:   cfg.TracingTimeout,
			BatchSize: cfg.TracingBatchSize,
		})
		if exp != nil {
			tracing.RegisterExporter(exp)
			slog.Info("tracing exporter started",
				slog.String("endpoint", endpoint),
				slog.Duration("timeout", cfg.TracingTimeout),
				slog.Int("batch_size", cfg.TracingBatchSize),
			)
		}
	}

	providerRegistry, err := providers.LoadProviderRegistry()
	if err != nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("providers: %w", err)
	}
	if providerRegistry != nil {
		slog.Info("frontier provider registry loaded",
			slog.Int("providers", providerRegistry.Len()),
			slog.String("names", fmt.Sprintf("%v", providerRegistry.ProviderNames())),
		)
	}

	if ragWatcher != nil {
		addCleanup(func() {
			ragWatcher.Stop()
			slog.Info("rag watcher stopped")
		})
	}

	if persistentStore != nil {
		addCleanup(func() {
			if err := persistentStore.Close(); err != nil {
				slog.Warn("rag persistent store close", slog.Any("err", err))
			}
		})
	}

	metricsStore, metricsObs := buildMetrics(cfg)
	// cacheWarmedEntries is set after the arbiter cache is created below;
	// declared here so the gauge provider closure can capture it (issue #1176).
	var cacheWarmedEntries int
	addCleanup(func() {
		if metricsStore != nil {
			if err := metricsStore.Close(); err != nil {
				slog.Error("metrics close", slog.Any("err", err))
			}
		}
	})

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
				RequestID:   e.RequestID,
				Path:        e.Path,
				ToolName:    e.ToolName,
				TraceParent: e.TraceParent,
				TraceState:  e.TraceState,
			}) {
				routeCounters.ObserveQualityQueueOverflow()
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
	addCleanup(func() {
		if verifier != nil {
			if err := verifier.Close(); err != nil {
				slog.Warn("quality verifier close", slog.Any("err", err))
			}
		}
	})

	// Gauge providers
	routeCounters.SetGaugeProviders(
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			var v uint64
			if verifier != nil {
				v = verifier.Dropped()
			}
			return []observability.GaugeSample{{
				Name: "nexus_quality_dropped_total", Value: float64(v),
			}}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			var qd, cc int
			if judgeEval != nil {
				qd = judgeEval.QueueDepth()
				cc = judgeEval.Concurrency()
			}
			return []observability.GaugeSample{
				{Name: "nexus_judge_queue_depth", Value: float64(qd)},
				{Name: "nexus_judge_concurrency", Value: float64(cc)},
			}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			var v uint64
			if judgeEval != nil {
				v = judgeEval.Dropped()
			}
			return []observability.GaugeSample{{
				Name: "nexus_judge_dropped_total", Value: float64(v),
			}}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			var v uint64
			if judgeEval != nil {
				v = judgeEval.FrontierSampled()
			}
			return []observability.GaugeSample{{
				Name: "nexus_judge_frontier_sampled_total", Value: float64(v),
			}}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			var qd, cc int
			if verifier != nil {
				qd = verifier.QueueDepth()
				cc = verifier.Concurrency()
			}
			return []observability.GaugeSample{
				{Name: "nexus_quality_queue_depth", Value: float64(qd)},
				{Name: "nexus_quality_concurrency", Value: float64(cc)},
			}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			var rc int
			if verifier != nil {
				rc = verifier.DroppedRingCapacity()
			}
			return []observability.GaugeSample{
				{Name: "nexus_quality_dropped_ring_capacity", Value: float64(rc)},
			}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			var v uint64
			if ms, ok := metricsStore.(*metrics.SQLiteStore); ok {
				v = ms.Dropped()
			}
			return []observability.GaugeSample{{
				Name: "nexus_metrics_dropped_total", Value: float64(v),
			}}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			ms, ok := metricsStore.(*metrics.SQLiteStore)
			if !ok || cfg.MetricsRetentionDays <= 0 {
				return nil
			}
			return []observability.GaugeSample{
				{Name: "nexus_metrics_prune_last_rows", Value: float64(ms.PruneLastRows())},
				{Name: "nexus_metrics_prune_last_timestamp_seconds", Value: float64(ms.PruneLastTimestamp())},
			}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			var v uint64
			if d, ok := recorder.(interface{ Dropped() uint64 }); ok {
				v = d.Dropped()
			}
			return []observability.GaugeSample{{
				Name: "nexus_telemetry_dropped_total", Value: float64(v),
			}}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			var v uint64
			if r, ok := recorder.(interface{ Rotations() uint64 }); ok {
				v = r.Rotations()
			}
			return []observability.GaugeSample{{
				Name: "nexus_telemetry_rotations_total", Value: float64(v),
			}}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			var v uint64
			if w, ok := recorder.(interface{ WriteErrors() uint64 }); ok {
				v = w.WriteErrors()
			}
			return []observability.GaugeSample{{
				Name: "nexus_telemetry_write_errors_total", Value: float64(v),
			}}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			exp := tracing.GlobalExporter()
			if exp == nil {
				return nil
			}
			return []observability.GaugeSample{{
				Name: "nexus_tracing_dropped_total", Value: float64(exp.Dropped()),
			}}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			exp := tracing.GlobalExporter()
			if exp == nil {
				return nil
			}
			return []observability.GaugeSample{{
				Name: "nexus_tracing_flush_failures_total", Value: float64(exp.FlushFailures()),
			}}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			exp := tracing.GlobalExporter()
			if exp == nil {
				return nil
			}
			return []observability.GaugeSample{{
				Name: "nexus_tracing_queue_depth", Value: float64(exp.QueueDepth()),
			}}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			exp := tracing.GlobalExporter()
			if exp == nil {
				return nil
			}
			return []observability.GaugeSample{{
				Name: "nexus_tracing_batch_size", Value: float64(exp.BatchCap()),
			}}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			if localConcLimiter == nil {
				return nil
			}
			return []observability.GaugeSample{
				{Name: "nexus_local_concurrency_effective_slots", Value: float64(localConcLimiter.Effective())},
				{Name: "nexus_local_concurrency_in_flight", Value: float64(localConcLimiter.InFlight())},
			}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			if localCooldown == nil {
				return nil
			}
			var v float64
			if localCooldown.Active() {
				v = 1
			}
			return []observability.GaugeSample{{
				Name: "nexus_local_cooldown_active", Value: v,
			}}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			if confidenceStore == nil {
				return nil
			}
			return []observability.GaugeSample{{
				Name: "nexus_confidence_store_rows_total", Value: float64(confidenceStore.RowsTotal()),
			}}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			return []observability.GaugeSample{{
				Name: "nexus_build_info",
				Labels: map[string]string{
					"version":    version,
					"commit":     commit,
					"go_version": runtime.Version(),
				},
				Value: 1,
			}}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			if judgeEval == nil {
				return nil
			}
			return []observability.GaugeSample{
				{Name: "nexus_judge_queue_depth", Value: float64(judgeEval.QueueDepth())},
			}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			if hpoller == nil {
				return nil
			}
			var healthy float64
			if hpoller.IsLocalHealthy() {
				healthy = 1
			}
			return []observability.GaugeSample{
				{Name: "nexus_ollama_healthy", Value: healthy},
				{Name: "nexus_ollama_failure_count", Value: float64(hpoller.FailureCount())},
			}
		}),
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			return []observability.GaugeSample{{
				Name: "nexus_cache_warmed_entries", Value: float64(cacheWarmedEntries),
			}}
		}),
	)

	middleware.Init(cfg.MetaPrompt, cfg.TOONNotice, cfg.TOONUnfenced, cfg.PromptInjectionIsolated())
	var mwChain []middleware.Middleware
	if cfg.MiddlewareChain != "" {
		var err error
		mwChain, err = middleware.BuildChain(cfg.MiddlewareChain)
		if err != nil {
			cleanup()
			return nil, nil, nil, fmt.Errorf("middleware chain: %w", err)
		}
		slog.Info("middleware chain configured",
			slog.String("chain", cfg.MiddlewareChain),
			slog.Int("count", len(mwChain)),
		)
	}
	var ctxAwareRAG middleware.ContextMiddleware
	if len(mwChain) > 0 || cfg.MiddlewareChain == "" {
		ctxAwareRAG = middleware.NewRAGMiddleware(store)
	}

	mux := http.NewServeMux()

	routeDecisionObs := handlers.RouteDecisionObserverFunc(func(e handlers.RouteDecisionEvent) {
		routeCounters.Observe(e.Route, e.Source, e.Confidence, e.TaskType, "")
		if e.CacheHit {
			routeCounters.ObserveSLMCacheHit(e.CacheHitKind)
		} else {
			routeCounters.ObserveSLMCacheMiss()
		}
		if e.Source == "dsl" {
			routeCounters.ObserveDSLHit(e.Reason)
		}
		if e.DSLMiss {
			routeCounters.ObserveDSLMiss()
		}
		if e.Source == string(router.SourceBudgetDownTier) {
			routeCounters.IncBudgetDowntier()
		}
	})
	rejectionObs := handlers.RejectionObserverFunc(func(e handlers.RejectionEvent) {
		routeCounters.ObserveRejection(e.Reason)
	})
	fusionOutcomeObs := handlers.FusionOutcomeObserverFunc(func(e handlers.FusionOutcomeEvent) {
		routeCounters.ObserveFusionOutcome(e.SkipReason)
	})
	cascadeFallbackObs := handlers.CascadeFallbackObserverFunc(func(e handlers.CascadeFallbackEvent) {
		routeCounters.ObserveCascadeFallback(e.Reason)
	})
	var arbiterCacheObserver func(bool)
	if cfg.ArbiterCacheTTL > 0 {
		arbiterCacheObserver = func(cacheHit bool) {
			routeCounters.ObserveArbiterCacheHit(cacheHit)
		}
	}
	panelPanicObs := func() {
		routeCounters.ObservePanelPanic()
	}
	injectionHitObs := func(mode string) {
		routeCounters.ObservePromptInjectionHit(mode)
	}
	upstream.ConfigureMaxResponseBytes(int64(cfg.EffectiveMaxResponseBytes()))
	if cfg.EffectiveMaxResponseBytes() < config.DefaultMaxResponseBytes {
		slog.Warn("upstream response body cap is below the 64 MiB default",
			slog.Int("max_response_bytes", cfg.EffectiveMaxResponseBytes()),
			slog.String("hint", "operator is tightening the cap; confirm this is intentional"),
		)
	}
	var arbiterCache *upstream.ArbiterCache
	if cfg.ArbiterCacheTTL > 0 {
		arbiterCache = upstream.NewArbiterCache(cfg.ArbiterCacheTTL, cfg.ArbiterCacheMaxEntries)
		slog.Info("fusion arbiter cache enabled",
			slog.Duration("ttl", cfg.ArbiterCacheTTL),
			slog.Int("max_entries", cfg.ArbiterCacheMaxEntries),
		)
		arbiterCache.SetEvictionObserver(func(reason string) {
			routeCounters.ObserveArbiterCacheEviction(reason)
		})
		// Boot-time pre-warming from historical SQLite metrics (issue #1176).
		// Only fires when explicitly opted in and the metrics store is a
		// SQLiteStore with recent arbiter synthesis data.
		if cfg.CacheWarmOnBoot && cfg.CacheWarmLimit > 0 {
			cacheWarmedEntries = warmArbiterCache(arbiterCache, metricsStore, cfg)
		}
	}

	// Coalesce (issue #1155): deduplicate identical concurrent
	// non-streaming cascade requests via singleflight.
	var coalescer *upstream.Coalescer
	if cfg.CoalesceEnabled {
		coalescer = upstream.NewCoalescer(cfg.CoalesceTTL, cfg.CoalesceMaxEntries)
		slog.Info("request coalescing enabled",
			slog.Duration("ttl", cfg.CoalesceTTL),
			slog.Int("max_entries", cfg.CoalesceMaxEntries),
		)
	}

	mux.Handle("/metrics", routeCounters.Handler())
	slog.Info("metrics endpoint serves prometheus text format",
		slog.String("path", "/metrics"),
	)

	mux.Handle("/metrics/stages", stageCollector.Handler())
	slog.Info("stage latency histograms exposed",
		slog.String("path", "/metrics/stages"),
	)

	ragObserver := handlers.RAGObserverFunc(func(e handlers.RAGEvent) {
		if e.Hit {
			routeCounters.ObserveRAGHit()
		} else {
			routeCounters.ObserveRAGMiss(e.MissReason)
		}
		if e.IndexPath != "" {
			outcome := "miss"
			if e.Hit {
				outcome = "hit"
			}
			circuitCollector.ObserveRAGSimilarity(e.IndexPath, outcome, e.Score, e.EffectiveThreshold)
		}
	})

	ragCacheObserver := func(hit bool) {
		if hit {
			routeCounters.ObserveRAGCacheHit()
		} else {
			routeCounters.ObserveRAGCacheMiss()
		}
	}

	var slmCache *router.SLMCache
	if cfg.SLMCacheEnabled() {
		if cfg.SLMCacheSemanticThreshold > 0 {
			slmCache = router.NewSLMCacheWithEmbedder(cfg.SLMCacheTTL, cfg.SLMCacheMaxEntries, ragEmbedder, cfg.SLMCacheSemanticThreshold)
			slog.Info("slm decision cache enabled (with semantic deduplication)",
				slog.Duration("ttl", cfg.SLMCacheTTL),
				slog.Int("max_entries", cfg.SLMCacheMaxEntries),
				slog.Float64("semantic_threshold", cfg.SLMCacheSemanticThreshold),
			)
		} else {
			slmCache = router.NewSLMCache(cfg.SLMCacheTTL, cfg.SLMCacheMaxEntries)
			slog.Info("slm decision cache enabled",
				slog.Duration("ttl", cfg.SLMCacheTTL),
				slog.Int("max_entries", cfg.SLMCacheMaxEntries),
			)
		}
		slmCache.SetEvictionObserver(func(reason string) {
			routeCounters.ObserveSLMCacheEviction(reason)
		})
		slmCache.SetEmbedErrorObserver(func() {
			routeCounters.ObserveSLMCacheEmbedError()
		})
		slmCache.SetMaxStale(cfg.SLMCacheMaxStale)
		slmCache.SetStaleCleanupThreshold(cfg.SLMCacheStaleCleanupThreshold)
		slmCache.SetMaxScanEntries(cfg.SLMCacheSemanticScanLimit)
	} else {
		slog.Info("slm decision cache disabled (NEXUS_SLMCACHE_TTL<=0)")
	}

	routeCounters.SetGaugeProviders(
		observability.GaugeProviderFunc(func() []observability.GaugeSample {
			if slmCache == nil {
				return nil
			}
			return []observability.GaugeSample{
				{Name: "nexus_slm_cache_entries", Value: float64(slmCache.Len())},
				{Name: "nexus_slm_cache_max_entries", Value: float64(slmCache.MaxEntries())},
				{Name: "nexus_slm_cache_stale_entries", Value: float64(slmCache.Stale())},
			}
		}),
	)

	// Enable exemplars on both collectors based on config (issue #1171).
	circuitCollector.SetExemplarsEnabled(cfg.MetricsExemplars)
	stageCollector.SetExemplarsEnabled(cfg.MetricsExemplars)

	routeCounters.SetCollector(circuitCollector)

	circuitBreakerObs := circuitBreakerAdapter{
		recordFailure:      circuitCollector.RecordCircuitFailure,
		recordRecovery:     circuitCollector.RecordCircuitRecovery,
		incEmbedderFailure: circuitCollector.IncEmbedderFailure,
		incRAGCircuitTrip:  circuitCollector.IncRAGCircuitTrip,
		incRAGCircuitRecov: circuitCollector.IncRAGCircuitRecover,
	}

	if ragEmbed != nil {
		kind := string(cfg.EmbedderType)
		if kind == "" {
			kind = "ollama"
		}
		ragEmbed.SetTripCallback(kind, circuitBreakerObs.IncRAGCircuitTrip)
	}

	// Tamper-evident audit log (issue #1153). Constructed only when
	// enabled + a path is set; otherwise auditObs stays nil and the hot
	// path is byte-for-byte identical to pre-issue-#1153 behaviour.
	var auditObs handlers.AuditObserver
	if cfg.AuditEnabled && cfg.AuditPath != "" {
		aud, err := audit.Open(cfg.AuditPath, audit.ParseSyncMode(cfg.AuditSync))
		if err != nil {
			cleanup()
			return nil, nil, nil, fmt.Errorf("audit: %w", err)
		}
		addCleanup(func() { _ = aud.Close() })
		auditObs = handlers.AuditObserverFunc(func(e handlers.AuditEvent) {
			_ = aud.Record(audit.AuditEntry{
				Timestamp: e.Timestamp,
				RequestID: e.RequestID,
				ClientIP:  e.ClientIP,
				KeyHash:   e.KeyHash,
				Route:     e.Route,
				Model:     e.Model,
				Outcome:   e.Outcome,
			})
		})
	} else {
		slog.Info("audit log disabled (NEXUS_AUDIT_ENABLED=false or NEXUS_AUDIT_PATH empty)")
	}

	deps := handlers.Deps{
		Config:                  cfg,
		Client:                  httpClient,
		RAG:                     store,
		FusionEmbedder:          ragEmbed,
		SLM:                     slm,
		MiddlewareChain:         mwChain,
		ContextAwareRAG:         ctxAwareRAG,
		Confidence:              confidenceObs,
		ConfidenceErrorHook:     func(category string, err error) { circuitCollector.IncConfidenceError() },
		SLMCache:                slmCache,
		JudgeObserver:           judgeObs,
		QualityObserver:         qualityO,
		MetricsObserver:         metricsObs,
		Recorder:                recorder,
		Health:                  hpoller,
		BudgetObserver:          budgetObserver(probeMgr),
		LocalLimiter:            localLimiter,
		LocalCooldown:           localCooldown,
		RouteDecisionObserver:   routeDecisionObs,
		RejectionObserver:       rejectionObs,
		FusionOutcomeObserver:   fusionOutcomeObs,
		RAGObserver:             ragObserver,
		RAGCacheObserver:        ragCacheObserver,
		CascadeFallbackObserver: cascadeFallbackObs,
		ArbiterCacheObserver:    arbiterCacheObserver,
		PanelPanicObserver:      panelPanicObs,
		InjectionHitObserver:    injectionHitObs,
		CircuitBreakerObserver:  circuitBreakerObs,
		RedactionObserver: func(profile string, substitutions int64) {
			routeCounters.ObserveRedaction(profile, substitutions)
		},
		AuditObserver: auditObs,
		ArbiterCache:  arbiterCache,
		Coalescer:     coalescer,
		Providers:     providerRegistry,
		PipelineStageObserver: handlers.PipelineStageObserverFunc(
			func(e handlers.PipelineStageEvent) {
				stageCollector.ObservePipelineStage(observability.PipelineStageEvent{
					RAGRetrievalMs:      e.RAGRetrievalMs,
					PromptEngineeringMs: e.PromptEngineeringMs,
					TOONCompressionMs:   e.TOONCompressionMs,
					SLMRoutingMs:        e.SLMRoutingMs,
					UpstreamFirstByteMs: e.UpstreamFirstByteMs,
					SLMConfidence:       e.SLMConfidence,
					SLMTaskType:         e.SLMTaskType,
					TraceID:             e.TraceID,
					SpanID:              e.SpanID,
				})
			},
		),
		LatencyObserver: handlers.LatencyObserverFunc(
			func(e handlers.LatencyEvent) {
				circuitCollector.Submit(observability.ObservabilityEvent{
					Route:          e.Route,
					TotalLatencyMs: int64(e.LatencySeconds * 1000),
					TTFTMs:         int64(e.TTFTSeconds * 1000),
					TraceID:        e.TraceID,
					SpanID:         e.SpanID,
				})
				circuitCollector.ObserveLatency(e.Route, int64(e.LatencySeconds*1000))
			},
		),
		LocalPatternsRegex: cfg.DSLLocalPatterns,
	}
	// Only wire SpendGuard when the budget subsystem is actually enabled.
	// Assigning a nil *budget.Guard to the interface field would make the
	// interface non-nil (typed-nil) and cause a nil-pointer panic when the
	// handler calls Check/Record.
	if budgetGuard != nil {
		deps.SpendGuard = budgetGuard
		deps.BudgetChecker = budgetGuard
	}
	chatHandler := handlers.Chat(deps)
	if rateLimiter != nil {
		rateLimiter.SetRejectionHook(func() {
			routeCounters.ObserveRejection(handlers.RejectionRateLimit)
		})
		rateLimiter.SetAllowHook(func(bucketID string, utilizationPct float64) {
			circuitCollector.ObserveRateLimitUtilization(bucketID, utilizationPct)
		})
		chatHandler = rateLimiter.Wrap(chatHandler)
	}
	mux.Handle("/v1/chat/completions", chatHandler)

	mux.HandleFunc("/healthz", healthzHandler(hpoller, frontierHealthPoller, probeMgr, cfg))
	slog.Info("healthz endpoint serves dynamic budget JSON",
		slog.String("ollama_url", cfg.OllamaURL),
	)

	mux.HandleFunc("/readyz", handlers.ReadyzHandler(handlers.ReadyzDeps{
		Health:             hpoller,
		FrontierConfigured: cfg.FrontierEnabled(),
		Mode:               cfg.ReadinessMode,
	}))
	slog.Info("readyz endpoint registered",
		slog.String("mode", cfg.ReadinessMode),
	)

	mux.Handle("/status", handlers.Status(handlers.StatusDeps{
		JudgeEnabled: func() bool { return judgeEval != nil && judgeEval.Enabled() },
		JudgeDepth: func() int {
			if judgeEval == nil {
				return 0
			}
			return judgeEval.QueueDepth()
		},
		JudgeCapacity: func() int { return cfg.JudgeQueueDepth },
		JudgeWorkers: func() int {
			if judgeEval == nil {
				return 0
			}
			return judgeEval.Concurrency()
		},
		QualityEnabled: func() bool { return verifier != nil && verifier.Enabled() },
		QualityDepth: func() int {
			if verifier == nil {
				return 0
			}
			return verifier.QueueDepth()
		},
		QualityCapacity: func() int { return cfg.QualityQueueDepth },
		QualityWorkers: func() int {
			if verifier == nil {
				return 0
			}
			return verifier.Concurrency()
		},
		RAGHealthy: func(ctx context.Context) bool {
			return emb.IsHealthy(ctx)
		},
		RAGIndexedExamples: func() int { return store.Size() },
		RAGDiagnostics: func(ctx context.Context) handlers.RAGStatus {
			documentCount := store.Size()
			stats := rag.StoreStats{}
			if provider, ok := store.(interface{ Stats() rag.StoreStats }); ok {
				stats = provider.Stats()
			}
			healthy := ragEmbedder.IsHealthy(ctx)
			storeType := "memory"
			storePath := ""
			if persistentStore != nil {
				storeType = "sqlite"
				storePath = persistentStore.Path()
			}
			hitRate := 0.0
			if stats.RetrievalAttempts > 0 {
				hitRate = float64(stats.RetrievalHits) / float64(stats.RetrievalAttempts)
			}
			status := handlers.RAGStatus{
				Healthy:         healthy,
				IndexedExamples: documentCount,
				StoreType:       storeType,
				StorePath:       storePath,
				DocumentCount:   documentCount,
				Threshold:       store.Threshold(),
				IndexMode:       store.IndexMode(),
				IndexGeneration: stats.IndexGeneration,
				LastIndexAt:     stats.LastIndexAt,
				DedupSkipped:    stats.DedupSkipped,
			}
			status.Embedder.Type = string(cfg.EmbedderType)
			status.Embedder.Model = cfg.EmbeddingModel
			status.Embedder.Healthy = healthy
			status.Embedder.CircuitOpen = store.IsBreakerOpen()
			status.Retrieval.Attempts = stats.RetrievalAttempts
			status.Retrieval.Hits = stats.RetrievalHits
			status.Retrieval.Misses = stats.RetrievalMisses
			status.Retrieval.HitRate = hitRate
			status.Retrieval.EmptyStoreMisses = stats.EmptyStoreMisses
			status.Retrieval.ThresholdMisses = stats.ThresholdMisses
			status.Retrieval.EmbedErrors = stats.EmbedErrors
			status.Retrieval.InjectionSkippedSizeLimit = stats.InjectionSkippedSizeLimit
			status.Retrieval.MissesByReason = map[string]uint64{
				"empty_store": stats.EmptyStoreMisses,
				"threshold":   stats.ThresholdMisses,
				"embed_error": stats.EmbedErrors,
			}
			status.Cache.Enabled = cfg.RAGEmbedCacheSize > 0 && cfg.RAGEmbedCacheTTL > 0
			status.Cache.Hits = stats.CacheHits
			status.Cache.Misses = stats.CacheMisses
			total := stats.CacheHits + stats.CacheMisses
			if total > 0 {
				status.Cache.HitRate = float64(stats.CacheHits) / float64(total)
			}
			return status
		},
		RoutingSnapshot: func() handlers.RoutingSnapshot {
			snap := routeCounters.Snapshot()
			return handlers.RoutingSnapshot{Decisions: snap}
		},
		Uptime:             func() time.Duration { return time.Since(startTime) },
		RateLimiterEnabled: func() bool { return rateLimiter != nil && rateLimiter.Enabled() },
		RateLimiterRPM:     func() int { return rateLimiter.RPM() },
		RateLimiterBurst:   func() int { return rateLimiter.Burst() },
		BudgetEnabled: func() bool {
			if budgetGuard == nil {
				return false
			}
			return budgetGuard.Limit() > 0
		},
		BudgetDailyLimitUSD: func() float64 {
			if budgetGuard == nil {
				return 0
			}
			return budgetGuard.Limit()
		},
		BudgetCurrentSpendUSD: func() float64 {
			if budgetGuard == nil {
				return 0
			}
			return budgetGuard.State().Spent
		},
		BudgetResetAt: func() time.Time {
			if budgetGuard == nil {
				return time.Time{}
			}
			return budgetGuard.State().NextReset
		},
		MetricsDBWritable: func() bool {
			if metricsStore == nil {
				return false
			}
			if ss, ok := metricsStore.(*metrics.SQLiteStore); ok {
				return ss.Writable()
			}
			return false
		},
		MetricsDBPath: func() string {
			if metricsStore == nil {
				return ""
			}
			if ss, ok := metricsStore.(*metrics.SQLiteStore); ok {
				return ss.Path()
			}
			return ""
		},
		SLMCacheEnabled: func() bool { return slmCache != nil && slmCache.Enabled() },
		SLMCacheTTLSeconds: func() int {
			if slmCache == nil {
				return 0
			}
			return slmCache.TTLSeconds()
		},
		ArbiterCacheEnabled: func() bool { return arbiterCache != nil && arbiterCache.Enabled() },
		ArbiterCacheTTLSeconds: func() int {
			if arbiterCache == nil {
				return 0
			}
			return arbiterCache.TTLSeconds()
		},
		Version: func() string { return version },
		FrontierHealth: func() []handlers.FrontierProviderHealth {
			if frontierHealthPoller == nil {
				return nil
			}
			var out []handlers.FrontierProviderHealth
			for _, st := range frontierHealthPoller.States() {
				out = append(out, handlers.FrontierProviderHealth{
					Name:         st.Name,
					Healthy:      st.Healthy,
					FailureCount: st.FailureCount,
				})
			}
			return out
		},
	}))
	slog.Info("status endpoint serves async subsystem diagnostics")

	if cfg.ModelsEndpointEnabled {
		mh := handlers.Models(handlers.ModelsDeps{
			Config: cfg,
			Client: httpClient,
		})
		mux.Handle("/v1/models", mh)
		mux.Handle("/v1/models/", mh)
		slog.Info("models endpoint enabled",
			slog.Duration("cache_ttl", cfg.ModelsCacheTTL),
		)
	} else {
		slog.Info("models endpoint disabled (NEXUS_MODELS_ENDPOINT=false)")
	}

	// Built-in web dashboard (issue #1182). Opt-in via
	// NEXUS_DASHBOARD_ENDPOINT; serves a self-contained HTML page from
	// the SQLite metrics store. The route lives on the same mux that
	// SecurityHeaders wraps, so it inherits response hardening. It is
	// rate-limited (when a limiter is configured) exactly like the
	// chat path, and auth-gated like /status (NEXUS_DASHBOARD_PUBLIC).
	if cfg.DashboardEndpointEnabled {
		endpoint := cfg.DashboardEndpoint
		if endpoint == "" {
			endpoint = "/dashboard"
		}
		// dashStore stays a nil interface when metrics is disabled;
		// the handler degrades to a static "metrics disabled" page.
		var dashStore handlers.DashboardStore
		if metricsStore != nil {
			dashStore = metricsStore
		}
		dashH := http.Handler(handlers.Dashboard(handlers.DashboardDeps{
			Store:     dashStore,
			CostPer1K: cfg.FrontierCostPer1K,
		}))
		if rateLimiter != nil {
			dashH = rateLimiter.Wrap(dashH)
		}
		mux.Handle(endpoint, dashH)
		slog.Info("dashboard endpoint enabled",
			slog.String("path", endpoint),
			slog.Bool("public", cfg.DashboardPublic),
		)
	} else {
		slog.Info("dashboard endpoint disabled (NEXUS_DASHBOARD_ENDPOINT=false)")
	}

	// Debug pprof + expvar endpoints (issue #1150). Registered on the
	// same mux as /metrics; gated by DebugPprofGate (API key or
	// loopback). Exempt from the main inbound auth gate via
	// publicPathExempt in main.go.
	handlers.RegisterDebugPprof(mux, cfg)

	slog.Info("starting nexus proxy",
		slog.String("addr", cfg.Addr),
		slog.String("local_model", cfg.LocalModel),
		slog.String("frontier_model", cfg.FrontierModel),
	)

	var rootHandler http.Handler = mux
	var authLimiter *ratelimit.AuthLimiter
	if cfg.AuthEnabled() {
		if cfg.AuthRateLimitEnabled() {
			authLimiter = ratelimit.NewAuthLimiter(
				cfg.AuthRateLimitRPM,
				cfg.AuthRateLimitBurst,
				cfg.AuthRateLimitWindow,
				parts.ipResolver,
			)
			authLimiter.SetOnBlock(func(reason string) {
				routeCounters.ObserveRejection(handlers.RejectionAuthRateLimit)
				circuitCollector.IncAuthBlocked(reason)
			})
			authLimiter.SetOnReap(func() {
				routeCounters.IncAuthReaperEvictions()
			})
			slog.Info("auth brute-force protection enabled",
				slog.Int("rpm", cfg.AuthRateLimitRPM),
				slog.Int("burst", cfg.AuthRateLimitBurst),
				slog.Duration("window", cfg.AuthRateLimitWindow),
			)
			routeCounters.SetGaugeProviders(
				observability.GaugeProviderFunc(func() []observability.GaugeSample {
					if authLimiter == nil {
						return nil
					}
					return []observability.GaugeSample{
						{Name: "nexus_auth_limiter_tracked_ips", Value: float64(authLimiter.BucketCount())},
						{Name: "nexus_auth_limiter_blocked_ips", Value: float64(authLimiter.BlockedCount())},
					}
				}),
			)
		}
		// Multi-key inbound auth (issue #1154). When NEXUS_API_KEYS_FILE
		// is set, load the credential set and use the multi-key middleware
		// so per-tenant attribution flows into metrics. The multi-key path
		// takes precedence over the single-key path.
		if cfg.APIKeysFile != "" {
			entries, err := auth.LoadAPIKeysFile(cfg.APIKeysFile)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("config: %w", err)
			}
			creds := auth.NewCredentialSet(entries)
			authMw := auth.NewMultiKeyMiddleware(cfg.ProxyAPIKey, creds, publicPathExempt(cfg), authLimiter, circuitCollector)
			parts.authMiddleware = authMw
			rootHandler = authMw.Wrap(mux)
			slog.Info("multi-key inbound auth enabled",
				slog.String("keys_file", cfg.APIKeysFile),
				slog.Int("key_count", creds.Len()),
				slog.Bool("status_public", cfg.StatusPublic),
			)
		} else {
			authenticator := buildAuthenticator(cfg, httpClient, addCleanup)
			// The "gate key" keeps Enabled() true for JWT/both modes even
			// when ProxyAPIKey is empty — the authenticator does the actual
			// validation.
			gateKey := cfg.ProxyAPIKey
			if gateKey == "" && authenticator != nil {
				gateKey = "jwt-gate"
			}
			authMw := auth.NewMiddlewareWithAuthenticator(gateKey, authenticator, publicPathExempt(cfg), authLimiter, circuitCollector)
			rootHandler = authMw.Wrap(mux)
			slog.Info("inbound auth enabled",
				slog.String("mode", cfg.AuthMode),
				slog.Bool("status_public", cfg.StatusPublic),
			)
		}
	} else {
		slog.Info("inbound auth disabled (NEXUS_PROXY_API_KEY unset)")
	}
	parts.authLimiter = authLimiter

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           buildHandler(rootHandler, cfg.TLSEnabled, routeCounters.ObserveHandlerPanic),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}

	return srv, parts, cleanup, nil
}

// buildHandler applies the outermost middleware chain (SecurityHeaders → Recover)
// to the supplied inner handler. This is the single source of truth for the
// outer middleware ordering so that main.go (via buildServer) and e2e integration
// tests (integration_test.go) construct identical wiring (issue #1160).
//
// The panicObs callback is invoked when Recover catches a panic; pass nil for
// a no-op. tlsEnabled controls HSTS emission (issue #444).
func buildHandler(inner http.Handler, tlsEnabled bool, panicObs func(string)) http.Handler {
	return handlers.SecurityHeaders(tlsEnabled)(handlers.Recover(panicObs)(inner))
}

// drainComponents is called from the signal handler to stop async
// workers before srv.Shutdown(). It closes the judge evaluator,
// rate limiter, auth limiter, and tracing exporter.
func (p *serverParts) drainComponents() {
	if p.judgeEval != nil {
		if err := p.judgeEval.Close(); err != nil {
			slog.Warn("judge close", slog.Any("err", err))
		}
	}
	if p.rateLimiter != nil {
		p.rateLimiter.Close()
	}
	if p.authLimiter != nil {
		p.authLimiter.Stop()
	}
	if p.exporterCloser != nil {
		if err := p.exporterCloser(); err != nil {
			slog.Warn("tracing exporter close", slog.Any("err", err))
		}
	}
}

// handleSIGHUP processes a config reload, updating hot-reloadable
// components in place.
func (p *serverParts) handleSIGHUP(cfg config.Config) config.Config {
	newCfg, result := config.ReloadHotReloadable(cfg)
	if p.rateLimiter != nil {
		p.rateLimiter.SetRPM(newCfg.RateLimitRPM)
		p.rateLimiter.SetBurst(newCfg.RateLimitBurst)
	}
	if p.authLimiter != nil {
		p.authLimiter.SetRPM(newCfg.AuthRateLimitRPM)
		p.authLimiter.SetBurst(newCfg.AuthRateLimitBurst)
		p.authLimiter.SetWindow(newCfg.AuthRateLimitWindow)
	}
	if p.ipResolver != nil {
		p.ipResolver.SetTrustedProxies(newCfg.TrustedProxies)
	}
	// Hot-reload multi-key credentials (issue #1154). Re-read the API
	// keys file so individual keys can be rotated without a restart.
	if p.authMiddleware != nil && newCfg.APIKeysFile != "" {
		if entries, err := auth.LoadAPIKeysFile(newCfg.APIKeysFile); err != nil {
			slog.Error("failed to reload API keys file, keeping previous credentials",
				slog.String("keys_file", newCfg.APIKeysFile),
				slog.Any("err", err),
			)
		} else {
			creds := auth.NewCredentialSet(entries)
			p.authMiddleware.SetCredentials(creds)
			slog.Info("multi-key credentials reloaded via SIGHUP",
				slog.String("keys_file", newCfg.APIKeysFile),
				slog.Int("key_count", creds.Len()),
			)
		}
	}
	newLogger := newCfg.NewLogger()
	slog.SetDefault(newLogger)
	slog.Info("config reloaded via SIGHUP",
		slog.Int("rate_limit_rpm", newCfg.RateLimitRPM),
		slog.Int("rate_limit_burst", newCfg.RateLimitBurst),
		slog.Int("auth_rate_limit_rpm", newCfg.AuthRateLimitRPM),
		slog.Int("auth_rate_limit_burst", newCfg.AuthRateLimitBurst),
		slog.Duration("auth_rate_limit_window", newCfg.AuthRateLimitWindow),
		slog.String("log_level", newCfg.LogLevel.String()),
		slog.String("log_format", newCfg.LogFormat.String()),
		slog.Bool("debug", newCfg.Debug),
	)
	for _, name := range result.NeedsRestart {
		slog.Warn("config change requires restart",
			slog.String("setting", name),
			slog.String("hint", "send SIGTERM/SIGINT to gracefully restart"),
		)
	}
	for _, warn := range result.Warnings {
		slog.Warn("config reload warning", slog.String("warning", warn))
	}
	return newCfg
}

// warmArbiterCache pre-warms the arbiter synthesis cache from historical
// SQLite metrics data (issue #1176). It queries the metrics store for
// the most recent arbiter syntheses within the cache TTL window, decodes
// the hex-encoded cache keys, and calls ArbiterCache.Warm. Returns the
// number of entries loaded. Errors are logged and non-fatal — a failed
// warm does not prevent the proxy from starting.
func warmArbiterCache(cache *upstream.ArbiterCache, store metrics.Store, cfg config.Config) int {
	reader, ok := store.(metrics.ArbiterSynthesisReader)
	if !ok || reader == nil {
		slog.Info("arbiter cache warm skipped (metrics store does not support synthesis queries)",
			slog.String("source", "sqlite"),
		)
		return 0
	}
	since := time.Now().Add(-cfg.ArbiterCacheTTL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := reader.RecentArbiterSyntheses(ctx, cfg.CacheWarmLimit, since)
	if err != nil {
		slog.Warn("arbiter cache warm query failed",
			slog.Any("err", err),
			slog.String("source", "sqlite"),
		)
		return 0
	}
	if len(rows) == 0 {
		slog.Info("arbiter cache warm: no historical syntheses found",
			slog.String("source", "sqlite"),
		)
		return 0
	}
	entries := make([]upstream.ArbiterCacheWarmEntry, 0, len(rows))
	for _, r := range rows {
		keyBytes, err := hex.DecodeString(r.CacheKeyHex)
		if err != nil || len(keyBytes) != 32 {
			slog.Debug("arbiter cache warm: skipping unparseable key",
				slog.String("cache_key_hex", r.CacheKeyHex),
			)
			continue
		}
		var key [32]byte
		copy(key[:], keyBytes)
		entries = append(entries, upstream.ArbiterCacheWarmEntry{
			Key:       key,
			Synthesis: r.Synthesis,
			WrittenAt: r.Timestamp,
		})
	}
	loaded, skippedStale := cache.Warm(entries)
	slog.Info("arbiter cache warmed",
		slog.Int("entries", loaded),
		slog.Int("skipped_stale", skippedStale),
		slog.String("source", "sqlite"),
	)
	return loaded
}

// buildAuthenticator constructs the JWT/OIDC authenticator when JWT or
// "both" mode is configured (issue #1152). Returns nil for static mode
// or when JWKS URL is absent — callers fall through to legacy paths.
func buildAuthenticator(cfg config.Config, httpClient *http.Client, addCleanup func(func())) auth.Authenticator {
	mode := cfg.AuthMode
	if mode != "jwt" && mode != "both" {
		return nil
	}
	if cfg.OIDCJWKSURL == "" {
		slog.Warn("JWT auth mode requested but NEXUS_OIDC_JWKS_URL is empty — JWT validation disabled")
		return nil
	}
	refresh := cfg.OIDCJWKSRefresh
	if refresh <= 0 {
		refresh = 15 * time.Minute
	}
	jwtAuth, err := auth.NewJWTAuthenticator(auth.JWKSConfig{
		JWKSURL:         cfg.OIDCJWKSURL,
		Issuer:          cfg.OIDCIssuer,
		Audience:        cfg.OIDCAudience,
		RefreshInterval: refresh,
		HTTPClient:      httpClient,
	})
	if err != nil {
		slog.Error("failed to create JWT authenticator",
			slog.String("jwks_url", cfg.OIDCJWKSURL),
			slog.Any("error", err),
		)
		return nil
	}
	addCleanup(func() { jwtAuth.Close() })
	slog.Info("JWT/OIDC authenticator initialized",
		slog.String("jwks_url", cfg.OIDCJWKSURL),
		slog.String("mode", mode),
		slog.Duration("refresh_interval", refresh),
	)
	return jwtAuth
}
