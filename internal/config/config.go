// Package config loads runtime configuration from environment variables.
//
// All values have safe defaults so the binary boots in development with a
// local Ollama instance. Secrets (FRONTIER_API_KEY) must be supplied via env
// in any non-development deployment.
package config

import (
	"encoding/json"
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
	// WriteTimeout bounds the time a slow client can hold a connection
	// before the server closes it, preventing slow-client attacks on SSE
	// streaming responses (issue #1069). It defaults to 300 seconds (5 min),
	// long enough for legitimate streaming clients while capping abuse; set
	// to 0 only when the proxy is behind a buffering reverse proxy that
	// handles slow reads at the network edge. ReadTimeout covers the full
	// request read (headers + body) and should be generous enough for
	// large chat-completion payloads.
	ReadTimeout    time.Duration // full request read deadline; 0 disables
	WriteTimeout   time.Duration // full response write deadline; 0 disables (streaming-safe)
	IdleTimeout    time.Duration // keep-alive idle wait; 0 disables
	MaxHeaderBytes int           // max request header bytes; 0 uses Go default (1 MiB)

	// TLSEnabled drives the Strict-Transport-Security emission policy
	// (issue #444). When true, the security-headers middleware stamps
	// `Strict-Transport-Security: max-age=31536000` on every response so
	// clients pin HTTPS and refuse plaintext fallbacks. When false the
	// header is omitted: emitting HSTS over plaintext would be ignored by
	// browsers AND is a spec violation. Operators terminate TLS either by
	// configuring a cert/key on the inbound listener (future work tracked
	// under #39) OR by fronting the proxy with a TLS-terminating reverse
	// proxy (nginx, Caddy, ELB, Cloudflare). For the reverse-proxy path,
	// set NEXUS_TLS_ENABLED=true on the proxy even though the proxy
	// itself only speaks plaintext — the security-headers middleware is
	// the single source of truth and trusts the operator-supplied
	// posture.
	TLSEnabled bool // emit HSTS; true when the effective inbound is TLS

	// Inbound mTLS client certificate verification (issue #1241). When
	// set, the proxy requires and verifies client certificates from
	// downstream agents using the supplied CA certificate file.
	// The Common Name of the verified certificate is surfaced in
	// structured logs and audit records via X-Nexus-Client-CN.
	// Bearer-token auth remains active as defense-in-depth.
	TLSClientCAFile string // path to CA cert for client cert verification

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

	// Model aliasing (issue #1184). Maps client-requested model names
	// to "providerName/upstreamModel" so an operator can transparently
	// remap e.g. "gpt-4" to "anthropic/claude-3-5-sonnet". When a
	// request's model matches an alias the handler rewrites the body
	// and routes directly to the target provider. Empty map = disabled
	// (backward compatible). ModelAliasesStrict, when true, returns
	// HTTP 400 for a model that matches no alias and no exact provider
	// model; false (default) passes unknown models through unchanged.
	ModelAliases       map[string]string // NEXUS_MODEL_ALIASES JSON
	ModelAliasesStrict bool              // NEXUS_MODEL_ALIASES_STRICT

	// Inbound auth (issue #109). When ProxyAPIKey is non-empty, every
	// non-exempt endpoint requires a matching Bearer token in the
	// Authorization header. /healthz and /metrics stay exempt for K8s
	// probes and Prometheus scrapers. /status is exempt only when
	// StatusPublic is true (default false) — the diagnostics surface
	// (frontier configured, judge enabled, VRAM state) is
	// reconnaissance-grade and should be gated by default.
	// gitleaks:allow // issue #1274: false positive — "APIKey" matches generic-api-key but this is a struct field name, not a secret.
	ProxyAPIKey  string // NEXUS_PROXY_API_KEY; empty disables auth
	StatusPublic bool   // NEXUS_STATUS_PUBLIC; exposes /status without auth

	// APIKeysFile (issue #1154) is the path to a JSON file containing
	// multiple inbound API keys with per-tenant labels. When set, takes
	// precedence over NEXUS_PROXY_API_KEY. Read at boot and on SIGHUP
	// hot-reload so individual keys can be rotated without disrupting
	// other tenants. Format: [{"key":"...","tenant":"team-a"}, ...].
	APIKeysFile string // NEXUS_API_KEYS_FILE; empty means single-key legacy path

	// Pluggable JWT/OIDC inbound authentication (issue #1152).
	// AuthMode selects the authenticator strategy: "static" (default,
	// pre-#1152), "jwt", or "both" (accept static OR JWT).
	// OIDCJWKSURL/Issuer/Audience configure the JWT validator.
	AuthMode        string        // NEXUS_AUTH_MODE (static|jwt|both)
	OIDCJWKSURL     string        // NEXUS_OIDC_JWKS_URL
	OIDCIssuer      string        // NEXUS_OIDC_ISSUER
	OIDCAudience    string        // NEXUS_OIDC_AUDIENCE
	OIDCJWKSRefresh time.Duration // NEXUS_OIDC_JWKS_REFRESH

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
	RAGDBPath       string        // on-disk SQLite database for the RAG store
	RAGPollInterval time.Duration // watcher cadence; 0 disables the watcher

	// RAG recursive indexing (issue #1149). When true, IndexDir and the
	// watcher use filepath.WalkDir to descend into all subdirectories.
	// FewShotExample.Filename stores the relative path from the root
	// (e.g. "internal/handlers/chat.go") to avoid primary-key collisions
	// when multiple directories contain files with the same name.
	RAGRecursive bool // recursive subdirectory indexing; false = flat scan

	// RAG embedding cache (issue #115). Prompt embeddings are
	// deterministic for a given model+text pair, so they are memoized
	// in a bounded LRU with TTL. RAGEmbedCacheSize=0 disables the cache;
	// RAGEmbedCacheTTL=0 disables caching (pass-through) even when size>0.
	RAGEmbedCacheSize        int           // max LRU entries (256)
	RAGEmbedCacheTTL         time.Duration // per-entry TTL (24h default); 0 = pass-through
	RAGEmbedCacheWaitTimeout time.Duration // max time a waiter waits for a concurrent load (5s default); issue #800

	// RAG circuit breaker (issue #222). After RAGCircuitBreakerThreshold
	// consecutive Ollama /api/embeddings failures the breaker trips and
	// enters a cooldown window during which Embed returns ErrCircuitOpen.
	RAGCircuitBreakerThreshold int           // consecutive failures to trip; 0 = disabled
	RAGCircuitBreakerCooldown  time.Duration // cooldown duration after trip

	// RAG batch embedding (issue #771). When > 0, IndexDir batches files
	// in groups of this size and calls EmbedBatch to reduce HTTP round-trips.
	// Set to 0 to disable batching (backward compatible with existing tests).
	RAGBatchSize int

	// RAG chunk token threshold (issue #1168). When > 0, files whose
	// token count exceeds this value are split into overlapping chunks
	// that prefer natural code boundaries (blank lines). Each chunk is
	// embedded and stored as a separate FewShotExample, enabling
	// finer-grained retrieval for large files. Set to 0 to disable
	// chunking (whole-file indexing, backward compatible).
	RAGChunkTokens int

	// RAG top-K retrieval (issue #1166). When > 1, up to K examples above
	// threshold are injected per request, ordered by descending score.
	// Default 1 preserves byte-for-byte backward compatibility.
	RAGTopK int // NEXUS_RAG_TOP_K; 1 = legacy single-example
	// RAGMaxInjectionTokens caps total injected context (issue #1166).
	// Lowest-ranked examples are truncated first. Default 4096.
	RAGMaxInjectionTokens int // NEXUS_RAG_MAX_INJECTION_TOKENS

	// RAG file extension filter (issue #1148). When non-empty, only files
	// whose extension matches one of the listed extensions are indexed.
	// When empty, all files are indexed (backward compatible).
	RAGFileExtensions []string // e.g. [".go", ".py", ".ts"]
	// RAG exclude patterns (issue #1148). Glob patterns matched against
	// the file's base name; matching files are skipped during indexing.
	RAGExcludePatterns []string // e.g. ["*_test.go", "*.gen.go"]

	// RAG semantic deduplication (issue #1243). When > 0 (e.g. 0.95),
	// new chunks whose cosine similarity to an existing chunk exceeds the
	// threshold are skipped at index time. By default dedup is scoped to
	// the same directory; set RAGDedupCrossDir to allow cross-directory
	// dedup. A value of 0 (default) disables dedup entirely (backward
	// compatible).
	RAGDedupThreshold float64 // NEXUS_RAG_DEDUP_THRESHOLD; 0 = disabled
	// RAGDedupCrossDir enables cross-directory deduplication (issue #1243).
	// When false (default), dedup only compares against chunks in the
	// same directory as the new chunk. When true, compares against all
	// indexed chunks regardless of directory.
	RAGDedupCrossDir bool // NEXUS_RAG_DEDUP_CROSS_DIR
	// RAG hybrid retrieval (issue #1242). When > 0, BM25 keyword scores
	// are combined with cosine similarity via Reciprocal Rank Fusion.
	// 0.0 = pure semantic (backward compatible), 1.0 = pure keyword.
	// Values between 0 and 1 blend both signals; 0.5 is a balanced default.
	RAGHybridWeight float64 // NEXUS_RAG_HYBRID_WEIGHT

	// Diag RAG minimum files (issue #1290). Minimum number of files
	// required in NEXUS_EXAMPLES_DIR for the rag_directory diagnostic
	// check to pass. 0 files = fail, 1..(min-1) = warn, ≥ min = pass.
	DiagRAGMinFiles int // NEXUS_DIAG_RAG_MIN_FILES; default 3

	// Routing
	TokenGuardrail                int           // estimated tokens above this force frontier (6000)
	SLMTimeout                    time.Duration // Qwen3-Coder routing timeout (8s)
	SLMCacheMaxEntries            int           // max entries in SLM routing decision cache (512)
	SLMCacheSemanticThreshold     float64       // cosine similarity floor for semantic cache hits (0.0..1.0, issue #245)
	SLMCacheMaxStale              int           // max stale entries before proactive eviction (0 = disabled, issue #835)
	SLMCacheStaleCleanupThreshold int           // Get-triggered eviction threshold; 0 = disabled (issue #1037)
	SLMCacheSemanticScanLimit     int           // max entries scanned in getSemantic; 0 = unlimited (issue #933)
	SLMConfidenceThreshold        float64       // hard escalation threshold: local/fusion decisions below this force frontier (default 0.3, issue #301)
	SLMTokenHint                  bool          // prepend [tokens: ~N] hint to SLM routing prompt (issue #1233)
	RoutingContextTurns           int           // prior conversation turns fed to the router for multi-turn context (default 3, issue #1147)
	RoutingContextChars           int           // char cap on the conversation-context window fed to the router (default 2000, issue #1147)
	FusionTimeout                 time.Duration // per-panel-member fetch timeout (120s), shared fallback
	FusionLocalTimeout            time.Duration // per-panel-member timeout for the local Ollama member (90s, issue #1164)
	FusionFrontierTimeout         time.Duration // per-panel-member timeout for the frontier API member (30s, issue #1164)
	CascadeTimeout                time.Duration // per-attempt timeout for cascade fallback (30s)
	CascadeTimeoutFloor           time.Duration // adaptive floor: minimum per-attempt timeout (5s, issue #1175)
	CascadeTimeoutCeiling         time.Duration // adaptive ceiling: maximum per-attempt timeout (120s, issue #1175)
	CascadeTimeoutPer1kTokens     time.Duration // additive per-1k prompt tokens; <=0 disables adaptive (1500ms, issue #1175)
	ArbiterTimeout                time.Duration // per-call timeout for the fusion arbiter stream (60s)

	// Frontier per-provider failover (issue #1157). When true and more
	// than one frontier provider is registered, the route=frontier
	// dispatch wraps in a frontier-only cascade: on a retryable failure
	// (5xx, timeout, connection reset) the next provider is tried before
	// returning an error to the client.
	FrontierFailover            bool // NEXUS_FRONTIER_FAILOVER (default true when >1 provider)
	FrontierFailoverMaxAttempts int  // NEXUS_FRONTIER_FAILOVER_MAX_ATTEMPTS (default 3)

	// Coalesce (issue #1155): deduplicate identical concurrent
	// non-streaming cascade requests via singleflight so bursty
	// duplicate traffic makes one upstream call instead of N.
	CoalesceEnabled    bool          // NEXUS_COALESCE_ENABLED (default false)
	CoalesceTTL        time.Duration // NEXUS_COALESCE_TTL (default 250ms)
	CoalesceMaxEntries int           // NEXUS_COALESCE_MAX_ENTRIES (default 512)

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
	DSLUnicodePatterns    []*regexp.Regexp // NEXUS_DSL_UNICODE_PATTERNS (issue #422)

	// DSL auto-promotion (issue #1165). The PatternPromoter periodically
	// scans historical SLM decisions and promotes frequently-routed n-gram
	// patterns into the DSL fast-pass, eliminating SLM latency for
	// predictable routing patterns. Setting all three to zero disables
	// auto-promotion (backward compatible).
	DSLPromotionMinSamples int           // NEXUS_DSL_PROMOTION_MIN_SAMPLES (default 20)
	DSLPromotionConfidence float64       // NEXUS_DSL_PROMOTION_CONFIDENCE (default 0.90)
	DSLPromotionInterval   time.Duration // NEXUS_DSL_PROMOTION_INTERVAL (default 1h)

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
	ProviderTailWeight      float64       // P95 blend factor in [0,1]; 0 = P50-only (legacy) (issue #450)
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

	// CostUseOutputTokens (issue #1183) switches the per-request cost
	// estimate from the legacy flat input-only rate to a per-provider
	// input+output token split. When false (default) the estimate is
	// byte-for-byte identical to the pre-issue-#1183 behaviour, so a
	// stock deployment's savings numbers are unaffected. When true the
	// handler looks up the serving provider's InputCostPer1K /
	// OutputCostPer1K via the registry and counts output tokens with
	// the tiktoken tokenizer instead of the bytes/4 heuristic.
	CostUseOutputTokens bool // true => per-provider input/output cost split (issue #1183)

	// AnthropicCacheMinSystemChars (issue #1245) is the minimum
	// system-field character count above which the Anthropic adapter
	// injects cache_control: {type: "ephemeral"} on the system field.
	// Zero or negative disables Anthropic prompt caching hints entirely.
	AnthropicCacheMinSystemChars int

	// AzureContentFilterEnabled (issue #1245) gates the Azure adapter's
	// content_filter finish_reason detection. When true, the adapter
	// rewrites content_filter finish reasons into structured OpenAI
	// error frames.
	AzureContentFilterEnabled bool

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
	// FusionSimilarityMode selects the algorithm used to compute panel-member
	// agreement in fusion (issue #1244). "jaccard" (default) uses lexical
	// token-set overlap; "semantic" uses cosine similarity via the RAG
	// embedder. When "semantic" is selected but the embedder is unavailable,
	// the computation falls back to Jaccard automatically.
	FusionSimilarityMode string // NEXUS_FUSION_SIMILARITY_MODE; default "jaccard"

	// Fusion arbiter synthesis cache (issue #232, #773). When ArbiterCacheTTL > 0,
	// arbiter synthesis responses are cached keyed by SHA-256 of
	// (first.Content, second.Content). Subsequent requests with identical
	// panel-member content return the cached synthesis text instead of
	// invoking the expensive frontier arbiter call. ArbiterCacheMaxEntries
	// caps memory at a fixed entry count with LRU eviction.
	ArbiterCacheTTL        time.Duration // NEXUS_ARBITER_CACHE_TTL; default 5m (0 disables)
	ArbiterCacheMaxEntries int           // NEXUS_ARBITER_CACHE_MAX_ENTRIES; default 512

	// Arbiter cache boot-time pre-warming (issue #1176). When
	// CacheWarmOnBoot is true, the boot sequence queries the SQLite
	// metrics store for recent arbiter syntheses still within the cache
	// TTL and loads them into the arbiter cache, cutting cold-start
	// latency. CacheWarmLimit caps the number of entries queried.
	CacheWarmOnBoot bool // NEXUS_CACHE_WARM_ON_BOOT; default false (opt-in)
	CacheWarmLimit  int  // NEXUS_CACHE_WARM_LIMIT; default 256

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

	// Frontier health poller (issue #1158). Mirrors the Ollama health
	// poller but probes each configured frontier API provider via
	// GET <BaseURL>/models. The ProviderSelector consults the per-
	// provider circuit state to skip providers whose circuit is open
	// so traffic fails over within one poll interval instead of
	// waiting for the 60s error-rate refresh. Set
	// NEXUS_FRONTIER_HEALTH_POLL_INTERVAL to 0 to disable (the selector
	// then falls back to error-rate-based exclusion).
	FrontierHealthPollInterval     time.Duration // background poll cadence (60s)
	FrontierHealthBreakerThreshold int           // consecutive failures before trip (3)
	FrontierHealthTimeout          time.Duration // per-probe HTTP timeout (5s)

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
	// ProbeThermalThreshold is the GPU junction temperature (°C) above
	// which the probe treats free VRAM as 0 and forces the static
	// guardrail (issue #597). 0 disables the thermal check.
	ProbeThermalThreshold int // GPU temp (°C) threshold; default 90, 0 disables
	// ProbeNVIDIAInterval is the cadence of the periodic NVIDIA
	// free-VRAM refresh (issue #1178). On NVIDIA-only hosts the AMD
	// sysfs path returns nothing, so without this refresh the
	// budget's FreeVRAMBytes — read on every Acquire by the
	// concurrency limiter — would stay frozen at boot for the whole
	// process lifetime. Zero (default) keeps the boot-only behaviour
	// (nvidia-smi is invoked once at boot and by `nexus check`).
	ProbeNVIDIAInterval time.Duration // periodic nvidia-smi refresh; 0 disables

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

	// Judge (async LLM-as-a-judge evaluator). All zero/empty values
	// disable the judge; the chat handler is unaffected when the
	// evaluator is wired to a no-op observer (see cmd/nexus/main.go).
	JudgeEnabled            bool          // true iff at least one judge parameter is non-zero
	JudgeURL                string        // frontier endpoint for judge calls
	JudgeModel              string        // judge model name (e.g. "gpt-4o")
	JudgeAPIKey             string        // bearer token; may equal FrontierKey
	JudgeSampleRate         float64       // 0..1; <=0 disables sampling
	JudgeFrontierSampleRate float64       // 0..1; fraction of frontier completions to judge (default 0.02)
	JudgeConcurrency        int           // max parallel judge calls (default 2)
	JudgeQueueDepth         int           // buffered channel size (default 64)
	JudgeTimeout            time.Duration // per-call judge timeout (default 30s)
	JudgeCostPer1KUSD       float64       // rough USD/1k-token rate for cost estimates
	JudgeDBPath             string        // on-disk SQLite database for judge scores; empty disables Detected
	JudgeAdaptiveEnabled    bool          // enable adaptive sampling based on rolling avg of recent scores (issue #1232)
	JudgeAdaptiveWindow     time.Duration // look-back period for rolling average of recent scores (issue #1301)
	JudgeAdaptiveHighConf   float64       // high confidence threshold — decay to min rate when avg > this (issue #1301)
	JudgeAdaptiveLowConf    float64       // low confidence threshold — increase to max rate when avg < this (issue #1301)
	// edits enqueue a background `cargo check` / `npx tsc` and the
	// verdict (1 = clean, 0 = fail/timeout) is reported via a
	// callback to cmd/nexus/main.go. QualityEnabled is true iff
	// QualityConcurrency is positive; the chat handler treats a
	// nil observer as "skip me" so the hot path is unaffected when
	// the verifier is dormant.
	QualityEnabled         bool          // true iff QualityConcurrency > 0
	QualityConcurrency     int           // max parallel verifier workers (default 2)
	QualityQueueDepth      int           // buffered channel size (default 64)
	QualityTimeout         time.Duration // per-check timeout (default 60s)
	QualityStderrCap       int           // stderr bytes retained per verdict (default 2 KiB)
	QualityDroppedRingSize int           // ring buffer capacity for dropped events (default 256)

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

	// InjectionScanRoles is the set of OpenAI message roles scanned for
	// prompt-injection override attempts in warn/strict mode (issue #481).
	// Defaults to ["system"] so a stock deployment is byte-for-byte
	// identical to the pre-#481 behaviour. Operators who also want
	// user-turn messages scanned (e.g. "ignore previous instructions"
	// appearing in a user message) set NEXUS_INJECTION_SCAN_ROLES=system,user.
	// Only "system" and "user" are honoured; anything else falls back to
	// ["system"]. Not hot-reloadable — the role set is read once at boot.
	InjectionScanRoles []string

	// Telemetry
	//
	// TelemetryPath is the on-disk JSON-lines log written by the
	// background telemetry goroutine. An empty value disables recording
	// (the handler installs a Noop recorder). Parent directories are
	// created on demand.
	//
	// TelemetryMaxBytes (issue #485) enables size-based rotation of the
	// JSONL file. When > 0, the active file is atomically renamed with a
	// timestamp suffix the moment the next record would push it past the
	// cap, and a fresh file is opened. 0 (default) preserves the
	// append-only, never-rotate behaviour. Not hot-reloadable — the file
	// handle must be swapped atomically at boot.
	//
	// TelemetryMaxFiles bounds the number of rotated files retained once
	// the cap is exceeded; the oldest is evicted. Only consulted when
	// TelemetryMaxBytes > 0. Not hot-reloadable.
	//
	// TelemetryBufferSize (issue #681) is the write buffer threshold in
	// bytes. Records are batched in memory and flushed to disk when the
	// buffer reaches this size. Defaults to 64 KiB. A single record
	// larger than this value triggers an immediate flush.
	//
	// TelemetryFlushInterval (issue #681) is the maximum time between
	// flushes. A background tick fires at this interval and flushes any
	// buffered records. Defaults to 5s. Together with TelemetryBufferSize
	// this amortises disk I/O over many records rather than writing each
	// record individually.
	//
	// MetricsDBPath is the on-disk SQLite database written by
	// internal/metrics (issue #4). An empty value disables the
	// metrics store (the handler treats a nil store as "skip me").
	// Parent directories are created on demand. The default lives
	// under the user's XDG-style cache directory so multiple checkouts
	// don't trample each other.
	TelemetryPath          string
	TelemetryMaxBytes      int
	TelemetryMaxFiles      int
	TelemetryBufferSize    int
	TelemetryFlushInterval time.Duration
	MetricsDBPath          string

	// MetricsRetentionDays is the TTL for the metrics requests table
	// (issue #483). When > 0, a background goroutine DELETEs rows whose
	// timestamp is older than this many days, waking roughly once per
	// hour. 0 (default) disables retention entirely — the table grows
	// without bound, matching pre-#483 behaviour. Not hot-reloadable:
	// the prune goroutine lifecycle is bound to the store's lifetime.
	MetricsRetentionDays int

	// MetricsBatchSize is the number of records that trigger a batched
	// SQLite transaction in the metrics store drain goroutine (issue #1234).
	// When the buffer reaches this size, the drain commits a
	// BEGIN...INSERT...COMMIT transaction. Default 64. BATCH_SIZE=1
	// reproduces the pre-batch per-record INSERT behaviour.
	MetricsBatchSize int

	// MetricsBatchTimeout is the maximum delay before a partial batch
	// is flushed (issue #1234). If the buffer has at least one record
	// and this duration elapses since the last flush, the drain commits
	// whatever is in the batch. Default 100ms. Together with
	// MetricsBatchSize this amortises WAL write amplification under load.
	MetricsBatchTimeout time.Duration

	// OTLP retry/back-off parameters (issue #803). These tune the
	// behaviour when the collector returns 5xx errors. The back-off
	// follows exponential growth: base * 2^(attempt-1) capped at max.
	//
	// TracerMaxRetries: maximum retry attempts after the initial POST
	// fails with a 5xx. Default 3 (total 4 attempts including initial).
	// Zero or negative falls back to the default.
	//
	// TracerRetryBaseDelay: initial back-off delay. Default 100ms.
	// Zero or negative falls back to the default.
	//
	// TracerRetryMaxDelay: ceiling on the back-off delay. Default 2s.
	// Zero or negative falls back to the default.
	TracerMaxRetries     int
	TracerRetryBaseDelay time.Duration
	TracerRetryMaxDelay  time.Duration

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

	// Debug pprof + expvar endpoints (issue #1150). When
	// DebugPprofEnabled is true, the server registers /debug/pprof/*
	// and /debug/vars on the unprotected mux. DebugPprofAPIKey gates
	// access: when set, a matching Bearer token is required (401
	// otherwise); when empty, only loopback peers are allowed (403
	// for non-loopback). Default false so a stock deployment has no
	// debug surface exposed.
	DebugPprofEnabled bool
	DebugPprofAPIKey  string

	// OpenAI-compatible model discovery (issue #78). When enabled the
	// proxy serves GET /v1/models and GET /v1/models/{id} listing the
	// configured local, router, and frontier models, plus any models
	// Ollama reports via /api/tags. The Ollama poll is cached for
	// ModelsCacheTTL; set to zero or negative to serve only the
	// configured models (no HTTP round-trip to Ollama per request).
	ModelsEndpointEnabled bool
	ModelsCacheTTL        time.Duration

	// Built-in web dashboard (issue #1182). When DashboardEndpointEnabled
	// is true the proxy serves GET <DashboardEndpoint> — a self-contained
	// HTML page (inline CSS/JS, no external assets) rendering savings and
	// routing metrics from the SQLite metrics store. Disabled by default
	// so a stock deployment exposes no extra surface. DashboardPublic
	// mirrors StatusPublic: when true the route bypasses inbound auth
	// (handy for an operator-only LAN). When false (default) the route is
	// gated by NEXUS_PROXY_API_KEY exactly like /status.
	DashboardEndpointEnabled bool
	DashboardEndpoint        string
	DashboardPublic          bool

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
	// the raw source value (env or YAML) and is surfaced in the
	// rate_limit_proxy_config diagnostic check so operators can see
	// the exact CIDR list `nexus check` evaluated.
	//
	// RateLimitRPM is the per-client request ceiling in requests per
	// minute; zero or negative disables rate limiting entirely so a
	// stock deployment is byte-for-byte identical to the pre-#75 path.
	// RateLimitBurst is the token-bucket capacity (max burst before
	// throttling); <=0 falls back to RateLimitRPM in the limiter.
	TrustedProxies    []*net.IPNet
	TrustedProxiesRaw string

	// AllowCIDRs is the inbound IP allowlist sourced from NEXUS_ALLOW_CIDRS.
	// When non-empty, only clients whose IP falls within at least one CIDR
	// are permitted; all others receive HTTP 403. When empty (the default),
	// the allowlist is disabled and all IPs are permitted. This provides
	// network-layer access control for local-only / air-gapped deployments
	// (issue #1240). AllowCIDRsRaw preserves the raw source value for
	// diagnostic display. AllowCIDRsStrict controls whether /healthz and
	// /metrics are exempt from the allowlist (default: not strict).
	AllowCIDRs       []*net.IPNet
	AllowCIDRsRaw    string
	AllowCIDRsStrict bool // when true, no path is exempt from the allowlist

	RateLimitRPM      int
	RateLimitBurst    int
	RateLimitByAPIKey bool // issue #776: bucket on SHA256(IP + ":" + APIKey) when true

	// MaxResponseBytes caps upstream response bodies read into memory
	// (issue #365). A malicious or misbehaving upstream returning
	// gigabytes could otherwise cause uncontrolled memory growth.
	// Zero or negative falls back to DefaultMaxResponseBytes.
	MaxResponseBytes int

	// CascadeMaxResponseBytes caps cascade response bodies. Default 64 MiB.
	// Independent from MaxResponseBytes so operators can tune cascade
	// bounds separately (issue #742). Zero or negative falls back to
	// DefaultMaxResponseBytes.
	CascadeMaxResponseBytes int

	// PoolBufferMaxBytes is the maximum capacity a pooled *bytes.Buffer
	// may retain to be returned to the sync.Pool (issue #1177). Buffers
	// that grew beyond this are discarded so a single huge response never
	// pins pool memory. Default 1 MiB. Zero or negative disables pooling.
	PoolBufferMaxBytes int

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

	// Init wizard profile (issue #1156). Selects the routing-knob
	// preset used by `nexus init --non-interactive`. Recognised values
	// are "local-first", "frontier-default", "fusion-balanced". Empty
	// falls back to "fusion-balanced" at the wizard layer; the proxy
	// server itself never reads this value, so it is not validated
	// during boot — it only steers the generated config file.
	InitProfile string

	// Tracing (issue #787). OTLP/JSON exporter wired via NewExporter +
	// RegisterExporter in main.go so spans are actually submitted to the
	// configured collector. All zero/empty values disable tracing.
	// Endpoint is the full OTLP HTTP URL including /v1/traces path.
	// Timeout bounds each POST (default 10s). QueueSize is the buffered
	// channel capacity (default 256). SampleRate is a [0,1] probability
	// that determines which traces are recorded; 0=never, 1=always,
	// and values between use a deterministic probability sampler.
	TracingEndpoint   string
	TracingTimeout    time.Duration
	TracingQueueSize  int
	TracingBatchSize  int
	TracingSampleRate float64

	// LogTraceID (issue #1169). When true and tracing is enabled, the
	// slog default logger is wrapped so every log record inside a
	// traced request's context carries trace_id and span_id attributes.
	// Default true so operators get log-to-trace correlation by default.
	LogTraceID bool

	// MetricsExemplars controls whether histogram buckets carry OTLP
	// trace exemplars in the Prometheus exposition (issue #1171).
	MetricsExemplars bool

	// OtelMetrics (issue #1238). OTLP/JSON metrics exporter.
	// Endpoint is the full OTLP HTTP URL including /v1/metrics path.
	// Empty disables metrics export entirely (zero overhead).
	// Interval is the periodic push cadence (default 60s). Timeout
	// bounds each POST (default 10s). Shares the same retry/back-off
	// tunables as tracing (NEXUS_TRACING_MAX_RETRIES etc.) for
	// consistency.
	OtelMetricsEndpoint string
	OtelMetricsInterval time.Duration
	OtelMetricsTimeout  time.Duration

	// Response-content redaction (issue #1172).
	RedactEnabled     bool
	RedactProfile     string
	RedactPatternsRaw string
	RedactBufferBytes int

	// SSRF egress guard (issue #1174).
	EgressGuardEnabled bool
	EgressAllowCIDRs   string // raw comma-separated CIDR string from env/YAML

	// Secret management (issue #1173).
	SecretBackend string
	VaultAddr     string
	VaultToken    string
	VaultRole     string
	VaultPath     string
	AWSSMPrefix   string
	SecretRefresh time.Duration

	// Tamper-evident audit log (issue #1153). When AuditEnabled is true
	// and AuditPath is non-empty, every /v1/chat/completions request
	// appends one hash-chained JSON line capturing who routed what, to
	// where, and when. AuditPath empty disables the file backend.
	// AuditSync is "full" (fsync after each append, default) or "none"
	// (rely on OS page cache). When disabled, behaviour is byte-for-byte
	// identical to pre-issue-#1153.
	AuditEnabled bool
	AuditPath    string
	AuditSync    string
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

// DefaultConfigFilePath returns the default YAML config file path:
// $XDG_CONFIG_HOME/nexus-proxy/config.yaml if XDG_CONFIG_HOME is set,
// otherwise ./config.yaml in the current working directory.
// Operators can override this via NEXUS_CONFIG_FILE.
func DefaultConfigFilePath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "nexus-proxy", "config.yaml")
	}
	return "./config.yaml"
}

