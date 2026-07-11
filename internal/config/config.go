// Package config loads runtime configuration from environment variables.
//
// All values have safe defaults so the binary boots in development with a
// local Ollama instance. Secrets (FRONTIER_API_KEY) must be supplied via env
// in any non-development deployment.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/anchapin/nexus-proxy/internal/middleware"
)

// Config holds all runtime knobs for the proxy. A zero value is invalid;
// always go through Load.
type Config struct {
	// HTTP server
	Addr string // ":8000"

	// Local Ollama
	OllamaURL      string // "http://localhost:11434"
	RouterModel    string // "qwen3-coder:4b"
	LocalModel     string // "qwen3-coder:8b"
	EmbeddingModel string // "nomic-embed-text"

	// Frontier API (OpenAI-compatible)
	FrontierURL   string // "https://api.openai.com/v1/chat/completions"
	FrontierModel string // "gpt-4o"
	FrontierKey   string // required for actual frontier traffic; may be empty in dev

	// Z.ai fallback (optional second frontier endpoint for the local-route cascade)
	ZAIURL   string // "https://api.z.ai/v1/chat/completions"
	ZAIModel string // "glm-4.6"
	ZAIKey   string // empty == skipped from cascade

	// RAG
	ExamplesDir  string  // "./few_shot_examples"
	RAGThreshold float64 // cosine similarity cutoff for retrieval (0.55)

	// RAG persistence (issue #46). When RAGDBPath is set, the few-shot
	// embeddings are cached on disk and reloaded on boot without
	// re-hitting Ollama. RAGPollInterval > 0 enables a background
	// goroutine that detects new / modified / deleted files in
	// ExamplesDir and updates the store incrementally. Set
	// NEXUS_RAG_DB="" to fall back to the legacy in-memory-only path.
	RAGDBPath       string        // on-disk SQLite database for the RAG store
	RAGPollInterval time.Duration // watcher cadence; 0 disables the watcher

	// Routing
	TokenGuardrail int           // estimated tokens above this force frontier (6000)
	SLMTimeout     time.Duration // Qwen3-Coder routing timeout (8s)
	FusionTimeout  time.Duration // per-panel-member fetch timeout (120s)
	CascadeTimeout time.Duration // per-attempt timeout for cascade fallback (30s)
	ArbiterTimeout time.Duration // per-call timeout for the fusion arbiter stream (60s)

	// Frontier provider selector (issue #45). When more than one
	// frontier provider is configured (frontier + z.ai), the chat
	// handler consults a router.ProviderSelector to pick the cheaper
	// / faster endpoint on every route=frontier request. The selector
	// reads per-model p50 latency and average cost from the metrics
	// store over SelectorWindow; SelectorRefreshInterval is the
	// background cache cadence. SelectorMinSamples is the per-provider
	// observation floor below which the selector falls back to the
	// first configured provider (no flapping on cold start).
	//
	// FrontierCostPer1K and ZAICostPer1K override the per-request USD
	// cost the metrics store assigns to each provider (a flat rate
	// per 1k input tokens). They mirror JudgeCostPer1KUSD but apply
	// to the per-provider slice that BuildFrontierProviders emits.
	SelectorWindow          time.Duration // look-back window for provider stats (1h)
	SelectorMinSamples      int           // per-provider observation floor (5)
	SelectorRefreshInterval time.Duration // background cache refresh cadence (60s)
	FrontierCostPer1K       float64       // USD per 1k input tokens for frontier (0.005)
	ZAICostPer1K            float64       // USD per 1k input tokens for z.ai (0.002)

	// Fusion progressive delivery (issue #48). When enabled and the
	// harness requests a streaming response, the chat handler
	// dispatches route=fusion to upstream.PanelStreaming instead of
	// the legacy Panel. PanelStreaming races both panel members,
	// streams the first to complete as a speculative OpenAI-
	// compatible SSE chunk, then either terminates (agreement) or
	// streams the arbiter's synthesis as additional chunks
	// (disagreement). FusionAgreementThreshold is the Jaccard
	// similarity cutoff above which the arbiter is skipped; the
	// value is clamped into [0, 1] by upstream.PanelStreaming so a
	// misconfigured operator can't disable the agreement-skip path
	// entirely (negative) or always skip it (>1).
	FusionProgressiveDelivery bool    // true iff NEXUS_FUSION_PROGRESSIVE is unset or "true" (default true)
	FusionAgreementThreshold  float64 // Jaccard ratio [0,1] above which arbiter is skipped (default 0.85)

	// Judge-guided adaptive routing (issue #47). Historical judge
	// scores are aggregated by task category in a SQLite table and fed
	// back to the SLM router as a confidence signal. All of this is
	// dormant unless the judge is enabled — when JudgeEnabled is false
	// the chat handler never wires a ConfidenceStore, so routing is
	// byte-for-byte identical to the pre-issue-47 behaviour.
	//
	// RoutingConfidenceDB   — on-disk SQLite path; empty disables the store.
	// Floor / Ceiling       — bounds of the neutral band (0.4 / 0.85).
	// MinSamples            — outcomes needed before a category is trusted (5).
	// Window                — sliding window for the aggregate (168h / 7d).
	RoutingConfidenceDB         string
	RoutingConfidenceFloor      float64
	RoutingConfidenceCeiling    float64
	RoutingConfidenceMinSamples int
	RoutingConfidenceWindow     time.Duration

	// Health (issue #8). The chat handler consults
	// internal/health.Health before issuing local-bound requests;
	// when Ollama is unreachable it short-circuits to frontier
	// (route=local) or skips the local panel member (route=fusion)
	// and stamps X-Nexus-Degraded: true on the response. The
	// breaker trips after HealthBreakerThreshold consecutive
	// failed probes and reopens on the first success.
	HealthPollInterval     time.Duration // background poll cadence (30s)
	HealthBreakerThreshold int           // consecutive failures before trip (3)
	HealthProbeTimeout     time.Duration // per-probe HTTP timeout (5s)

	// Hardware-aware VRAM probe (issue #6). The probe replaces the
	// static NEXUS_TOKEN_GUARDRAIL with a live measurement of the
	// loaded model's context_length (Ollama /api/ps) and free VRAM
	// (AMD sysfs). The chat handler uses the most recent budget;
	// when the probe is disabled or returns zero, the handler
	// falls back to the static TokenGuardrail value.
	ProbeEnabled       bool          // true iff ProbePollInterval > 0
	ProbePollInterval  time.Duration // background re-probe cadence (60s); 0 disables polling
	ProbeTimeout       time.Duration // per-probe HTTP timeout (5s)
	ProbeBytesPerToken int           // VRAM->token heuristic (256 KiB per token)

	// Local-route concurrency ceiling (issue #81). The limiter bounds
	// in-flight local-route requests so a small GPU does not OOM under
	// bursty load. NEXUS_LOCAL_MAX_CONCURRENT is the hard ceiling; the
	// live effective count is min(ceiling, freeVRAM/bytesPerSlot) using
	// the latest probe snapshot from internal/probe. Zero or negative
	// disables the limiter entirely (pre-#81 unlimited behaviour), so a
	// stock deployment is byte-for-byte unchanged when the operator
	// leaves the knob unset. LocalVRAMBytesPerSlot is the per-slot VRAM
	// reservation (default 2 GiB); it only affects the dynamic shrink
	// path — when the probe is unavailable the full Ceiling is used.
	LocalMaxConcurrent    int   // hard ceiling on concurrent local slots (0 disables)
	LocalVRAMBytesPerSlot int64 // VRAM bytes reserved per concurrent local slot (2 GiB)

	// Local-route cooldown (issue #80). After the cascade detects a
	// local (Ollama) failure and falls back, the chat handler arms a
	// short cooldown so subsequent requests within the window skip
	// local and go directly to the fallback route — closing the gap
	// between the cascade observing failure and the health poller
	// catching up. Zero disables the circuit (pre-#80 behaviour).
	LocalCooldown time.Duration // default 10s; 0 disables

	// HTTP request body cap (issue #11). The chat handler applies this
	// with http.MaxBytesReader before reading the request body, so an
	// oversized POST cannot exhaust proxy memory before the guardrail
	// runs. Zero or negative falls back to DefaultMaxBodyBytes.
	MaxBodyBytes int

	// Judge (async LLM-as-a-judge evaluator). All zero/empty values
	// disable the judge; the chat handler is unaffected when the
	// evaluator is wired to a no-op observer (see cmd/nexus/main.go).
	JudgeEnabled      bool          // true iff at least one judge parameter is non-zero
	JudgeURL          string        // frontier endpoint for judge calls
	JudgeModel        string        // judge model name (e.g. "gpt-4o")
	JudgeAPIKey       string        // bearer token; may equal FrontierKey
	JudgeSampleRate   float64       // 0..1; <=0 disables sampling
	JudgeConcurrency  int           // max parallel judge calls (default 2)
	JudgeQueueDepth   int           // buffered channel size (default 64)
	JudgeTimeout      time.Duration // per-call judge timeout (default 30s)
	JudgeCostPer1KUSD float64       // rough USD/1k-token rate for cost estimates

	// Quality (async AST/compiler verifier, issue #13). Detected
	// edits enqueue a background `cargo check` / `npx tsc` and the
	// verdict (1 = clean, 0 = fail/timeout) is reported via a
	// callback to cmd/nexus/main.go. QualityEnabled is true iff
	// QualityConcurrency is positive; the chat handler treats a
	// nil observer as "skip me" so the hot path is unaffected when
	// the verifier is dormant.
	QualityEnabled     bool          // true iff QualityConcurrency > 0
	QualityConcurrency int           // max parallel verifier workers (default 2)
	QualityQueueDepth  int           // buffered channel size (default 64)
	QualityTimeout     time.Duration // per-check timeout (default 60s)
	QualityStderrCap   int           // stderr bytes retained per verdict (default 2 KiB)

	// Middleware prompts
	MetaPrompt string // appended to system prompt by prompt_engine
	TOONNotice string // appended when TOON compression is applied

	// Prompt-injection hardening (issue #76). Controls whether the
	// proxy isolates its policy text from user-supplied system content
	// and whether suspicious injection patterns are logged or rejected.
	//   - off (default): legacy append behaviour, fully backward compatible.
	//   - warn: proxy text delimited + suspicious patterns logged.
	//   - strict: proxy text delimited + suspicious patterns rejected (400).
	PromptInjectionMode middleware.InjectionMode

	// Telemetry
	//
	// TelemetryPath is the on-disk JSON-lines log written by the
	// background telemetry goroutine. An empty value disables recording
	// (the handler installs a Noop recorder). Parent directories are
	// created on demand.
	//
	// MetricsDBPath is the on-disk SQLite database written by
	// internal/metrics (issue #4). An empty value disables the
	// metrics store (the handler treats a nil store as "skip me").
	// Parent directories are created on demand. The default lives
	// under the user's XDG-style cache directory so multiple checkouts
	// don't trample each other.
	TelemetryPath string
	MetricsDBPath string

	// Structured logging (issue #3). LogLevel maps NEXUS_LOG_LEVEL
	// ("debug" | "info" | "warn" | "error") to a slog.Level. LogFormat
	// maps NEXUS_LOG_FORMAT ("json" | "text") to a slog.Handler; json
	// is the production default, text is friendlier for local dev.
	LogLevel  slog.Level
	LogFormat LogFormat

	// Debug request/response tracing (issue #33). Debug is the master
	// switch: when false (the default) the chat handler takes the
	// production fast path with zero extra allocations. When true the
	// handler emits a structured trace per request — inbound summary,
	// middleware transforms, routing decision, upstream call, and a
	// truncated response preview. DebugBodyBytes caps the response
	// body preview so a runaway upstream cannot flood the log; zero or
	// negative falls back to DefaultDebugBodyBytes. Distinct from
	// LogLevel=debug: Debug adds payload-level visibility gated
	// independently so operators can turn it on without enabling
	// debug-level chatter from every other package.
	Debug          bool
	DebugBodyBytes int

	// OpenAI-compatible model discovery (issue #78). When enabled the
	// proxy serves GET /v1/models and GET /v1/models/{id} listing the
	// configured local, router, and frontier models, plus any models
	// Ollama reports via /api/tags. The Ollama poll is cached for
	// ModelsCacheTTL; set to zero or negative to serve only the
	// configured models (no HTTP round-trip to Ollama per request).
	ModelsEndpointEnabled bool
	ModelsCacheTTL        time.Duration
}

