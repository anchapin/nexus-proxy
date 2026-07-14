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
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/anchapin/nexus-proxy/internal/auth"
	"github.com/anchapin/nexus-proxy/internal/budget"
	"github.com/anchapin/nexus-proxy/internal/circuit"
	"github.com/anchapin/nexus-proxy/internal/concurrencylimit"
	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/handlers"
	"github.com/anchapin/nexus-proxy/internal/health"
	"github.com/anchapin/nexus-proxy/internal/judge"
	"github.com/anchapin/nexus-proxy/internal/metrics"
	"github.com/anchapin/nexus-proxy/internal/middleware"
	"github.com/anchapin/nexus-proxy/internal/observability"
	"github.com/anchapin/nexus-proxy/internal/probe"
	"github.com/anchapin/nexus-proxy/internal/quality"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/ratelimit"
	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/telemetry"
	"github.com/anchapin/nexus-proxy/internal/transport"
	"github.com/anchapin/nexus-proxy/internal/upstream"
)

const (
	formattingRegexPattern = `(?i)\b(css|format|docstring|lint|typo|boilerplate)\b`
	bootRAGTimeout         = 30 * time.Second
)

// version is the build version. Overridden at compile time via
// -ldflags "-X main.version=v1.2.3" in the Makefile and the release
// workflow. The default "dev" lets `nexus --version` work from a
// local `make build` without any special setup.
var version = "dev"