// configFilePath returns the effective config file path.
// NEXUS_CONFIG_FILE takes precedence if set and non-empty;
// otherwise DefaultConfigFilePath() is used.
func configFilePath() string {
	if f := os.Getenv("NEXUS_CONFIG_FILE"); f != "" {
		return f
	}
	return DefaultConfigFilePath()
}

// Load reads configuration from environment variables and optionally from a
// YAML config file, applying defaults suitable for local development.
// The config file path is determined by NEXUS_CONFIG_FILE if set,
// otherwise $XDG_CONFIG_HOME/nexus-proxy/config.yaml, or ./config.yaml
// as a fallback. Env vars always take precedence over file values.
// It returns an error only when a required value is malformed; missing
// optional values fall back to defaults.
func Load() (Config, error) {
	// First pass: load from config file if present.
	filePath := configFilePath()
	fileCfg, err := LoadFile(filePath)
	if err != nil {
		return Config{}, err
	}

	// Second pass: build config, starting from file values as baseline
	// so env vars (applied via the getEnv* helpers) can override them.
	cfg := Config{}

	// Helper to get a string from file config, falling back to env var
	// or default. This lets the rest of Load() use the standard getEnv*
	// pattern while still honouring file values as a lower-precedence base.
	getFileString := func(key, envKey, def string) string {
		// Mirrors getEnv: env wins only when non-empty (empty env == unset).
		if v, ok := os.LookupEnv(envKey); ok && v != "" {
			return v
		}
		// Env is unset or empty; use file value if non-empty, else default.
		if fileCfg != nil {
			if v, ok := fileCfg[key]; ok && v != "" {
				return v
			}
		}
		return def
	}

	getFileBool := func(key, envKey string, def bool) bool {
		if fileCfg != nil {
			if v, ok := fileCfg[key]; ok {
				// YAML bools are parsed as strings by our LoadFile
				// (yaml.v3 unmarshals bools to bool, but our LoadFile
				// serialises them to "true"/"false" strings).
				switch strings.ToLower(v) {
				case "true", "1", "yes":
					return true
				case "false", "0", "no":
					return false
				}
			}
		}
		return getEnvBool(envKey, def)
	}

	// Apply file-baseline + env-override for each top-level config field.
	// The getFile* helpers prefer env (via getEnv*) when the env var is
	// explicitly set, so existing deployments with env vars are unaffected.

	cfg.Addr = getFileString("addr", "NEXUS_ADDR", ":8000")
	cfg.OllamaURL = strings.TrimRight(getFileString("ollama_url", "NEXUS_OLLAMA_URL", "http://localhost:11434"), "/")
	cfg.RouterModel = getFileString("router_model", "NEXUS_ROUTER_MODEL", "qwen3-coder:4b")
	cfg.LocalModel = getFileString("local_model", "NEXUS_LOCAL_MODEL", "qwen3-coder:8b")
	cfg.EmbeddingModel = getFileString("embedding_model", "NEXUS_EMBEDDING_MODEL", "nomic-embed-text")
	cfg.FrontierURL = getFileString("frontier_url", "NEXUS_FRONTIER_URL", "https://api.openai.com/v1/chat/completions")
	cfg.FrontierModel = getFileString("frontier_model", "NEXUS_FRONTIER_MODEL", "gpt-4o")
	// gitleaks:allow // issue #1274: false positive — "API_KEY" matches generic-api-key but this is an env var name, not a secret.
	cfg.FrontierKey = getEnv("NEXUS_FRONTIER_API_KEY", "") // secrets via env only
	cfg.ZAIURL = getFileString("zai_url", "NEXUS_ZAI_URL", "https://api.z.ai/v1/chat/completions")
	cfg.ZAIModel = getFileString("zai_model", "NEXUS_ZAI_MODEL", "glm-4.6")
	// gitleaks:allow // issue #1274: false positive — "API_KEY" matches generic-api-key but this is an env var name, not a secret.
	cfg.ZAIKey = getEnv("NEXUS_ZAI_API_KEY", "") // secrets via env only
	// gitleaks:allow // issue #1274: false positive — "API_KEY" matches generic-api-key but this is an env var name, not a secret.
	cfg.ProxyAPIKey = getEnv("NEXUS_PROXY_API_KEY", "") // secrets via env only

	// Secret-manager backend configuration (issue #1173). These are always
	// read from env — they configure the resolver itself, not secrets.
	cfg.SecretBackend = getEnv("NEXUS_SECRET_BACKEND", "env")
	cfg.VaultAddr = getEnv("NEXUS_VAULT_ADDR", "")
	cfg.VaultToken = getEnv("NEXUS_VAULT_TOKEN", "")
	cfg.VaultRole = getEnv("NEXUS_VAULT_ROLE", "")
	cfg.VaultPath = getEnv("NEXUS_VAULT_PATH", "secret")
	cfg.AWSSMPrefix = getEnv("NEXUS_AWSSM_PREFIX", "")
	secretRefresh, err := getEnvDuration("NEXUS_SECRET_REFRESH", 0)
	if err != nil {
		return cfg, fmt.Errorf("config: NEXUS_SECRET_REFRESH: %w", err)
	}
	cfg.SecretRefresh = secretRefresh
	cfg.StatusPublic = getFileBool("status_public", "NEXUS_STATUS_PUBLIC", false)

	// Multi-key inbound auth (issue #1154). NEXUS_API_KEYS_FILE takes
	// precedence over NEXUS_PROXY_API_KEY when set. The file is parsed
	// by the auth package at boot and on SIGHUP hot-reload.
	cfg.APIKeysFile = getEnv("NEXUS_API_KEYS_FILE", "")
	cfg.AuthMode = getFileString("auth_mode", "NEXUS_AUTH_MODE", "static")
	cfg.OIDCJWKSURL = getFileString("oidc_jwks_url", "NEXUS_OIDC_JWKS_URL", "")
	cfg.OIDCIssuer = getFileString("oidc_issuer", "NEXUS_OIDC_ISSUER", "")
	cfg.OIDCAudience = getFileString("oidc_audience", "NEXUS_OIDC_AUDIENCE", "")
	{
		d, _ := getEnvDuration("NEXUS_OIDC_JWKS_REFRESH", 15*time.Minute)
		cfg.OIDCJWKSRefresh = d
	}
	cfg.ExamplesDir = getFileString("examples_dir", "NEXUS_EXAMPLES_DIR", "./few_shot_examples")
	cfg.MetaPrompt = defaultMetaPrompt
	cfg.TOONNotice = defaultTOONNotice
	cfg.TOONUnfenced = getFileBool("toon_unfenced", "NEXUS_TOON_UNFENCED", true)
	cfg.MiddlewareChain = getFileString("middleware_chain", "NEXUS_MIDDLEWARE_CHAIN", "")
	// TelemetryPath: getEnvAllowEmpty semantics for env (empty = disabled),
	// then file, then default.
	if v, ok := os.LookupEnv("NEXUS_TELEMETRY_PATH"); ok {
		cfg.TelemetryPath = v
	} else if fileCfg != nil {
		if v, ok := fileCfg["telemetry_path"]; ok {
			cfg.TelemetryPath = v
		} else {
			cfg.TelemetryPath = "./nexus-telemetry.jsonl"
		}
	} else {
		cfg.TelemetryPath = "./nexus-telemetry.jsonl"
	}
	// Size-based rotation knobs (issue #485). MAX_BYTES=0 disables
	// rotation entirely (the default, preserving pre-#485 behaviour).
	// MAX_FILES clamps the rotated-file retention and is only consulted
	// when MAX_BYTES > 0. Both require a restart to take effect because
	// swapping the file handle mid-stream is unsafe.
	telemetryMaxBytes, err := getEnvInt("NEXUS_TELEMETRY_MAX_BYTES", 0)
	if err != nil {
		return cfg, err
	}
	if telemetryMaxBytes < 0 {
		telemetryMaxBytes = 0
	}
	cfg.TelemetryMaxBytes = telemetryMaxBytes

	telemetryMaxFiles, err := getEnvInt("NEXUS_TELEMETRY_MAX_FILES", 5)
	if err != nil {
		return cfg, err
	}
	if telemetryMaxFiles < 1 {
		telemetryMaxFiles = 1
	}
	cfg.TelemetryMaxFiles = telemetryMaxFiles

	// Telemetry buffering (issue #681). BUFFER_SIZE defaults to 64 KiB
	// and FLUSH_INTERVAL to 5s. A single record larger than the buffer
	// triggers an immediate flush. Both require a restart to take effect.
	telemetryBufferSize, err := getEnvInt("NEXUS_TELEMETRY_BUFFER_SIZE", 64<<10)
	if err != nil {
		return cfg, err
	}
	if telemetryBufferSize <= 0 {
		telemetryBufferSize = 64 << 10
	}
	cfg.TelemetryBufferSize = telemetryBufferSize

	telemetryFlushInterval, err := getEnvDuration("NEXUS_TELEMETRY_FLUSH_INTERVAL", 5*time.Second)
	if err != nil {
		return cfg, err
	}
	if telemetryFlushInterval <= 0 {
		telemetryFlushInterval = 5 * time.Second
	}
	cfg.TelemetryFlushInterval = telemetryFlushInterval

	cfg.MetricsDBPath = getFileString("metrics_db", "NEXUS_METRICS_DB", DefaultMetricsDBPath())

	retentionDays, err := getEnvInt("NEXUS_METRICS_RETENTION_DAYS", 0)
	if err != nil {
		return cfg, err
	}
	if retentionDays < 0 {
		retentionDays = 0
	}
	cfg.MetricsRetentionDays = retentionDays

	// Metrics batch config (issue #1234). BatchSize defaults to 64;
	// BatchTimeout defaults to 100ms. Both also apply to the judge
	// SQLite store.
	metricsBatchSize, err := getEnvInt("NEXUS_METRICS_BATCH_SIZE", 64)
	if err != nil {
		return cfg, err
	}
	if metricsBatchSize < 1 {
		metricsBatchSize = 1
	}
	cfg.MetricsBatchSize = metricsBatchSize

	metricsBatchTimeout, err := getEnvDuration("NEXUS_METRICS_BATCH_TIMEOUT", 100*time.Millisecond)
	if err != nil {
		return cfg, err
	}
	if metricsBatchTimeout <= 0 {
		metricsBatchTimeout = 100 * time.Millisecond
	}
	cfg.MetricsBatchTimeout = metricsBatchTimeout

	// OTLP retry/back-off parameters (issue #803).
	tracerMaxRetries, err := getEnvInt("NEXUS_TRACING_MAX_RETRIES", 0)
	if err != nil {
		return cfg, err
	}
	if tracerMaxRetries < 0 {
		tracerMaxRetries = 0
	}
	cfg.TracerMaxRetries = tracerMaxRetries

	tracerRetryBaseDelay, err := getEnvDuration("NEXUS_TRACING_RETRY_BASE_DELAY", 0)
	if err != nil {
		return cfg, err
	}
	if tracerRetryBaseDelay < 0 {
		tracerRetryBaseDelay = 0
	}
	cfg.TracerRetryBaseDelay = tracerRetryBaseDelay

	tracerRetryMaxDelay, err := getEnvDuration("NEXUS_TRACING_RETRY_MAX_DELAY", 0)
	if err != nil {
		return cfg, err
	}
	if tracerRetryMaxDelay < 0 {
		tracerRetryMaxDelay = 0
	}
	cfg.TracerRetryMaxDelay = tracerRetryMaxDelay

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

	// RAG recursive subdirectory indexing (issue #1149). Default false
	// for backward compatibility.
	cfg.RAGRecursive = getFileBool("rag_recursive", "NEXUS_RAG_RECURSIVE", false)

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

	// RAG embed cache waiter timeout (issue #800). When multiple goroutines
	// request the same key concurrently, waiters block on the in-flight
	// inner.Embed call. If the inner call takes too long or the waiting
	// goroutine's context is cancelled, the waiter gives up after this
	// timeout and falls through to a direct inner call. A value of 0
	// disables the timeout (waiters wait indefinitely — pre-issue-#800
	// behaviour). Default 5s is long enough to benefit from coalescing
	// without excessive latency on cache misses.
	waitTimeout, err := getEnvDuration("NEXUS_RAG_EMBED_CACHE_WAIT_TIMEOUT", 5*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.RAGEmbedCacheWaitTimeout = waitTimeout

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

	ragBatchSize, err := getEnvInt("NEXUS_RAG_BATCH_SIZE", 32)
	if err != nil {
		return cfg, err
	}
	if ragBatchSize < 0 {
		ragBatchSize = 0
	}
	cfg.RAGBatchSize = ragBatchSize

	ragChunkTokens, err := getEnvInt("NEXUS_RAG_CHUNK_TOKENS", 0)
	if err != nil {
		return cfg, err
	}
	if ragChunkTokens < 0 {
		ragChunkTokens = 0
	}
	cfg.RAGChunkTokens = ragChunkTokens

	// RAG top-K retrieval (issue #1166). Default 1 = legacy single-example.
	ragTopK, err := getEnvInt("NEXUS_RAG_TOP_K", 1)
	if err != nil {
		return cfg, err
	}
	if ragTopK < 1 {
		ragTopK = 1
	}
	cfg.RAGTopK = ragTopK

	// RAG injection token cap (issue #1166). Lowest-ranked examples are
	// truncated first when total tokens exceed this budget.
	ragMaxInjTokens, err := getEnvInt("NEXUS_RAG_MAX_INJECTION_TOKENS", 4096)
	if err != nil {
		return cfg, err
	}
	if ragMaxInjTokens < 1 {
		ragMaxInjTokens = 4096
	}
	cfg.RAGMaxInjectionTokens = ragMaxInjTokens

	// RAG file extension filter (issue #1148). Comma-separated list of
	// extensions (e.g. ".go,.py,.ts"). When empty, all files are indexed.
	cfg.RAGFileExtensions = ragpkg.ParseCommaSeparated(getEnvAllowEmpty("NEXUS_RAG_FILE_EXTENSIONS", ""))
	// RAG exclude patterns (issue #1148). Comma-separated glob patterns
	// (e.g. "*_test.go,*.gen.go"). Matching files are skipped.
	cfg.RAGExcludePatterns = ragpkg.ParseCommaSeparated(getEnvAllowEmpty("NEXUS_RAG_EXCLUDE_PATTERNS", ""))

	// RAG semantic deduplication threshold (issue #1243). Cosine similarity
	// above which a new chunk is suppressed in favor of an existing one.
	// 0 (default) disables dedup entirely.
	dedupThreshold, err := getEnvFloat("NEXUS_RAG_DEDUP_THRESHOLD", 0)
	if err != nil {
		return cfg, err
	}
	if dedupThreshold < 0 {
		dedupThreshold = 0
	}
	if dedupThreshold > 1 {
		dedupThreshold = 1
	}
	cfg.RAGDedupThreshold = dedupThreshold
	// RAG cross-directory dedup (issue #1243). When false (default), dedup
	// only compares against chunks in the same directory. When true,
	// compares against all indexed chunks regardless of directory.
	cfg.RAGDedupCrossDir = getEnvBool("NEXUS_RAG_DEDUP_CROSS_DIR", false)
	// RAG hybrid BM25 + semantic retrieval (issue #1242). 0.0 = pure semantic,
	// 1.0 = pure keyword. Default 0.0 preserves byte-for-byte backward compatibility.
	hybridWeight, err := getEnvFloat("NEXUS_RAG_HYBRID_WEIGHT", 0.0)
	if err != nil {
		return cfg, fmt.Errorf("NEXUS_RAG_HYBRID_WEIGHT: %w", err)
	}
	cfg.RAGHybridWeight = hybridWeight

	diagRAGMinFiles, err := getEnvInt("NEXUS_DIAG_RAG_MIN_FILES", 3)
	if err != nil {
		return cfg, fmt.Errorf("NEXUS_DIAG_RAG_MIN_FILES: %w", err)
	}
	cfg.DiagRAGMinFiles = diagRAGMinFiles

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

	// SLM token hint (issue #1233). When true, prepends [tokens: ~N]
	// to the routingText passed to the SLM so it can make better-informed
	// routing decisions for medium-length prompts. Does not affect the
	// guardrail or any other stage. Default true.
	cfg.SLMTokenHint = getEnvBool("NEXUS_SLM_TOKEN_HINT", true)

	// Conversation-context window for routing (issue #1147). The handler
	// assembles a bounded summary of prior turns so the DSL fast-pass and
	// SLM see the conversational thread — preventing misrouting of terse
	// follow-ups like "fix it". Turns=0 disables injection entirely
	// (byte-for-byte identical to pre-#1147 behaviour). Turns is capped at
	// 10 by BuildConversationContext; chars bounds the SLM payload size.
	routingContextTurns, err := getEnvInt("NEXUS_ROUTING_CONTEXT_TURNS", 3)
	if err != nil {
		return cfg, err
	}
	cfg.RoutingContextTurns = routingContextTurns

	routingContextChars, err := getEnvInt("NEXUS_ROUTING_CONTEXT_CHARS", 2000)
	if err != nil {
		return cfg, err
	}
	cfg.RoutingContextChars = routingContextChars

	fusionTimeout, err := getEnvDuration("NEXUS_FUSION_TIMEOUT", 120*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.FusionTimeout = fusionTimeout

	// Issue #1164: independent per-member timeouts for fusion panels.
	// When either is unset/zero the code falls back to FusionTimeout so
	// existing deployments see byte-for-byte identical behaviour.
	fusionLocalTimeout, err := getEnvDuration("NEXUS_FUSION_LOCAL_TIMEOUT", 90*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.FusionLocalTimeout = fusionLocalTimeout

	fusionFrontierTimeout, err := getEnvDuration("NEXUS_FUSION_FRONTIER_TIMEOUT", 30*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.FusionFrontierTimeout = fusionFrontierTimeout

	cascadeTimeout, err := getEnvDuration("NEXUS_CASCADE_TIMEOUT", 30*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.CascadeTimeout = cascadeTimeout

	// Adaptive cascade per-attempt timeout (issue #1175). Scales the
	// timeout by prompt token count instead of a single fixed value:
	// clamp(floor + per1k * tokens/1000, floor, ceiling). When
	// PER_1K_TOKENS <= 0 the fixed NEXUS_CASCADE_TIMEOUT is used
	// (backward compatible).
	cascadeFloor, err := getEnvDuration("NEXUS_CASCADE_TIMEOUT_FLOOR", 5*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.CascadeTimeoutFloor = cascadeFloor

	cascadeCeiling, err := getEnvDuration("NEXUS_CASCADE_TIMEOUT_CEILING", 120*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.CascadeTimeoutCeiling = cascadeCeiling

	cascadePer1k, err := getEnvDuration("NEXUS_CASCADE_TIMEOUT_PER_1K_TOKENS", 1500*time.Millisecond)
	if err != nil {
		return cfg, err
	}
	cfg.CascadeTimeoutPer1kTokens = cascadePer1k

	// Frontier per-provider failover (issue #1157). Default true so
	// operators with >1 provider get automatic failover without opt-in.
	cfg.FrontierFailover = getEnvBool("NEXUS_FRONTIER_FAILOVER", true)
	failoverMaxAttempts, err := getEnvInt("NEXUS_FRONTIER_FAILOVER_MAX_ATTEMPTS", 3)
	if err != nil {
		return cfg, err
	}
	if failoverMaxAttempts < 1 {
		failoverMaxAttempts = 1
	}
	cfg.FrontierFailoverMaxAttempts = failoverMaxAttempts

	// Coalesce (issue #1155).
	cfg.CoalesceEnabled = getEnvBool("NEXUS_COALESCE_ENABLED", false)
	coalesceTTL, err := getEnvDuration("NEXUS_COALESCE_TTL", 250*time.Millisecond)
	if err != nil {
		return cfg, err
	}
	cfg.CoalesceTTL = coalesceTTL
	coalesceMaxEntries, err := getEnvInt("NEXUS_COALESCE_MAX_ENTRIES", 512)
	if err != nil {
		return cfg, err
	}
	cfg.CoalesceMaxEntries = coalesceMaxEntries

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

	// DSL Unicode patterns (issue #422). Matches non-ASCII text categories
	// like \p{Han} for Chinese. Empty means no Unicode fast-pass.
	dslUnicode, err := getEnvRegexps("NEXUS_DSL_UNICODE_PATTERNS", "")
	if err != nil {
		return cfg, err
	}
	cfg.DSLUnicodePatterns = dslUnicode

	// DSL auto-promotion (issue #1165). Setting all three to zero disables
	// auto-promotion entirely (backward compatible).
	dslPromoMinSamples, err := getEnvInt("NEXUS_DSL_PROMOTION_MIN_SAMPLES", 20)
	if err != nil {
		return cfg, err
	}
	cfg.DSLPromotionMinSamples = dslPromoMinSamples
	dslPromoConf, err := getEnvFloat("NEXUS_DSL_PROMOTION_CONFIDENCE", 0.90)
	if err != nil {
		return cfg, err
	}
	cfg.DSLPromotionConfidence = dslPromoConf
	dslPromotionInterval, err := getEnvDuration("NEXUS_DSL_PROMOTION_INTERVAL", time.Hour)
	if err != nil {
		return cfg, err
	}
	cfg.DSLPromotionInterval = dslPromotionInterval

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

	// Provider tail weight (issue #450). Blends P95 latency into the
	// selector's effective-latency score so a provider with severe
	// long-tail stalls stops ranking identically to a steady
	// provider with the same median. 0 = legacy P50-only ordering;
	// 1 = full P95 weighting. Values outside [0,1] fail boot
	// rather than being silently clamped so a typo (e.g. "1.5" or
	// "-0.1") surfaces immediately instead of flipping the
	// ranking in subtle ways.
	tailWeight, err := getEnvFloat("NEXUS_PROVIDER_TAIL_WEIGHT", 0.0)
	if err != nil {
		return cfg, err
	}
	if tailWeight < 0 || tailWeight > 1 {
		return cfg, configError("NEXUS_PROVIDER_TAIL_WEIGHT", "must be a number in [0,1]", os.Getenv("NEXUS_PROVIDER_TAIL_WEIGHT"), strconv.FormatFloat(0.0, 'f', -1, 64))
	}
	cfg.ProviderTailWeight = tailWeight

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

	// Per-provider cost model with input/output token split (issue #1183).
	// Defaults to false so the per-request estimate stays byte-for-byte
	// identical to the legacy flat input-only rate. Operators opt in to
	// the richer per-provider model that counts output tokens separately.
	cfg.CostUseOutputTokens = getEnvBool("NEXUS_COST_USE_OUTPUT_TOKENS", false)

	// Anthropic cache-control hint threshold (issue #1245). Default
	// 1024 characters: below this the cache overhead exceeds the
	// savings for most Anthropic models. Zero disables.
	anthropicCacheMinChars, err := getEnvInt("NEXUS_ANTHROPIC_CACHE_MIN_SYSTEM_CHARS", 1024)
	if err != nil {
		return cfg, err
	}
	cfg.AnthropicCacheMinSystemChars = anthropicCacheMinChars

	// Azure content filter detection (issue #1245). Default false
	// so the Azure adapter remains a pure no-op until opted in.
	cfg.AzureContentFilterEnabled = getEnvBool("NEXUS_AZURE_CONTENT_FILTER_ENABLED", false)

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

	// Fusion similarity mode (issue #1244): "jaccard" (default, lexical) or
	// "semantic" (cosine similarity via RAG embedder).
	// Fusion similarity mode (issue #1244). "jaccard" is the default
	// (byte-for-byte backward compatible). "semantic" uses cosine
	// similarity via the RAG embedder, falling back to Jaccard when
	// the embedder is unavailable.
	simMode := strings.ToLower(strings.TrimSpace(
		getEnv("NEXUS_FUSION_SIMILARITY_MODE", "jaccard")))
	switch simMode {
	case "jaccard", "semantic":
		cfg.FusionSimilarityMode = simMode
	default:
		slog.Warn("unknown NEXUS_FUSION_SIMILARITY_MODE value, falling back to jaccard",
			slog.String("value", simMode))
		cfg.FusionSimilarityMode = "jaccard"
	}

	// Fusion arbiter synthesis cache (issue #232, #773). NEXUS_ARBITER_CACHE_TTL=0
	// disables the cache entirely — every disagreement calls the arbiter.
	// When set to a positive duration (default 5m), identical panel-member
	// content within the TTL window returns the cached synthesis text
	// without calling the arbiter.
	arbiterCacheTTL, err := getEnvDuration("NEXUS_ARBITER_CACHE_TTL", 5*time.Minute)
	if err != nil {
		return cfg, err
	}
	cfg.ArbiterCacheTTL = arbiterCacheTTL

	// Arbiter cache max entries (issue #773). Caps memory at ~512 entries
	// with simple LRU eviction when the cap is reached.
	arbiterCacheMax, err := getEnvInt("NEXUS_ARBITER_CACHE_MAX_ENTRIES", 512)
	if err != nil {
		return cfg, err
	}
	if arbiterCacheMax < 0 {
		arbiterCacheMax = 0
	}
	cfg.ArbiterCacheMaxEntries = arbiterCacheMax

	// Arbiter cache boot-time pre-warming (issue #1176). Opt-in: the
	// default is false so boot is byte-for-byte identical to pre-#1176
	// behaviour. When true the boot sequence queries the SQLite metrics
	// store for recent arbiter syntheses within the cache TTL window.
	cfg.CacheWarmOnBoot = getEnvBool("NEXUS_CACHE_WARM_ON_BOOT", false)

	cacheWarmLimit, err := getEnvInt("NEXUS_CACHE_WARM_LIMIT", 256)
	if err != nil {
		return cfg, err
	}
	if cacheWarmLimit < 0 {
		cacheWarmLimit = 0
	}
	cfg.CacheWarmLimit = cacheWarmLimit

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

	// Max stale entries before proactive eviction triggers in getSemantic
	// (issue #835). 0 disables proactive eviction (stale entries accumulate
	// silently until the next Set call); a positive value causes getSemantic
	// to spawn a background eviction goroutine when stale > maxStale.
	slmCacheMaxStale, err := getEnvInt("NEXUS_SLMCACHE_MAX_STALE", 0)
	if err != nil {
		return cfg, err
	}
	if slmCacheMaxStale < 0 {
		slmCacheMaxStale = 0
	}
	cfg.SLMCacheMaxStale = slmCacheMaxStale

	// Stale cleanup threshold for Get-triggered eviction (issue #1037).
	// When staleCleanupThreshold > 0 and StaleEntries() > threshold,
	// Get spawns a background goroutine to evict stale entries. This prevents
	// stale entries from accumulating in read-heavy workloads where Set is not
	// called frequently enough to trigger eviction on write. Default 0 (disabled).
	slmCacheStaleCleanupThreshold, err := getEnvInt("NEXUS_SLMCACHE_STALE_CLEANUP_THRESHOLD", 0)
	if err != nil {
		return cfg, err
	}
	if slmCacheStaleCleanupThreshold < 0 {
		slmCacheStaleCleanupThreshold = 0
	}
	cfg.SLMCacheStaleCleanupThreshold = slmCacheStaleCleanupThreshold

	// Semantic scan limit for SLM cache (issue #933). When maxScanEntries > 0,
	// getSemantic stops scanning after examining maxScanEntries entries. 0 (the
	// default) means unlimited — all entries are scanned.
	slmCacheSemanticScanLimit, err := getEnvInt("NEXUS_SLMCACHE_SEMANTIC_SCAN_LIMIT", 0)
	if err != nil {
		return cfg, err
	}
	if slmCacheSemanticScanLimit < 0 {
		slmCacheSemanticScanLimit = 0
	}
	cfg.SLMCacheSemanticScanLimit = slmCacheSemanticScanLimit

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

	// Frontier health poller (issue #1158). Defaults: 60s poll cadence,
	// 3-failure breaker, 5s per-probe HTTP timeout. Set
	// NEXUS_FRONTIER_HEALTH_POLL_INTERVAL to 0 to disable; the selector
	// then falls back to error-rate-based provider exclusion.
	frontierHealthPoll, err := getEnvDuration("NEXUS_FRONTIER_HEALTH_POLL_INTERVAL", 60*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.FrontierHealthPollInterval = frontierHealthPoll

	frontierHealthBreaker, err := getEnvInt("NEXUS_FRONTIER_HEALTH_BREAKER_THRESHOLD", 3)
	if err != nil {
		return cfg, err
	}
	cfg.FrontierHealthBreakerThreshold = frontierHealthBreaker

	frontierHealthTimeout, err := getEnvDuration("NEXUS_FRONTIER_HEALTH_TIMEOUT", 5*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.FrontierHealthTimeout = frontierHealthTimeout

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

	// Thermal throttle threshold (issue #597). When the GPU junction
	// temperature read from AMD hwmon temp1_input exceeds this value,
	// the probe collapses the VRAM budget to zero so the router falls
	// back to the static guardrail instead of routing heavy prompts to
	// a thermally-clamped GPU. Default 90 °C (typical AMD Radeon
	// throttle ceiling); 0 disables the check.
	probeThermal, err := getEnvInt("NEXUS_PROBE_THERMAL_THRESHOLD", 90)
	if err != nil {
		return cfg, err
	}
	if probeThermal < 0 {
		probeThermal = 0
	}
	cfg.ProbeThermalThreshold = probeThermal

	// Periodic NVIDIA free-VRAM refresh (issue #1178). The default
	// of 0 keeps the boot-only nvidia-smi behaviour; a positive
	// duration arms a background goroutine in the probe Manager that
	// republishes the budget so the VRAM-aware limiter adapts to
	// model-swap and co-tenant VRAM-grab events on NVIDIA-only hosts.
	probeNVIDIAInterval, err := getEnvDuration("NEXUS_PROBE_NVIDIA_INTERVAL", 0)
	if err != nil {
		return cfg, err
	}
	if probeNVIDIAInterval < 0 {
		probeNVIDIAInterval = 0
	}
	cfg.ProbeNVIDIAInterval = probeNVIDIAInterval

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
		return cfg, configError("NEXUS_SERVER_READ_TIMEOUT", "must not be negative", os.Getenv("NEXUS_SERVER_READ_TIMEOUT"), DefaultServerReadTimeout.String())
	}
	cfg.ReadTimeout = readTimeout

	writeTimeout, err := getEnvDuration("NEXUS_SERVER_WRITE_TIMEOUT", DefaultServerWriteTimeout)
	if err != nil {
		return cfg, err
	}
	if writeTimeout < 0 {
		return cfg, configError("NEXUS_SERVER_WRITE_TIMEOUT", "must not be negative", os.Getenv("NEXUS_SERVER_WRITE_TIMEOUT"), DefaultServerWriteTimeout.String())
	}
	cfg.WriteTimeout = writeTimeout

	idleTimeout, err := getEnvDuration("NEXUS_SERVER_IDLE_TIMEOUT", DefaultServerIdleTimeout)
	if err != nil {
		return cfg, err
	}
	if idleTimeout < 0 {
		return cfg, configError("NEXUS_SERVER_IDLE_TIMEOUT", "must not be negative", os.Getenv("NEXUS_SERVER_IDLE_TIMEOUT"), DefaultServerIdleTimeout.String())
	}
	cfg.IdleTimeout = idleTimeout

	maxHeader, err := getEnvInt("NEXUS_SERVER_MAX_HEADER_BYTES", DefaultServerMaxHeaderBytes)
	if err != nil {
		return cfg, err
	}
	if maxHeader < 0 {
		return cfg, configError("NEXUS_SERVER_MAX_HEADER_BYTES", "must not be negative", os.Getenv("NEXUS_SERVER_MAX_HEADER_BYTES"), strconv.Itoa(DefaultServerMaxHeaderBytes))
	}
	cfg.MaxHeaderBytes = maxHeader

	// Effective inbound TLS posture (issue #444). Drives the security-
	// headers middleware HSTS gate. Default false: stock deployments
	// serve plaintext and HSTS would be a spec violation. Set true when
	// the proxy terminates TLS directly OR sits behind a TLS-terminating
	// proxy that strips/rewrites the inner scheme — in both cases the
	// outer hop is HTTPS and HSTS is safe to advertise.
	cfg.TLSEnabled = getEnvBool("NEXUS_TLS_ENABLED", false)

	// Inbound mTLS client certificate verification (issue #1241). When
	// non-empty, the proxy requires and verifies client certificates
	// from downstream agents using the supplied CA certificate file.
	// Bearer-token auth remains active as defense-in-depth.
	cfg.TLSClientCAFile = getEnv("NEXUS_TLS_CLIENT_CA_FILE", "")

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
		return cfg, configError("NEXUS_SHUTDOWN_TIMEOUT", "must not be negative", os.Getenv("NEXUS_SHUTDOWN_TIMEOUT"), DefaultShutdownTimeout.String())
	}
	if shutdownTimeout == 0 {
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

	frontierSampleRate, err := getEnvFloat("NEXUS_JUDGE_FRONTIER_SAMPLE_RATE", 0.02)
	if err != nil {
		return cfg, err
	}
	cfg.JudgeFrontierSampleRate = frontierSampleRate

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

	// Judge adaptive sampling (issue #1232). When enabled, the sample
	// rate is dynamically adjusted based on the rolling average of
	// recent judge scores: decay to 1% if avg > high, increase to 10%
	// if avg < low, else hold at 5%. Windows and thresholds are
	// configurable via NEXUS_JUDGE_ADAPTIVE_WINDOW / _HIGH_CONFIDENCE /
	// _LOW_CONFIDENCE (issue #1301).
	cfg.JudgeAdaptiveEnabled = getEnvBool("NEXUS_JUDGE_ADAPTIVE_ENABLED", false)

	adaptiveWindow, err := getEnvDuration("NEXUS_JUDGE_ADAPTIVE_WINDOW", 30*time.Second)
	if err != nil {
		return cfg, fmt.Errorf("NEXUS_JUDGE_ADAPTIVE_WINDOW: %w", err)
	}
	cfg.JudgeAdaptiveWindow = adaptiveWindow

	highConf, err := getEnvFloat("NEXUS_JUDGE_ADAPTIVE_HIGH_CONFIDENCE", 4.0)
	if err != nil {
		return cfg, fmt.Errorf("NEXUS_JUDGE_ADAPTIVE_HIGH_CONFIDENCE: %w", err)
	}
	cfg.JudgeAdaptiveHighConf = highConf

	lowConf, err := getEnvFloat("NEXUS_JUDGE_ADAPTIVE_LOW_CONFIDENCE", 3.0)
	if err != nil {
		return cfg, fmt.Errorf("NEXUS_JUDGE_ADAPTIVE_LOW_CONFIDENCE: %w", err)
	}
	cfg.JudgeAdaptiveLowConf = lowConf

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

	// Deprecated-key warnings are emitted centrally via the registry
	// (issue #1180). The #924 alias (NEXUS_QUALITY_DROPED_RING_SIZE)
	// is tracked there; value parsing still reads the current name.
	droppedRingSize, err := getEnvInt("NEXUS_QUALITY_DROPPED_RING_SIZE", 256)
	if err != nil {
		return cfg, err
	}
	cfg.QualityDroppedRingSize = droppedRingSize

	cfg.QualityEnabled = cfg.QualityConcurrency > 0

	// Structured logging (issue #3). Defaults match the production
	// expectation: JSON to stderr at info level. Operators flip on
	// debug by setting NEXUS_LOG_LEVEL=debug, and switch to a
	// human-friendly text handler with NEXUS_LOG_FORMAT=text.
	logLevel, logLevelErr := parseLogLevel(os.Getenv("NEXUS_LOG_LEVEL"))
	if logLevelErr != nil {
		slog.Warn("invalid NEXUS_LOG_LEVEL, using info level", slog.String("reason", logLevelErr.Error()))
	}
	cfg.LogLevel = logLevel
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

	// Debug pprof + expvar endpoints (issue #1150). Off by default so
	// production has no debug surface. The API key is read via
	// getEnvAllowEmpty so an operator can explicitly set it to "" to
	// force loopback-only mode.
	cfg.DebugPprofEnabled = parseBoolEnv("NEXUS_DEBUG_PPROF_ENABLED", false)
	cfg.DebugPprofAPIKey = getEnvAllowEmpty("NEXUS_DEBUG_PPROF_API_KEY", "")

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

	// Model aliasing (issue #1184). NEXUS_MODEL_ALIASES is a JSON map
	// of client-requested model names to "providerName/upstreamModel".
	// Empty/unset = disabled (backward compatible). Invalid JSON fails
	// boot so an operator typo does not silently pass models through.
	if raw := os.Getenv("NEXUS_MODEL_ALIASES"); raw != "" {
		var aliases map[string]string
		if err := json.Unmarshal([]byte(raw), &aliases); err != nil {
			return cfg, fmt.Errorf("config: NEXUS_MODEL_ALIASES: %w", err)
		}
		cfg.ModelAliases = aliases
	}
	cfg.ModelAliasesStrict = parseBoolEnv("NEXUS_MODEL_ALIASES_STRICT", false)

	// Built-in web dashboard (issue #1182). Disabled by default so a
	// stock deployment exposes no extra HTTP surface; opt in with
	// NEXUS_DASHBOARD_ENDPOINT=true. The endpoint path defaults to
	// /dashboard. NEXUS_DASHBOARD_PUBLIC mirrors NEXUS_STATUS_PUBLIC:
	// when true the route bypasses the inbound auth gate.
	cfg.DashboardEndpointEnabled = parseBoolEnv("NEXUS_DASHBOARD_ENDPOINT", false)
	cfg.DashboardEndpoint = getEnvAllowEmpty("NEXUS_DASHBOARD_PATH", "/dashboard")
	if cfg.DashboardEndpoint == "" {
		cfg.DashboardEndpoint = "/dashboard"
	}
	cfg.DashboardPublic = parseBoolEnv("NEXUS_DASHBOARD_PUBLIC", false)

	// Prompt-injection hardening (issue #76). Defaults to warn so a
	// stock deployment logs injection attempts out of the box.
	// Operators can set NEXUS_PROMPT_INJECTION_MODE=off to disable,
	// or strict to reject 400. Unknown values fall back to warn.
	cfg.PromptInjectionMode = middleware.ParseInjectionMode(
		os.Getenv("NEXUS_PROMPT_INJECTION_MODE"),
	)
	// Injection scan roles (issue #481). Defaults to "system" so today's
	// system-only scan is byte-for-byte unchanged. Empty / unrecognised
	// values fall back to ["system"]. Not hot-reloadable — read once at boot.
	rawRoles := getEnv("NEXUS_INJECTION_SCAN_ROLES", "system")
	roles, unrecognized := parseInjectionScanRoles(rawRoles)
	cfg.InjectionScanRoles = roles
	// Warn only when unrecognized tokens exist AND the fallback is ["system"].
	// This means the user specified at least one invalid value that caused
	// the parser to discard everything and fall back to the default.
	// Cases like "system,user" (both valid) or "system,foo" (foo invalid,
	// fallback to ["system"]) are distinguished by checking the resulting
	// roles set, not the raw input string (issue #879).
	if len(unrecognized) > 0 && len(roles) == 1 && roles[0] == "system" {
		slog.Warn("unrecognised injection scan role(s): falling back to [system]",
			slog.String("ignored", strings.Join(unrecognized, ",")),
		)
	}
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

	// NEXUS_ALLOW_CIDRS is a comma-separated inbound IP allowlist.
	// When non-empty, only clients whose IP falls within at least one
	// CIDR are permitted; all others receive HTTP 403. This provides
	// network-layer access control for local-only deployments (issue #1240).
	// Invalid CIDRs fail boot with a clear error. A bare IP (no /prefix)
	// is accepted and treated as a /32 or /128.
	cfg.AllowCIDRsRaw = strings.TrimSpace(os.Getenv("NEXUS_ALLOW_CIDRS"))
	if cfg.AllowCIDRsRaw != "" {
		parsed, err := parseTrustedProxies(cfg.AllowCIDRsRaw)
		if err != nil {
			return cfg, fmt.Errorf("config: invalid NEXUS_ALLOW_CIDRS entry %q: %w", cfg.AllowCIDRsRaw, err)
		}
		cfg.AllowCIDRs = parsed
	}

	// NEXUS_ALLOW_CIDRS_STRICT controls whether /healthz and /metrics are
	// exempt from the inbound allowlist. Default (false) means those
	// endpoints are always reachable; true removes the exemption so the
	// allowlist applies to all paths.
	cfg.AllowCIDRsStrict = getEnvBool("NEXUS_ALLOW_CIDRS_STRICT", false)

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

	// RateLimitByAPIKey (issue #776): when true, the bucket key is
	// SHA256(IP + ":" + APIKey) instead of just IP.
	cfg.RateLimitByAPIKey = parseBoolEnv("NEXUS_RATE_LIMIT_BY_API_KEY", false)

	// MaxResponseBytes caps upstream response bodies (issue #365). Default
	// 64 MiB accommodates large frontier completions; operators who proxy
	// very long responses can raise it. Zero or negative falls back to
	// DefaultMaxResponseBytes.
	maxRespBytes, err := getEnvInt("NEXUS_MAX_RESPONSE_BYTES", DefaultMaxResponseBytes)
	if err != nil {
		return cfg, err
	}
	cfg.MaxResponseBytes = maxRespBytes

	// CascadeMaxResponseBytes caps cascade response bodies (issue #742).
	// Independent from MaxResponseBytes so operators can tune cascade bounds
	// separately. Default 64 MiB. Zero or negative falls back to DefaultMaxResponseBytes.
	cascadeMaxRespBytes, err := getEnvInt("NEXUS_CASCADE_MAX_RESPONSE_BYTES", DefaultMaxResponseBytes)
	if err != nil {
		return cfg, err
	}
	cfg.CascadeMaxResponseBytes = cascadeMaxRespBytes

	// PoolBufferMaxBytes caps retained pooled buffer capacity (issue
	// #1177). Default 1 MiB; zero or negative disables pooling entirely.
	poolBufMax, err := getEnvInt("NEXUS_POOL_BUFFER_MAX_BYTES", DefaultPoolBufferMaxBytes)
	if err != nil {
		return cfg, err
	}
	cfg.PoolBufferMaxBytes = poolBufMax

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

	// Init wizard profile (issue #1156). Only consumed by `nexus init`;
	// the server never reads it, so there is no boot-time validation.
	cfg.InitProfile = getEnv("NEXUS_INIT_PROFILE", "")

	// Tracing (issue #787). Endpoint empty disables tracing entirely
	// (NewExporter returns nil, RegisterExporter is never called).
	cfg.TracingEndpoint = getEnvAllowEmpty("NEXUS_TRACING_ENDPOINT", "")

	tracingTimeout, err := getEnvDuration("NEXUS_TRACING_TIMEOUT", 10*time.Second)
	if err != nil {
		return cfg, err
	}
	if tracingTimeout < 0 {
		return cfg, configError("NEXUS_TRACING_TIMEOUT", "must not be negative", os.Getenv("NEXUS_TRACING_TIMEOUT"), DefaultTracingTimeout.String())
	}
	cfg.TracingTimeout = tracingTimeout

	tracingQueueSize := 0
	tracingQueueSize, _ = getEnvInt("NEXUS_TRACING_QUEUE_SIZE", 256)
	if tracingQueueSize < 0 {
		tracingQueueSize = 256
	}
	cfg.TracingQueueSize = tracingQueueSize

	tracingBatchSize := 0
	tracingBatchSize, _ = getEnvInt("NEXUS_TRACING_BATCH_SIZE", 64)
	if tracingBatchSize < 1 {
		tracingBatchSize = 64
	}
	cfg.TracingBatchSize = tracingBatchSize

	tracingSampleRate := 0.0
	tracingSampleRate, _ = getEnvFloat("NEXUS_TRACING_SAMPLE_RATE", 1.0)
	if tracingSampleRate < 0 {
		tracingSampleRate = 0
	}
	if tracingSampleRate > 1 {
		tracingSampleRate = 1
	}
	cfg.TracingSampleRate = tracingSampleRate

	// Log trace ID injection (issue #1169). Default true so operators
	// get log-to-trace correlation by default when tracing is enabled.
	cfg.LogTraceID = getEnvBool("NEXUS_LOG_TRACE_ID", true)

	// Metrics exemplars (issue #1171). Defaults to true when tracing
	// is active (TracingEndpoint set) so operators get exemplars
	// automatically; explicitly false when tracing is off.
	cfg.MetricsExemplars = getEnvBool("NEXUS_METRICS_EXEMPLARS", cfg.TracingEndpoint != "")

	// OtelMetrics (issue #1238). OTLP/JSON metrics exporter. Empty
	// endpoint disables export entirely (zero overhead).
	cfg.OtelMetricsEndpoint = getEnvAllowEmpty("NEXUS_OTEL_METRICS_ENDPOINT", "")

	otelMetricsInterval, err := getEnvDuration("NEXUS_OTEL_METRICS_INTERVAL", 60*time.Second)
	if err != nil {
		return cfg, err
	}
	if otelMetricsInterval <= 0 {
		otelMetricsInterval = 60 * time.Second
	}
	cfg.OtelMetricsInterval = otelMetricsInterval

	otelMetricsTimeout, err := getEnvDuration("NEXUS_OTEL_METRICS_TIMEOUT", 10*time.Second)
	if err != nil {
		return cfg, err
	}
	if otelMetricsTimeout < 0 {
		return cfg, configError("NEXUS_OTEL_METRICS_TIMEOUT", "must not be negative", os.Getenv("NEXUS_OTEL_METRICS_TIMEOUT"), DefaultTracingTimeout.String())
	}
	cfg.OtelMetricsTimeout = otelMetricsTimeout

	// Response-content redaction (issue #1172).
	cfg.RedactEnabled = getEnvBool("NEXUS_REDACT_ENABLED", false)
	cfg.RedactProfile = getEnv("NEXUS_REDACT_PROFILE", RedactProfileDefault)
	cfg.RedactPatternsRaw = getEnvAllowEmpty("NEXUS_REDACT_PATTERNS", "")

	redactBuffer, err := getEnvInt("NEXUS_REDACT_BUFFER_BYTES", DefaultRedactBufferBytes)
	if err != nil {
		return cfg, err
	}
	if redactBuffer < 0 {
		redactBuffer = DefaultRedactBufferBytes
	}
	cfg.RedactBufferBytes = redactBuffer

	// SSRF egress guard (issue #1174). Defaults to enabled=true so a
	// stock deployment is protected out of the box. The allowlist
	// defaults to empty (no override); operators running local Ollama
	// should set NEXUS_EGRESS_ALLOW=127.0.0.0/8 to permit loopback.
	cfg.EgressGuardEnabled = getEnvBool("NEXUS_EGRESS_BLOCK_PRIVATE", true)
	cfg.EgressAllowCIDRs = getEnvAllowEmpty("NEXUS_EGRESS_ALLOW", "")

	// Tamper-evident audit log (issue #1153). Disabled by default; when
	// enabled + path set, every proxied request appends one hash-chained
	// JSON line with client attribution.
	cfg.AuditEnabled = getEnvBool("NEXUS_AUDIT_ENABLED", false)
	cfg.AuditPath = getEnvAllowEmpty("NEXUS_AUDIT_PATH", "")
	cfg.AuditSync = getEnv("NEXUS_AUDIT_SYNC", "full")

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	ValidateShutdownTimeout(cfg)
	ValidateFusionTimeouts(cfg)

	// Emit structured warnings for any deprecated env vars that are set
	// (issue #1180). Advisory only — does not alter parsed values.
	WarnDeprecatedEnv()

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
		return configError("NEXUS_READINESS_MODE", `must be "strict" or "degraded"`, c.ReadinessMode, "degraded")
	}
	switch c.SecretBackend {
	case "", "env", "vault", "awssm":
		// Recognised values.
	default:
		return fmt.Errorf("config: NEXUS_SECRET_BACKEND value %q is not recognised; want \"env\", \"vault\", or \"awssm\"", c.SecretBackend)
	}
	switch c.RedactProfile {
	case "off", "secrets", "pii", "custom":
		// Recognised values.
	default:
		return fmt.Errorf("config: NEXUS_REDACT_PROFILE value %q is not recognised; want \"off\", \"secrets\", \"pii\", or \"custom\"", c.RedactProfile)
	}
	if c.RedactProfile == "custom" && c.RedactPatternsRaw == "" {
		return fmt.Errorf("config: NEXUS_REDACT_PROFILE is \"custom\" but NEXUS_REDACT_PATTERNS is empty; supply comma-separated regex patterns")
	}
	switch c.AuthMode {
	case "static", "jwt", "both":
		// Recognised values.
	default:
		return fmt.Errorf("config: NEXUS_AUTH_MODE value %q is not recognised; want \"static\", \"jwt\", or \"both\"", c.AuthMode)
	}
	return nil
}

// ValidateShutdownTimeout emits a boot warning when the graceful shutdown drain
// window is shorter than the inbound read deadline (issue #121). A drain shorter
// than the read timeout can truncate in-flight request-body uploads mid-read.
// The warning is emitted via slog so the caller must ensure the logger is wired
// (slog is initialised before Load / LoadYAML in main.go). Skipped when
// ReadTimeout is 0 (disabled), since there is no inbound deadline to underrun.
func ValidateShutdownTimeout(cfg Config) {
	if cfg.ReadTimeout > 0 && cfg.ShutdownTimeout < cfg.ReadTimeout {
		slog.Warn("shutdown drain shorter than read timeout: in-flight uploads may be truncated mid-read",
			slog.Duration("shutdown_timeout", cfg.ShutdownTimeout),
			slog.Duration("read_timeout", cfg.ReadTimeout),
			slog.String("hint", "set NEXUS_SHUTDOWN_TIMEOUT >= NEXUS_SERVER_READ_TIMEOUT"),
		)
	}
}

// ValidateFusionTimeouts emits a boot warning when the local panel-member
// timeout is shorter than the frontier timeout (issue #1164). Local Ollama
// models typically need more time than a frontier API, so a configuration
// where local expires first is almost certainly a mistake.
func ValidateFusionTimeouts(cfg Config) {
	if cfg.FusionLocalTimeout > 0 && cfg.FusionFrontierTimeout > 0 &&
		cfg.FusionLocalTimeout < cfg.FusionFrontierTimeout {
		slog.Warn("fusion local timeout shorter than frontier timeout",
			slog.Duration("local_timeout", cfg.FusionLocalTimeout),
			slog.Duration("frontier_timeout", cfg.FusionFrontierTimeout),
			slog.String("hint", "set NEXUS_FUSION_LOCAL_TIMEOUT >= NEXUS_FUSION_FRONTIER_TIMEOUT"),
		)
	}
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

// DefaultServerWriteTimeout is the default full response write deadline
// (issue #1069). 300s (5 min) prevents slow-client connection exhaustion
// while accommodating legitimate long-running SSE streams. Set to 0 to
// disable (streaming-unlimited opt-in).
const DefaultServerWriteTimeout = 300 * time.Second

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

// DefaultTracingTimeout is the default OTLP exporter POST timeout (issue #804).
// 10s is conservative for local collectors; operators with high-latency
// collectors can increase this via NEXUS_TRACING_TIMEOUT.
const DefaultTracingTimeout = 10 * time.Second

// DefaultMaxBodyBytes is the fallback request-body cap (issue #11). 1 MiB
// matches the typical OpenAI chat-completions request envelope; agents that
// need more room can raise it via NEXUS_MAX_BODY_BYTES.
const DefaultMaxBodyBytes = 1 << 20 // 1 MiB

// DefaultMaxResponseBytes is the default cap on upstream response bodies
// (issue #365). 64 MiB accommodates large frontier completions while
// preventing memory exhaustion from a malicious upstream.
const DefaultMaxResponseBytes = 64 << 20 // 64 MiB

// DefaultPoolBufferMaxBytes is the default retention cap for pooled
// response-body buffers (issue #1177). 1 MiB is generous for typical
// multi-KiB completions while preventing a single huge response from
// pinning pool memory. Zero or negative disables pooling entirely.
const DefaultPoolBufferMaxBytes = 1 << 20 // 1 MiB

// EffectiveMaxBodyBytes returns the request-body cap the chat handler should
// enforce. Zero or negative values fall back to DefaultMaxBodyBytes so a
// zero-value Config (e.g. inside unit tests) still gets a sane cap.
func (c Config) EffectiveMaxBodyBytes() int {
	if c.MaxBodyBytes > 0 {
		return c.MaxBodyBytes
	}
	return DefaultMaxBodyBytes
}

// EffectiveMaxResponseBytes returns the upstream response-body cap.
// Zero or negative values fall back to DefaultMaxResponseBytes so a
// zero-value Config (e.g. inside unit tests) still gets a sane cap.
func (c Config) EffectiveMaxResponseBytes() int {
	if c.MaxResponseBytes > 0 {
		return c.MaxResponseBytes
	}
	return DefaultMaxResponseBytes
}

// EffectiveCascadeMaxResponseBytes returns the cascade response-body cap.
// Zero or negative values fall back to DefaultMaxResponseBytes so a
// zero-value Config (e.g. inside unit tests) still gets a sane cap.
func (c Config) EffectiveCascadeMaxResponseBytes() int {
	if c.CascadeMaxResponseBytes > 0 {
		return c.CascadeMaxResponseBytes
	}
	return DefaultMaxResponseBytes
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

// Redaction defaults (issue #1172).
const (
	// RedactProfileDefault is the default profile when NEXUS_REDACT_PROFILE
	// is unset. "off" disables redaction entirely.
	RedactProfileDefault = "off"
	// DefaultRedactBufferBytes is the default rolling buffer cap for
	// cross-chunk multi-line pattern matching.
	DefaultRedactBufferBytes = 4 * 1024
)

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

// AllowCIDRsConfigured reports whether any inbound allowlist CIDRs are set.
// When true, only clients in the allowlist are permitted; others get 403.
func (c Config) AllowCIDRsConfigured() bool { return len(c.AllowCIDRs) > 0 }

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
		return nil, fmt.Errorf("config: invalid NEXUS_TRUSTED_PROXIES entry %q (expected CIDR or IP); see .env.example", p)
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
// identically to the pre-auth proxy. Active when either the legacy
// NEXUS_PROXY_API_KEY or the multi-key NEXUS_API_KEYS_FILE is set
// (issue #1154), or when JWT/OIDC mode is configured (issue #1152).
func (c Config) AuthEnabled() bool {
	if c.ProxyAPIKey != "" || c.APIKeysFile != "" {
		return true
	}
	if (c.AuthMode == "jwt" || c.AuthMode == "both") && c.OIDCJWKSURL != "" {
		return true
	}
	return false
}

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
// (issue #46) should be started. Disabled when RAGPollInterval is
// zero OR when the persistent store itself is disabled.
func (c Config) RAGWatcherEnabled() bool {
	return c.RAGPersistentEnabled() && c.RAGPollInterval > 0
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
	// Warnings contains messages for values that were adjusted (e.g. out-of-range
	// floats clamped to their valid bounds).
	Warnings []string
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
	if v := os.Getenv("NEXUS_METRICS_RETENTION_DAYS"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n != prev.MetricsRetentionDays {
			result.NeedsRestart = append(result.NeedsRestart, "NEXUS_METRICS_RETENTION_DAYS")
		}
	}

	// Trusted proxies: re-parse from env so the SIGHUP handler can push the
	// updated list into the live ipResolver without a restart (issue #896).
	next.TrustedProxiesRaw = strings.TrimSpace(os.Getenv("NEXUS_TRUSTED_PROXIES"))
	if parsed, err := parseTrustedProxies(next.TrustedProxiesRaw); err != nil {
		// Bogus value after boot: add to NeedsRestart so the operator is told
		// a restart is required instead of silently preserving the previous
		// value (issue #1055).
		slog.Warn("invalid NEXUS_TRUSTED_PROXIES, restart required to apply change",
			slog.String("reason", err.Error()))
		next.TrustedProxies = prev.TrustedProxies
		result.NeedsRestart = append(result.NeedsRestart, "NEXUS_TRUSTED_PROXIES")
	} else {
		next.TrustedProxies = parsed
	}

	// Allow CIDRs: re-parse from env so the SIGHUP handler can push the
	// updated list into the live allowlister without a restart (issue #1240).
	next.AllowCIDRsRaw = strings.TrimSpace(os.Getenv("NEXUS_ALLOW_CIDRS"))
	if parsed, err := parseTrustedProxies(next.AllowCIDRsRaw); err != nil {
		// Bogus value after boot: add to NeedsRestart so the operator is told
		// a restart is required instead of silently preserving the previous
		// value (issue #1055).
		slog.Warn("invalid NEXUS_ALLOW_CIDRS, restart required to apply change",
			slog.String("reason", err.Error()))
		next.AllowCIDRs = prev.AllowCIDRs
		result.NeedsRestart = append(result.NeedsRestart, "NEXUS_ALLOW_CIDRS")
	} else {
		next.AllowCIDRs = parsed
	}

	// Hot-reloadable settings.

	// AllowCIDRsStrict is hot-reloadable (issue #1240).
	next.AllowCIDRsStrict = getEnvBool("NEXUS_ALLOW_CIDRS_STRICT", prev.AllowCIDRsStrict)

	// Multi-key auth file (issue #1154): re-read the path so the SIGHUP
	// handler can reload credentials without a restart.
	next.APIKeysFile = os.Getenv("NEXUS_API_KEYS_FILE")

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

	// Auth brute-force limiter (issue #895).
	authRateLimitRPM, _ := getEnvInt("NEXUS_AUTH_RATE_LIMIT_RPM", prev.AuthRateLimitRPM)
	if authRateLimitRPM < 0 {
		authRateLimitRPM = 0
	}
	next.AuthRateLimitRPM = authRateLimitRPM

	authRateLimitBurst, _ := getEnvInt("NEXUS_AUTH_RATE_LIMIT_BURST", prev.AuthRateLimitBurst)
	if authRateLimitBurst < 0 {
		authRateLimitBurst = 0
	}
	next.AuthRateLimitBurst = authRateLimitBurst

	authRateLimitWindow, _ := getEnvDuration("NEXUS_AUTH_RATE_LIMIT_WINDOW", prev.AuthRateLimitWindow)
	if authRateLimitWindow < 0 {
		authRateLimitWindow = 0
	}
	if authRateLimitWindow == 0 {
		authRateLimitWindow = 5 * time.Minute
	}
	next.AuthRateLimitWindow = authRateLimitWindow

	logLevel, logLevelErr := parseLogLevel(os.Getenv("NEXUS_LOG_LEVEL"))
	if logLevelErr != nil {
		slog.Warn("invalid NEXUS_LOG_LEVEL, using info level", slog.String("reason", logLevelErr.Error()))
	}
	next.LogLevel = logLevel
	next.LogFormat = parseLogFormat(os.Getenv("NEXUS_LOG_FORMAT"))
	next.Debug = parseBoolEnv("NEXUS_DEBUG", prev.Debug)

	shutdownTimeout, _ := getEnvDuration("NEXUS_SHUTDOWN_TIMEOUT", prev.ShutdownTimeout)
	if shutdownTimeout < 0 {
		shutdownTimeout = 0
	}
	if shutdownTimeout == 0 {
		shutdownTimeout = DefaultShutdownTimeout
	}
	next.ShutdownTimeout = shutdownTimeout

	// Server read timeout (issue #77).
	readTimeout, _ := getEnvDuration("NEXUS_SERVER_READ_TIMEOUT", prev.ReadTimeout)
	if readTimeout < 0 {
		readTimeout = 0
	}
	if readTimeout == 0 {
		readTimeout = DefaultServerReadTimeout
	}
	next.ReadTimeout = readTimeout

	// Re-validate shutdown vs read-timeout ordering after the reload
	// (issue #990). The boot check in ValidateShutdownTimeout only runs
	// once; a SIGHUP that shrinks ShutdownTimeout below ReadTimeout
	// would otherwise go unnoticed.
	if next.ReadTimeout > 0 && next.ShutdownTimeout < next.ReadTimeout {
		slog.Warn("shutdown drain shorter than read timeout: in-flight uploads may be truncated mid-read",
			slog.Duration("shutdown_timeout", next.ShutdownTimeout),
			slog.Duration("read_timeout", next.ReadTimeout),
			slog.String("hint", "set NEXUS_SHUTDOWN_TIMEOUT >= NEXUS_SERVER_READ_TIMEOUT"),
		)
	}

	// Hot-reloadable float fields: re-read from env and clamp to valid range [0,1].
	// Emit a warning when the raw value was out of bounds (issue #1054).
	budgetAlertThreshold, _ := getEnvFloat("NEXUS_BUDGET_ALERT_THRESHOLD", prev.BudgetAlertThreshold)
	if budgetAlertThreshold < 0 || budgetAlertThreshold > 1 {
		clamped := clampFloat(budgetAlertThreshold, 0, 1)
		warn := fmt.Sprintf("NEXUS_BUDGET_ALERT_THRESHOLD value %g is outside valid range [0,1]; clamped to %g",
			budgetAlertThreshold, clamped)
		result.Warnings = append(result.Warnings, warn)
		slog.Warn(warn)
		budgetAlertThreshold = clamped
	}
	next.BudgetAlertThreshold = budgetAlertThreshold

	agreementThreshold, _ := getEnvFloat("NEXUS_FUSION_AGREEMENT_THRESHOLD", prev.FusionAgreementThreshold)
	if agreementThreshold < 0 || agreementThreshold > 1 {
		clamped := clampFloat(agreementThreshold, 0, 1)
		warn := fmt.Sprintf("NEXUS_FUSION_AGREEMENT_THRESHOLD value %g is outside valid range [0,1]; clamped to %g",
			agreementThreshold, clamped)
		result.Warnings = append(result.Warnings, warn)
		slog.Warn(warn)
		agreementThreshold = clamped
	}
	next.FusionAgreementThreshold = agreementThreshold

	tracingSampleRate, _ := getEnvFloat("NEXUS_TRACING_SAMPLE_RATE", prev.TracingSampleRate)
	if tracingSampleRate < 0 || tracingSampleRate > 1 {
		clamped := clampFloat(tracingSampleRate, 0, 1)
		warn := fmt.Sprintf("NEXUS_TRACING_SAMPLE_RATE value %g is outside valid range [0,1]; clamped to %g",
			tracingSampleRate, clamped)
		result.Warnings = append(result.Warnings, warn)
		slog.Warn(warn)
		tracingSampleRate = clamped
	}
	next.TracingSampleRate = tracingSampleRate

	// OIDC JWKS refresh interval (issue #1306): re-read from env so SIGHUP
	// pushes the updated interval into any live JWTAuthenticator on next refresh.
	jwksRefresh, _ := getEnvDuration("NEXUS_OIDC_JWKS_REFRESH", prev.OIDCJWKSRefresh)
	if jwksRefresh <= 0 {
		jwksRefresh = 15 * time.Minute
	}
	next.OIDCJWKSRefresh = jwksRefresh

	// RAG circuit breaker threshold: re-read from env so SIGHUP pushes the
	// updated threshold into the live RAG embedder circuit breaker.
	cbThreshold, _ := getEnvInt("NEXUS_RAG_CIRCUIT_BREAKER_THRESHOLD", prev.RAGCircuitBreakerThreshold)
	if cbThreshold < 0 {
		cbThreshold = 0
	}
	next.RAGCircuitBreakerThreshold = cbThreshold

	return next, result
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// configError renders an actionable config-validation error (issue #1181).
// The message bundles the offending env var, a human-readable constraint,
// the raw value the operator supplied, the safe default, and a pointer to
// .env.example so the fix is self-evident without reading the source:
//
//	config: NEXUS_SERVER_READ_TIMEOUT must not be negative; got "-5s".
//	        Unset the var to use the default (30s); see .env.example
//
// The result is a plain error (not a custom type) so the existing error
// contract is preserved — callers assert on err != nil or substrings, and
// wrapping the underlying parse error (%w) keeps errors.Is working.
func configError(key, constraint, gotValue, defaultStr string) error {
	return fmt.Errorf(
		"config: %s %s; got %q. Unset the var to use the default (%s); see .env.example",
		key, constraint, gotValue, defaultStr,
	)
}

// parseInjectionScanRoles canonicalises the comma-separated role list
// from NEXUS_INJECTION_SCAN_ROLES (issue #481). It lower-cases, trims,
// deduplicates, and keeps only recognised roles ("system", "user"). An
// empty or fully-unrecognised input returns []string{"system"} so the
// default scan scope is byte-for-byte identical to the pre-#481 path.
//
// The second return value is the list of tokens that were not recognised.
// Callers should log a warning when this is non-empty and the resulting
// roles are ["system"] (silent fallback), indicating a possible typo.
func parseInjectionScanRoles(raw string) ([]string, []string) {
	seen := make(map[string]bool)
	var roles []string
	var unrecognized []string
	for _, r := range strings.Split(raw, ",") {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" {
			continue
		}
		switch r {
		case "system", "user":
			if !seen[r] {
				seen[r] = true
				roles = append(roles, r)
			}
		default:
			unrecognized = append(unrecognized, r)
		}
	}
	if len(roles) == 0 {
		return []string{"system"}, unrecognized
	}
	return roles, unrecognized
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
		return 0, configError(key, "must be an integer", v, strconv.Itoa(def))
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

func getEnvFloat(key string, def float64) (float64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, configError(key, "must be a number", v, strconv.FormatFloat(def, 'f', -1, 64))
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
		return 0, configError(key, `must be a Go duration string (e.g. "8s", "2m")`, v, def.String())
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
			return nil, fmt.Errorf("config: %s pattern %q is not a valid regex: %w. Unset the var to restore the built-in default; see .env.example", key, p, err)
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
// the same verbosity as before (issue #3). Invalid values return
// slog.LevelInfo with an error so callers can log the misconfiguration.
func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	case "", "info":
		return slog.LevelInfo, nil
	default:
		return slog.LevelInfo, fmt.Errorf("config: invalid NEXUS_LOG_LEVEL %q; want debug, info, warn, or error; see .env.example", raw)
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

// ConfigField describes a single configuration knob for the `nexus config show`
// command. Key is the canonical NEXUS_ env var name; Value is the resolved
// string representation; Source is "env" | "file" | "default";
// HotReloadable is true when the knob can be changed at runtime via SIGHUP.
type ConfigField struct {
	Key           string
	Value         string
	Source        string // "env", "file", "default"
	HotReloadable bool
}

// hotReloadableEnvs is the set of env vars that ReloadHotReloadable() can
// re-read at runtime without a restart. Derived from the function body.
var hotReloadableEnvs = map[string]bool{
	"NEXUS_TRUSTED_PROXIES":               true,
	"NEXUS_API_KEYS_FILE":                 true,
	"NEXUS_RATE_LIMIT_RPM":                true,
	"NEXUS_RATE_LIMIT_BURST":              true,
	"NEXUS_AUTH_RATE_LIMIT_RPM":           true,
	"NEXUS_AUTH_RATE_LIMIT_BURST":         true,
	"NEXUS_AUTH_RATE_LIMIT_WINDOW":        true,
	"NEXUS_LOG_LEVEL":                     true,
	"NEXUS_LOG_FORMAT":                    true,
	"NEXUS_DEBUG":                         true,
	"NEXUS_SHUTDOWN_TIMEOUT":              true,
	"NEXUS_SERVER_READ_TIMEOUT":           true,
	"NEXUS_BUDGET_ALERT_THRESHOLD":        true,
	"NEXUS_FUSION_AGREEMENT_THRESHOLD":    true,
	"NEXUS_TRACING_SAMPLE_RATE":           true,
	"NEXUS_OIDC_JWKS_REFRESH":             true, // issue #1306
	"NEXUS_RAG_CIRCUIT_BREAKER_THRESHOLD": true,
}

// IsHotReloadable returns true when the given env var name (e.g. "NEXUS_RATE_LIMIT_RPM")
// can be re-read at runtime via SIGHUP without a full restart.
func IsHotReloadable(envKey string) bool {
	return hotReloadableEnvs[envKey]
}

// EnvToYAMLKey maps environment-variable names to their YAMLConfig snake_case
// equivalents.  Exported so the `nexus config show --diff` command can
// determine which YAML key corresponds to a given env var.
var EnvToYAMLKey = map[string]string{
	"NEXUS_ADDR":                              "addr",
	"NEXUS_SERVER_READ_TIMEOUT":               "server_read_timeout",
	"NEXUS_SERVER_WRITE_TIMEOUT":              "server_write_timeout",
	"NEXUS_SERVER_IDLE_TIMEOUT":               "server_idle_timeout",
	"NEXUS_SERVER_MAX_HEADER_BYTES":           "server_max_header_bytes",
	"NEXUS_SHUTDOWN_TIMEOUT":                  "shutdown_timeout",
	"NEXUS_MAX_BODY_BYTES":                    "max_body_bytes",
	"NEXUS_TLS_ENABLED":                       "tls_enabled",
	"NEXUS_LOG_LEVEL":                         "log_level",
	"NEXUS_LOG_FORMAT":                        "log_format",
	"NEXUS_DEBUG":                             "debug",
	"NEXUS_DEBUG_BODY_BYTES":                  "debug_body_bytes",
	"NEXUS_DEBUG_PPROF_ENABLED":               "debug_pprof_enabled",
	"NEXUS_DEBUG_PPROF_API_KEY":               "debug_pprof_api_key",
	"NEXUS_OLLAMA_URL":                        "ollama_url",
	"NEXUS_ROUTER_MODEL":                      "router_model",
	"NEXUS_LOCAL_MODEL":                       "local_model",
	"NEXUS_EMBEDDING_MODEL":                   "embedding_model",
	"NEXUS_FRONTIER_URL":                      "frontier_url",
	"NEXUS_FRONTIER_MODEL":                    "frontier_model",
	"NEXUS_FRONTIER_API_KEY":                  "frontier_api_key", // gitleaks:allow // issue #1274: false positive — map key is an env var name, not a secret.
	"NEXUS_FRONTIER_COST_PER_1K":              "frontier_cost_per_1k",
	"NEXUS_ZAI_URL":                           "zai_url",
	"NEXUS_ZAI_MODEL":                         "zai_model",
	"NEXUS_ZAI_API_KEY":                       "zai_api_key",
	"NEXUS_ZAI_COST_PER_1K":                   "zai_cost_per_1k",
	"NEXUS_PROXY_API_KEY":                     "proxy_api_key", // gitleaks:allow // issue #1274: false positive — map key is an env var name, not a secret.
	"NEXUS_STATUS_PUBLIC":                     "status_public",
	"NEXUS_API_KEYS_FILE":                     "api_keys_file",
	"NEXUS_AUTH_MODE":                         "auth_mode",
	"NEXUS_OIDC_JWKS_URL":                     "oidc_jwks_url",
	"NEXUS_OIDC_ISSUER":                       "oidc_issuer",
	"NEXUS_OIDC_AUDIENCE":                     "oidc_audience",
	"NEXUS_OIDC_JWKS_REFRESH":                 "oidc_jwks_refresh",
	"NEXUS_COST_BASELINE_PROVIDER":            "cost_baseline_provider",
	"NEXUS_COST_BASELINE_MODEL":               "cost_baseline_model",
	"NEXUS_COST_BASELINE_RATE_PER_1K":         "cost_baseline_rate_per_1k",
	"NEXUS_COST_USE_OUTPUT_TOKENS":            "cost_use_output_tokens",
	"NEXUS_ANTHROPIC_CACHE_MIN_SYSTEM_CHARS":  "anthropic_cache_min_system_chars",
	"NEXUS_AZURE_CONTENT_FILTER_ENABLED":      "azure_content_filter_enabled",
	"NEXUS_BUDGET_DAILY_LIMIT":                "budget_daily_limit",
	"NEXUS_BUDGET_ALERT_ENABLED":              "budget_alert_enabled",
	"NEXUS_BUDGET_ALERT_THRESHOLD":            "budget_alert_threshold",
	"NEXUS_BUDGET_ALERT_WEBHOOK_URL":          "budget_alert_webhook_url",
	"NEXUS_SELECTOR_WINDOW":                   "selector_window",
	"NEXUS_SELECTOR_MIN_SAMPLES":              "selector_min_samples",
	"NEXUS_SELECTOR_REFRESH":                  "selector_refresh",
	"NEXUS_PROVIDER_TAIL_WEIGHT":              "provider_tail_weight",
	"NEXUS_EXAMPLES_DIR":                      "examples_dir",
	"NEXUS_RAG_THRESHOLD":                     "rag_threshold",
	"NEXUS_EMBEDDER_TYPE":                     "embedder_type",
	"NEXUS_EMBEDDER_BASE_URL":                 "embedder_base_url",
	"NEXUS_COHERE_API_KEY":                    "cohere_api_key",
	"NEXUS_RAG_DB":                            "rag_db",
	"NEXUS_RAG_POLL_INTERVAL":                 "rag_poll_interval",
	"NEXUS_RAG_RECURSIVE":                     "rag_recursive",
	"NEXUS_RAG_EMBED_CACHE_SIZE":              "rag_embed_cache_size",
	"NEXUS_RAG_EMBED_CACHE_TTL":               "rag_embed_cache_ttl",
	"NEXUS_RAG_EMBED_CACHE_WAIT_TIMEOUT":      "rag_embed_cache_wait_timeout",
	"NEXUS_RAG_CIRCUIT_BREAKER_THRESHOLD":     "rag_circuit_breaker_threshold",
	"NEXUS_RAG_CIRCUIT_BREAKER_COOLDOWN":      "rag_circuit_breaker_cooldown",
	"NEXUS_RAG_BATCH_SIZE":                    "rag_batch_size",
	"NEXUS_RAG_CHUNK_TOKENS":                  "rag_chunk_tokens",
	"NEXUS_RAG_TOP_K":                         "rag_top_k",
	"NEXUS_RAG_MAX_INJECTION_TOKENS":          "rag_max_injection_tokens",
	"NEXUS_RAG_FILE_EXTENSIONS":               "rag_file_extensions",
	"NEXUS_RAG_EXCLUDE_PATTERNS":              "rag_exclude_patterns",
	"NEXUS_TOKEN_GUARDRAIL":                   "token_guardrail",
	"NEXUS_SLM_TIMEOUT":                       "slm_timeout",
	"NEXUS_SLM_CACHE_MAX_ENTRIES":             "slm_cache_max_entries",
	"NEXUS_SLMCACHE_SIMILARITY_THRESHOLD":     "slm_cache_similarity_threshold",
	"NEXUS_SLMCACHE_MAX_STALE":                "slm_cache_max_stale",
	"NEXUS_SLMCACHE_STALE_CLEANUP_THRESHOLD":  "slm_cache_stale_cleanup_threshold",
	"NEXUS_SLMCACHE_SEMANTIC_SCAN_LIMIT":      "slm_cache_semantic_scan_limit",
	"NEXUS_SLM_CONFIDENCE_THRESHOLD":          "slm_confidence_threshold",
	"NEXUS_ROUTING_CONTEXT_TURNS":             "routing_context_turns",
	"NEXUS_ROUTING_CONTEXT_CHARS":             "routing_context_chars",
	"NEXUS_FUSION_TIMEOUT":                    "fusion_timeout",
	"NEXUS_FUSION_LOCAL_TIMEOUT":              "fusion_local_timeout",
	"NEXUS_FUSION_FRONTIER_TIMEOUT":           "fusion_frontier_timeout",
	"NEXUS_CASCADE_TIMEOUT":                   "cascade_timeout",
	"NEXUS_CASCADE_TIMEOUT_FLOOR":             "cascade_timeout_floor",
	"NEXUS_CASCADE_TIMEOUT_CEILING":           "cascade_timeout_ceiling",
	"NEXUS_CASCADE_TIMEOUT_PER_1K_TOKENS":     "cascade_timeout_per_1k_tokens",
	"NEXUS_ARBITER_TIMEOUT":                   "arbiter_timeout",
	"NEXUS_FRONTIER_FAILOVER":                 "frontier_failover",
	"NEXUS_FRONTIER_FAILOVER_MAX_ATTEMPTS":    "frontier_failover_max_attempts",
	"NEXUS_COALESCE_ENABLED":                  "coalesce_enabled",
	"NEXUS_COALESCE_TTL":                      "coalesce_ttl",
	"NEXUS_COALESCE_MAX_ENTRIES":              "coalesce_max_entries",
	"NEXUS_DSL_FORMATTING_PATTERNS":           "dsl_formatting_patterns",
	"NEXUS_DSL_FUSION_PATTERNS":               "dsl_fusion_patterns",
	"NEXUS_DSL_LOCAL_PATTERNS":                "dsl_local_patterns",
	"NEXUS_DSL_UNICODE_PATTERNS":              "dsl_unicode_patterns",
	"NEXUS_DSL_PROMOTION_MIN_SAMPLES":         "dsl_promotion_min_samples",
	"NEXUS_DSL_PROMOTION_CONFIDENCE":          "dsl_promotion_confidence",
	"NEXUS_DSL_PROMOTION_INTERVAL":            "dsl_promotion_interval",
	"NEXUS_FUSION_PROGRESSIVE":                "fusion_progressive",
	"NEXUS_FUSION_AGREEMENT_THRESHOLD":        "fusion_agreement_threshold",
	"NEXUS_FUSION_SIMILARITY_MODE":            "fusion_similarity_mode",
	"NEXUS_ARBITER_CACHE_TTL":                 "arbiter_cache_ttl",
	"NEXUS_ARBITER_CACHE_MAX_ENTRIES":         "arbiter_cache_max_entries",
	"NEXUS_CACHE_WARM_ON_BOOT":                "cache_warm_on_boot",
	"NEXUS_CACHE_WARM_LIMIT":                  "cache_warm_limit",
	"NEXUS_ROUTING_CONFIDENCE_DB":             "routing_confidence_db",
	"NEXUS_ROUTING_CONFIDENCE_FLOOR":          "routing_confidence_floor",
	"NEXUS_ROUTING_CONFIDENCE_CEILING":        "routing_confidence_ceiling",
	"NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES":    "routing_confidence_min_samples",
	"NEXUS_ROUTING_CONFIDENCE_WINDOW":         "routing_confidence_window",
	"NEXUS_SLM_CACHE_TTL":                     "slm_cache_ttl",
	"NEXUS_HEALTH_POLL_INTERVAL":              "health_poll_interval",
	"NEXUS_HEALTH_BREAKER_THRESHOLD":          "health_breaker_threshold",
	"NEXUS_HEALTH_PROBE_TIMEOUT":              "health_probe_timeout",
	"NEXUS_FRONTIER_HEALTH_POLL_INTERVAL":     "frontier_health_poll_interval",
	"NEXUS_FRONTIER_HEALTH_BREAKER_THRESHOLD": "frontier_health_breaker_threshold",
	"NEXUS_FRONTIER_HEALTH_TIMEOUT":           "frontier_health_timeout",
	"NEXUS_PROBE_INTERVAL":                    "probe_interval",
	"NEXUS_PROBE_TIMEOUT":                     "probe_timeout",
	"NEXUS_PROBE_BYTES_PER_TOKEN":             "probe_bytes_per_token",
	"NEXUS_PROBE_THERMAL_THRESHOLD":           "probe_thermal_threshold",
	"NEXUS_PROBE_NVIDIA_INTERVAL":             "probe_nvidia_interval",
	"NEXUS_LOCAL_MAX_CONCURRENT":              "local_max_concurrent",
	"NEXUS_LOCAL_VRAM_BYTES_PER_SLOT":         "local_vram_bytes_per_slot",
	"NEXUS_LOCAL_COOLDOWN":                    "local_cooldown",
	"NEXUS_METRICS_DB":                        "metrics_db",
	"NEXUS_METRICS_RETENTION_DAYS":            "metrics_retention_days",
	"NEXUS_MAX_RESPONSE_BYTES":                "max_response_bytes",
	"NEXUS_CASCADE_MAX_RESPONSE_BYTES":        "cascade_max_response_bytes",
	"NEXUS_POOL_BUFFER_MAX_BYTES":             "pool_buffer_max_bytes",
	"NEXUS_AUTH_RATE_LIMIT_RPM":               "auth_rate_limit_rpm",
	"NEXUS_AUTH_RATE_LIMIT_BURST":             "auth_rate_limit_burst",
	"NEXUS_AUTH_RATE_LIMIT_WINDOW":            "auth_rate_limit_window",
	"NEXUS_TRUSTED_PROXIES":                   "trusted_proxies",
	"NEXUS_RATE_LIMIT_RPM":                    "rate_limit_rpm",
	"NEXUS_RATE_LIMIT_BURST":                  "rate_limit_burst",
	"NEXUS_RATE_LIMIT_BY_API_KEY":             "rate_limit_by_api_key",
	"NEXUS_READINESS_MODE":                    "readiness_mode",
	"NEXUS_INIT_PROFILE":                      "init_profile",
	"NEXUS_TRACING_ENDPOINT":                  "tracing_endpoint",
	"NEXUS_TRACING_TIMEOUT":                   "tracing_timeout",
	"NEXUS_TRACING_QUEUE_SIZE":                "tracing_queue_size",
	"NEXUS_TRACING_BATCH_SIZE":                "tracing_batch_size",
	"NEXUS_TRACING_SAMPLE_RATE":               "tracing_sample_rate",
	"NEXUS_LOG_TRACE_ID":                      "log_trace_id",
	"NEXUS_METRICS_EXEMPLARS":                 "metrics_exemplars",
	"NEXUS_REDACT_ENABLED":                    "redact_enabled",
	"NEXUS_REDACT_PROFILE":                    "redact_profile",
	"NEXUS_REDACT_PATTERNS":                   "redact_patterns",
	"NEXUS_REDACT_BUFFER_BYTES":               "redact_buffer_bytes",
	"NEXUS_EGRESS_BLOCK_PRIVATE":              "egress_block_private",
	"NEXUS_EGRESS_ALLOW":                      "egress_allow",
	"NEXUS_SECRET_BACKEND":                    "secret_backend",
	"NEXUS_VAULT_ADDR":                        "vault_addr",
	"NEXUS_VAULT_TOKEN":                       "vault_token",
	"NEXUS_VAULT_ROLE":                        "vault_role",
	"NEXUS_VAULT_PATH":                        "vault_path",
	"NEXUS_AWSSM_PREFIX":                      "awssm_prefix",
	"NEXUS_SECRET_REFRESH":                    "secret_refresh",
	"NEXUS_AUDIT_ENABLED":                     "audit_enabled",
	"NEXUS_AUDIT_PATH":                        "audit_path",
	"NEXUS_AUDIT_SYNC":                        "audit_sync",
	"NEXUS_TELEMETRY_PATH":                    "telemetry_path",
	"NEXUS_TELEMETRY_MAX_BYTES":               "telemetry_max_bytes",
	"NEXUS_TELEMETRY_MAX_FILES":               "telemetry_max_files",
	"NEXUS_TELEMETRY_BUFFER_SIZE":             "telemetry_buffer_size",
	"NEXUS_TELEMETRY_FLUSH_INTERVAL":          "telemetry_flush_interval",
	"NEXUS_MODELS_ENDPOINT":                   "models_endpoint",
	"NEXUS_MODELS_CACHE_TTL":                  "models_cache_ttl",
	"NEXUS_DASHBOARD_ENDPOINT":                "dashboard_endpoint",
	"NEXUS_DASHBOARD_PUBLIC":                  "dashboard_public",
	"NEXUS_JUDGE_URL":                         "judge_url",
	"NEXUS_JUDGE_MODEL":                       "judge_model",
	"NEXUS_JUDGE_API_KEY":                     "judge_api_key",
	"NEXUS_JUDGE_SAMPLE_RATE":                 "judge_sample_rate",
	"NEXUS_JUDGE_FRONTIER_SAMPLE_RATE":        "judge_frontier_sample_rate",
	"NEXUS_JUDGE_CONCURRENCY":                 "judge_concurrency",
	"NEXUS_JUDGE_QUEUE":                       "judge_queue",
	"NEXUS_JUDGE_TIMEOUT":                     "judge_timeout",
	"NEXUS_JUDGE_COST_PER_1K":                 "judge_cost_per_1k",
	"NEXUS_JUDGE_DB":                          "judge_db",
	"NEXUS_JUDGE_ADAPTIVE_ENABLED":            "judge_adaptive_enabled",
	"NEXUS_QUALITY_CONCURRENCY":               "quality_concurrency",
	"NEXUS_QUALITY_QUEUE":                     "quality_queue",
	"NEXUS_QUALITY_TIMEOUT":                   "quality_timeout",
	"NEXUS_QUALITY_STDERR_CAP":                "quality_stderr_cap",
	"NEXUS_QUALITY_DROPPED_RING_SIZE":         "quality_dropped_ring_size",
	"NEXUS_TOON_UNFENCED":                     "toon_unfenced",
	"NEXUS_MIDDLEWARE_CHAIN":                  "middleware_chain",
	"NEXUS_INJECTION_SCAN_ROLES":              "injection_scan_roles",
	"NEXUS_PROMPT_INJECTION_MODE":             "prompt_injection_mode",
	"NEXUS_MODEL_ALIASES":                     "model_aliases",
	"NEXUS_MODEL_ALIASES_STRICT":              "model_aliases_strict",
}

// injectionModeString returns the canonical env-var string for an InjectionMode.
func injectionModeString(m middleware.InjectionMode) string {
	switch m {
	case middleware.InjectionModeOff:
		return "off"
	case middleware.InjectionModeWarn:
		return "warn"
	case middleware.InjectionModeStrict:
		return "strict"
	default:
		return "unknown"
	}
}

// fieldSource determines whether a given env var resolved from env, file, or default.
// fileCfg is the YAML config map (nil if no file was loaded).
func fieldSource(envKey string, fileCfg map[string]string) string {
	if v, ok := os.LookupEnv(envKey); ok && v != "" {
		return "env"
	}
	if fileCfg != nil {
		yamlKey := EnvToYAMLKey[envKey]
		if yamlKey != "" {
			if v, ok := fileCfg[yamlKey]; ok && v != "" {
				return "file"
			}
		}
	}
	return "default"
}

// envField describes one NEXUS env var with a pointer to the corresponding
// field in Config and a format function to render the current value as a string.
type envField struct {
	envKey   string
	getValue func(*Config) string
}

// allEnvFields is the exhaustive list of NEXUS_ env vars surfaced by `nexus config show`.
// Each entry knows how to read its value from a loaded Config. Fields are ordered
// alphabetically by env key for deterministic output.
var allEnvFields = []envField{
	{"NEXUS_ADDR", func(c *Config) string { return c.Addr }},
	{"NEXUS_API_KEYS_FILE", func(c *Config) string { return c.APIKeysFile }},
	{"NEXUS_AUTH_MODE", func(c *Config) string { return c.AuthMode }},
	{"NEXUS_AUTH_RATE_LIMIT_BURST", func(c *Config) string { return fmt.Sprintf("%d", c.AuthRateLimitBurst) }},
	{"NEXUS_AUTH_RATE_LIMIT_RPM", func(c *Config) string { return fmt.Sprintf("%d", c.AuthRateLimitRPM) }},
	{"NEXUS_AUTH_RATE_LIMIT_WINDOW", func(c *Config) string { return c.AuthRateLimitWindow.String() }},
	{"NEXUS_AUDIT_ENABLED", func(c *Config) string { return fmt.Sprintf("%t", c.AuditEnabled) }},
	{"NEXUS_AUDIT_PATH", func(c *Config) string { return c.AuditPath }},
	{"NEXUS_AUDIT_SYNC", func(c *Config) string { return c.AuditSync }},
	{"NEXUS_AWSSM_PREFIX", func(c *Config) string { return c.AWSSMPrefix }},
	{"NEXUS_AZURE_CONTENT_FILTER_ENABLED", func(c *Config) string { return fmt.Sprintf("%t", c.AzureContentFilterEnabled) }},
	{"NEXUS_BUDGET_ALERT_ENABLED", func(c *Config) string { return fmt.Sprintf("%t", c.BudgetAlertEnabled) }},
	{"NEXUS_BUDGET_ALERT_THRESHOLD", func(c *Config) string { return fmt.Sprintf("%g", c.BudgetAlertThreshold) }},
	{"NEXUS_BUDGET_ALERT_WEBHOOK_URL", func(c *Config) string { return c.BudgetAlertWebhookURL }},
	{"NEXUS_BUDGET_DAILY_LIMIT", func(c *Config) string { return fmt.Sprintf("%g", c.BudgetDailyLimit) }},
	{"NEXUS_CACHE_WARM_LIMIT", func(c *Config) string { return fmt.Sprintf("%d", c.CacheWarmLimit) }},
	{"NEXUS_CACHE_WARM_ON_BOOT", func(c *Config) string { return fmt.Sprintf("%t", c.CacheWarmOnBoot) }},
	{"NEXUS_CASCADE_MAX_RESPONSE_BYTES", func(c *Config) string { return fmt.Sprintf("%d", c.CascadeMaxResponseBytes) }},
	{"NEXUS_CASCADE_TIMEOUT", func(c *Config) string { return c.CascadeTimeout.String() }},
	{"NEXUS_CASCADE_TIMEOUT_CEILING", func(c *Config) string { return c.CascadeTimeoutCeiling.String() }},
	{"NEXUS_CASCADE_TIMEOUT_FLOOR", func(c *Config) string { return c.CascadeTimeoutFloor.String() }},
	{"NEXUS_CASCADE_TIMEOUT_PER_1K_TOKENS", func(c *Config) string { return c.CascadeTimeoutPer1kTokens.String() }},
	{"NEXUS_COALESCE_ENABLED", func(c *Config) string { return fmt.Sprintf("%t", c.CoalesceEnabled) }},
	{"NEXUS_COALESCE_MAX_ENTRIES", func(c *Config) string { return fmt.Sprintf("%d", c.CoalesceMaxEntries) }},
	{"NEXUS_COALESCE_TTL", func(c *Config) string { return c.CoalesceTTL.String() }},
	{"NEXUS_COST_BASELINE_MODEL", func(c *Config) string { return c.CostBaselineModel }},
	{"NEXUS_COST_BASELINE_PROVIDER", func(c *Config) string { return c.CostBaselineProvider }},
	{"NEXUS_COST_BASELINE_RATE_PER_1K", func(c *Config) string { return fmt.Sprintf("%g", c.CostBaselineRatePer1K) }},
	{"NEXUS_COST_USE_OUTPUT_TOKENS", func(c *Config) string { return fmt.Sprintf("%t", c.CostUseOutputTokens) }},
	{"NEXUS_DASHBOARD_ENDPOINT", func(c *Config) string { return c.DashboardEndpoint }},
	{"NEXUS_DASHBOARD_PUBLIC", func(c *Config) string { return fmt.Sprintf("%t", c.DashboardPublic) }},
	{"NEXUS_DEBUG", func(c *Config) string { return fmt.Sprintf("%t", c.Debug) }},
	{"NEXUS_DEBUG_BODY_BYTES", func(c *Config) string { return fmt.Sprintf("%d", c.DebugBodyBytes) }},
	{"NEXUS_DEBUG_PPROF_API_KEY", func(c *Config) string { return c.DebugPprofAPIKey }},
	{"NEXUS_DEBUG_PPROF_ENABLED", func(c *Config) string { return fmt.Sprintf("%t", c.DebugPprofEnabled) }},
	{"NEXUS_DSL_FORMATTING_PATTERNS", func(c *Config) string { return regexpStrings(c.DSLFormattingPatterns) }},
	{"NEXUS_DSL_FUSION_PATTERNS", func(c *Config) string { return regexpStrings(c.DSLFusionPatterns) }},
	{"NEXUS_DSL_LOCAL_PATTERNS", func(c *Config) string { return regexpStrings(c.DSLLocalPatterns) }},
	{"NEXUS_DSL_PROMOTION_CONFIDENCE", func(c *Config) string { return fmt.Sprintf("%g", c.DSLPromotionConfidence) }},
	{"NEXUS_DSL_PROMOTION_INTERVAL", func(c *Config) string { return c.DSLPromotionInterval.String() }},
	{"NEXUS_DSL_PROMOTION_MIN_SAMPLES", func(c *Config) string { return fmt.Sprintf("%d", c.DSLPromotionMinSamples) }},
	{"NEXUS_DSL_UNICODE_PATTERNS", func(c *Config) string { return regexpStrings(c.DSLUnicodePatterns) }},
	{"NEXUS_EGRESS_ALLOW", func(c *Config) string { return c.EgressAllowCIDRs }},
	{"NEXUS_EGRESS_BLOCK_PRIVATE", func(c *Config) string { return fmt.Sprintf("%t", c.EgressGuardEnabled) }},
	{"NEXUS_EMBEDDER_BASE_URL", func(c *Config) string { return c.EmbedderBaseURL }},
	{"NEXUS_EMBEDDER_TYPE", func(c *Config) string { return string(c.EmbedderType) }},
	{"NEXUS_EXAMPLES_DIR", func(c *Config) string { return c.ExamplesDir }},
	// gitleaks:allow // issue #1274: false positive — map key is an env var name, not a secret.
	{"NEXUS_FRONTIER_API_KEY", func(c *Config) string { return redact(c.FrontierKey) }},
	{"NEXUS_FRONTIER_COST_PER_1K", func(c *Config) string { return fmt.Sprintf("%g", c.FrontierCostPer1K) }},
	{"NEXUS_FRONTIER_FAILOVER", func(c *Config) string { return fmt.Sprintf("%t", c.FrontierFailover) }},
	{"NEXUS_FRONTIER_FAILOVER_MAX_ATTEMPTS", func(c *Config) string { return fmt.Sprintf("%d", c.FrontierFailoverMaxAttempts) }},
	{"NEXUS_FRONTIER_HEALTH_BREAKER_THRESHOLD", func(c *Config) string { return fmt.Sprintf("%d", c.FrontierHealthBreakerThreshold) }},
	{"NEXUS_FRONTIER_HEALTH_POLL_INTERVAL", func(c *Config) string { return c.FrontierHealthPollInterval.String() }},
	{"NEXUS_FRONTIER_HEALTH_TIMEOUT", func(c *Config) string { return c.FrontierHealthTimeout.String() }},
	{"NEXUS_FRONTIER_MODEL", func(c *Config) string { return c.FrontierModel }},
	{"NEXUS_FRONTIER_URL", func(c *Config) string { return c.FrontierURL }},
	{"NEXUS_FUSION_AGREEMENT_THRESHOLD", func(c *Config) string { return fmt.Sprintf("%g", c.FusionAgreementThreshold) }},
	{"NEXUS_FUSION_FRONTIER_TIMEOUT", func(c *Config) string { return c.FusionFrontierTimeout.String() }},
	{"NEXUS_FUSION_LOCAL_TIMEOUT", func(c *Config) string { return c.FusionLocalTimeout.String() }},
	{"NEXUS_FUSION_PROGRESSIVE", func(c *Config) string { return fmt.Sprintf("%t", c.FusionProgressiveDelivery) }},
	{"NEXUS_FUSION_SIMILARITY_MODE", func(c *Config) string { return c.FusionSimilarityMode }},
	{"NEXUS_FUSION_TIMEOUT", func(c *Config) string { return c.FusionTimeout.String() }},
	{"NEXUS_HEALTH_BREAKER_THRESHOLD", func(c *Config) string { return fmt.Sprintf("%d", c.HealthBreakerThreshold) }},
	{"NEXUS_HEALTH_POLL_INTERVAL", func(c *Config) string { return c.HealthPollInterval.String() }},
	{"NEXUS_HEALTH_PROBE_TIMEOUT", func(c *Config) string { return c.HealthProbeTimeout.String() }},
	{"NEXUS_INJECTION_SCAN_ROLES", func(c *Config) string { return strings.Join(c.InjectionScanRoles, ",") }},
	{"NEXUS_JUDGE_API_KEY", func(c *Config) string { return redact(c.JudgeAPIKey) }},
	{"NEXUS_JUDGE_COST_PER_1K", func(c *Config) string { return fmt.Sprintf("%g", c.JudgeCostPer1KUSD) }},
	{"NEXUS_JUDGE_CONCURRENCY", func(c *Config) string { return fmt.Sprintf("%d", c.JudgeConcurrency) }},
	{"NEXUS_JUDGE_DB", func(c *Config) string { return c.JudgeDBPath }},
	{"NEXUS_JUDGE_FRONTIER_SAMPLE_RATE", func(c *Config) string { return fmt.Sprintf("%g", c.JudgeFrontierSampleRate) }},
	{"NEXUS_JUDGE_MODEL", func(c *Config) string { return c.JudgeModel }},
	{"NEXUS_JUDGE_QUEUE", func(c *Config) string { return fmt.Sprintf("%d", c.JudgeQueueDepth) }},
	{"NEXUS_JUDGE_SAMPLE_RATE", func(c *Config) string { return fmt.Sprintf("%g", c.JudgeSampleRate) }},
	{"NEXUS_JUDGE_TIMEOUT", func(c *Config) string { return c.JudgeTimeout.String() }},
	{"NEXUS_JUDGE_URL", func(c *Config) string { return c.JudgeURL }},
	{"NEXUS_JUDGE_ADAPTIVE_ENABLED", func(c *Config) string { return fmt.Sprintf("%t", c.JudgeAdaptiveEnabled) }},
	{"NEXUS_JUDGE_ADAPTIVE_WINDOW", func(c *Config) string { return c.JudgeAdaptiveWindow.String() }},
	{"NEXUS_JUDGE_ADAPTIVE_HIGH_CONFIDENCE", func(c *Config) string { return fmt.Sprintf("%g", c.JudgeAdaptiveHighConf) }},
	{"NEXUS_JUDGE_ADAPTIVE_LOW_CONFIDENCE", func(c *Config) string { return fmt.Sprintf("%g", c.JudgeAdaptiveLowConf) }},
	{"NEXUS_LOCAL_COOLDOWN", func(c *Config) string { return c.LocalCooldown.String() }},
	{"NEXUS_LOCAL_MAX_CONCURRENT", func(c *Config) string { return fmt.Sprintf("%d", c.LocalMaxConcurrent) }},
	{"NEXUS_LOCAL_MODEL", func(c *Config) string { return c.LocalModel }},
	{"NEXUS_LOCAL_VRAM_BYTES_PER_SLOT", func(c *Config) string { return fmt.Sprintf("%d", c.LocalVRAMBytesPerSlot) }},
	{"NEXUS_LOG_FORMAT", func(c *Config) string { return c.LogFormat.String() }},
	{"NEXUS_LOG_LEVEL", func(c *Config) string { return c.LogLevel.String() }},
	{"NEXUS_LOG_TRACE_ID", func(c *Config) string { return fmt.Sprintf("%t", c.LogTraceID) }},
	{"NEXUS_MAX_BODY_BYTES", func(c *Config) string { return fmt.Sprintf("%d", c.MaxBodyBytes) }},
	{"NEXUS_MAX_RESPONSE_BYTES", func(c *Config) string { return fmt.Sprintf("%d", c.MaxResponseBytes) }},
	{"NEXUS_METRICS_DB", func(c *Config) string { return c.MetricsDBPath }},
	{"NEXUS_METRICS_EXEMPLARS", func(c *Config) string { return fmt.Sprintf("%t", c.MetricsExemplars) }},
	{"NEXUS_METRICS_RETENTION_DAYS", func(c *Config) string { return fmt.Sprintf("%d", c.MetricsRetentionDays) }},
	{"NEXUS_MIDDLEWARE_CHAIN", func(c *Config) string { return c.MiddlewareChain }},
	{"NEXUS_MODELS_CACHE_TTL", func(c *Config) string { return c.ModelsCacheTTL.String() }},
	{"NEXUS_MODELS_ENDPOINT", func(c *Config) string { return fmt.Sprintf("%t", c.ModelsEndpointEnabled) }},
	{"NEXUS_OIDC_AUDIENCE", func(c *Config) string { return c.OIDCAudience }},
	{"NEXUS_OIDC_ISSUER", func(c *Config) string { return c.OIDCIssuer }},
	{"NEXUS_OIDC_JWKS_REFRESH", func(c *Config) string { return c.OIDCJWKSRefresh.String() }},
	{"NEXUS_OIDC_JWKS_URL", func(c *Config) string { return c.OIDCJWKSURL }},
	{"NEXUS_OLLAMA_URL", func(c *Config) string { return c.OllamaURL }},
	{"NEXUS_POOL_BUFFER_MAX_BYTES", func(c *Config) string { return fmt.Sprintf("%d", c.PoolBufferMaxBytes) }},
	{"NEXUS_PROBE_BYTES_PER_TOKEN", func(c *Config) string { return fmt.Sprintf("%d", c.ProbeBytesPerToken) }},
	{"NEXUS_PROBE_INTERVAL", func(c *Config) string { return c.ProbePollInterval.String() }},
	{"NEXUS_PROBE_NVIDIA_INTERVAL", func(c *Config) string { return c.ProbeNVIDIAInterval.String() }},
	{"NEXUS_PROBE_THERMAL_THRESHOLD", func(c *Config) string { return fmt.Sprintf("%d", c.ProbeThermalThreshold) }},
	{"NEXUS_PROBE_TIMEOUT", func(c *Config) string { return c.ProbeTimeout.String() }},
	{"NEXUS_PROMPT_INJECTION_MODE", func(c *Config) string { return injectionModeString(c.PromptInjectionMode) }},
	{"NEXUS_PROVIDER_TAIL_WEIGHT", func(c *Config) string { return fmt.Sprintf("%g", c.ProviderTailWeight) }},
	// gitleaks:allow // issue #1274: false positive — map key is an env var name, not a secret.
	{"NEXUS_PROXY_API_KEY", func(c *Config) string { return redact(c.ProxyAPIKey) }},
	{"NEXUS_RAG_BATCH_SIZE", func(c *Config) string { return fmt.Sprintf("%d", c.RAGBatchSize) }},
	{"NEXUS_RAG_CHUNK_TOKENS", func(c *Config) string { return fmt.Sprintf("%d", c.RAGChunkTokens) }},
	{"NEXUS_RAG_CIRCUIT_BREAKER_COOLDOWN", func(c *Config) string { return c.RAGCircuitBreakerCooldown.String() }},
	{"NEXUS_RAG_CIRCUIT_BREAKER_THRESHOLD", func(c *Config) string { return fmt.Sprintf("%d", c.RAGCircuitBreakerThreshold) }},
	{"NEXUS_RAG_DB", func(c *Config) string { return c.RAGDBPath }},
	{"NEXUS_RAG_EMBED_CACHE_SIZE", func(c *Config) string { return fmt.Sprintf("%d", c.RAGEmbedCacheSize) }},
	{"NEXUS_RAG_EMBED_CACHE_TTL", func(c *Config) string { return c.RAGEmbedCacheTTL.String() }},
	{"NEXUS_RAG_EMBED_CACHE_WAIT_TIMEOUT", func(c *Config) string { return c.RAGEmbedCacheWaitTimeout.String() }},
	{"NEXUS_RAG_EXCLUDE_PATTERNS", func(c *Config) string { return strings.Join(c.RAGExcludePatterns, ",") }},
	{"NEXUS_RAG_FILE_EXTENSIONS", func(c *Config) string { return strings.Join(c.RAGFileExtensions, ",") }},
	{"NEXUS_RAG_MAX_INJECTION_TOKENS", func(c *Config) string { return fmt.Sprintf("%d", c.RAGMaxInjectionTokens) }},
	{"NEXUS_RAG_POLL_INTERVAL", func(c *Config) string { return c.RAGPollInterval.String() }},
	{"NEXUS_RAG_RECURSIVE", func(c *Config) string { return fmt.Sprintf("%t", c.RAGRecursive) }},
	{"NEXUS_RAG_THRESHOLD", func(c *Config) string { return fmt.Sprintf("%g", c.RAGThreshold) }},
	{"NEXUS_RAG_TOP_K", func(c *Config) string { return fmt.Sprintf("%d", c.RAGTopK) }},
	{"NEXUS_RATE_LIMIT_BURST", func(c *Config) string { return fmt.Sprintf("%d", c.RateLimitBurst) }},
	{"NEXUS_RATE_LIMIT_BY_API_KEY", func(c *Config) string { return fmt.Sprintf("%t", c.RateLimitByAPIKey) }},
	{"NEXUS_RATE_LIMIT_RPM", func(c *Config) string { return fmt.Sprintf("%d", c.RateLimitRPM) }},
	{"NEXUS_READINESS_MODE", func(c *Config) string { return c.ReadinessMode }},
	{"NEXUS_REDACT_BUFFER_BYTES", func(c *Config) string { return fmt.Sprintf("%d", c.RedactBufferBytes) }},
	{"NEXUS_REDACT_ENABLED", func(c *Config) string { return fmt.Sprintf("%t", c.RedactEnabled) }},
	{"NEXUS_REDACT_PATTERNS", func(c *Config) string { return c.RedactPatternsRaw }},
	{"NEXUS_REDACT_PROFILE", func(c *Config) string { return c.RedactProfile }},
	{"NEXUS_ROUTING_CONFIDENCE_CEILING", func(c *Config) string { return fmt.Sprintf("%g", c.RoutingConfidenceCeiling) }},
	{"NEXUS_ROUTING_CONFIDENCE_DB", func(c *Config) string { return c.RoutingConfidenceDB }},
	{"NEXUS_ROUTING_CONFIDENCE_FLOOR", func(c *Config) string { return fmt.Sprintf("%g", c.RoutingConfidenceFloor) }},
	{"NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES", func(c *Config) string { return fmt.Sprintf("%d", c.RoutingConfidenceMinSamples) }},
	{"NEXUS_ROUTING_CONFIDENCE_WINDOW", func(c *Config) string { return c.RoutingConfidenceWindow.String() }},
	{"NEXUS_SECRET_BACKEND", func(c *Config) string { return c.SecretBackend }},
	{"NEXUS_SECRET_REFRESH", func(c *Config) string { return c.SecretRefresh.String() }},
	{"NEXUS_SELECTOR_MIN_SAMPLES", func(c *Config) string { return fmt.Sprintf("%d", c.SelectorMinSamples) }},
	{"NEXUS_SELECTOR_REFRESH", func(c *Config) string { return c.SelectorRefreshInterval.String() }},
	{"NEXUS_SELECTOR_WINDOW", func(c *Config) string { return c.SelectorWindow.String() }},
	{"NEXUS_SERVER_IDLE_TIMEOUT", func(c *Config) string { return c.IdleTimeout.String() }},
	{"NEXUS_SERVER_MAX_HEADER_BYTES", func(c *Config) string { return fmt.Sprintf("%d", c.MaxHeaderBytes) }},
	{"NEXUS_SERVER_READ_TIMEOUT", func(c *Config) string { return c.ReadTimeout.String() }},
	{"NEXUS_SERVER_WRITE_TIMEOUT", func(c *Config) string { return c.WriteTimeout.String() }},
	{"NEXUS_SHUTDOWN_TIMEOUT", func(c *Config) string { return c.ShutdownTimeout.String() }},
	{"NEXUS_SLM_CACHE_MAX_ENTRIES", func(c *Config) string { return fmt.Sprintf("%d", c.SLMCacheMaxEntries) }},
	{"NEXUS_SLMCACHE_MAX_STALE", func(c *Config) string { return fmt.Sprintf("%d", c.SLMCacheMaxStale) }},
	{"NEXUS_SLMCACHE_SEMANTIC_SCAN_LIMIT", func(c *Config) string { return fmt.Sprintf("%d", c.SLMCacheSemanticScanLimit) }},
	{"NEXUS_SLMCACHE_SIMILARITY_THRESHOLD", func(c *Config) string { return fmt.Sprintf("%g", c.SLMCacheSemanticThreshold) }},
	{"NEXUS_SLMCACHE_STALE_CLEANUP_THRESHOLD", func(c *Config) string { return fmt.Sprintf("%d", c.SLMCacheStaleCleanupThreshold) }},
	{"NEXUS_SLM_CACHE_TTL", func(c *Config) string { return c.SLMCacheTTL.String() }},
	{"NEXUS_SLM_CONFIDENCE_THRESHOLD", func(c *Config) string { return fmt.Sprintf("%g", c.SLMConfidenceThreshold) }},
	{"NEXUS_SLM_TIMEOUT", func(c *Config) string { return c.SLMTimeout.String() }},
	{"NEXUS_STATUS_PUBLIC", func(c *Config) string { return fmt.Sprintf("%t", c.StatusPublic) }},
	{"NEXUS_TELEMETRY_BUFFER_SIZE", func(c *Config) string { return fmt.Sprintf("%d", c.TelemetryBufferSize) }},
	{"NEXUS_TELEMETRY_FLUSH_INTERVAL", func(c *Config) string { return c.TelemetryFlushInterval.String() }},
	{"NEXUS_TELEMETRY_MAX_BYTES", func(c *Config) string { return fmt.Sprintf("%d", c.TelemetryMaxBytes) }},
	{"NEXUS_TELEMETRY_MAX_FILES", func(c *Config) string { return fmt.Sprintf("%d", c.TelemetryMaxFiles) }},
	{"NEXUS_TELEMETRY_PATH", func(c *Config) string { return c.TelemetryPath }},
	{"NEXUS_TLS_ENABLED", func(c *Config) string { return fmt.Sprintf("%t", c.TLSEnabled) }},
	{"NEXUS_TOKEN_GUARDRAIL", func(c *Config) string { return fmt.Sprintf("%d", c.TokenGuardrail) }},
	{"NEXUS_TOON_UNFENCED", func(c *Config) string { return fmt.Sprintf("%t", c.TOONUnfenced) }},
	{"NEXUS_TRACING_BATCH_SIZE", func(c *Config) string { return fmt.Sprintf("%d", c.TracingBatchSize) }},
	{"NEXUS_TRACING_ENDPOINT", func(c *Config) string { return c.TracingEndpoint }},
	{"NEXUS_TRACING_QUEUE_SIZE", func(c *Config) string { return fmt.Sprintf("%d", c.TracingQueueSize) }},
	{"NEXUS_TRACING_SAMPLE_RATE", func(c *Config) string { return fmt.Sprintf("%g", c.TracingSampleRate) }},
	{"NEXUS_TRACING_TIMEOUT", func(c *Config) string { return c.TracingTimeout.String() }},
	{"NEXUS_TRUSTED_PROXIES", func(c *Config) string { return c.TrustedProxiesRaw }},
	{"NEXUS_VAULT_ADDR", func(c *Config) string { return c.VaultAddr }},
	{"NEXUS_VAULT_PATH", func(c *Config) string { return c.VaultPath }},
	{"NEXUS_VAULT_ROLE", func(c *Config) string { return c.VaultRole }},
	{"NEXUS_VAULT_TOKEN", func(c *Config) string { return redact(c.VaultToken) }},
	{"NEXUS_ZAI_API_KEY", func(c *Config) string { return redact(c.ZAIKey) }},
	{"NEXUS_ZAI_COST_PER_1K", func(c *Config) string { return fmt.Sprintf("%g", c.ZAICostPer1K) }},
	{"NEXUS_ZAI_MODEL", func(c *Config) string { return c.ZAIModel }},
	{"NEXUS_ZAI_URL", func(c *Config) string { return c.ZAIURL }},
	// Model aliases is a JSON map - show it as a single string
	{"NEXUS_MODEL_ALIASES", func(c *Config) string {
		if c.ModelAliases == nil {
			return ""
		}
		b, _ := json.Marshal(c.ModelAliases)
		return string(b)
	}},
	{"NEXUS_MODEL_ALIASES_STRICT", func(c *Config) string { return fmt.Sprintf("%t", c.ModelAliasesStrict) }},
	{"NEXUS_ANTHROPIC_CACHE_MIN_SYSTEM_CHARS", func(c *Config) string { return fmt.Sprintf("%d", c.AnthropicCacheMinSystemChars) }},
	{"NEXUS_ARBITER_CACHE_MAX_ENTRIES", func(c *Config) string { return fmt.Sprintf("%d", c.ArbiterCacheMaxEntries) }},
	{"NEXUS_ARBITER_CACHE_TTL", func(c *Config) string { return c.ArbiterCacheTTL.String() }},
	{"NEXUS_ARBITER_TIMEOUT", func(c *Config) string { return c.ArbiterTimeout.String() }},
	{"NEXUS_COHERE_API_KEY", func(c *Config) string { return redact(c.CohereAPIKey) }},
	{"NEXUS_EMBEDDING_MODEL", func(c *Config) string { return c.EmbeddingModel }},
	{"NEXUS_INIT_PROFILE", func(c *Config) string { return c.InitProfile }},
	{"NEXUS_QUALITY_CONCURRENCY", func(c *Config) string { return fmt.Sprintf("%d", c.QualityConcurrency) }},
	{"NEXUS_QUALITY_DROPPED_RING_SIZE", func(c *Config) string { return fmt.Sprintf("%d", c.QualityDroppedRingSize) }},
	{"NEXUS_QUALITY_QUEUE", func(c *Config) string { return fmt.Sprintf("%d", c.QualityQueueDepth) }},
	{"NEXUS_QUALITY_STDERR_CAP", func(c *Config) string { return fmt.Sprintf("%d", c.QualityStderrCap) }},
	{"NEXUS_QUALITY_TIMEOUT", func(c *Config) string { return c.QualityTimeout.String() }},
	{"NEXUS_ROUTING_CONTEXT_CHARS", func(c *Config) string { return fmt.Sprintf("%d", c.RoutingContextChars) }},
	{"NEXUS_ROUTING_CONTEXT_TURNS", func(c *Config) string { return fmt.Sprintf("%d", c.RoutingContextTurns) }},
}

// regexpStrings renders a []*regexp.Regexp slice as a comma-separated string
// of the strings matched (one pattern per entry).
func regexpStrings(re []*regexp.Regexp) string {
	if len(re) == 0 {
		return ""
	}
	var parts []string
	for _, r := range re {
		parts = append(parts, r.String())
	}
	return strings.Join(parts, ",")
}

// redact returns the input string with all but the first and last 4 chars
// replaced by "●", unless the string is shorter than 8 chars in which case
// it returns "●●●●●●●●".
func redact(s string) string {
	if len(s) <= 8 {
		return "●●●●●●●●"
	}
	if len(s) <= 12 {
		return s[:4] + strings.Repeat("●", len(s)-4)
	}
	return s[:4] + strings.Repeat("●", len(s)-8) + s[len(s)-4:]
}

// ShowFields returns all resolved configuration fields with their sources
// and hot-reload status. fileCfg is the raw YAML config map (nil if no file
// was loaded). The returned slice is sorted alphabetically by env var name.
func (c *Config) ShowFields(fileCfg map[string]string) []ConfigField {
	var fields []ConfigField
	for _, f := range allEnvFields {
		fields = append(fields, ConfigField{
			Key:           f.envKey,
			Value:         f.getValue(c),
			Source:        fieldSource(f.envKey, fileCfg),
			HotReloadable: IsHotReloadable(f.envKey),
		})
	}
	return fields
}