// DefaultMetricsDBPath returns the canonical metrics DB location:
// $XDG_CACHE_HOME/nexus-proxy/metrics.db (or the OS default for
// os.UserCacheDir when XDG_CACHE_HOME is unset). Tests and operators
// can override with NEXUS_METRICS_DB.
func DefaultMetricsDBPath() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		// Fall back to a dot-directory in $CWD so dev / CI runs
		// still get a writable location.
		base = "./.cache"
	}
	return filepath.Join(base, "nexus-proxy", "metrics.db")
}

// DefaultRoutingConfidenceDBPath returns the canonical location for the
// judge-guided adaptive routing store (issue #47):
// $XDG_CACHE_HOME/nexus-proxy/routing_confidence.db. Operators override
// with NEXUS_ROUTING_CONFIDENCE_DB (empty disables the store).
func DefaultRoutingConfidenceDBPath() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = "./.cache"
	}
	return filepath.Join(base, "nexus-proxy", "routing_confidence.db")
}

// DefaultRAGDBPath returns the canonical location for the persistent
// RAG store (issue #46): $XDG_CACHE_HOME/nexus-proxy/rag.db. Operators
// override with NEXUS_RAG_DB (empty disables the store, falling back
// to the legacy in-memory-only path).
func DefaultRAGDBPath() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = "./.cache"
	}
	return filepath.Join(base, "nexus-proxy", "rag.db")
}

