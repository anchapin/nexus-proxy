// Package config loads runtime configuration from environment variables
// with optional layered overrides from a YAML config file (issue #31).
//
// Resolution order: env var > config file > built-in default.
//
// All values have safe defaults so the binary boots in development with a
// local Ollama instance. Secrets (FRONTIER_API_KEY) must be supplied via env
// in any non-development deployment — the YAML file can reference them
// via ${VAR} expansion so they never appear on disk.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime knobs for the proxy. A zero value is invalid;
// always go through Load.
type Config struct {
	// ConfigFile is the path to the YAML config file that was loaded
	// (after --config / NEXUS_CONFIG / CWD discovery), or "" when no
	// file was found. Reported in the boot log so operators can see
	// which source the binary is reading (issue #31).
	ConfigFile string

	// HTTP server
	Addr string // ":8000"

	// Inbound proxy authentication (issue #37). When at least one key
	// is configured, inbound requests must present a valid
	// `Authorization: Bearer <key>` or `X-API-Key` header or the
	// proxy returns 401 with an OpenAI-compatible error envelope.
	// /healthz stays exempt so orchestrator liveness probes keep
	// working. When neither variable is set, ProxyAuthEnabled is
	// false and the proxy behaves identically to today (zero
	// breaking change for localhost dev).
	ProxyAPIKey      string   // single key from NEXUS_PROXY_API_KEY
	ProxyAPIKeys     []string // comma-separated list from NEXUS_PROXY_API_KEYS (rotation)
	ProxyAuthEnabled bool     // true when at least one key is non-empty

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

	// Routing
	TokenGuardrail int           // estimated tokens above this force frontier (6000)
	SLMTimeout     time.Duration // Qwen3-Coder routing timeout (8s)
	FusionTimeout  time.Duration // per-panel-member fetch timeout (120s)
	CascadeTimeout time.Duration // per-attempt timeout for cascade fallback (30s)
	ArbiterTimeout time.Duration // per-call timeout for the fusion arbiter stream (60s)

	// VRAM-aware concurrency gate (issue #35). Caps the number of
	// RouteLocal requests (and the local panel member of
	// RouteFusion) that may dispatch local Ollama concurrently;
	// waiters queue-and-wait up to LocalQueueTimeout, after which
	// the chat handler fast-promotes to frontier via SkipLocal and
	// stamps X-Nexus-Overflow: true on the response. LocalMaxConcurrent
	// <= 0 disables the gate (the chat handler treats a nil/disabled
	// Limiter as "unlimited", preserving the pre-#35 behaviour).
	LocalMaxConcurrent int           // max concurrent local route dispatches (2)
	LocalQueueTimeout  time.Duration // max wait for a slot before overflow promotion (5s)

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

	// HTTP connection pool (issue #34). Every outbound call —
	// chat upstream, SLM routing, RAG embedding, health polling,
	// VRAM probing, and the judge evaluator — shares a single
	// *http.Client wired with a custom *http.Transport sized for a
	// multi-agent local development workload. http.DefaultClient's
	// default Transport caps MaxIdleConnsPerHost=2, which serialises
	// the chat handler behind a tiny pool and pays handshake latency
	// on every call. The defaults below give the chat-class pool
	// 16 idle conns per host (typical: local Ollama) on a 90s
	// keep-alive window.
	//
	// Background pollers (health, VRAM) reuse a separate, lighter
	// client built by transport.NewProbe that caps
	// MaxIdleConnsPerHost=1 so the pollers do not collectively
	// reserve N idle slots on the primary pool.
	HTTPMaxIdleConns        int           // total idle conns across all hosts (100)
	HTTPMaxIdleConnsPerHost int           // idle conns reserved per host (16)
	HTTPMaxConnsPerHost     int           // total (in-flight+idle) conns per host; 0 == unlimited
	HTTPIdleConnTimeout     time.Duration // idle keep-alive window (90s)
	HTTPDisableKeepAlives   bool          // true disables HTTP keep-alive (rarely wanted)

	// Rate limiting (issue #38). A stdlib-only token-bucket limiter
	// caps both per-client-IP and aggregate request rates. All four
	// values default to zero (disabled), so the proxy behaves
	// identically to before when the operator does not configure them.
	RateLimitRPM           int     // per-client requests per minute (0 = disabled)
	RateLimitBurst         int     // per-client burst capacity (0 = RPM)
	GlobalRateLimitRPM     int     // aggregate RPM across all clients (0 = disabled)
	FrontierDailyBudgetUSD float64 // rolling 24h spend cap for RouteFrontier (0 = disabled)

	// Structured logging (issue #3). LogLevel maps NEXUS_LOG_LEVEL
	// ("debug" | "info" | "warn" | "error") to a slog.Level. LogFormat
	// maps NEXUS_LOG_FORMAT ("json" | "text") to a slog.Handler; json
	// is the production default, text is friendlier for local dev.
	LogLevel  slog.Level
	LogFormat LogFormat
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

// configKeys maps the YAML "section.key" paths to env var names. This
// is the one source of truth that bridges the structured YAML view
// and the existing NEXUS_* env-var surface; if you add a new config
// field, add it here AND to nexus.yaml.example.
//
// The parser emits lowercased "section.key" paths. Keys not present in
// this map are silently dropped from the file's contribution so a stray
// YAML entry (typo, leftover from an older schema) cannot break boot.
var configKeys = map[string]string{
	"server.addr":                         "NEXUS_ADDR",
	"server.max_body_bytes":               "NEXUS_MAX_BODY_BYTES",
	"log.level":                           "NEXUS_LOG_LEVEL",
	"log.format":                          "NEXUS_LOG_FORMAT",
	"ollama.url":                          "NEXUS_OLLAMA_URL",
	"ollama.router_model":                 "NEXUS_ROUTER_MODEL",
	"ollama.local_model":                  "NEXUS_LOCAL_MODEL",
	"ollama.embedding_model":              "NEXUS_EMBEDDING_MODEL",
	"frontier.url":                        "NEXUS_FRONTIER_URL",
	"frontier.model":                      "NEXUS_FRONTIER_MODEL",
	"frontier.api_key":                    "NEXUS_FRONTIER_API_KEY",
	"zai.url":                             "NEXUS_ZAI_URL",
	"zai.model":                           "NEXUS_ZAI_MODEL",
	"zai.api_key":                         "NEXUS_ZAI_API_KEY",
	"rag.examples_dir":                    "NEXUS_EXAMPLES_DIR",
	"rag.threshold":                       "NEXUS_RAG_THRESHOLD",
	"routing.token_guardrail":             "NEXUS_TOKEN_GUARDRAIL",
	"routing.slm_timeout":                 "NEXUS_SLM_TIMEOUT",
	"routing.fusion_timeout":              "NEXUS_FUSION_TIMEOUT",
	"routing.cascade_timeout":             "NEXUS_CASCADE_TIMEOUT",
	"routing.arbiter_timeout":             "NEXUS_ARBITER_TIMEOUT",
	"routing.local_max_concurrent":        "NEXUS_LOCAL_MAX_CONCURRENT",
	"routing.local_queue_timeout":         "NEXUS_LOCAL_QUEUE_TIMEOUT",
	"health.poll_interval":                "NEXUS_HEALTH_POLL_INTERVAL",
	"health.breaker_threshold":            "NEXUS_HEALTH_BREAKER_THRESHOLD",
	"health.probe_timeout":                "NEXUS_HEALTH_PROBE_TIMEOUT",
	"judge.url":                           "NEXUS_JUDGE_URL",
	"judge.model":                         "NEXUS_JUDGE_MODEL",
	"judge.api_key":                       "NEXUS_JUDGE_API_KEY",
	"judge.sample_rate":                   "NEXUS_JUDGE_SAMPLE_RATE",
	"judge.concurrency":                   "NEXUS_JUDGE_CONCURRENCY",
	"judge.queue":                         "NEXUS_JUDGE_QUEUE",
	"judge.timeout":                       "NEXUS_JUDGE_TIMEOUT",
	"judge.cost_per_1k":                   "NEXUS_JUDGE_COST_PER_1K",
	"telemetry.path":                      "NEXUS_TELEMETRY_PATH",
	"metrics.db_path":                     "NEXUS_METRICS_DB",
	"quality.concurrency":                 "NEXUS_QUALITY_CONCURRENCY",
	"quality.queue":                       "NEXUS_QUALITY_QUEUE",
	"quality.timeout":                     "NEXUS_QUALITY_TIMEOUT",
	"quality.stderr_cap":                  "NEXUS_QUALITY_STDERR_CAP",
	"probe.interval":                      "NEXUS_PROBE_INTERVAL",
	"probe.timeout":                       "NEXUS_PROBE_TIMEOUT",
	"probe.bytes_per_token":               "NEXUS_PROBE_BYTES_PER_TOKEN",
	"http.max_idle_conns":                 "NEXUS_HTTP_MAX_IDLE_CONNS",
	"http.max_idle_conns_per_host":        "NEXUS_HTTP_MAX_IDLE_CONNS_PER_HOST",
	"http.max_conns_per_host":             "NEXUS_HTTP_MAX_CONNS_PER_HOST",
	"http.idle_conn_timeout":              "NEXUS_HTTP_IDLE_CONN_TIMEOUT",
	"http.disable_keepalives":             "NEXUS_HTTP_DISABLE_KEEPALIVES",
	"ratelimit.per_client_rpm":            "NEXUS_RATE_LIMIT_RPM",
	"ratelimit.per_client_burst":          "NEXUS_RATE_LIMIT_BURST",
	"ratelimit.global_rpm":                "NEXUS_GLOBAL_RATE_LIMIT_RPM",
	"ratelimit.frontier_daily_budget_usd": "NEXUS_FRONTIER_DAILY_BUDGET_USD",
}

// fileMapFromKeys translates the parsed section.key map into an
// env-keyed map (only known keys are forwarded).
func fileMapFromKeys(parsed map[string]string) map[string]string {
	if parsed == nil {
		return nil
	}
	out := make(map[string]string, len(parsed))
	for k, v := range parsed {
		if envKey, ok := configKeys[k]; ok {
			out[envKey] = v
		}
	}
	return out
}

// resolveConfigPath walks the precedence chain (flag > env > CWD) and
// returns the path to load, or "" when no file is configured. It is
// split out from Load so tests can assert the chain without spinning
// up the full config struct.
func resolveConfigPath() string {
	if p := ConfigPathOverride(); p != "" {
		return p
	}
	if p := os.Getenv("NEXUS_CONFIG"); p != "" {
		return p
	}
	return DiscoverConfigFile()
}

// Load reads configuration from environment variables with layered
// overrides from a YAML config file (issue #31). Resolution order:
// env var > config file > built-in default. The config file is
// resolved via the precedence chain:
//
//  1. SetConfigPathOverride (from main.go's --config flag)
//  2. NEXUS_CONFIG env var
//  3. nexus.yaml / nexus.yml / nexus.json in CWD
//
// A missing file is non-fatal — the boot falls back to env-only, which
// matches the pre-issue-#31 behaviour exactly. A malformed file is
// fatal so operators notice the typo during boot instead of silently
// getting partial config.
func Load() (Config, error) {
	// 1. Resolve and load the config file (if any).
	path := resolveConfigPath()
	var fileMap map[string]string
	if path != "" {
		parsed, err := LoadFile(path)
		if err != nil {
			return Config{}, err
		}
		// LoadFile returns (nil, nil) for a missing file — graceful
		// degradation per issue #31 AC. Only known keys are forwarded
		// so unknown YAML entries never break boot.
		fileMap = fileMapFromKeys(parsed)
		if fileMap == nil {
			path = ""
		}
	}

	cfg := Config{
		ConfigFile:     path,
		Addr:           resolveString("NEXUS_ADDR", fileMap, ":8000"),
		OllamaURL:      strings.TrimRight(resolveString("NEXUS_OLLAMA_URL", fileMap, "http://localhost:11434"), "/"),
		RouterModel:    resolveString("NEXUS_ROUTER_MODEL", fileMap, "qwen3-coder:4b"),
		LocalModel:     resolveString("NEXUS_LOCAL_MODEL", fileMap, "qwen3-coder:8b"),
		EmbeddingModel: resolveString("NEXUS_EMBEDDING_MODEL", fileMap, "nomic-embed-text"),
		FrontierURL:    resolveString("NEXUS_FRONTIER_URL", fileMap, "https://api.openai.com/v1/chat/completions"),
		FrontierModel:  resolveString("NEXUS_FRONTIER_MODEL", fileMap, "gpt-4o"),
		FrontierKey:    resolveString("NEXUS_FRONTIER_API_KEY", fileMap, ""),
		ZAIURL:         resolveString("NEXUS_ZAI_URL", fileMap, "https://api.z.ai/v1/chat/completions"),
		ZAIModel:       resolveString("NEXUS_ZAI_MODEL", fileMap, "glm-4.6"),
		ZAIKey:         resolveString("NEXUS_ZAI_API_KEY", fileMap, ""),
		ExamplesDir:    resolveString("NEXUS_EXAMPLES_DIR", fileMap, "./few_shot_examples"),
		MetaPrompt:     defaultMetaPrompt,
		TOONNotice:     defaultTOONNotice,
		TelemetryPath:  resolveAllowEmpty("NEXUS_TELEMETRY_PATH", fileMap, "./nexus-telemetry.jsonl"),
		MetricsDBPath:  resolveAllowEmpty("NEXUS_METRICS_DB", fileMap, DefaultMetricsDBPath()),
	}

	// Inbound proxy authentication (issue #37). NEXUS_PROXY_API_KEY is
	// the single-key surface; NEXUS_PROXY_API_KEYS is a comma-separated
	// list for zero-downtime key rotation. Both contribute to the
	// accepted set. Secrets are read from env only (not the YAML file
	// map) so a key never lands on disk — this matches the existing
	// NEXUS_FRONTIER_API_KEY stance and the security intent of the
	// issue. resolveString still consults fileMap but these keys are
	// absent from configKeys, so fileMap never carries them.
	cfg.ProxyAPIKey = resolveString("NEXUS_PROXY_API_KEY", nil, "")
	cfg.ProxyAPIKeys = splitCSV(resolveString("NEXUS_PROXY_API_KEYS", nil, ""))
	cfg.ProxyAuthEnabled = len(cfg.ProxyAuthKeys()) > 0

	threshold, err := resolveFloat("NEXUS_RAG_THRESHOLD", fileMap, 0.55)
	if err != nil {
		return cfg, err
	}
	cfg.RAGThreshold = threshold

	guardrail, err := resolveInt("NEXUS_TOKEN_GUARDRAIL", fileMap, 6000)
	if err != nil {
		return cfg, err
	}
	cfg.TokenGuardrail = guardrail

	slmTimeout, err := resolveDuration("NEXUS_SLM_TIMEOUT", fileMap, 8*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.SLMTimeout = slmTimeout

	fusionTimeout, err := resolveDuration("NEXUS_FUSION_TIMEOUT", fileMap, 120*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.FusionTimeout = fusionTimeout

	cascadeTimeout, err := resolveDuration("NEXUS_CASCADE_TIMEOUT", fileMap, 30*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.CascadeTimeout = cascadeTimeout

	// Fusion arbiter synthesis (issue #12). Shorter than FusionTimeout
	// because the arbiter is doing synthesis, not generation — a slow
	// arbiter should not pin the whole request indefinitely.
	arbiterTimeout, err := resolveDuration("NEXUS_ARBITER_TIMEOUT", fileMap, 60*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.ArbiterTimeout = arbiterTimeout

	// VRAM-aware concurrency gate (issue #35). The chat handler
	// holds a slot for the lifetime of any RouteLocal request
	// (and the local panel member of RouteFusion) so concurrent
	// coding agents cannot collectively OOM Ollama. The queue
	// timeout is how long a request is willing to wait for a
	// slot before the handler fast-promotes to frontier and
	// stamps X-Nexus-Overflow: true. Both values default to
	// safe operator-friendly numbers; setting
	// NEXUS_LOCAL_MAX_CONCURRENT=0 disables the gate entirely
	// (the handler treats it as unlimited).
	localMax, err := resolveInt("NEXUS_LOCAL_MAX_CONCURRENT", fileMap, 2)
	if err != nil {
		return cfg, err
	}
	if localMax < 0 {
		// Negative is a config typo; clamp to disabled rather
		// than panic at boot. Operators who want "unlimited"
		// should set 0 explicitly per the issue spec.
		localMax = 0
	}
	cfg.LocalMaxConcurrent = localMax

	localQueue, err := resolveDuration("NEXUS_LOCAL_QUEUE_TIMEOUT", fileMap, 5*time.Second)
	if err != nil {
		return cfg, err
	}
	if localQueue < 0 {
		// Negative queue timeout is meaningless; fold into the
		// "try once and give up" semantics so a bad value at
		// least still rejects on overflow.
		localQueue = 0
	}
	cfg.LocalQueueTimeout = localQueue

	// Ollama health poller (issue #8). Defaults: 30s poll cadence,
	// 3-failure breaker, 5s per-probe HTTP timeout. Set
	// NEXUS_HEALTH_POLL_INTERVAL to "0" to disable polling entirely;
	// the chat handler then behaves as if Ollama is always healthy
	// (i.e. it will still try the local route on every request and
	// pay the upstream timeout if Ollama is down).
	healthPoll, err := resolveDuration("NEXUS_HEALTH_POLL_INTERVAL", fileMap, 30*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.HealthPollInterval = healthPoll

	healthBreaker, err := resolveInt("NEXUS_HEALTH_BREAKER_THRESHOLD", fileMap, 3)
	if err != nil {
		return cfg, err
	}
	cfg.HealthBreakerThreshold = healthBreaker

	healthProbe, err := resolveDuration("NEXUS_HEALTH_PROBE_TIMEOUT", fileMap, 5*time.Second)
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
	probeInterval, err := resolveDuration("NEXUS_PROBE_INTERVAL", fileMap, 60*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.ProbePollInterval = probeInterval

	probeTimeout, err := resolveDuration("NEXUS_PROBE_TIMEOUT", fileMap, 5*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.ProbeTimeout = probeTimeout

	probeBytes, err := resolveInt("NEXUS_PROBE_BYTES_PER_TOKEN", fileMap, 256*1024)
	if err != nil {
		return cfg, err
	}
	if probeBytes < 0 {
		probeBytes = 0
	}
	cfg.ProbeBytesPerToken = probeBytes
	cfg.ProbeEnabled = cfg.ProbePollInterval > 0

	// HTTP connection-pool tuning (issue #34). Defaults sized for a
	// multi-agent local-dev workload — chat handler + SLM + RAG +
	// judge all hitting the same Ollama host. The stdlib default of
	// MaxIdleConnsPerHost=2 is the very issue this fixes.
	httpMaxIdle, err := resolveInt("NEXUS_HTTP_MAX_IDLE_CONNS", fileMap, 100)
	if err != nil {
		return cfg, err
	}
	if httpMaxIdle < 0 {
		httpMaxIdle = 0
	}
	cfg.HTTPMaxIdleConns = httpMaxIdle

	httpMaxIdlePerHost, err := resolveInt("NEXUS_HTTP_MAX_IDLE_CONNS_PER_HOST", fileMap, 16)
	if err != nil {
		return cfg, err
	}
	if httpMaxIdlePerHost < 0 {
		httpMaxIdlePerHost = 0
	}
	cfg.HTTPMaxIdleConnsPerHost = httpMaxIdlePerHost

	// MaxConnsPerHost=0 is a valid value (== unlimited), so we don't
	// clamp negatives the way we do for the idle counters above.
	httpMaxPerHost, err := resolveInt("NEXUS_HTTP_MAX_CONNS_PER_HOST", fileMap, 0)
	if err != nil {
		return cfg, err
	}
	if httpMaxPerHost < 0 {
		httpMaxPerHost = 0
	}
	cfg.HTTPMaxConnsPerHost = httpMaxPerHost

	httpIdleTimeout, err := resolveDuration("NEXUS_HTTP_IDLE_CONN_TIMEOUT", fileMap, 90*time.Second)
	if err != nil {
		return cfg, err
	}
	if httpIdleTimeout < 0 {
		httpIdleTimeout = 0
	}
	cfg.HTTPIdleConnTimeout = httpIdleTimeout

	// Keep-alive toggle. Treated as a strict boolean to match the
	// rest of the config surface: "true" | "1" | "yes" | "on" => true
	// (case-insensitive); anything else => false. Empty stays at the
	// built-in default (false). We deliberately do not parse ints
	// here so operators do not have to memorise a "0 == off, 1 == on"
	// quirk that no other env var in this file uses.
	cfg.HTTPDisableKeepAlives = resolveBool("NEXUS_HTTP_DISABLE_KEEPALIVES", fileMap, false)

	// Hard request-body cap (issue #11). Default 1 MiB matches typical
	// OpenAI-compatible request sizes; the chat handler wraps r.Body
	// with http.MaxBytesReader so an oversized POST is rejected with
	// 413 before any allocation happens.
	maxBodyBytes, err := resolveInt("NEXUS_MAX_BODY_BYTES", fileMap, DefaultMaxBodyBytes)
	if err != nil {
		return cfg, err
	}
	cfg.MaxBodyBytes = maxBodyBytes

	// Judge (issue #15). Defaults: z.ai-style endpoint, sample 10% of
	// local-route successes, 2 concurrent workers, 30s per call. When
	// JudgeURL is unset we fall back to NEXUS_FRONTIER_URL so a stock
	// config still works.
	cfg.JudgeURL = resolveString("NEXUS_JUDGE_URL", fileMap, "https://api.z.ai/v1/chat/completions")
	if !isConfigSourceSet("NEXUS_JUDGE_URL", fileMap) {
		cfg.JudgeURL = cfg.FrontierURL
	}
	cfg.JudgeModel = resolveString("NEXUS_JUDGE_MODEL", fileMap, cfg.FrontierModel)
	cfg.JudgeAPIKey = resolveString("NEXUS_JUDGE_API_KEY", fileMap, cfg.FrontierKey)

	sampleRate, err := resolveFloat("NEXUS_JUDGE_SAMPLE_RATE", fileMap, 0.1)
	if err != nil {
		return cfg, err
	}
	cfg.JudgeSampleRate = sampleRate

	concurrency, err := resolveInt("NEXUS_JUDGE_CONCURRENCY", fileMap, 2)
	if err != nil {
		return cfg, err
	}
	cfg.JudgeConcurrency = concurrency

	queueDepth, err := resolveInt("NEXUS_JUDGE_QUEUE", fileMap, 64)
	if err != nil {
		return cfg, err
	}
	cfg.JudgeQueueDepth = queueDepth

	judgeTimeout, err := resolveDuration("NEXUS_JUDGE_TIMEOUT", fileMap, 30*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.JudgeTimeout = judgeTimeout

	costRate, err := resolveFloat("NEXUS_JUDGE_COST_PER_1K", fileMap, 0.002)
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
	qualityConcurrency, err := resolveInt("NEXUS_QUALITY_CONCURRENCY", fileMap, 2)
	if err != nil {
		return cfg, err
	}
	cfg.QualityConcurrency = qualityConcurrency

	qualityQueueDepth, err := resolveInt("NEXUS_QUALITY_QUEUE", fileMap, 64)
	if err != nil {
		return cfg, err
	}
	cfg.QualityQueueDepth = qualityQueueDepth

	qualityTimeout, err := resolveDuration("NEXUS_QUALITY_TIMEOUT", fileMap, 60*time.Second)
	if err != nil {
		return cfg, err
	}
	cfg.QualityTimeout = qualityTimeout

	stderrCap, err := resolveInt("NEXUS_QUALITY_STDERR_CAP", fileMap, 2*1024)
	if err != nil {
		return cfg, err
	}
	cfg.QualityStderrCap = stderrCap

	cfg.QualityEnabled = cfg.QualityConcurrency > 0

	// Rate limiting + daily frontier budget (issue #38). All four
	// values default to zero (disabled). When RateLimitRPM > 0 the
	// proxy enforces a per-client-IP token bucket; when
	// GlobalRateLimitRPM > 0 the proxy enforces an aggregate bucket
	// checked before the per-client bucket. FrontierDailyBudgetUSD
	// gates only RouteFrontier (local routing is never budget-blocked).
	rateRPM, err := resolveInt("NEXUS_RATE_LIMIT_RPM", fileMap, 0)
	if err != nil {
		return cfg, err
	}
	cfg.RateLimitRPM = rateRPM

	rateBurst, err := resolveInt("NEXUS_RATE_LIMIT_BURST", fileMap, 0)
	if err != nil {
		return cfg, err
	}
	cfg.RateLimitBurst = rateBurst

	globalRPM, err := resolveInt("NEXUS_GLOBAL_RATE_LIMIT_RPM", fileMap, 0)
	if err != nil {
		return cfg, err
	}
	cfg.GlobalRateLimitRPM = globalRPM

	dailyBudget, err := resolveFloat("NEXUS_FRONTIER_DAILY_BUDGET_USD", fileMap, 0)
	if err != nil {
		return cfg, err
	}
	cfg.FrontierDailyBudgetUSD = dailyBudget

	// Structured logging (issue #3). Defaults match the production
	// expectation: JSON to stderr at info level. Operators flip on
	// debug by setting NEXUS_LOG_LEVEL=debug, and switch to a
	// human-friendly text handler with NEXUS_LOG_FORMAT=text.
	cfg.LogLevel = parseLogLevel(resolveString("NEXUS_LOG_LEVEL", fileMap, ""))
	cfg.LogFormat = parseLogFormat(resolveString("NEXUS_LOG_FORMAT", fileMap, ""))

	return cfg, nil
}

// FrontierEnabled reports whether a frontier API key is configured. The proxy
// still runs without one (fusion will degrade to local-only), but frontier
// routing will return 401s if attempted.
func (c Config) FrontierEnabled() bool { return c.FrontierKey != "" }

// ProxyAuthKeys returns the combined, de-duplicated, non-empty set of
// accepted inbound API keys (NEXUS_PROXY_API_KEY plus the entries of
// NEXUS_PROXY_API_KEYS). Returns nil when authentication is disabled,
// so cmd/nexus can pass the slice straight to auth.Middleware (which
// is a no-op for an empty slice). De-duplication is order-preserving
// so the single-key stays first, matching operator intuition.
func (c Config) ProxyAuthKeys() []string {
	seen := make(map[string]struct{})
	var out []string
	for _, k := range append([]string{c.ProxyAPIKey}, c.ProxyAPIKeys...) {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// splitCSV splits a comma-separated string into trimmed, non-empty
// fields. Used for NEXUS_PROXY_API_KEYS so operators can configure key
// rotation without a YAML array. Returns nil for an empty/blank input
// (which leaves ProxyAPIKeys nil and thus disables auth).
func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// IsLoopbackAddr reports whether addr binds only to the loopback
// interface. Used by the boot warning (issue #37) to detect when the
// proxy is exposed to the network without authentication.
//
// Addresses that bind all interfaces are NOT loopback:
//
//	":8000"        -> all interfaces (0.0.0.0)
//	"0.0.0.0:8000" -> all IPv4 interfaces
//	"[::]:8000"    -> all IPv6 interfaces
//
// Loopback examples:
//
//	"127.0.0.1:8000" -> IPv4 loopback
//	"[::1]:8000"     -> IPv6 loopback
//	"localhost:8000" -> loopback hostname
//
// An unresolvable hostname (other than "localhost") is treated as
// non-loopback so the boot warning errs on the side of firing.
func IsLoopbackAddr(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.TrimSpace(host)
	if host == "" {
		// ":8000" binds every interface.
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// Unknown hostname form (e.g. "nexus.local"); without a DNS
	// resolution we cannot prove loopback, so treat as exposed.
	return false
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

// TelemetryEnabled reports whether the on-disk recorder should be started.
// Disabled when TelemetryPath is empty.
func (c Config) TelemetryEnabled() bool { return c.TelemetryPath != "" }

// MetricsEnabled reports whether the SQLite metrics store should be
// opened. Disabled when MetricsDBPath is empty.
func (c Config) MetricsEnabled() bool { return c.MetricsDBPath != "" }

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

// resolveString returns env value when the env var is set and non-empty,
// then the YAML file value when present (even if empty — operators can
// explicitly clear a value with `key: ""`), then the built-in default.
func resolveString(key string, fileMap map[string]string, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	if v, ok := fileMap[key]; ok {
		return v
	}
	return def
}

// resolveAllowEmpty is like resolveString but treats env="" as set. Used
// for the telemetry path / metrics DB so operators can disable them by
// setting NEXUS_TELEMETRY_PATH="" or NEXUS_METRICS_DB="".
func resolveAllowEmpty(key string, fileMap map[string]string, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	if v, ok := fileMap[key]; ok {
		return v
	}
	return def
}

// resolveInt reads an int from env (non-empty), file, or default. A
// malformed value in either source produces an error so the boot fails
// loud instead of silently using a zero.
func resolveInt(key string, fileMap map[string]string, def int) (int, error) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("config: %s must be an integer: %w", key, err)
		}
		return n, nil
	}
	if v, ok := fileMap[key]; ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("config: %s in file must be an integer: %w", key, err)
		}
		return n, nil
	}
	return def, nil
}

// resolveFloat reads a float from env (non-empty), file, or default.
func resolveFloat(key string, fileMap map[string]string, def float64) (float64, error) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("config: %s must be a number: %w", key, err)
		}
		return f, nil
	}
	if v, ok := fileMap[key]; ok && v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("config: %s in file must be a number: %w", key, err)
		}
		return f, nil
	}
	return def, nil
}

// resolveDuration reads a time.Duration from env (non-empty), file, or default.
func resolveDuration(key string, fileMap map[string]string, def time.Duration) (time.Duration, error) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("config: %s must be a duration (e.g. 8s, 2m): %w", key, err)
		}
		return d, nil
	}
	if v, ok := fileMap[key]; ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("config: %s in file must be a duration: %w", key, err)
		}
		return d, nil
	}
	return def, nil
}

// resolveBool reads a boolean from env (non-empty), file, or default.
// Truthy spellings — case-insensitive — are: "true", "1", "yes", "on".
// Anything else is false. Operators who set NEXUS_HTTP_DISABLE_KEEPALIVES
// to a non-truthy value get the default (false), which is what they
// want on every host that has a working keep-alive.
func resolveBool(key string, fileMap map[string]string, def bool) bool {
	parse := func(raw string) (bool, bool) {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
		return false, false
	}
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, ok := parse(v); ok {
			return b
		}
		// Fall through to default; this matches the "ignore
		// typos, do not break boot" stance taken by parseLogLevel
		// and parseLogFormat for NEXUS_LOG_*.
	}
	if v, ok := fileMap[key]; ok && v != "" {
		if b, ok := parse(v); ok {
			return b
		}
	}
	return def
}

// isConfigSourceSet reports whether key was set via env (non-empty) OR
// present in fileMap. Used for "fall back if not set" logic that
// previously relied solely on os.Getenv.
func isConfigSourceSet(key string, fileMap map[string]string) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return true
	}
	if _, ok := fileMap[key]; ok {
		return true
	}
	return false
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
