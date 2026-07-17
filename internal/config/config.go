// Package config loads runtime configuration from environment variables.
//
// All values have safe defaults so the binary boots in development with a
// local Ollama instance. Secrets (FRONTIER_API_KEY) must be supplied via env
// in any non-development deployment.
//
// # Unknown env var detection
//
// Load() warns about any NEXUS_* env vars that are set but not recognised
// by the config loader. This catches typos like NEXUS_FRONTIER_APIKEY (missing
// underscore) or NEXUS_JUDGE_SAMPLE_RATES (trailing S) before they cause
// silent feature-disable surprises at runtime.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/anchapin/nexus-proxy/internal/middleware"
	ragpkg "github.com/anchapin/nexus-proxy/internal/rag"
)

// knownEnvVars is the exhaustive allowlist of NEXUS_* environment variables
// that the config loader recognises. Variables not in this map are reported
// as unknown (likely typos) via a warn-level log message at boot.
//
// Add new variables here when adding a new config field; the checker is
// case-sensitive so NEXUS_FOO and NEXUS_Foo are distinct.
var knownEnvVars = map[string]struct{}{
	// Server
	"NEXUS_ADDR":                    {},
	"NEXUS_SERVER_READ_TIMEOUT":     {},
	"NEXUS_SERVER_WRITE_TIMEOUT":    {},
	"NEXUS_SERVER_IDLE_TIMEOUT":     {},
	"NEXUS_SERVER_MAX_HEADER_BYTES": {},
	"NEXUS_SHUTDOWN_TIMEOUT":        {},
	// Ollama
	"NEXUS_OLLAMA_URL":      {},
	"NEXUS_ROUTER_MODEL":    {},
	"NEXUS_LOCAL_MODEL":     {},
	"NEXUS_EMBEDDING_MODEL": {},
	// Frontier
	"NEXUS_FRONTIER_URL":         {},
	"NEXUS_FRONTIER_MODEL":       {},
	"NEXUS_FRONTIER_API_KEY":     {},
	"NEXUS_FRONTIER_COST_PER_1K": {},
	// Z.ai
	"NEXUS_ZAI_URL":         {},
	"NEXUS_ZAI_MODEL":       {},
	"NEXUS_ZAI_API_KEY":     {},
	"NEXUS_ZAI_COST_PER_1K": {},
	// Auth
	"NEXUS_PROXY_API_KEY": {},
	"NEXUS_STATUS_PUBLIC": {},
	// RAG
	"NEXUS_EXAMPLES_DIR":                  {},
	"NEXUS_RAG_THRESHOLD":                 {},
	"NEXUS_RAG_DB":                        {},
	"NEXUS_RAG_POLL_INTERVAL":             {},
	"NEXUS_RAG_WATCHER_DISABLED":          {},
	"NEXUS_RAG_EMBED_CACHE_SIZE":          {},
	"NEXUS_RAG_EMBED_CACHE_TTL":           {},
	"NEXUS_RAG_CIRCUIT_BREAKER_THRESHOLD": {},
	"NEXUS_RAG_CIRCUIT_BREAKER_COOLDOWN":  {},
	// Embedder plugin (issue #238)
	"NEXUS_EMBEDDER_TYPE":     {},
	"NEXUS_EMBEDDER_BASE_URL": {},
	"NEXUS_COHERE_API_KEY":    {},
	// Routing
	"NEXUS_TOKEN_GUARDRAIL":               {},
	"NEXUS_SLM_TIMEOUT":                   {},
	"NEXUS_SLM_CACHE_MAX_ENTRIES":         {},
	"NEXUS_SLM_CACHE_TTL":                 {},
	"NEXUS_SLMCACHE_SIMILARITY_THRESHOLD": {},
	"NEXUS_SLM_CONFIDENCE_THRESHOLD":      {},
	"NEXUS_FUSION_TIMEOUT":                {},
	"NEXUS_FUSION_PER_FETCH_TIMEOUT":      {},
	"NEXUS_CASCADE_TIMEOUT":               {},
	"NEXUS_ARBITER_TIMEOUT":               {},
	"NEXUS_FUSION_PROGRESSIVE":            {},
	"NEXUS_FUSION_AGREEMENT_THRESHOLD":    {},
	"NEXUS_ARBITER_CACHE_TTL":             {},
	// DSL
	"NEXUS_DSL_FORMATTING_PATTERNS": {},
	"NEXUS_DSL_FUSION_PATTERNS":     {},
	"NEXUS_DSL_LOCAL_PATTERNS":      {},
	// Provider selector (issue #45)
	"NEXUS_SELECTOR_WINDOW":      {},
	"NEXUS_SELECTOR_MIN_SAMPLES": {},
	"NEXUS_SELECTOR_REFRESH":     {},
	// Cost avoidance baseline (issue #73)
	"NEXUS_COST_BASELINE_PROVIDER":    {},
	"NEXUS_COST_BASELINE_MODEL":       {},
	"NEXUS_COST_BASELINE_RATE_PER_1K": {},
	// Judge (issue #15)
	"NEXUS_JUDGE_URL":         {},
	"NEXUS_JUDGE_MODEL":       {},
	"NEXUS_JUDGE_API_KEY":     {},
	"NEXUS_JUDGE_SAMPLE_RATE": {},
	"NEXUS_JUDGE_CONCURRENCY": {},
	"NEXUS_JUDGE_QUEUE":       {},
	"NEXUS_JUDGE_TIMEOUT":     {},
	"NEXUS_JUDGE_COST_PER_1K": {},
	"NEXUS_JUDGE_DB":          {},
	// Quality verifier (issue #13)
	"NEXUS_QUALITY_CONCURRENCY": {},
	"NEXUS_QUALITY_QUEUE":       {},
	"NEXUS_QUALITY_TIMEOUT":     {},
	"NEXUS_QUALITY_STDERR_CAP":  {},
	// Health (issue #8)
	"NEXUS_HEALTH_POLL_INTERVAL":     {},
	"NEXUS_HEALTH_BREAKER_THRESHOLD": {},
	"NEXUS_HEALTH_PROBE_TIMEOUT":     {},
	// VRAM probe (issue #6)
	"NEXUS_PROBE_INTERVAL":        {},
	"NEXUS_PROBE_TIMEOUT":         {},
	"NEXUS_PROBE_BYTES_PER_TOKEN": {},
	// Local concurrency (issue #81)
	"NEXUS_LOCAL_MAX_CONCURRENT":      {},
	"NEXUS_LOCAL_VRAM_BYTES_PER_SLOT": {},
	// Local cooldown (issue #80)
	"NEXUS_LOCAL_COOLDOWN": {},
	// Body cap (issue #11)
	"NEXUS_MAX_BODY_BYTES": {},
	// Middleware
	"NEXUS_MIDDLEWARE_CHAIN":      {},
	"NEXUS_META_PROMPT":           {},
	"NEXUS_TOON_NOTICE":           {},
	"NEXUS_TOON_UNFENCED":         {},
	"NEXUS_PROMPT_INJECTION_MODE": {},
	// Telemetry & tracing
	"NEXUS_TELEMETRY_PATH":   {},
	"NEXUS_METRICS_DB":       {},
	"NEXUS_TRACING_ENDPOINT": {},
	"NEXUS_TRACING_TIMEOUT":  {},
	// Structured logging (issue #3)
	"NEXUS_LOG_LEVEL":  {},
	"NEXUS_LOG_FORMAT": {},
	// Debug tracing (issue #33)
	"NEXUS_DEBUG":            {},
	"NEXUS_DEBUG_BODY_BYTES": {},
	// Model discovery (issue #78)
	"NEXUS_MODELS_ENDPOINT":  {},
	"NEXUS_MODELS_CACHE_TTL": {},
	// Trusted proxies & rate limiting (issue #75)
	"NEXUS_TRUSTED_PROXIES":  {},
	"NEXUS_RATE_LIMIT_RPM":   {},
	"NEXUS_RATE_LIMIT_BURST": {},
	// Auth brute-force (issue #296)
	"NEXUS_AUTH_RATE_LIMIT_RPM":    {},
	"NEXUS_AUTH_RATE_LIMIT_BURST":  {},
	"NEXUS_AUTH_RATE_LIMIT_WINDOW": {},
	// Budget guard (issue #183, #201)
	"NEXUS_BUDGET_DAILY_LIMIT":       {},
	"NEXUS_BUDGET_ALERT_ENABLED":     {},
	"NEXUS_BUDGET_ALERT_THRESHOLD":   {},
	"NEXUS_BUDGET_ALERT_WEBHOOK_URL": {},
	// Readiness (issue #302)
	"NEXUS_READINESS_MODE": {},
	// Routing confidence (issue #47)
	"NEXUS_ROUTING_CONFIDENCE_DB":          {},
	"NEXUS_ROUTING_CONFIDENCE_FLOOR":       {},
	"NEXUS_ROUTING_CONFIDENCE_CEILING":     {},
	"NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES": {},
	"NEXUS_ROUTING_CONFIDENCE_WINDOW":      {},
}