// Load reads configuration from environment variables, applying defaults
// suitable for local development. It returns an error only when a required
// value is malformed; missing optional values fall back to defaults.
func Load() (Config, error) {
	cfg := Config{
		Addr:           getEnv("NEXUS_ADDR", ":8000"),
		OllamaURL:      strings.TrimRight(getEnv("NEXUS_OLLAMA_URL", "http://localhost:11434"), "/"),
		RouterModel:    getEnv("NEXUS_ROUTER_MODEL", "qwen3-coder:4b"),
		LocalModel:     getEnv("NEXUS_LOCAL_MODEL", "qwen3-coder:8b"),
		EmbeddingModel: getEnv("NEXUS_EMBEDDING_MODEL", "nomic-embed-text"),
		FrontierURL:    getEnv("NEXUS_FRONTIER_URL", "https://api.openai.com/v1/chat/completions"),
		FrontierModel:  getEnv("NEXUS_FRONTIER_MODEL", "gpt-4o"),
		FrontierKey:    getEnv("NEXUS_FRONTIER_API_KEY", ""),
		ZAIURL:         getEnv("NEXUS_ZAI_URL", "https://api.z.ai/v1/chat/completions"),
		ZAIModel:       getEnv("NEXUS_ZAI_MODEL", "glm-4.6"),
		ZAIKey:         getEnv("NEXUS_ZAI_API_KEY", ""),
		ExamplesDir:    getEnv("NEXUS_EXAMPLES_DIR", "./few_shot_examples"),
		MetaPrompt:     defaultMetaPrompt,
		TOONNotice:     defaultTOONNotice,
		TelemetryPath:  getEnvAllowEmpty("NEXUS_TELEMETRY_PATH", "./nexus-telemetry.jsonl"),
		MetricsDBPath:  getEnvAllowEmpty("NEXUS_METRICS_DB", DefaultMetricsDBPath()),
	}

	threshold, err := getEnvFloat("NEXUS_RAG_THRESHOLD", 0.55)
	if err != nil {
		return cfg, err
	}
	cfg.RAGThreshold = threshold

	// RAG persistence (issue #46). The DB path defaults to the user
	// cache dir so multiple checkouts don't trample each other.
	// NEXUS_RAG_POLL_INTERVAL=0 disables the file watcher but leaves
	// persistence on (boot still loads from disk).
	cfg.RAGDBPath = getEnvAllowEmpty("NEXUS_RAG_DB", DefaultRAGDBPath())

	pollInterval, err := getEnvDuration("NEXUS_RAG_POLL_INTERVAL", 30*time.Second)
	if err != nil {
		return cfg, err
	}
	if pollInterval < 0 {
		pollInterval = 0
	}
	cfg.RAGPollInterval = pollInterval

	guardrail, err := getEnvInt("NEXUS_TOKEN_GUARDRAIL", 6000)
	if err != nil {
		return cfg, err
	}
	cfg.TokenGuardrail = guardrail

	slmTimeout, err := getEnvDuration("NEXUS_SLM_TIMEOUT", 8*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.SLMTimeout = slmTimeout

	fusionTimeout, err := getEnvDuration("NEXUS_FUSION_TIMEOUT", 120*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.FusionTimeout = fusionTimeout

	cascadeTimeout, err := getEnvDuration("NEXUS_CASCADE_TIMEOUT", 30*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.CascadeTimeout = cascadeTimeout

	// Fusion arbiter synthesis (issue #12). Shorter than FusionTimeout
	// because the arbiter is doing synthesis, not generation — a slow
	// arbiter should not pin the whole request indefinitely.
	arbiterTimeout, err := getEnvDuration("NEXUS_ARBITER_TIMEOUT", 60*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.ArbiterTimeout = arbiterTimeout

	// Frontier provider selector (issue #45). Look-back window,
	// observation floor, and cache cadence. Defaults match the
	// router.DefaultSelector* constants; operators can shorten
	// SelectorWindow during development to see fresh picks.
	selectorWindow, err := getEnvDuration("NEXUS_SELECTOR_WINDOW", time.Hour)
	if err != nil {
		return cfg, err
	}
	if selectorWindow < 0 {
		selectorWindow = 0
	}
	cfg.SelectorWindow = selectorWindow

	selectorMin, err := getEnvInt("NEXUS_SELECTOR_MIN_SAMPLES", 5)
	if err != nil {
		return cfg, err
	}
	if selectorMin < 0 {
		selectorMin = 0
	}
	cfg.SelectorMinSamples = selectorMin

	selectorRefresh, err := getEnvDuration("NEXUS_SELECTOR_REFRESH", 60*time.Second)
	if err != nil {
		return cfg, err
	}
	if selectorRefresh < 0 {
		selectorRefresh = 0
	}
	cfg.SelectorRefreshInterval = selectorRefresh

	// Per-provider cost rates. Defaults approximate OpenAI gpt-4o
	// (~$5/M input tokens) and z.ai glm-4.6 (~$2/M). The chat
	// handler passes these through to BuildFrontierProviders so the
	// selector has a deterministic cost weight even before the
	// metrics store has observed any traffic.
	frontierCost, err := getEnvFloat("NEXUS_FRONTIER_COST_PER_1K", 0.005)
	if err != nil {
		return cfg, err
	}
	if frontierCost < 0 {
		frontierCost = 0
	}
	cfg.FrontierCostPer1K = frontierCost

	zaiCost, err := getEnvFloat("NEXUS_ZAI_COST_PER_1K", 0.002)
	if err != nil {
		return cfg, err
	}
	if zaiCost < 0 {
		zaiCost = 0
	}
	cfg.ZAICostPer1K = zaiCost

	// Fusion progressive delivery (issue #48). Defaults to ON so a
	// stock `.env.example` boots into the new behaviour; operators
	// who want to opt out (e.g. to A/B test against the old
	// blocking Panel) set NEXUS_FUSION_PROGRESSIVE=false. An empty
	// or unparseable value falls back to the default rather than
	// failing boot, since the knob is purely an optimisation.
	cfg.FusionProgressiveDelivery = parseBoolEnv("NEXUS_FUSION_PROGRESSIVE", true)
	agreementThreshold, err := getEnvFloat("NEXUS_FUSION_AGREEMENT_THRESHOLD", 0.85)
	if err != nil {
		return cfg, err
	}
	cfg.FusionAgreementThreshold = agreementThreshold

	// Judge-guided adaptive routing (issue #47). Defaults keep the
	// feature dormant unless the judge is enabled and a DB path is
	// configured. The DB defaults to a sibling of the metrics DB so a
	// stock deployment gets both without extra config.
	cfg.RoutingConfidenceDB = getEnvAllowEmpty("NEXUS_ROUTING_CONFIDENCE_DB", DefaultRoutingConfidenceDBPath())

	confFloor, err := getEnvFloat("NEXUS_ROUTING_CONFIDENCE_FLOOR", 0.4)
	if err != nil {
		return cfg, err
	}
	cfg.RoutingConfidenceFloor = confFloor

	confCeiling, err := getEnvFloat("NEXUS_ROUTING_CONFIDENCE_CEILING", 0.85)
	if err != nil {
		return cfg, err
	}
	cfg.RoutingConfidenceCeiling = confCeiling

	confMinSamples, err := getEnvInt("NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES", 5)
	if err != nil {
		return cfg, err
	}
	cfg.RoutingConfidenceMinSamples = confMinSamples

	confWindow, err := getEnvDuration("NEXUS_ROUTING_CONFIDENCE_WINDOW", 168*time.Hour)
	if err != nil {
		return cfg, err
	}
	cfg.RoutingConfidenceWindow = confWindow

	// Ollama health poller (issue #8). Defaults: 30s poll cadence,
	// 3-failure breaker, 5s per-probe HTTP timeout. Set
	// NEXUS_HEALTH_POLL_INTERVAL to "0" to disable polling entirely;
	// the chat handler then behaves as if Ollama is always healthy
	// (i.e. it will still try the local route on every request and
	// pay the upstream timeout if Ollama is down).
	healthPoll, err := getEnvDuration("NEXUS_HEALTH_POLL_INTERVAL", 30*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.HealthPollInterval = healthPoll

	healthBreaker, err := getEnvInt("NEXUS_HEALTH_BREAKER_THRESHOLD", 3)
	if err != nil {
		return cfg, err
	}
	cfg.HealthBreakerThreshold = healthBreaker

	healthProbe, err := getEnvDuration("NEXUS_HEALTH_PROBE_TIMEOUT", 5*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.HealthProbeTimeout = healthProbe

	// Hardware-aware VRAM probe (issue #6). Defaults: 60s poll,
	// 5s per-probe timeout, 256 KiB per token heuristic (which
	// works out to ~32k tokens of safe headroom on an 8 GiB GPU).
	// Set NEXUS_PROBE_INTERVAL to "0" to disable periodic polling
	// entirely; the boot probe still runs synchronously once. When
	// the probe is disabled or returns zero (Ollama down + no AMD
	// sysfs), the chat handler falls back to TokenGuardrail.
	probeInterval, err := getEnvDuration("NEXUS_PROBE_INTERVAL", 60*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.ProbePollInterval = probeInterval

	probeTimeout, err := getEnvDuration("NEXUS_PROBE_TIMEOUT", 5*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.ProbeTimeout = probeTimeout

	probeBytes, err := getEnvInt("NEXUS_PROBE_BYTES_PER_TOKEN", 256*1024)
	if err != nil {
		return cfg, err
	}
	if probeBytes < 0 {
		probeBytes = 0
	}
	cfg.ProbeBytesPerToken = probeBytes
	cfg.ProbeEnabled = cfg.ProbePollInterval > 0

	// Local-route concurrency ceiling (issue #81). The limiter is
	// dormant unless the operator sets NEXUS_LOCAL_MAX_CONCURRENT
	// above zero, so a stock deployment is byte-for-byte identical to
	// the pre-#81 unlimited path. When enabled, the effective slot
	// count is min(ceiling, freeVRAM/bytesPerSlot) recomputed on every
	// acquire from the latest probe snapshot; when the probe is
	// unavailable the full ceiling is used.
	localMax, err := getEnvInt("NEXUS_LOCAL_MAX_CONCURRENT", 0)
	if err != nil {
		return cfg, err
	}
	if localMax < 0 {
		localMax = 0
	}
	cfg.LocalMaxConcurrent = localMax

	localSlotBytes, err := getEnvInt("NEXUS_LOCAL_VRAM_BYTES_PER_SLOT", int(DefaultLocalVRAMBytesPerSlot))
	if err != nil {
		return cfg, err
	}
	if localSlotBytes < 0 {
		localSlotBytes = int(DefaultLocalVRAMBytesPerSlot)
	}
	cfg.LocalVRAMBytesPerSlot = int64(localSlotBytes)

	// Local-route cooldown (issue #80). Arms a short cooldown after
	// the cascade detects a local failure so subsequent requests skip
	// local and go directly to the fallback route. Default 10s; zero
	// disables the circuit (pre-#80 behaviour).
	localCooldown, err := getEnvDuration("NEXUS_LOCAL_COOLDOWN", 10*time.Second)
	if err != nil {
		return cfg, err
	}
	if localCooldown < 0 {
		localCooldown = 0
	}
	cfg.LocalCooldown = localCooldown

	// Hard request-body cap (issue #11). Default 1 MiB matches typical
	// OpenAI-compatible request sizes; the chat handler wraps r.Body
	// with http.MaxBytesReader so an oversized POST is rejected with
	// 413 before any allocation happens.
	maxBodyBytes, err := getEnvInt("NEXUS_MAX_BODY_BYTES", DefaultMaxBodyBytes)
	if err != nil {
		return cfg, err
	}
	cfg.MaxBodyBytes = maxBodyBytes

	// Judge (issue #15). Defaults: z.ai-style endpoint, sample 10% of
	// local-route successes, 2 concurrent workers, 30s per call. When
	// JudgeURL is unset we fall back to NEXUS_FRONTIER_URL so a stock
	// config still works.
	cfg.JudgeURL = getEnv("NEXUS_JUDGE_URL", "https://api.z.ai/v1/chat/completions")
	if v := os.Getenv("NEXUS_JUDGE_URL"); v == "" {
		cfg.JudgeURL = cfg.FrontierURL
	}
	cfg.JudgeModel = getEnv("NEXUS_JUDGE_MODEL", cfg.FrontierModel)
	cfg.JudgeAPIKey = getEnv("NEXUS_JUDGE_API_KEY", cfg.FrontierKey)

	sampleRate, err := getEnvFloat("NEXUS_JUDGE_SAMPLE_RATE", 0.1)
	if err != nil {
		return cfg, err
	}
	cfg.JudgeSampleRate = sampleRate

	concurrency, err := getEnvInt("NEXUS_JUDGE_CONCURRENCY", 2)
	if err != nil {
		return cfg, err
	}
	cfg.JudgeConcurrency = concurrency

	queueDepth, err := getEnvInt("NEXUS_JUDGE_QUEUE", 64)
	if err != nil {
		return cfg, err
	}
	cfg.JudgeQueueDepth = queueDepth

	judgeTimeout, err := getEnvDuration("NEXUS_JUDGE_TIMEOUT", 30*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.JudgeTimeout = judgeTimeout

	costRate, err := getEnvFloat("NEXUS_JUDGE_COST_PER_1K", 0.002)
	if err != nil {
		return cfg, err
	}
	cfg.JudgeCostPer1KUSD = costRate

	// The judge is "enabled" iff the operator actually configured
	// sampling above zero. Zero/negative rate keeps the worker pool
	// dormant even if the env vars are partially populated (a common
	// condition during local development).
	cfg.JudgeEnabled = cfg.JudgeSampleRate > 0 && cfg.JudgeURL != "" && cfg.JudgeModel != ""

	// Quality verifier (issue #13). The verifier is dormant when
	// QualityConcurrency is non-positive; the chat handler treats a
	// nil observer as "no-op", so the hot path is unaffected by an
	// unconfigured quality pipeline (same pattern as the judge).
	qualityConcurrency, err := getEnvInt("NEXUS_QUALITY_CONCURRENCY", 2)
	if err != nil {
		return cfg, err
	}
	cfg.QualityConcurrency = qualityConcurrency

	qualityQueueDepth, err := getEnvInt("NEXUS_QUALITY_QUEUE", 64)
	if err != nil {
		return cfg, err
	}
	cfg.QualityQueueDepth = qualityQueueDepth

	qualityTimeout, err := getEnvDuration("NEXUS_QUALITY_TIMEOUT", 60*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.QualityTimeout = qualityTimeout

	stderrCap, err := getEnvInt("NEXUS_QUALITY_STDERR_CAP", 2*1024)
	if err != nil {
		return cfg, err
	}
	cfg.QualityStderrCap = stderrCap

	cfg.QualityEnabled = cfg.QualityConcurrency > 0

	// Structured logging (issue #3). Defaults match the production
	// expectation: JSON to stderr at info level. Operators flip on
	// debug by setting NEXUS_LOG_LEVEL=debug, and switch to a
	// human-friendly text handler with NEXUS_LOG_FORMAT=text.
	cfg.LogLevel = parseLogLevel(os.Getenv("NEXUS_LOG_LEVEL"))
	cfg.LogFormat = parseLogFormat(os.Getenv("NEXUS_LOG_FORMAT"))

	// Debug tracing (issue #33). Off by default so production has
	// zero overhead. Body preview is bounded by NEXUS_DEBUG_BODY_BYTES
	// (default DefaultDebugBodyBytes = 512); zero or negative falls
	// back to the default.
	cfg.Debug = parseBoolEnv("NEXUS_DEBUG", false)
	debugBodyBytes, err := getEnvInt("NEXUS_DEBUG_BODY_BYTES", DefaultDebugBodyBytes)
	if err != nil {
		return cfg, err
	}
	cfg.DebugBodyBytes = debugBodyBytes

	// OpenAI-compatible model discovery (issue #78). Enabled by
	// default so a stock deployment is discoverable by OpenAI-
	// compatible clients; operators who do not want the proxy to
	// advertise its model list set NEXUS_MODELS_ENDPOINT=false.
	cfg.ModelsEndpointEnabled = parseBoolEnv("NEXUS_MODELS_ENDPOINT", true)

	modelsCacheTTL, err := getEnvDuration("NEXUS_MODELS_CACHE_TTL", 5*time.Minute)
	if err != nil {
		return cfg, err
	}
	cfg.ModelsCacheTTL = modelsCacheTTL

	// Prompt-injection hardening (issue #76). Defaults to off so a
	// stock deployment boots with byte-for-byte legacy behaviour.
	// Operators opt into warn (log only) or strict (reject 400) by
	// setting NEXUS_PROMPT_INJECTION_MODE. Unknown values fall back
	// to off rather than failing boot.
	cfg.PromptInjectionMode = middleware.ParseInjectionMode(
		os.Getenv("NEXUS_PROMPT_INJECTION_MODE"),
	)

	return cfg, nil
}

// FrontierEnabled reports whether a frontier API key is configured. The proxy
// still runs without one (fusion will degrade to local-only), but frontier
// routing will return 401s if attempted.
func (c Config) FrontierEnabled() bool { return c.FrontierKey != "" }

// FrontierProvider describes one configured frontier endpoint. The
// chat handler consults FrontierProviders when route=frontier is
// selected; when more than one provider is configured, the handler
// also consults a router.ProviderSelector (issue #45) to pick the
// cheaper / faster endpoint based on observed metrics.
type FrontierProvider struct {
	Name         string  // "frontier" or "zai"
	URL          string  // upstream endpoint
	Model        string  // OpenAI-compatible model name
	APIKey       string  // bearer token (empty for the local endpoint)
	CostPer1KUSD float64 // USD per 1k input tokens (selector weight)
}

// FrontierProviders returns the configured frontier endpoints in
// declaration order (frontier first, z.ai second — same order as the
// existing cascade). Providers with an empty APIKey are omitted so a
// half-configured deployment cannot accidentally proxy requests to an
// unauthenticated endpoint. CostPer1KUSD is sourced from
// FrontierCostPer1K / ZAICostPer1K so the selector has a deterministic
// cost weight even before the metrics store has observed traffic.
func (c Config) FrontierProviders() []FrontierProvider {
	out := make([]FrontierProvider, 0, 2)
	if c.FrontierKey != "" {
		out = append(out, FrontierProvider{
			Name:         "frontier",
			URL:          c.FrontierURL,
			Model:        c.FrontierModel,
			APIKey:       c.FrontierKey,
			CostPer1KUSD: c.FrontierCostPer1K,
		})
	}
	if c.ZAIKey != "" {
		out = append(out, FrontierProvider{
			Name:         "zai",
			URL:          c.ZAIURL,
			Model:        c.ZAIModel,
			APIKey:       c.ZAIKey,
			CostPer1KUSD: c.ZAICostPer1K,
		})
	}
	return out
}

// DefaultMaxBodyBytes is the fallback request-body cap (issue #11). 1 MiB
// matches the typical OpenAI chat-completions request envelope; agents that
// need more room can raise it via NEXUS_MAX_BODY_BYTES.
const DefaultMaxBodyBytes = 1 << 20 // 1 MiB

// EffectiveMaxBodyBytes returns the request-body cap the chat handler should
// enforce. Zero or negative values fall back to DefaultMaxBodyBytes so a
// zero-value Config (e.g. inside unit tests) still gets a sane cap.
func (c Config) EffectiveMaxBodyBytes() int {
	if c.MaxBodyBytes > 0 {
		return c.MaxBodyBytes
	}
	return DefaultMaxBodyBytes
}

// DefaultDebugBodyBytes is the upper bound on the response-body preview
// the debug trace logs (issue #33). 512 bytes is enough to identify the
// upstream model, see the first few tokens, and recognise a malformed
// reply, while keeping the log line a reasonable size.
const DefaultDebugBodyBytes = 512

// DefaultLocalVRAMBytesPerSlot is the VRAM reservation each concurrent
// local-route slot assumes when NEXUS_LOCAL_VRAM_BYTES_PER_SLOT is unset
// (issue #81). 2 GiB keeps a Q4-quantised 8B model plus a modest context
// resident; on the PRD's target 8-12 GiB GPUs this yields ~3-5 effective
// slots once the loaded model's footprint is accounted for. The value
// only affects the dynamic shrink path; when the probe is unavailable
// the full NEXUS_LOCAL_MAX_CONCURRENT ceiling is used regardless.
const DefaultLocalVRAMBytesPerSlot int64 = 2 << 30 // 2 GiB

// EffectiveDebugBodyBytes returns the response-body preview cap the
// debug trace should honour. Zero or negative falls back to
// DefaultDebugBodyBytes so a zero-value Config (typical in tests) still
// gets a sane cap.
func (c Config) EffectiveDebugBodyBytes() int {
	if c.DebugBodyBytes > 0 {
		return c.DebugBodyBytes
	}
	return DefaultDebugBodyBytes
}

// TelemetryEnabled reports whether the on-disk recorder should be started.
// Disabled when TelemetryPath is empty.
func (c Config) TelemetryEnabled() bool { return c.TelemetryPath != "" }

// ModelsCacheEnabled reports whether the Ollama /api/tags poll should
// supplement the configured models list (issue #78). Disabled when
// ModelsCacheTTL is zero or negative — the handler then serves only
// the configured local/router/frontier models with no HTTP round-trip
// to Ollama per request.
func (c Config) ModelsCacheEnabled() bool { return c.ModelsCacheTTL > 0 }

// PromptInjectionIsolated reports whether the proxy should isolate
// its policy text from user-supplied system content (issue #76).
// True in warn and strict modes; false in the default off mode so
// the legacy append path is preserved.
func (c Config) PromptInjectionIsolated() bool {
	return c.PromptInjectionMode == middleware.InjectionModeWarn ||
		c.PromptInjectionMode == middleware.InjectionModeStrict
}

// MetricsEnabled reports whether the SQLite metrics store should be
// opened. Disabled when MetricsDBPath is empty.
func (c Config) MetricsEnabled() bool { return c.MetricsDBPath != "" }

// RoutingConfidenceEnabled reports whether the judge-guided adaptive
// routing store (issue #47) should be opened. It requires BOTH a configured
// DB path AND the judge to be enabled: without judge scores there is no
// data to aggregate, and the acceptance criteria mandate that a disabled
// judge produces byte-for-byte identical routing (no DB queries).
func (c Config) RoutingConfidenceEnabled() bool {
	return c.RoutingConfidenceDB != "" && c.JudgeEnabled
}

// RAGPersistentEnabled reports whether the SQLite-backed RAG store
// (issue #46) should be opened. Disabled when RAGDBPath is empty,
// which preserves the legacy in-memory-only behaviour for operators
// who want zero on-disk state.
func (c Config) RAGPersistentEnabled() bool { return c.RAGDBPath != "" }

// RAGWatcherEnabled reports whether the background file watcher
// (issue #46) should be started. Disabled when RAGPollInterval is
// zero OR when the persistent store itself is disabled.
func (c Config) RAGWatcherEnabled() bool {
	return c.RAGPersistentEnabled() && c.RAGPollInterval > 0
}

// NewLogger returns a *slog.Logger configured per LogLevel / LogFormat,
// always writing to stderr. Centralising the construction in config
// keeps main.go free of slog option plumbing and lets tests construct
// matching loggers (issue #3).
func (c Config) NewLogger() *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.LogLevel}
	var h slog.Handler
	switch c.LogFormat {
	case LogFormatText:
		h = slog.NewTextHandler(os.Stderr, opts)
	default:
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// getEnvAllowEmpty is like getEnv but returns the empty string when the
// caller has explicitly set the variable to "". Used for the telemetry path
// so operators can disable recording with NEXUS_TELEMETRY_PATH="".
func getEnvAllowEmpty(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func getEnvInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer: %w", key, err)
	}
	return n, nil
}

func getEnvFloat(key string, def float64) (float64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a number: %w", key, err)
	}
	return f, nil
}

func getEnvDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a duration (e.g. 8s, 2m): %w", key, err)
	}
	return d, nil
}

// parseBoolEnv maps a string env value to a bool with the supplied
// default. Accepts the canonical spellings (true/false, 1/0, yes/no,
// on/off) case-insensitively; an empty / unparseable value falls back
// to def rather than failing boot. Used for the opt-in feature flag
// NEXUS_FUSION_PROGRESSIVE (issue #48) where falling back to the
// default is the safest failure mode.
func parseBoolEnv(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return def
	}
}

// LogFormat is the wire format for the structured logger. JSON is the
// production default; Text is friendlier for local development (issue #3).
type LogFormat int

const (
	LogFormatJSON LogFormat = iota
	LogFormatText
)

// String renders the LogFormat as the canonical env-var spelling.
func (f LogFormat) String() string {
	switch f {
	case LogFormatJSON:
		return "json"
	case LogFormatText:
		return "text"
	default:
		return "unknown"
	}
}

// parseLogLevel maps NEXUS_LOG_LEVEL to a slog.Level. Unknown / unset
// values fall back to slog.LevelInfo so a stock `.env.example` boots at
// the same verbosity as before (issue #3).
func parseLogLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	case "", "info":
		return slog.LevelInfo
	default:
		return slog.LevelInfo
	}
}

// parseLogFormat maps NEXUS_LOG_FORMAT to a LogFormat. Unknown / unset
// values fall back to LogFormatJSON.
func parseLogFormat(raw string) LogFormat {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "text":
		return LogFormatText
	case "", "json":
		return LogFormatJSON
	default:
		return LogFormatJSON
	}
}

const (
	defaultMetaPrompt = `
[PROXY METADATA ENHANCEMENT]: 
- ROLE: You are an elite, autonomous Principal AI Software Engineer.
- REASONING (Chain-of-Thought): You must ALWAYS think step-by-step. Analyze the requirements, edge cases, and architectural impact before generating a single line of code.
- CONSTRAINTS: Prioritize modularity, memory efficiency, and strict security patterns. Do not silently ignore errors or swallow exceptions.
- FORMATTING: Provide clean, well-commented code. Do not use generic pleasantries.`

	defaultTOONNotice = "\n\n[PROXY SYSTEM NOTE]: Data arrays have been compressed using Token-Oriented Object Notation (TOON). The format is `object_name[count]{key1,key2}:\n  val1,val2`. Read the schema header to map the comma-separated rows."
)