func main() {
	// Subcommand dispatch (issue #32). The default invocation
	// (no args) starts the proxy; `nexus check` (alias `nexus
	// doctor`) runs the boot-time diagnostic suite and exits.
	// Anything else is reserved for future subcommands; an
	// unknown verb is rejected here so the operator gets a
	// clear error before config.Load runs.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "check", "doctor":
			os.Exit(runCheck(os.Args[2:], os.Stdout, os.Stderr))
		case "-h", "--help", "help":
			fmt.Fprintln(os.Stderr, "Usage: nexus [check|doctor]")
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "Run with no arguments to start the proxy.")
			fmt.Fprintln(os.Stderr, "Run `nexus check` to validate boot-time configuration.")
			fmt.Fprintln(os.Stderr, "Run `nexus --version` to print the build version.")
			os.Exit(0)
		case "-v", "--version", "version":
			printVersion(os.Stdout)
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "nexus: unknown subcommand %q\n\n", os.Args[1])
			fmt.Fprintln(os.Stderr, "Usage: nexus [check|doctor]")
			os.Exit(2)
		}
	}

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

	// Shared pooled HTTP client for all outbound upstream calls (issue #184).
	// Connection pooling reduces TCP handshake overhead across Ollama,
	// frontier API, and arbiter calls. Created once and passed to every
	// collaborator so all traffic shares the same transport.
	httpClient := transport.NewFromEnv()

	emb, err := rag.NewEmbedder(cfg.EmbedderType, cfg.EmbedderBaseURL, cfg.EmbeddingModel, cfg.FrontierKey, httpClient)
	if err != nil {
		log.Fatalf("rag embedder: %v", err)
	}
	slog.Info("rag embedder configured",
		slog.String("type", string(cfg.EmbedderType)),
		slog.String("model", cfg.EmbeddingModel),
	)
	bootCtx, cancel := context.WithTimeout(context.Background(), bootRAGTimeout)
	defer cancel()

	// RAG embedding cache (issue #115). Wrap the Ollama embedder with
	// a bounded LRU so repeat prompts skip the /api/embeddings
	// round-trip. A size of 0 disables the cache (falls back to the
	// raw embedder).
	var ragEmbedder rag.Embedder = emb
	if cfg.RAGEmbedCacheSize > 0 {
		ragEmbedder = rag.NewCachedEmbedder(emb, cfg.RAGEmbedCacheSize)
		slog.Info("rag embedding cache enabled",
			slog.Int("max_entries", cfg.RAGEmbedCacheSize),
		)
	}

	// RAG store (issue #46). When NEXUS_RAG_DB is set, open the
	// SQLite-backed PersistentStore and LoadOrIndex it. The boot
	// path is a one-shot: Load if the DB has rows, otherwise
	// IndexDir (which embeds via Ollama AND persists each result).
	// When NEXUS_RAG_DB is empty we fall back to the legacy
	// in-memory-only Store with the original IndexDir semantics —
	// the proxy is byte-for-byte identical to the pre-issue-46
	// behaviour.
	store, persistentStore, ragWatcher := buildRAGStore(cfg, ragEmbedder, bootCtx)

	slm := router.NewSLMClient(cfg.OllamaURL, cfg.RouterModel, cfg.SLMTimeout, httpClient)
	// Judge-guided adaptive routing (issue #47): the confidence
	// floor/ceiling bound the neutral band the SLM uses when a
	// confidence signal is supplied. Zero values fall back to the
	// router defaults, so this is safe even when the feature is off.
	slm.ConfidenceFloor = cfg.RoutingConfidenceFloor
	slm.ConfidenceCeiling = cfg.RoutingConfidenceCeiling
	// SLM routing decision cache (issue #162). Zero values fall back
	// to the NewSLMClient defaults (5m TTL, 512 max entries).
	slm.CacheTTL = cfg.SLMCacheTTL
	slm.CacheMaxEntries = cfg.SLMCacheMaxEntries
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
			cfg.LocalModel,
			cfg.HealthPollInterval,
			cfg.HealthBreakerThreshold,
			cfg.HealthProbeTimeout,
			httpClient,
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
	probeImpl := probe.NewOllamaProbe(cfg.OllamaURL, httpClient)
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

	// VRAM-aware local-route concurrency ceiling (issue #81). The
	// limiter is dormant unless the operator set
	// NEXUS_LOCAL_MAX_CONCURRENT above zero, so a stock deployment is
	// byte-for-byte identical to the pre-#81 unlimited path. When
	// enabled, the effective slot count is recomputed from the latest
	// probe snapshot on every Acquire: min(ceiling, freeVRAM/
	// bytesPerSlot). When the probe is unavailable (still booting,
	// disabled, or every signal missing) the full ceiling is used so a
	// missing probe never opens the floodgates. The closure reads
	// probeMgr directly so the limiter never imports internal/probe.
	var localLimiter handlers.LocalLimiter
	if cfg.LocalMaxConcurrent > 0 {
		localLimiter = concurrencylimit.New(
			cfg.LocalMaxConcurrent,
			cfg.LocalVRAMBytesPerSlot,
			func() int64 { return probeMgr.Get().FreeVRAMBytes },
		)
		slog.Info("local-route concurrency limiter enabled",
			slog.Int("ceiling", cfg.LocalMaxConcurrent),
			slog.Int64("bytes_per_slot", cfg.LocalVRAMBytesPerSlot),
		)
	} else {
		slog.Info("local-route concurrency limiter disabled (NEXUS_LOCAL_MAX_CONCURRENT<=0)")
	}

	// Local-route cooldown (issue #80). Arms a short cooldown after
	// the cascade detects a local failure so subsequent requests skip
	// local and go directly to the fallback route — closing the gap
	// between the cascade observing failure and the health poller
	// tripping its breaker. Disabled (nil) when NEXUS_LOCAL_COOLDOWN
	// is zero; the hot path is byte-for-byte identical to pre-#80.
	var localCooldown *circuit.Cooldown
	if cfg.LocalCooldown > 0 {
		localCooldown = circuit.New(cfg.LocalCooldown)
		slog.Info("local-route cooldown enabled",
			slog.Duration("cooldown", cfg.LocalCooldown),
		)
	} else {
		slog.Info("local-route cooldown disabled (NEXUS_LOCAL_COOLDOWN<=0)")
	}

	// Trusted-proxy-aware client-IP resolver + per-client rate limiter
	// (issue #75). The resolver honours X-Forwarded-For / X-Real-IP
	// ONLY when the direct TCP peer is in the NEXUS_TRUSTED_PROXIES
	// CIDR allowlist; with an empty list (the default) it trusts
	// nobody and uses the direct peer IP, so an attacker who reaches
	// the proxy directly cannot spoof per-client rate-limit buckets.
	ipResolver := ratelimit.NewClientIPResolver(cfg.TrustedProxies)

	// Boot hardening warning (issue #75). Rate limiting behind a
	// non-loopback bind with no trusted proxies is almost certainly a
	// misconfiguration: either the proxy is directly exposed (so a
	// single NATed IP can exhaust the whole per-client budget by
	// rotating X-Forwarded-For — except the trust-nobody resolver
	// already prevents that), OR the operator meant to put the proxy
	// behind nginx / a cloud LB and forgot to whitelist its CIDR
	// (so legitimate clients all share the proxy's IP bucket). Warn
	// loudly either way; boot never fails on this.
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
		rateLimiter = ratelimit.NewMiddleware(cfg.RateLimitRPM, cfg.RateLimitBurst, ipResolver)
		slog.Info("rate limiter enabled",
			slog.Int("rpm", cfg.RateLimitRPM),
			slog.Int("burst", cfg.RateLimitBurst),
			slog.Bool("trusted_proxies", cfg.TrustedProxiesConfigured()),
		)
	} else {
		slog.Info("rate limiter disabled (NEXUS_RATE_LIMIT_RPM<=0)")
	}

	// Graceful shutdown sanity check (issue #121). The drain window
	// must be at least as long as the inbound read deadline —
	// otherwise a SIGTERM that arrives while a client is still
	// uploading a large request body truncates that request mid-read
	// even though the server nominally had time to finish it. Warn
	// (rather than fail boot) so an operator who deliberately wants a
	// tight drain can keep it; the warning makes the truncation
	// visible in the logs. Skipped when ReadTimeout is 0 (disabled),
	// since there is no inbound deadline to underrun.
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

	// Budget guard for tracking spend (issue #183). Created early so it
	// can be passed to the judge evaluator. When BudgetEnabled is false
	// the guard is a no-op (limit=0).
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
			BudgetGuard: budgetGuard,
		}
		// Issue #198: open the SQLite-backed judge store when
		// NEXUS_JUDGE_DB is set (default path if unset). On error
		// we log and fall back to the in-memory MemoryStorage so
		// the proxy stays alive — judge scores are best-effort
		// telemetry, not a correctness requirement.
		var storage judge.Storage
		if cfg.JudgeDBEnabled() {
			store, err := judge.OpenSQLiteStore(cfg.JudgeDBPath)
			if err != nil {
				slog.Error("judge SQLite store open failed, falling back to in-memory",
					slog.String("path", cfg.JudgeDBPath),
					slog.Any("err", err),
				)
				storage = judge.NewMemoryStorage()
			} else {
				storage = store
				slog.Info("judge SQLite store opened",
					slog.String("path", store.Path()),
				)
			}
		} else {
			storage = judge.NewMemoryStorage()
			slog.Info("judge SQLite store disabled (NEXUS_JUDGE_DB is empty); using in-memory store")
		}

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

		judgeEval = judge.NewEvaluator(evalCfg, httpClient, storage)
		judgeObs = handlers.JudgeObserverFunc(func(c handlers.LocalCompletion) bool {
			if !judgeEval.Sample() {
				return false
			}
			if bridge != nil {
				bridge.note(c.RequestID, router.Categorize(c.Instruction))
			}
			if !judgeEval.Enqueue(judge.Sample{
				RequestID:   c.RequestID,
				Instruction: c.Instruction,
				Output:      c.Output,
				LocalModel:  c.LocalModel,
				TraceParent: c.TraceParent,
				TraceState:  c.TraceState,
			}) {
				if bridge != nil {
					bridge.forget(c.RequestID)
				}
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

	// File watcher (issue #46). Spawned only when persistence is
	// enabled AND the operator set NEXUS_RAG_POLL_INTERVAL > 0.
	// Stop blocks on shutdown so the goroutine has a chance to
	// drain before the DB handle closes below.
	if ragWatcher != nil {
		defer func() {
			ragWatcher.Stop()
			slog.Info("rag watcher stopped")
		}()
	}

	// Persistent store (issue #46). Close flushes WAL frames after
	// the watcher has stopped, so any final Upsert from a pending
	// scan lands on disk.
	if persistentStore != nil {
		defer func() {
			if err := persistentStore.Close(); err != nil {
				slog.Warn("rag persistent store close", slog.Any("err", err))
			}
		}()
	}

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
				RequestID:   e.RequestID,
				Path:        e.Path,
				ToolName:    e.ToolName,
				TraceParent: e.TraceParent,
				TraceState:  e.TraceState,
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

	// Middleware chain (issue #224). Initialize the middleware registry
	// with the config values so closures capture the per-config state.
	// Empty MiddlewareChain uses the built-in default chain.
	middleware.Init(cfg.MetaPrompt, cfg.TOONNotice, cfg.PromptInjectionIsolated())
	var mwChain []middleware.Middleware
	if cfg.MiddlewareChain != "" {
		var err error
		mwChain, err = middleware.BuildChain(cfg.MiddlewareChain)
		if err != nil {
			log.Fatalf("middleware chain: %v", err)
		}
		slog.Info("middleware chain configured",
			slog.String("chain", cfg.MiddlewareChain),
			slog.Int("count", len(mwChain)),
		)
	}
	// ContextAwareRAG is the RAG middleware that needs request context.
	// Nil when the chain doesn't contain "rag" (operator removed it).
	var ctxAwareRAG middleware.ContextMiddleware
	if len(mwChain) > 0 || cfg.MiddlewareChain == "" {
		ctxAwareRAG = middleware.NewRAGMiddleware(store, cfg.RAGThreshold)
	}

	mux := http.NewServeMux()

	// Route-decision counters (issue #74). The in-process counter set
	// records every planner Decision that crosses the chat handler so
	// operators get a Prometheus-text view of routing attribution
	// without depending on the JSONL file or the SQLite store. The
	// handler never imports observability — this closure adapts the
	// neutral RouteDecisionEvent shape to the RouteCounters.Observe
	// signature. /metrics is served by the handler returned by
	// RouteCounters.Handler() so a scrape is always an atomic
	// snapshot.
	routeCounters := observability.NewRouteCounters()
	routeDecisionObs := handlers.RouteDecisionObserverFunc(func(e handlers.RouteDecisionEvent) {
		routeCounters.Observe(e.Route, e.Source, e.Confidence, e.TaskType, "")
		// Issue #206: record SLM cache hit/miss.
		if e.CacheHit {
			routeCounters.ObserveSLMCacheHit()
		} else {
			routeCounters.ObserveSLMCacheMiss()
		}
	})
	// Rejection observer (issue #119). The chat handler dispatches
	// one RejectionEvent per early-return path; the closure forwards
	// the reason to the in-process counter so it surfaces in
	// /metrics as nexus_requests_rejected_total{reason}. The
	// rate-limit middleware's 429 path is wired separately below
	// because it fires before the chat handler.
	rejectionObs := handlers.RejectionObserverFunc(func(e handlers.RejectionEvent) {
		routeCounters.ObserveRejection(e.Reason)
	})
	// Fusion outcome observer (issue #187). Records whether the fusion
	// arbiter was skipped (panel members agreed) or invoked (disagreement).
	// Surfaces as nexus_fusion_arbiter_total{outcome="skipped"|"invoked"}.
	fusionOutcomeObs := handlers.FusionOutcomeObserverFunc(func(e handlers.FusionOutcomeEvent) {
		routeCounters.ObserveFusionOutcome(e.ArbiterSkipped)
	})
	// Cascade fallback observer (issue #205): the chat handler dispatches
	// one CascadeFallbackEvent per request when a retryable step failure
	// caused the cascade to fall back to the next step. The closure
	// forwards the reason to the in-process counter so it surfaces in
	// /metrics as nexus_cascade_fallback_total{reason}.
	cascadeFallbackObs := handlers.CascadeFallbackObserverFunc(func(e handlers.CascadeFallbackEvent) {
		routeCounters.ObserveCascadeFallback(e.Reason)
	})
	// Arbiter cache observer (issue #232). The chat handler reports
	// cache hits and misses for fusion arbiter synthesis via this
	// closure, which forwards to the in-process counter so it
	// surfaces in /metrics as nexus_fusion_arbiter_cache_total{hit}.
	var arbiterCacheObserver func(bool)
	if cfg.ArbiterCacheTTL > 0 {
		arbiterCacheObserver = func(cacheHit bool) {
			routeCounters.ObserveArbiterCacheHit(cacheHit)
		}
	}
	// Arbiter synthesis cache (issue #232). Created when TTL > 0;
	// nil means caching is disabled.
	var arbiterCache *upstream.ArbiterCache
	if cfg.ArbiterCacheTTL > 0 {
		arbiterCache = upstream.NewArbiterCache()
		slog.Info("fusion arbiter cache enabled",
			slog.Duration("ttl", cfg.ArbiterCacheTTL),
		)
	}
	mux.Handle("/metrics", routeCounters.Handler())
	slog.Info("metrics endpoint serves prometheus text format",
		slog.String("path", "/metrics"),
	)

	ragObserver := handlers.RAGObserverFunc(func(e handlers.RAGEvent) {
		if e.Hit {
			routeCounters.ObserveRAGHit(e.Filename)
		} else {
			routeCounters.ObserveRAGMiss(e.MissReason)
		}
	})

	// SLM decision cache (issue #206). When NEXUS_SLM_CACHE_TTL > 0,
	// deduplicate identical prompts within the TTL window so repeated
	// requests don't trigger an SLM call.
	var slmCache *router.SLMCache
	if cfg.SLMCacheEnabled() {
		if cfg.SLMCacheSemanticThreshold > 0 {
			slmCache = router.NewSLMCacheWithEmbedder(cfg.SLMCacheTTL, ragEmbedder, cfg.SLMCacheSemanticThreshold)
			slog.Info("slm decision cache enabled (with semantic deduplication)",
				slog.Duration("ttl", cfg.SLMCacheTTL),
				slog.Float64("semantic_threshold", cfg.SLMCacheSemanticThreshold),
			)
		} else {
			slmCache = router.NewSLMCache(cfg.SLMCacheTTL)
			slog.Info("slm decision cache enabled",
				slog.Duration("ttl", cfg.SLMCacheTTL),
			)
		}
	} else {
		slog.Info("slm decision cache disabled (NEXUS_SLMCACHE_TTL<=0)")
	}

	chatHandler := handlers.Chat(handlers.Deps{
		Config:                  cfg,
		Client:                  httpClient,
		RAG:                     store,
		SLM:                     slm,
		FormattingRegex:         re,
		MiddlewareChain:         mwChain,
		ContextAwareRAG:         ctxAwareRAG,
		Confidence:              confidenceObs,
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
		CascadeFallbackObserver: cascadeFallbackObs,
		ArbiterCacheObserver:    arbiterCacheObserver,
		ArbiterCache:            arbiterCache,
	})
	// Apply the per-client rate limiter (issue #75) as the outermost
	// wrapper so a flood of requests is rejected before any middleware
	// / RAG / routing work runs. A nil/disabled limiter returns the
	// handler unchanged (zero overhead).
	//
	// Issue #119: install the rejection hook so a 429 increments the
	// nexus_requests_rejected_total{reason="rate_limit"} counter,
	// making rate-limit rejections visible in /metrics alongside the
	// handler-level rejections (method, body_too_large, bad_request).
	if rateLimiter != nil {
		rateLimiter.SetRejectionHook(func() {
			routeCounters.ObserveRejection(handlers.RejectionRateLimit)
		})
		chatHandler = rateLimiter.Wrap(chatHandler)
	}
	mux.Handle("/v1/chat/completions", chatHandler)

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

	// /status endpoint (issue #109). Exposes operator-facing
	// diagnostics: frontier configured (boolean only — no URL or
	// model name), judge enabled, VRAM budget, and uptime. Behind
	// auth by default; set NEXUS_STATUS_PUBLIC=true for a public
	// status page (e.g. behind a reverse-proxy ACL).
	mux.HandleFunc("/status", statusHandler(hpoller, probeMgr, cfg, judgeEval != nil, time.Now()))
	if cfg.StatusPublic {
		slog.Info("status endpoint is PUBLIC (NEXUS_STATUS_PUBLIC=true)")
	} else {
		slog.Info("status endpoint is gated behind auth (set NEXUS_STATUS_PUBLIC=true to expose)")
	}

	// OpenAI-compatible model discovery (issue #78). GET /v1/models
	// returns the configured local/router/frontier models (and any
	// Ollama /api/tags entries when NEXUS_MODELS_CACHE_TTL > 0);
	// GET /v1/models/{id} returns a single model or 404. Disabled
	// wholesale when NEXUS_MODELS_ENDPOINT=false. No provider secrets
	// (API keys) are ever exposed — the handler emits only id/object/
	// created/owned_by per the OpenAI Models schema.
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

	slog.Info("starting nexus proxy",
		slog.String("addr", cfg.Addr),
		slog.String("local_model", cfg.LocalModel),
		slog.String("frontier_model", cfg.FrontierModel),
	)

	// Inbound auth (issue #109). When NEXUS_PROXY_API_KEY is set,
	// wrap the mux with a bearer-token gate. /healthz and /metrics
	// are always exempt (probes + scrapers); /status is exempt only
	// when NEXUS_STATUS_PUBLIC=true. When the key is empty the
	// middleware is a pass-through (zero overhead).
	var rootHandler http.Handler = mux
	if cfg.AuthEnabled() {
		authMw := auth.NewMiddleware(cfg.ProxyAPIKey, publicPathExempt(cfg))
		rootHandler = authMw.Wrap(mux)
		slog.Info("inbound auth enabled",
			slog.Bool("status_public", cfg.StatusPublic),
		)
	} else {
		slog.Info("inbound auth disabled (NEXUS_PROXY_API_KEY unset)")
	}

	// Security headers (issue #235) are the OUTERMOST layer so every
	// response — including 401, 429, and 500 error envelopes — carries
	// the hardening headers. Inside that we apply panic recovery so a
	// nil dereference or surprise regex anywhere downstream is turned
	// into a structured slog.Error plus a 500 JSON envelope (or a
	// trailing SSE error frame when the response already started
	// streaming) instead of a TCP reset with no body. Zero overhead
	// on the happy path.
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           middleware.SecurityHeaders()(handlers.Recover()(rootHandler)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}

	// Graceful shutdown: stop accepting new connections, drain
	// in-flight requests, then drain the judge queue so we don't
	// lose pending JudgeScore records. The telemetry recorder is
	// closed via the deferred call above so it always flushes,
	// even on log.Fatalf. The drain window is configurable via
	// NEXUS_SHUTDOWN_TIMEOUT (issue #121) so operators running long
	// frontier SSE streams are not truncated by the prior hardcoded
	// 10s ceiling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("shutting down, draining judge queue and closing server",
			slog.Duration("drain_budget", cfg.ShutdownTimeout),
		)
		if judgeEval != nil {
			if err := judgeEval.Close(); err != nil {
				slog.Warn("judge close", slog.Any("err", err))
			}
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
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
func buildRAGStore(cfg config.Config, emb rag.Embedder, bootCtx context.Context) (rag.RAGStore, *rag.PersistentStore, *rag.Watcher) {
	// emb is already wrapped with CachedEmbedder by the caller (issue #115)
	cachedEmb := emb
	if !cfg.RAGPersistentEnabled() {
		slog.Info("rag persistent store disabled (NEXUS_RAG_DB is empty); using in-memory store")
		store := rag.NewStore(cachedEmb, cfg.RAGThreshold)
		if err := store.IndexDir(bootCtx, cfg.ExamplesDir); err != nil {
			slog.Warn("rag index failed", slog.Any("err", err))
		}
		return store, nil, nil
	}

	ps, err := rag.OpenPersistentStore(cfg.RAGDBPath, cachedEmb, cfg.RAGThreshold)
	if err != nil {
		// Persistence is a best-effort optimisation. Fall back to
		// the in-memory store so the proxy still serves traffic —
		// an operator with a broken cache should not blackhole
		// requests.
		slog.Error("rag persistent store open failed, falling back to in-memory store",
			slog.String("path", cfg.RAGDBPath),
			slog.Any("err", err),
		)
		store := rag.NewStore(cachedEmb, cfg.RAGThreshold)
		if err := store.IndexDir(bootCtx, cfg.ExamplesDir); err != nil {
			slog.Warn("rag index failed", slog.Any("err", err))
		}
		return store, nil, nil
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
		store := rag.NewStore(cachedEmb, cfg.RAGThreshold)
		if err := store.IndexDir(bootCtx, cfg.ExamplesDir); err != nil {
			slog.Warn("rag index failed", slog.Any("err", err))
		}
		return store, nil, nil
	}
	slog.Info("rag persistent store ready",
		slog.String("path", cfg.RAGDBPath),
		slog.Int("examples", n),
	)

	var watcher *rag.Watcher
	if cfg.RAGWatcherEnabled() {
		watcher = rag.NewWatcher(ps, cfg.ExamplesDir, cfg.RAGPollInterval)
		watcher.Start(context.Background())
		slog.Info("rag file watcher enabled",
			slog.String("dir", cfg.ExamplesDir),
			slog.Duration("interval", cfg.RAGPollInterval),
		)
	} else {
		slog.Info("rag file watcher disabled (NEXUS_RAG_POLL_INTERVAL=0); boot-time load only")
	}

	return ps, ps, watcher
}

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
			BaselineCostUSD:   e.BaselineCostUSD,
			SavingsUSD:        e.SavingsUSD,
			OutputTokens:      e.OutputTokens,
			TTFTMs:            e.TTFTMs,
			TotalLatencyMs:    e.TotalLatencyMs,
			TPS:               e.TPS,
			Streaming:         e.Streaming,
			Error:             e.Error,
			RouteSource:       e.RouteSource,
			RouteReason:       e.RouteReason,
			SLMConfidence:     e.SLMConfidence,
			SLMTaskType:       e.SLMTaskType,
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

// publicPathExempt returns true for paths that must bypass the
// inbound auth gate (issue #109). /healthz and /metrics are always
// exempt so K8s probes and Prometheus scrapers work without
// credentials. /status is exempt only when NEXUS_STATUS_PUBLIC=true
// (default false) — the diagnostics surface (frontier configured,
// judge enabled, VRAM state) is reconnaissance-grade and should be
// gated by default.
func publicPathExempt(cfg config.Config) func(*http.Request) bool {
	return func(r *http.Request) bool {
		switch r.URL.Path {
		case "/healthz", "/metrics":
			return true
		case "/status":
			return cfg.StatusPublic
		default:
			return false
		}
	}
}

// statusHandler returns the /status handler (issue #109). Unlike
// /healthz (which is designed for liveness probes), /status exposes
// operator-facing diagnostics: whether the frontier API is
// configured, whether the judge evaluator is enabled, the current
// VRAM budget, and uptime. The frontier field is a boolean (not the
// URL or model name) so the response is safe to expose publicly if
// the operator opts in via NEXUS_STATUS_PUBLIC=true.
func statusHandler(hpoller *health.Health, mgr *probe.Manager, cfg config.Config, judgeEnabled bool, startTime time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		budget := probe.Budget{Source: probe.SourceStatic}
		if mgr != nil {
			budget = mgr.Get()
		}
		displayTokens := budget.Tokens
		if displayTokens <= 0 {
			displayTokens = cfg.TokenGuardrail
		}

		resp := struct {
			Status             string `json:"status"`
			FrontierConfigured bool   `json:"frontier_configured"`
			OllamaHealthy      bool   `json:"ollama_healthy"`
			JudgeEnabled       bool   `json:"judge_enabled"`
			BudgetTokens       int    `json:"budget_tokens"`
			BudgetSource       string `json:"budget_source"`
			FreeVRAMBytes      int64  `json:"free_vram_bytes,omitempty"`
			ModelContext       int    `json:"model_context,omitempty"`
			UptimeSeconds      int64  `json:"uptime_seconds"`
		}{
			Status:             "ok",
			FrontierConfigured: cfg.FrontierKey != "",
			OllamaHealthy:      hpoller == nil || hpoller.IsLocalHealthy(),
			JudgeEnabled:       judgeEnabled,
			BudgetTokens:       displayTokens,
			BudgetSource:       string(budget.Source),
			FreeVRAMBytes:      budget.FreeVRAMBytes,
			ModelContext:       budget.ModelContext,
			UptimeSeconds:      int64(time.Since(startTime).Seconds()),
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}