// Config holds all runtime knobs for the proxy. A zero value is invalid;
// always go through Load.
type Config struct {
	// HTTP server
	Addr string // ":8000"

	// HTTP listener timeouts and header cap (issue #77). These bound
	// the inbound connection so a slowloris-style client or an
	// oversized header cannot exhaust the server. Distinct from the
	// outbound NEXUS_HTTP_* transport knobs — these apply to the
	// http.Server listener, not to upstream client calls.
	//
	// WriteTimeout defaults to 0 (disabled) so SSE streaming
	// responses are never killed mid-stream; set it only when the
	// proxy is behind a buffering reverse proxy. ReadTimeout covers
	// the full request read (headers + body) and should be generous
	// enough for large chat-completion payloads.
	ReadTimeout    time.Duration // full request read deadline; 0 disables
	WriteTimeout   time.Duration // full response write deadline; 0 disables (streaming-safe)
	IdleTimeout    time.Duration // keep-alive idle wait; 0 disables
	MaxHeaderBytes int           // max request header bytes; 0 uses Go default (1 MiB)

	// Graceful shutdown timeout (issue #121). Upper bound on the drain
	// window the HTTP server observes after SIGTERM/SIGINT — a frontier
	// SSE stream mid-token or a fusion arbiter call that just opened its
	// 60s WithTimeout window needs more than the prior hardcoded 10s.
	// 0 falls back to the default (30s) so the knob can never accidentally
	// disable the drain. Validated against ReadTimeout at boot: a drain
	// shorter than the inbound read deadline emits a warning because
	// in-flight requests would still be truncated mid-read.
	ShutdownTimeout time.Duration // SIGTERM/SIGINT drain window

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

	// Inbound auth (issue #109). When ProxyAPIKey is non-empty, every
	// non-exempt endpoint requires a matching Bearer token in the
	// Authorization header. /healthz and /metrics stay exempt for K8s
	// probes and Prometheus scrapers. /status is exempt only when
	// StatusPublic is true (default false) — the diagnostics surface
	// (frontier configured, judge enabled, VRAM state) is
	// reconnaissance-grade and should be gated by default.
	ProxyAPIKey  string // NEXUS_PROXY_API_KEY; empty disables auth
	StatusPublic bool   // NEXUS_STATUS_PUBLIC; exposes /status without auth

	// RAG
	ExamplesDir  string  // "./few_shot_examples"
	RAGThreshold float64 // cosine similarity cutoff for retrieval (0.55)

	// RAG embedder plugin interface (issue #238). EmbedderType selects
	// the backend: "ollama" (default), "openai", or "cohere". When
	// "openai" or "cohere" is selected the corresponding API key must
	// also be configured. The base URL for the remote backends defaults
	// to the standard public endpoints; override via NEXUS_EMBEDDER_BASE_URL.
	EmbedderType    ragpkg.EmbedderType // "ollama" | "openai" | "cohere"
	EmbedderBaseURL string              // base URL for openai/cohere embedder
	CohereAPIKey    string              // NEXUS_COHERE_API_KEY

	// RAG persistence (issue #46). When RAGDBPath is set, the few-shot
	// embeddings are cached on disk and reloaded on boot without
	// re-hitting Ollama. RAGPollInterval > 0 enables a background
	// goroutine that detects new / modified / deleted files in
	// ExamplesDir and updates the store incrementally. Set
	// NEXUS_RAG_DB="" to fall back to the legacy in-memory-only path.
	RAGDBPath          string        // on-disk SQLite database for the RAG store
	RAGPollInterval    time.Duration // watcher cadence; 0 means DefaultRAGPollInterval
	RAGWatcherDisabled bool          // true to disable the watcher regardless of interval

	// RAG embedding cache (issue #115). Prompt embeddings are
	// deterministic for a given model+text pair, so they are memoized
	// in a bounded LRU with TTL. RAGEmbedCacheSize=0 disables the cache;
	// RAGEmbedCacheTTL=0 disables caching (pass-through) even when size>0.
	RAGEmbedCacheSize int           // max LRU entries (256)
	RAGEmbedCacheTTL  time.Duration // per-entry TTL (24h default); 0 = pass-through

	// RAG circuit breaker (issue #222). After RAGCircuitBreakerThreshold
	// consecutive Ollama /api/embeddings failures the breaker trips and
	// enters a cooldown window during which Embed returns ErrCircuitOpen.
	RAGCircuitBreakerThreshold int           // consecutive failures to trip; 0 = disabled
	RAGCircuitBreakerCooldown  time.Duration // cooldown duration after trip

	// Routing
	TokenGuardrail            int           // estimated tokens above this force frontier (6000)
	SLMTimeout                time.Duration // Qwen3-Coder routing timeout (8s)
	SLMCacheMaxEntries        int           // max entries in SLM routing decision cache (512)
	SLMCacheSemanticThreshold float64       // cosine similarity floor for semantic cache hits (0.0..1.0, issue #245)
	SLMConfidenceThreshold    float64       // hard escalation threshold: local/fusion decisions below this force frontier (default 0.3, issue #301)
	FusionTimeout             time.Duration // per-panel-member fetch timeout (120s)
	FusionPerFetchTimeout     time.Duration // per-fetch timeout fallback for fusion panel members (120s); used when perFetchTimeout <= 0
	CascadeTimeout            time.Duration // per-attempt timeout for cascade fallback (30s)
	ArbiterTimeout            time.Duration // per-call timeout for the fusion arbiter stream (60s)

	// DSL fast-pass patterns (issue #305). DSLFormattingPatterns
	// matches simple formatting keywords (css, format, docstring, ...).
	// DSLFusionPatterns matches architecture keywords that warrant
	// running both local and frontier (fusion). DSLLocalPatterns
	// matches common coding task keywords (refactor, security scan,
	// ...). All three default to the prior hardcoded behaviour when
	// the corresponding env var is unset.
	DSLFormattingPatterns []*regexp.Regexp // NEXUS_DSL_FORMATTING_PATTERNS
	DSLFusionPatterns     []*regexp.Regexp // NEXUS_DSL_FUSION_PATTERNS
	DSLLocalPatterns      []*regexp.Regexp // NEXUS_DSL_LOCAL_PATTERNS

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

	// Cost-avoidance baseline (issue #73). The baseline represents
	// what each request WOULD have cost if sent to the frontier
	// provider at the frontier rate, regardless of the actual route.
	// savings_usd = max(baseline_cost - actual_cost, 0). When
	// CostBaselineProvider / CostBaselineModel are empty the proxy
	// defaults to the configured frontier provider / model, so a
	// stock deployment gets cost-avoidance tracking without extra
	// configuration. CostBaselineRatePer1K defaults to
	// FrontierCostPer1K so the baseline and the actual frontier
	// pricing stay consistent.
	CostBaselineProvider  string  // "frontier" (default) or a custom provider name
	CostBaselineModel     string  // NEXUS_FRONTIER_MODEL (default) or a custom model
	CostBaselineRatePer1K float64 // USD per 1k tokens for baseline valuation

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

	// Fusion arbiter synthesis cache (issue #232). When ArbiterCacheTTL > 0,
	// arbiter synthesis responses are cached keyed by a hash of
	// (first.Content, second.Content). Subsequent requests with identical
	// panel-member content return the cached synthesis text instead of
	// invoking the expensive frontier arbiter call. Set to 0 to disable
	// the cache (all arbiter calls are made, no caching).
	ArbiterCacheTTL time.Duration // NEXUS_ARBITER_CACHE_TTL; 0 disables

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

	// SLM decision cache (issue #206). Deduplicates identical prompts
	// within a TTL window so repeated requests don't trigger an SLM call.
	// NEXUS_SLM_CACHE_TTL <= 0 disables the cache.
	SLMCacheTTL time.Duration

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

	// Rolling 24h frontier spend guard (issue #183, #201). The guard
	// tracks USD costs over a sliding 24-hour window and rejects new
	// frontier/fusion requests with HTTP 429 when the daily limit is
	// exhausted. BudgetEnabled is true when BudgetDailyLimit > 0.
	//
	// Alerting (issue #201): When BudgetAlertEnabled is true, the guard
	// invokes the alerter callbacks when spend is recorded, when the
	// budget is exceeded, and when spend crosses the approaching
	// threshold (BudgetAlertThreshold fraction of the limit, default 80%).
	// The alerter updates Prometheus counters and logs at warn/error
	// level. Set BudgetAlertWebhookURL to enable webhook alerting.
	BudgetDailyLimit      float64 // USD; NEXUS_BUDGET_DAILY_LIMIT
	BudgetAlertEnabled    bool    // true iff NEXUS_BUDGET_ALERT_ENABLED is "true"
	BudgetAlertThreshold  float64 // fraction of limit that triggers "approaching" alert [0,1]; default 0.8
	BudgetAlertWebhookURL string  // optional webhook URL for JSON alert payloads

	// HTTP request body cap (issue #11). The chat handler applies this
	// with http.MaxBytesReader before reading the request body, so an
	// oversized POST cannot exhaust proxy memory before the guardrail
	// runs. Zero or negative falls back to DefaultMaxBodyBytes.
	MaxBodyBytes int

	// Upstream response cap (issue #386). BufferedFetchWithContext,
	// FetchPanel, and fetchCascadeStep apply this via io.LimitReader
	// so a malicious or misbehaving upstream returning multi-GB
	// responses cannot exhaust proxy memory. The read error (including
	// LimitReader's io.ErrUnexpectedEOF when the limit is hit) is
	// propagated to the caller. Zero or negative falls back to
	// DefaultMaxUpstreamResponseBytes.
	MaxUpstreamResponseBytes int64

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
	JudgeDBPath       string        // on-disk SQLite database for judge scores; empty disables Detected
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
	MetaPrompt   string // appended to system prompt by prompt_engine
	TOONNotice   string // appended when TOON compression is applied
	TOONUnfenced bool   // issue #123: compress bare (unfenced) JSON arrays; default true

	// Middleware chain (issue #224). Comma-separated ordered list of
	// registered middleware names to apply per request. Empty uses the
	// built-in default: "promptEngineering,rag,compressJSONBlocks,appendSystemNote".
	MiddlewareChain string

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

	// Tracing (issue #372). Endpoint is the full OTLP/JSON HTTP URL.
	// Empty disables distributed tracing (zero overhead).
	TracingEndpoint string
	TracingTimeout  time.Duration

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

	// Trusted-proxy enforcement + rate limiting (issue #75).
	//
	// TrustedProxies is the parsed CIDR allowlist sourced from
	// NEXUS_TRUSTED_PROXIES (comma-separated, e.g.
	// "10.0.0.0/8,172.16.0.0/12"). Only when the direct TCP peer is in
	// this list does the proxy honour X-Forwarded-For / X-Real-IP for
	// client-identity purposes. An empty/nil list means "trust nobody":
	// the direct peer IP is always used and forwarded headers are
	// ignored, so attackers who can reach the proxy directly cannot
	// spoof per-client rate-limit buckets. TrustedProxiesRaw preserves
	// the raw env value for diagnostics (boot warning echo).
	//
	// RateLimitRPM is the per-client request ceiling in requests per
	// minute; zero or negative disables rate limiting entirely so a
	// stock deployment is byte-for-byte identical to the pre-#75 path.
	// RateLimitBurst is the token-bucket capacity (max burst before
	// throttling); <=0 falls back to RateLimitRPM in the limiter.
	TrustedProxies    []*net.IPNet
	TrustedProxiesRaw string
	RateLimitRPM      int
	RateLimitBurst    int

	// Auth brute-force protection (issue #296). Tracks per-client-IP
	// auth failures and blocks the client after AuthRateLimitBurst
	// consecutive failures within a sliding AuthRateLimitWindow
	// window. AuthRateLimitRPM controls the steady-state refill rate.
	// When AuthRateLimitRPM <= 0 the limiter is disabled (transparent
	// passthrough) so a stock deployment without the env vars set is
	// unchanged from pre-#296 behaviour.
	AuthRateLimitRPM    int
	AuthRateLimitBurst  int
	AuthRateLimitWindow time.Duration // window for auth failure tracking (default 5 min)

	// Readiness mode for /readyz (issue #302). Controls whether the
	// readiness probe returns 503 when Ollama is down (strict) or
	// always returns 200 while surfacing the degraded flag (degraded,
	// the default). Unknown values fail validation at boot rather
	// than silently falling back, so a typo in NEXUS_READINESS_MODE
	// is caught immediately instead of producing an indeterminate state.
	ReadinessMode string
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

// DefaultRAGPollInterval is the default cadence for the background file
// watcher (issue #409). Operators override with NEXUS_RAG_POLL_INTERVAL;
// set NEXUS_RAG_WATCHER_DISABLED=true to disable the watcher.
const DefaultRAGPollInterval = 60 * time.Second

// DefaultJudgeDBPath returns the canonical location for the judge
// SQLite store (issue #198): $XDG_CACHE_HOME/nexus-proxy/judge.db.
// Operators override with NEXUS_JUDGE_DB (empty disables persistence,
// falling back to the in-memory MemoryStorage).
func DefaultJudgeDBPath() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = "./.cache"
	}
	return filepath.Join(base, "nexus-proxy", "judge.db")
}

