// Command nexus is the entry point for the Nexus Proxy. It loads
// configuration from the environment, constructs the chat handler with its
// collaborators (RAG store, SLM client, formatting regex, judge observer,
// telemetry recorder), and serves /v1/chat/completions.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/handlers"
	"github.com/anchapin/nexus-proxy/internal/judge"
	"github.com/anchapin/nexus-proxy/internal/metrics"
	"github.com/anchapin/nexus-proxy/internal/quality"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/telemetry"
)

const (
	formattingRegexPattern = `(?i)\b(css|format|docstring|lint|typo|boilerplate)\b`
	bootRAGTimeout         = 30 * time.Second
	shutdownTimeout        = 10 * time.Second
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	emb := rag.NewOllamaEmbedder(cfg.OllamaURL, cfg.EmbeddingModel, nil)
	store := rag.NewStore(emb, cfg.RAGThreshold)
	bootCtx, cancel := context.WithTimeout(context.Background(), bootRAGTimeout)
	defer cancel()
	if err := store.IndexDir(bootCtx, cfg.ExamplesDir); err != nil {
		log.Printf("[BOOT WARN]: RAG index failed: %v", err)
	}

	slm := router.NewSLMClient(cfg.OllamaURL, cfg.RouterModel, cfg.SLMTimeout, nil)
	re := regexp.MustCompile(formattingRegexPattern)

	// Async LLM-as-a-judge evaluator (issue #15). The handler never
	// imports internal/judge; we plug the observer in here via a
	// closure that adapts LocalCompletion to the evaluator's
	// Sample + Enqueue entry points.
	var (
		judgeEval *judge.Evaluator
		judgeObs  handlers.JudgeObserver
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
		store := judge.NewMemoryStorage()
		judgeEval = judge.NewEvaluator(evalCfg, http.DefaultClient, store)
		judgeObs = handlers.JudgeObserverFunc(func(c handlers.LocalCompletion) {
			if !judgeEval.Sample() {
				return
			}
			if !judgeEval.Enqueue(judge.Sample{
				RequestID:   c.RequestID,
				Instruction: c.Instruction,
				Output:      c.Output,
				LocalModel:  c.LocalModel,
			}) {
				log.Printf("[JUDGE DROP]: queue full, dropped %s", c.RequestID)
			}
		})
		log.Printf("[BOOT]: judge enabled (url=%s model=%s rate=%.2f concurrency=%d)",
			cfg.JudgeURL, cfg.JudgeModel, cfg.JudgeSampleRate, cfg.JudgeConcurrency)
	} else {
		log.Println("[BOOT]: judge disabled (sample rate <= 0 or no API key)")
	}

	recorder := buildRecorder(cfg)
	defer func() {
		if err := recorder.Close(); err != nil {
			log.Printf("[TELEMETRY ERROR]: close: %v", err)
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
				log.Printf("[METRICS ERROR]: close: %v", err)
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
					log.Printf("[QUALITY]: request=%s path=%q root=%q pass=%v exit=%d err=%v",
						v.Event.RequestID, v.Event.Path, v.RepoRoot, v.Pass, v.ExitCode, v.Err)
					return
				}
				log.Printf("[QUALITY]: request=%s path=%q root=%q kind=%s pass=%v exit=%d duration=%dms",
					v.Event.RequestID, v.Event.Path, v.RepoRoot, v.Kind, v.Pass, v.ExitCode, v.DurationMs)
			}),
		})
		qualityO = handlers.QualityObserverFunc(func(e handlers.QualityEvent) {
			if !verifier.Submit(quality.Event{
				RequestID: e.RequestID,
				Path:      e.Path,
				ToolName:  e.ToolName,
			}) {
				log.Printf("[QUALITY DROP]: queue full, dropped %s path=%q", e.RequestID, e.Path)
			}
		})
		log.Printf("[BOOT]: quality verifier enabled (concurrency=%d queue=%d timeout=%s)",
			cfg.QualityConcurrency, cfg.QualityQueueDepth, cfg.QualityTimeout)
	} else {
		log.Println("[BOOT]: quality verifier disabled (concurrency <= 0)")
	}
	defer func() {
		if verifier != nil {
			if err := verifier.Close(); err != nil {
				log.Printf("[SHUTDOWN WARN]: quality verifier: %v", err)
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
		JudgeObserver:   judgeObs,
		QualityObserver: qualityO,
		MetricsObserver: metricsObs,
		Recorder:        recorder,
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("Starting Nexus Proxy on %s (local=%s frontier=%s)", cfg.Addr, cfg.LocalModel, cfg.FrontierModel)
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
		log.Println("[SHUTDOWN]: draining judge queue and closing server...")
		if judgeEval != nil {
			if err := judgeEval.Close(); err != nil {
				log.Printf("[SHUTDOWN WARN]: judge close: %v", err)
			}
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[SHUTDOWN WARN]: server: %v", err)
		}
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
}

// buildRecorder constructs the telemetry recorder from config. A disabled
// TelemetryPath returns a Noop so the handler can stay recorder-agnostic.
func buildRecorder(cfg config.Config) telemetry.Recorder {
	if !cfg.TelemetryEnabled() {
		log.Println("[TELEMETRY]: disabled (NEXUS_TELEMETRY_PATH is empty)")
		return telemetry.Noop{}
	}
	r, err := telemetry.NewJSONLRecorder(cfg.TelemetryPath)
	if err != nil {
		log.Printf("[TELEMETRY ERROR]: recorder init failed (%v); falling back to Noop", err)
		return telemetry.Noop{}
	}
	log.Printf("[TELEMETRY]: recording to %s", r.Path())
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
		log.Println("[METRICS]: disabled (NEXUS_METRICS_DB is empty)")
		return nil, nil
	}
	store, err := metrics.Open(cfg.MetricsDBPath)
	if err != nil {
		log.Printf("[METRICS ERROR]: open failed (%v); metrics disabled", err)
		return nil, nil
	}
	if ss, ok := store.(*metrics.SQLiteStore); ok {
		log.Printf("[METRICS]: recording to %s", ss.Path())
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