// Load reads configuration from environment variables, applying defaults
// suitable for local development. It returns an error only when a required
// value is malformed; missing optional values fall back to defaults.
func Load() (Config, error) {
	cfg := Config{
		Addr:            getEnv("NEXUS_ADDR", ":8000"),
		OllamaURL:       strings.TrimRight(getEnv("NEXUS_OLLAMA_URL", "http://localhost:11434"), "/"),
		RouterModel:     getEnv("NEXUS_ROUTER_MODEL", "qwen3-coder:4b"),
		LocalModel:      getEnv("NEXUS_LOCAL_MODEL", "qwen3-coder:8b"),
		EmbeddingModel:  getEnv("NEXUS_EMBEDDING_MODEL", "nomic-embed-text"),
		FrontierURL:     getEnv("NEXUS_FRONTIER_URL", "https://api.openai.com/v1/chat/completions"),
		FrontierModel:   getEnv("NEXUS_FRONTIER_MODEL", "gpt-4o"),
		FrontierKey:     getEnv("NEXUS_FRONTIER_API_KEY", ""),
		ZAIURL:          getEnv("NEXUS_ZAI_URL", "https://api.z.ai/v1/chat/completions"),
		ZAIModel:        getEnv("NEXUS_ZAI_MODEL", "glm-4.6"),
		ZAIKey:          getEnv("NEXUS_ZAI_API_KEY", ""),
		ProxyAPIKey:     getEnv("NEXUS_PROXY_API_KEY", ""),
		StatusPublic:    getEnvBool("NEXUS_STATUS_PUBLIC", false),
		ExamplesDir:     getEnv("NEXUS_EXAMPLES_DIR", "./few_shot_examples"),
		MetaPrompt:      defaultMetaPrompt,
		TOONNotice:      defaultTOONNotice,
		TOONUnfenced:    getEnvBool("NEXUS_TOON_UNFENCED", true),
		MiddlewareChain: getEnv("NEXUS_MIDDLEWARE_CHAIN", ""),
		TelemetryPath:   getEnvAllowEmpty("NEXUS_TELEMETRY_PATH", "./nexus-telemetry.jsonl"),
		MetricsDBPath:   getEnvAllowEmpty("NEXUS_METRICS_DB", DefaultMetricsDBPath()),
		TracingEndpoint: getEnvAllowEmpty("NEXUS_TRACING_ENDPOINT", ""),
	}

	tracingTimeout, err := getEnvDuration("NEXUS_TRACING_TIMEOUT", 10*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.TracingTimeout = tracingTimeout

	threshold, err := getEnvFloat("NEXUS_RAG_THRESHOLD", 0.55)
	if err != nil {
		return cfg, err
	}
	cfg.RAGThreshold = threshold

	// RAG persistence (issue #46). The DB path defaults to the user
	// cache dir so multiple checkouts don't trample each other.
	// NEXUS_RAG_POLL_INTERVAL=0 means "use the default interval";
	// NEXUS_RAG_WATCHER_DISABLED=true explicitly disables the watcher.
	cfg.RAGDBPath = getEnvAllowEmpty("NEXUS_RAG_DB", DefaultRAGDBPath())

	pollInterval, err := getEnvDuration("NEXUS_RAG_POLL_INTERVAL", DefaultRAGPollInterval)
	if err != nil {
		return cfg, err
	}
	if pollInterval < 0 {
		pollInterval = 0
	}
	if pollInterval == 0 {
		pollInterval = DefaultRAGPollInterval
	}
	cfg.RAGPollInterval = pollInterval

	cfg.RAGWatcherDisabled = getEnvBool("NEXUS_RAG_WATCHER_DISABLED", false)

	// RAG embedding cache size (issue #115). Default 256 keeps the
	// cache useful for repetitive coding prompts while bounding
	// memory. Set to 0 to disable.
	embedCacheSize, err := getEnvInt("NEXUS_RAG_EMBED_CACHE_SIZE", 256)
	if err != nil {
		return cfg, err
	}
	cfg.RAGEmbedCacheSize = embedCacheSize

	// RAG embed cache TTL (issue #303). A TTL is required for EmbedCache
	// to be active; a value of 0 makes it a pass-through. Default 24h
	// keeps entries alive across a typical working day while still
	// eventually evicting stale entries.
	ragCacheTTL, err := getEnvDuration("NEXUS_RAG_EMBED_CACHE_TTL", 24*time.Hour)
	if err != nil {
		return cfg, err
	}
	cfg.RAGEmbedCacheTTL = ragCacheTTL

	// RAG embedder plugin interface (issue #238). The type selects
	// which backend the RAG store uses for vector embeddings.
	// Defaults to "ollama" so a stock deployment is unchanged.
	cfg.EmbedderType = ragpkg.EmbedderType(strings.ToLower(strings.TrimSpace(
		getEnv("NEXUS_EMBEDDER_TYPE", "ollama"))))
	// Base URL for remote embedder backends (openai/cohere). The
	// default matches each provider's public endpoint, so operators
	// only need to set the API key.
	switch cfg.EmbedderType {
	case ragpkg.EmbedderTypeOpenAI:
		cfg.EmbedderBaseURL = getEnv("NEXUS_EMBEDDER_BASE_URL", "https://api.openai.com/v1")
	case ragpkg.EmbedderTypeCohere:
		cfg.EmbedderBaseURL = getEnv("NEXUS_EMBEDDER_BASE_URL", "https://api.cohere.ai/v1")
	default:
		// Ollama: base URL is already in OllamaURL; reuse it.
		cfg.EmbedderBaseURL = cfg.OllamaURL
	}
	cfg.CohereAPIKey = getEnv("NEXUS_COHERE_API_KEY", "")

	// RAG circuit breaker (issue #222).
	cbThreshold, err := getEnvInt("NEXUS_RAG_CIRCUIT_BREAKER_THRESHOLD", 3)
	if err != nil {
		return cfg, err
	}
	cfg.RAGCircuitBreakerThreshold = cbThreshold

	cbCooldown, err := getEnvDuration("NEXUS_RAG_CIRCUIT_BREAKER_COOLDOWN", 30*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.RAGCircuitBreakerCooldown = cbCooldown

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

	// SLM routing decision cache (issue #162). Max
	// entries caps memory at ~512 entries with simple LRU eviction.
	slmCacheMax, err := getEnvInt("NEXUS_SLM_CACHE_MAX_ENTRIES", 512)
	if err != nil {
		return cfg, err
	}
	if slmCacheMax < 0 {
		slmCacheMax = 0
	}
	cfg.SLMCacheMaxEntries = slmCacheMax

	// SLM confidence hard-escalation threshold (issue #301). When the
	// SLM returns local/fusion with confidence below this value, the
	// planner overrides to frontier. The default (0.3) is deliberately
	// conservative: it only fires when the SLM is quite uncertain,
	// preserving the cost savings of local routing for clear-cut cases.
	// Set to 0 to disable the hard override (soft bias via
	// DecideWithConfidence still applies when a ConfidenceStore is
	// wired). The threshold is not validated — a value outside [0,1]
	// simply never triggers in practice.
	slmConfThreshold, err := getEnvFloat("NEXUS_SLM_CONFIDENCE_THRESHOLD", 0.3)
	if err != nil {
		return cfg, err
	}
	cfg.SLMConfidenceThreshold = slmConfThreshold

	fusionTimeout, err := getEnvDuration("NEXUS_FUSION_TIMEOUT", 120*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.FusionTimeout = fusionTimeout

	// Fusion per-fetch timeout fallback (issue #385). Used by upstream.withDefault
	// when the perFetchTimeout argument to Panel/PanelStreaming is <= 0.
	// Defaults to the same value as FusionTimeout for backward compatibility.
	fusionPerFetchTimeout, err := getEnvDuration("NEXUS_FUSION_PER_FETCH_TIMEOUT", 120*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.FusionPerFetchTimeout = fusionPerFetchTimeout

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

	// DSL fast-pass patterns (issue #305). Defaults match the prior
	// hardcoded behaviour so operators who upgrade see identical routing.
	dslFormatting, err := getEnvRegexps("NEXUS_DSL_FORMATTING_PATTERNS",
		`(?i)\b(css|format|docstring|lint|typo|boilerplate|debug|fix bug|git commit|sql query|parse json|validate input|regex|api endpoint|test|optimize|readme)\b`)
	if err != nil {
		return cfg, err
	}
	cfg.DSLFormattingPatterns = dslFormatting

	dslFusion, err := getEnvRegexps("NEXUS_DSL_FUSION_PATTERNS", `(?i)\b(architectural design|system architecture)\b`)
	if err != nil {
		return cfg, err
	}
	cfg.DSLFusionPatterns = dslFusion

	dslLocal, err := getEnvRegexps("NEXUS_DSL_LOCAL_PATTERNS",
		`(?i)\b(refactor|security scan|generate tests|explain this code|performance analysis)\b`)
	if err != nil {
		return cfg, err
	}
	cfg.DSLLocalPatterns = dslLocal

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

	// Cost-avoidance baseline (issue #73). Provider and model
	// default to the configured frontier values so a stock
	// deployment gets cost-avoidance tracking without extra config.
	// The rate defaults to FrontierCostPer1K so the baseline stays
	// consistent with the actual frontier pricing.
	cfg.CostBaselineProvider = getEnv("NEXUS_COST_BASELINE_PROVIDER", "frontier")
	cfg.CostBaselineModel = getEnv("NEXUS_COST_BASELINE_MODEL", cfg.FrontierModel)
	baselineRate, err := getEnvFloat("NEXUS_COST_BASELINE_RATE_PER_1K", cfg.FrontierCostPer1K)
	if err != nil {
		return cfg, err
	}
	if baselineRate < 0 {
		baselineRate = 0
	}
	cfg.CostBaselineRatePer1K = baselineRate

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

	// Fusion arbiter synthesis cache (issue #232). NEXUS_ARBITER_CACHE_TTL=0
	// (the default) disables the cache entirely — every disagreement
	// calls the arbiter. When set to a positive duration, identical
	// panel-member content within the TTL window returns the cached
	// synthesis text without calling the arbiter.
	arbiterCacheTTL, err := getEnvDuration("NEXUS_ARBITER_CACHE_TTL", 0)
	if err != nil {
		return cfg, err
	}
	cfg.ArbiterCacheTTL = arbiterCacheTTL

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

	// SLM decision cache TTL (issue #206). Set
	// NEXUS_SLM_CACHE_TTL to "0" to disable the cache entirely;
	// the planner then always calls the SLM (pre-cache behaviour).
	slmCacheTTL, err := getEnvDuration("NEXUS_SLM_CACHE_TTL", 30*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.SLMCacheTTL = slmCacheTTL

	// Semantic similarity threshold for SLM cache (issue #245).
	// 0.0 disables semantic deduplication; >0 uses cosine-similarity
	// fallback in the SLMCache so prompts with the same intent but
	// different wording share the same cached route decision.
	slmCacheSemThreshold, err := getEnvFloat("NEXUS_SLMCACHE_SIMILARITY_THRESHOLD", 0.0)
	if err != nil {
		return cfg, err
	}
	if slmCacheSemThreshold < 0 {
		slmCacheSemThreshold = 0
	}
	if slmCacheSemThreshold > 1.0 {
		slmCacheSemThreshold = 1.0
	}
	cfg.SLMCacheSemanticThreshold = slmCacheSemThreshold

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

	// Upstream response cap (issue #386). BufferedFetchWithContext,
	// FetchPanel, and fetchCascadeStep wrap the response body with
	// io.LimitReader so a malicious upstream cannot exhaust proxy memory.
	// The limit is exposed via EffectiveMaxUpstreamResponseBytes so
	// callers that need a different cap (e.g. the arbiter synthesis
	// path) can pass it down without re-parsing env. Default 10 MiB
	// accommodates multi-turn conversations with long contexts.
	maxUpstreamBytes, err := getEnvInt64("NEXUS_MAX_UPSTREAM_RESPONSE_BYTES", DefaultMaxUpstreamResponseBytes)
	if err != nil {
		return cfg, err
	}
	cfg.MaxUpstreamResponseBytes = maxUpstreamBytes

	// HTTP listener timeouts and header cap (issue #77). These bound
	// the inbound connection independently of the outbound transport
	// knobs (NEXUS_HTTP_*). WriteTimeout defaults to 0 (disabled) so
	// SSE streaming responses are never killed mid-stream. Negative
	// durations and negative header sizes are rejected so a typo in
	// .env fails fast at boot rather than silently disabling a guard.
	readTimeout, err := getEnvDuration("NEXUS_SERVER_READ_TIMEOUT", DefaultServerReadTimeout)
	if err != nil {
		return cfg, err
	}
	if readTimeout < 0 {
		return cfg, fmt.Errorf("config: NEXUS_SERVER_READ_TIMEOUT must not be negative, got %s", readTimeout)
	}
	cfg.ReadTimeout = readTimeout

	writeTimeout, err := getEnvDuration("NEXUS_SERVER_WRITE_TIMEOUT", 0)
	if err != nil {
		return cfg, err
	}
	if writeTimeout < 0 {
		return cfg, fmt.Errorf("config: NEXUS_SERVER_WRITE_TIMEOUT must not be negative, got %s", writeTimeout)
	}
	cfg.WriteTimeout = writeTimeout

	idleTimeout, err := getEnvDuration("NEXUS_SERVER_IDLE_TIMEOUT", DefaultServerIdleTimeout)
	if err != nil {
		return cfg, err
	}
	if idleTimeout < 0 {
		return cfg, fmt.Errorf("config: NEXUS_SERVER_IDLE_TIMEOUT must not be negative, got %s", idleTimeout)
	}
	cfg.IdleTimeout = idleTimeout

	maxHeader, err := getEnvInt("NEXUS_SERVER_MAX_HEADER_BYTES", DefaultServerMaxHeaderBytes)
	if err != nil {
		return cfg, err
	}
	if maxHeader < 0 {
		return cfg, fmt.Errorf("config: NEXUS_SERVER_MAX_HEADER_BYTES must not be negative, got %d", maxHeader)
	}
	cfg.MaxHeaderBytes = maxHeader

	// Graceful shutdown drain window (issue #121). Replaces the prior
	// hardcoded `const shutdownTimeout = 10 * time.Second` in main.go
	// so operators can tune the SIGTERM drain to match their longest
	// legitimate streaming response (frontier SSE can run 120s+). A
	// stock K8s deployment should set this just below its pod's
	// terminationGracePeriodSeconds. Zero is treated as "use the
	// default" (not "no drain") so a misconfigured .env cannot disable
	// the drain and leak in-flight requests. Negative is rejected so a
	// typo fails fast at boot. The boot-time warning when the drain is
	// shorter than ReadTimeout is emitted in main.go (the logger is
	// not yet wired here).
	shutdownTimeout, err := getEnvDuration("NEXUS_SHUTDOWN_TIMEOUT", DefaultShutdownTimeout)
	if err != nil {
		return cfg, err
	}
	if shutdownTimeout < 0 {
		return cfg, fmt.Errorf("config: NEXUS_SHUTDOWN_TIMEOUT must not be negative, got %s", shutdownTimeout)
	}
	if shutdownTimeout == 0 {
		slog.Warn("config: NEXUS_SHUTDOWN_TIMEOUT=0 is not supported; remapping to DefaultShutdownTimeout (30s) — set a positive duration to control the drain window",
			"default", DefaultShutdownTimeout)
		shutdownTimeout = DefaultShutdownTimeout
	}
	cfg.ShutdownTimeout = shutdownTimeout

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

	// Judge SQLite persistence (issue #198). When set, scores are
	// written to an on-disk SQLite store that survives restarts.
	// Empty (the default) uses the in-memory MemoryStorage.
	cfg.JudgeDBPath = getEnvAllowEmpty("NEXUS_JUDGE_DB", DefaultJudgeDBPath())

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

	// Prompt-injection hardening (issue #76). Defaults to warn so a
	// stock deployment logs injection attempts out of the box.
	// Operators can set NEXUS_PROMPT_INJECTION_MODE=off to disable,
	// or strict to reject 400. Unknown values fall back to warn.
	cfg.PromptInjectionMode = middleware.ParseInjectionMode(
		os.Getenv("NEXUS_PROMPT_INJECTION_MODE"),
	)
	// Trusted-proxy enforcement + rate limiting (issue #75).
	//
	// NEXUS_TRUSTED_PROXIES is a comma-separated CIDR list. Empty
	// (the default) means "trust nobody": the direct peer IP is used
	// and X-Forwarded-For / X-Real-IP are ignored, so an attacker who
	// reaches the proxy directly cannot spoof per-client rate-limit
	// buckets. Operators running Nexus behind nginx / a cloud load
	// balancer set this to the proxy's CIDR so forwarded headers are
	// honoured only from the real reverse proxy.
	//
	// Invalid CIDRs fail boot with a clear error — silently falling
	// back to "trust nobody" would mask a misconfiguration that
	// accidentally disables XFF honouiring for a legit deployment.
	cfg.TrustedProxiesRaw = strings.TrimSpace(os.Getenv("NEXUS_TRUSTED_PROXIES"))
	parsed, err := parseTrustedProxies(cfg.TrustedProxiesRaw)
	if err != nil {
		return cfg, err
	}
	cfg.TrustedProxies = parsed

	// Rolling 24h frontier spend guard (issue #183, #201). When
	// BudgetDailyLimit > 0 the guard is active. Alerting is
	// separately enabled via BudgetAlertEnabled.
	budgetDailyLimit, err := getEnvFloat("NEXUS_BUDGET_DAILY_LIMIT", 0)
	if err != nil {
		return cfg, err
	}
	if budgetDailyLimit < 0 {
		budgetDailyLimit = 0
	}
	cfg.BudgetDailyLimit = budgetDailyLimit

	cfg.BudgetAlertEnabled = parseBoolEnv("NEXUS_BUDGET_ALERT_ENABLED", false)

	budgetAlertThreshold, err := getEnvFloat("NEXUS_BUDGET_ALERT_THRESHOLD", 0.8)
	if err != nil {
		return cfg, err
	}
	if budgetAlertThreshold < 0 {
		budgetAlertThreshold = 0
	}
	if budgetAlertThreshold > 1 {
		budgetAlertThreshold = 1
	}
	cfg.BudgetAlertThreshold = budgetAlertThreshold

	cfg.BudgetAlertWebhookURL = getEnv("NEXUS_BUDGET_ALERT_WEBHOOK_URL", "")

	// Per-client rate ceiling (requests/minute). Zero or negative
	// disables the limiter entirely so a stock deployment is
	// byte-for-byte identical to the pre-#75 path. Burst is the
	// token-bucket capacity; <=0 falls back to RPM in the limiter.
	rateRPM, err := getEnvInt("NEXUS_RATE_LIMIT_RPM", 0)
	if err != nil {
		return cfg, err
	}
	if rateRPM < 0 {
		rateRPM = 0
	}
	cfg.RateLimitRPM = rateRPM

	rateBurst, err := getEnvInt("NEXUS_RATE_LIMIT_BURST", 0)
	if err != nil {
		return cfg, err
	}
	if rateBurst < 0 {
		rateBurst = 0
	}
	cfg.RateLimitBurst = rateBurst

	// Auth brute-force protection (issue #296). Defaults: RPM 5, burst 3,
	// window 5 min. When RPM <= 0 the limiter is disabled so a stock
	// deployment with no NEXUS_AUTH_RATE_LIMIT_RPM is byte-for-byte
	// identical to the pre-#296 behaviour.
	authRateRPM, err := getEnvInt("NEXUS_AUTH_RATE_LIMIT_RPM", 5)
	if err != nil {
		return cfg, err
	}
	if authRateRPM < 0 {
		authRateRPM = 0
	}
	cfg.AuthRateLimitRPM = authRateRPM

	authRateBurst, err := getEnvInt("NEXUS_AUTH_RATE_LIMIT_BURST", 3)
	if err != nil {
		return cfg, err
	}
	if authRateBurst < 0 {
		authRateBurst = 0
	}
	cfg.AuthRateLimitBurst = authRateBurst

	authRateWindow, err := getEnvDuration("NEXUS_AUTH_RATE_LIMIT_WINDOW", 5*time.Minute)
	if err != nil {
		return cfg, err
	}
	if authRateWindow < 0 {
		authRateWindow = 0
	}
	cfg.AuthRateLimitWindow = authRateWindow

	// Readiness mode for /readyz (issue #302). Controls whether the
	// readiness probe returns 503 when Ollama is down (strict) or
	// always returns 200 while surfacing the degraded flag (degraded,
	// the default). Unrecognised values fail boot rather than silently
	// falling back.
	cfg.ReadinessMode = getEnv("NEXUS_READINESS_MODE", "degraded")

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	checkUnknownEnvVars()
	return cfg, nil
}

// Validate checks that the loaded configuration is internally consistent
// and that all enum-like fields contain recognised values. It is called
// automatically at the end of Load(); unit tests that construct a Config
// directly should call it before use.
func (c Config) Validate() error {
	switch c.ReadinessMode {
	case "strict", "degraded":
		// Recognised values.
	default:
		return fmt.Errorf("config: NEXUS_READINESS_MODE value %q is not recognised; want \"strict\" or \"degraded\"", c.ReadinessMode)
	}
	return nil
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

// DefaultServerReadTimeout is the default inbound request read deadline
// (issue #77). 30s is generous for chat-completion payloads while still
// disconnecting slow-header/slow-body abuse well before the connection
// ties up a goroutine for minutes.
const DefaultServerReadTimeout = 30 * time.Second

// DefaultServerIdleTimeout is the default keep-alive idle wait (issue #77).
// 120s matches Go's http.DefaultServer zero-value behaviour and keeps a
// warm connection ready for the next request without holding it forever.
const DefaultServerIdleTimeout = 120 * time.Second

// DefaultServerMaxHeaderBytes is the default cap on the total size of
// the HTTP request headers (issue #77). Matches Go's
// http.DefaultMaxHeaderBytes (1 MiB) — large enough for auth cookies and
// content-type metadata, small enough to reject header-flood abuse.
const DefaultServerMaxHeaderBytes = 1 << 20 // 1 MiB

// DefaultShutdownTimeout is the default graceful-shutdown drain window
// (issue #121). 30s accommodates a frontier SSE stream mid-token and a
// fusion arbiter call that just opened its 60s WithTimeout window while
// staying comfortably under a typical K8s terminationGracePeriodSeconds
// of 30s. Operators running longer upstreams (or larger
// terminationGracePeriodSeconds) raise this via NEXUS_SHUTDOWN_TIMEOUT.
const DefaultShutdownTimeout = 30 * time.Second

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

// DefaultMaxUpstreamResponseBytes is the fallback cap on buffered upstream
// responses (issue #386). 10 MiB accommodates multi-turn conversations with
// long contexts while preventing a malicious or misbehaving upstream from
// exhausting proxy memory. Callers that need a different cap (e.g. the
// arbiter synthesis path) pass it directly; this default is used only
// when MaxUpstreamResponseBytes is zero.
const DefaultMaxUpstreamResponseBytes int64 = 10 << 20 // 10 MiB

// EffectiveMaxUpstreamResponseBytes returns the upstream response cap for
// BufferedFetchWithContext, FetchPanel, and fetchCascadeStep. Zero or
// negative falls back to DefaultMaxUpstreamResponseBytes so a zero-value
// Config (e.g. inside unit tests) still gets a sane cap.
func (c Config) EffectiveMaxUpstreamResponseBytes() int64 {
	if c.MaxUpstreamResponseBytes > 0 {
		return c.MaxUpstreamResponseBytes
	}
	return DefaultMaxUpstreamResponseBytes
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

// BudgetEnabled reports whether the rolling 24h frontier spend guard is
// active (issue #183). Disabled when BudgetDailyLimit <= 0.
func (c Config) BudgetEnabled() bool { return c.BudgetDailyLimit > 0 }

// RateLimitEnabled reports whether the per-client rate limiter is
// active (issue #75). Disabled when RateLimitRPM <= 0 so a stock
// deployment is byte-for-byte identical to the pre-#75 path.
func (c Config) RateLimitEnabled() bool { return c.RateLimitRPM > 0 }

// AuthRateLimitEnabled reports whether the auth brute-force limiter is
// active (issue #296). Disabled when AuthRateLimitRPM <= 0 so a stock
// deployment with no NEXUS_AUTH_RATE_LIMIT_RPM is byte-for-byte identical
// to the pre-#296 path.
func (c Config) AuthRateLimitEnabled() bool { return c.AuthRateLimitRPM > 0 }

// TrustedProxiesConfigured reports whether any trusted-proxy CIDRs are
// set. Used by the boot-time warning to detect the "rate limit on +
// non-loopback bind + no trusted proxies" misconfiguration that would
// let a single NATed IP exhaust the whole per-client budget.
func (c Config) TrustedProxiesConfigured() bool { return len(c.TrustedProxies) > 0 }

// IsLoopbackBind reports whether the configured NEXUS_ADDR binds only
// to a loopback interface. A listen address is considered loopback when
// its host portion is empty ("", as in ":8000" — binds all interfaces
// but is typically reached via localhost in dev), "localhost", or a
// literal loopback IP (127.0.0.0/8, ::1). Used by the boot warning to
// decide whether missing trusted-proxy config is dangerous.
//
// Note: ":8000" technically binds all interfaces, but we classify it as
// loopback-safe because the canonical production deployment sets an
// explicit non-loopback address (e.g. "0.0.0.0:8000") when exposing the
// proxy beyond the host. The warning targets operators who explicitly
// opened the proxy to the network.
func (c Config) IsLoopbackBind() bool {
	host := listenHost(c.Addr)
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Unknown host name; treat conservatively as non-loopback so
		// the warning fires rather than suppressing it.
		return false
	}
	return ip.IsLoopback()
}

// listenHost extracts the host portion of a "host:port" listen address,
// returning the whole string when it contains no port. IPv6 bracketed
// addresses ("[::1]:8000") are handled by net.SplitHostPort.
func listenHost(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// parseTrustedProxies parses the NEXUS_TRUSTED_PROXIES env value into a
// slice of *net.IPNet. Empty input yields nil + no error ("trust
// nobody"). A bare IP (no /prefix) is promoted to a host route
// (/32 for IPv4, /128 for IPv6) for ergonomic single-host trust
// entries. Any invalid entry returns an error naming the offender.
//
// Mirrors ratelimit.ParseTrustedCIDRs but is duplicated here so
// internal/config does not import internal/ratelimit (keeping the
// dependency direction clean: config is a leaf package).
func parseTrustedProxies(raw string) ([]*net.IPNet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]*net.IPNet, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ipnet, err := net.ParseCIDR(p); err == nil {
			out = append(out, ipnet)
			continue
		}
		if ip := net.ParseIP(p); ip != nil {
			if ip.To4() != nil {
				out = append(out, &net.IPNet{IP: ip.To4(), Mask: net.CIDRMask(32, 32)})
			} else {
				out = append(out, &net.IPNet{IP: ip.To16(), Mask: net.CIDRMask(128, 128)})
			}
			continue
		}
		return nil, fmt.Errorf("config: invalid NEXUS_TRUSTED_PROXIES entry %q (expected CIDR or IP)", p)
	}
	return out, nil
}

// RoutingConfidenceEnabled reports whether the judge-guided adaptive
// routing store (issue #47) should be opened. It requires BOTH a configured
// DB path AND the judge to be enabled: without judge scores there is no
// data to aggregate, and the acceptance criteria mandate that a disabled
// judge produces byte-for-byte identical routing (no DB queries).
func (c Config) RoutingConfidenceEnabled() bool {
	return c.RoutingConfidenceDB != "" && c.JudgeEnabled
}

// AuthEnabled reports whether the inbound API-key gate (issue #109)
// is active. When false, all endpoints are open — the binary behaves
// identically to the pre-auth proxy.
func (c Config) AuthEnabled() bool { return c.ProxyAPIKey != "" }

// SLMCacheEnabled reports whether the SLM decision cache (issue #206)
// should be active. Disabled when SLMCacheTTL <= 0, which preserves
// the pre-cache behaviour of always calling the SLM.
func (c Config) SLMCacheEnabled() bool {
	return c.SLMCacheTTL > 0
}

// RAGPersistentEnabled reports whether the SQLite-backed RAG store
// (issue #46) should be opened. Disabled when RAGDBPath is empty,
// which preserves the legacy in-memory-only behaviour for operators
// who want zero on-disk state.
func (c Config) RAGPersistentEnabled() bool { return c.RAGDBPath != "" }

// RAGWatcherEnabled reports whether the background file watcher
// (issue #46) should be started. Disabled when the persistent store
// itself is disabled or when NEXUS_RAG_WATCHER_DISABLED=true.
func (c Config) RAGWatcherEnabled() bool {
	return c.RAGPersistentEnabled() && !c.RAGWatcherDisabled && c.RAGPollInterval > 0
}

// JudgeDBEnabled reports whether the SQLite-backed judge store
// (issue #198) should be opened. Disabled when JudgeDBPath is empty,
// which preserves the legacy in-memory-only behaviour.
func (c Config) JudgeDBEnabled() bool { return c.JudgeDBPath != "" }

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

// HotReloadResult captures the outcome of a SIGHUP config reload (issue #306).
type HotReloadResult struct {
	// NeedsRestart is the list of env vars that were changed but require
	// a full proxy restart to take effect.
	NeedsRestart []string
}

// ReloadHotReloadable re-reads environment variables for settings that are
// safe to change at runtime (NEXUS_RATE_LIMIT_RPM, NEXUS_RATE_LIMIT_BURST,
// NEXUS_LOG_LEVEL, NEXUS_LOG_FORMAT, NEXUS_DEBUG) and returns a new Config
// with those fields updated. Settings that require a restart
// (NEXUS_OLLAMA_URL, NEXUS_FRONTIER_API_KEY, NEXUS_METRICS_DB) are checked
// and reported via NeedsRestart if they have changed since the last load.
// The caller should apply in-place changes (rate limiter RPM/Burst, logger
// level/format, debug flag) and log NeedsRestart with a restart hint.
func ReloadHotReloadable(prev Config) (Config, HotReloadResult) {
	result := HotReloadResult{}
	next := prev // start from previous so non-reloadable fields are preserved

	// Check which restart-required settings have changed.
	if v := os.Getenv("NEXUS_OLLAMA_URL"); v != "" && v != prev.OllamaURL {
		result.NeedsRestart = append(result.NeedsRestart, "NEXUS_OLLAMA_URL")
	}
	if v := os.Getenv("NEXUS_FRONTIER_API_KEY"); v != "" && v != prev.FrontierKey {
		result.NeedsRestart = append(result.NeedsRestart, "NEXUS_FRONTIER_API_KEY")
	}
	if v := os.Getenv("NEXUS_METRICS_DB"); v != "" && v != prev.MetricsDBPath {
		result.NeedsRestart = append(result.NeedsRestart, "NEXUS_METRICS_DB")
	}

	// Hot-reloadable settings.
	rateRPM, _ := getEnvInt("NEXUS_RATE_LIMIT_RPM", prev.RateLimitRPM)
	if rateRPM < 0 {
		rateRPM = 0
	}
	next.RateLimitRPM = rateRPM

	rateBurst, _ := getEnvInt("NEXUS_RATE_LIMIT_BURST", prev.RateLimitBurst)
	if rateBurst < 0 {
		rateBurst = 0
	}
	next.RateLimitBurst = rateBurst

	next.LogLevel = parseLogLevel(os.Getenv("NEXUS_LOG_LEVEL"))
	next.LogFormat = parseLogFormat(os.Getenv("NEXUS_LOG_FORMAT"))
	next.Debug = parseBoolEnv("NEXUS_DEBUG", prev.Debug)

	return next, result
}

// checkUnknownEnvVars scans the process environment for any NEXUS_* variables
// that are not in the knownEnvVars allowlist and logs a warn-level message for
// each one. This catches typos (NEXUS_FRONTIER_APIKEY, NEXUS_JUDGE_SAMPLE_RATES,
// etc.) before they cause silent feature-disable surprises at runtime.
func checkUnknownEnvVars() {
	const prefix = "NEXUS_"
	for _, e := range os.Environ() {
		if idx := strings.Index(e, "="); idx >= 0 {
			key := e[:idx]
			if strings.HasPrefix(key, prefix) {
				if _, ok := knownEnvVars[key]; !ok {
					slog.Warn("config: unknown NEXUS_* env var — check for a typo or remove it",
						"env_var", key)
				}
			}
		}
	}
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

// getEnvBool reads a boolean environment variable. Accepts
// "true"/"1"/"yes" (case-insensitive) as true; anything else is
// false. Returns def when the variable is unset or empty.
func getEnvBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

func getEnvInt64(key string, def int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
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

// getEnvRegexps parses a comma-separated list of regex patterns and
// compiles each one. The default is returned when the env var is unset
// or empty. An invalid pattern causes a fatal error at boot time.
func getEnvRegexps(key string, defaultPattern string) ([]*regexp.Regexp, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		v = defaultPattern
	}
	// Special case: if the env var was explicitly set to empty string,
	// return empty slice (operator wants to disable this DSL branch).
	if ok && v == "" {
		return []*regexp.Regexp{}, nil
	}
	patterns := strings.Split(v, ",")
	result := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("config: %s pattern %q is not a valid regex: %w", key, p, err)
		}
		result = append(result, re)
	}
	return result, nil
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
