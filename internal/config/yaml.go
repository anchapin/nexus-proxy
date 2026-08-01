package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/anchapin/nexus-proxy/internal/middleware"
	"github.com/anchapin/nexus-proxy/internal/providers"
	ragpkg "github.com/anchapin/nexus-proxy/internal/rag"
	"gopkg.in/yaml.v3"
)

// YAMLConfig is the decoded shape of a config.yaml file. Field names are
// lower-case snake_case aliases for the corresponding NEXUS_ env var
// (e.g. ollama_url, token_guardrail, fusion_timeout).
// Unset fields emit no error; LoadYAML fills those from env or hard defaults.
type YAMLConfig struct {
	// Server
	Addr            string `yaml:"addr"`
	ReadTimeout     string `yaml:"server_read_timeout"`
	WriteTimeout    string `yaml:"server_write_timeout"`
	IdleTimeout     string `yaml:"server_idle_timeout"`
	MaxHeaderBytes  int    `yaml:"server_max_header_bytes"`
	ShutdownTimeout string `yaml:"shutdown_timeout"`
	MaxBodyBytes    int    `yaml:"max_body_bytes"`
	TLSEnabled      bool   `yaml:"tls_enabled"`

	// Logging
	LogLevel  string `yaml:"log_level"`
	LogFormat string `yaml:"log_format"`

	// Debug
	Debug          bool `yaml:"debug"`
	DebugBodyBytes int  `yaml:"debug_body_bytes"`

	// Ollama
	OllamaURL      string `yaml:"ollama_url"`
	RouterModel    string `yaml:"router_model"`
	LocalModel     string `yaml:"local_model"`
	EmbeddingModel string `yaml:"embedding_model"`

	// Frontier
	FrontierURL       string  `yaml:"frontier_url"`
	FrontierModel     string  `yaml:"frontier_model"`
	FrontierKey       string  `yaml:"frontier_api_key"`
	FrontierCostPer1K float64 `yaml:"frontier_cost_per_1k"`

	// Z.ai
	ZAIURL       string  `yaml:"zai_url"`
	ZAIModel     string  `yaml:"zai_model"`
	ZAIKey       string  `yaml:"zai_api_key"`
	ZAICostPer1K float64 `yaml:"zai_cost_per_1k"`

	// Auth
	ProxyAPIKey  string `yaml:"proxy_api_key"`
	StatusPublic bool   `yaml:"status_public"`

	// Cost baseline
	CostBaselineProvider  string  `yaml:"cost_baseline_provider"`
	CostBaselineModel     string  `yaml:"cost_baseline_model"`
	CostBaselineRatePer1K float64 `yaml:"cost_baseline_rate_per_1k"`

	// Per-provider cost model (issue #1183)
	CostUseOutputTokens bool `yaml:"cost_use_output_tokens"`

	// Budget
	BudgetDailyLimit      float64 `yaml:"budget_daily_limit"`
	BudgetAlertEnabled    bool    `yaml:"budget_alert_enabled"`
	BudgetAlertThreshold  float64 `yaml:"budget_alert_threshold"`
	BudgetAlertWebhookURL string  `yaml:"budget_alert_webhook_url"`

	// Selector
	SelectorWindow          string  `yaml:"selector_window"`
	SelectorMinSamples      int     `yaml:"selector_min_samples"`
	SelectorRefreshInterval string  `yaml:"selector_refresh_interval"`
	ProviderTailWeight      float64 `yaml:"provider_tail_weight"`

	// Providers (issue #1185). An optional explicit list of frontier
	// providers with their adapter type. Mirrors the env-driven
	// NEXUS_PROVIDER_<NAME>_* surface; each entry's `type` is validated
	// against the providers package's allowed set.
	Providers []yamlProviderEntry `yaml:"providers"`

	// RAG
	ExamplesDir              string  `yaml:"examples_dir"`
	RAGThreshold             float64 `yaml:"rag_threshold"`
	EmbedderType             string  `yaml:"embedder_type"`
	EmbedderBaseURL          string  `yaml:"embedder_base_url"`
	CohereAPIKey             string  `yaml:"cohere_api_key"`
	RAGDBPath                string  `yaml:"rag_db_path"`
	RAGPollInterval          string  `yaml:"rag_poll_interval"`
	RAGEmbedCacheSize        int     `yaml:"rag_embed_cache_size"`
	RAGEmbedCacheTTL         string  `yaml:"rag_embed_cache_ttl"`
	RAGEmbedCacheWaitTimeout string  `yaml:"rag_embed_cache_wait_timeout"`
	RAGBatchSize             int     `yaml:"rag_batch_size"`

	// Routing
	TokenGuardrail                int     `yaml:"token_guardrail"`
	SLMTimeout                    string  `yaml:"slm_timeout"`
	SLMCacheMaxEntries            int     `yaml:"slm_cache_max_entries"`
	SLMCacheTTL                   string  `yaml:"slm_cache_ttl"`
	SLMCacheSemanticThreshold     float64 `yaml:"slm_cache_similarity_threshold"`
	SLMCacheMaxStale              int     `yaml:"slm_cache_max_stale"`               // issue #835
	SLMCacheStaleCleanupThreshold int     `yaml:"slm_cache_stale_cleanup_threshold"` // issue #1037
	SLMCacheSemanticScanLimit     int     `yaml:"slm_cache_semantic_scan_limit"`     // issue #933
	FusionTimeout                 string  `yaml:"fusion_timeout"`
	CascadeTimeout                string  `yaml:"cascade_timeout"`
	CascadeTimeoutFloor           string  `yaml:"cascade_timeout_floor"`         // issue #1175
	CascadeTimeoutCeiling         string  `yaml:"cascade_timeout_ceiling"`       // issue #1175
	CascadeTimeoutPer1kTokens     string  `yaml:"cascade_timeout_per_1k_tokens"` // issue #1175
	ArbiterTimeout                string  `yaml:"arbiter_timeout"`
	CascadeMaxResponseBytes       int     `yaml:"cascade_max_response_bytes"`
	MaxResponseBytes              int     `yaml:"max_response_bytes"`
	PoolBufferMaxBytes            int     `yaml:"pool_buffer_max_bytes"`

	// Fusion
	FusionProgressiveDelivery bool    `yaml:"fusion_progressive_delivery"`
	FusionAgreementThreshold  float64 `yaml:"fusion_agreement_threshold"`
	ArbiterCacheTTL           string  `yaml:"arbiter_cache_ttl"`
	ArbiterCacheMaxEntries    int     `yaml:"arbiter_cache_max_entries"`
	CacheWarmOnBoot           bool    `yaml:"cache_warm_on_boot"`
	CacheWarmLimit            int     `yaml:"cache_warm_limit"`

	// Health
	HealthPollInterval     string `yaml:"health_poll_interval"`
	HealthBreakerThreshold int    `yaml:"health_breaker_threshold"`
	HealthProbeTimeout     string `yaml:"health_probe_timeout"`

	// Probe
	ProbeInterval         string `yaml:"probe_interval"`
	ProbeTimeout          string `yaml:"probe_timeout"`
	ProbeBytesPerToken    int    `yaml:"probe_bytes_per_token"`
	ProbeThermalThreshold int    `yaml:"probe_thermal_threshold"`
	ProbeNVIDIAInterval   string `yaml:"probe_nvidia_interval"`

	// Local concurrency
	LocalMaxConcurrent    int    `yaml:"local_max_concurrent"`
	LocalVRAMBytesPerSlot int64  `yaml:"local_vram_bytes_per_slot"`
	LocalCooldown         string `yaml:"local_cooldown"`

	// Judge
	JudgeURL          string  `yaml:"judge_url"`
	JudgeModel        string  `yaml:"judge_model"`
	JudgeAPIKey       string  `yaml:"judge_api_key"`
	JudgeSampleRate   float64 `yaml:"judge_sample_rate"`
	JudgeConcurrency  int     `yaml:"judge_concurrency"`
	JudgeQueueDepth   int     `yaml:"judge_queue"`
	JudgeTimeout      string  `yaml:"judge_timeout"`
	JudgeCostPer1KUSD float64 `yaml:"judge_cost_per_1k"`
	JudgeDBPath       string  `yaml:"judge_db_path"`

	// Routing confidence
	RoutingConfidenceDB         string  `yaml:"routing_confidence_db"`
	RoutingConfidenceFloor      float64 `yaml:"routing_confidence_floor"`
	RoutingConfidenceCeiling    float64 `yaml:"routing_confidence_ceiling"`
	RoutingConfidenceMinSamples int     `yaml:"routing_confidence_min_samples"`
	RoutingConfidenceWindow     string  `yaml:"routing_confidence_window"`

	// Quality
	QualityConcurrency     int    `yaml:"quality_concurrency"`
	QualityQueueDepth      int    `yaml:"quality_queue"`
	QualityTimeout         string `yaml:"quality_timeout"`
	QualityStderrCap       int    `yaml:"quality_stderr_cap"`
	QualityDroppedRingSize int    `yaml:"quality_dropped_ring_size"`

	// Middleware
	MetaPrompt   string `yaml:"meta_prompt"`
	TOONNotice   string `yaml:"toon_notice"`
	TOONUnfenced bool   `yaml:"toon_unfenced"`

	// Prompt injection
	PromptInjectionMode string `yaml:"prompt_injection_mode"`
	InjectionScanRoles  string `yaml:"injection_scan_roles"`

	// Telemetry
	TelemetryPath          string `yaml:"telemetry_path"`
	TelemetryMaxBytes      int    `yaml:"telemetry_max_bytes"`
	TelemetryMaxFiles      int    `yaml:"telemetry_max_files"`
	TelemetryBufferSize    int    `yaml:"telemetry_buffer_size"`
	TelemetryFlushInterval string `yaml:"telemetry_flush_interval"`
	MetricsDBPath          string `yaml:"metrics_db_path"`
	// MetricsRetentionDays (issue #483) sets a TTL on the requests
	// table. 0 = disabled (grow without bound). Not hot-reloadable.
	MetricsRetentionDays int `yaml:"metrics_retention_days"`

	// OTLP retry/back-off parameters (issue #803).
	TracerMaxRetries     int    `yaml:"tracer_max_retries"`
	TracerRetryBaseDelay string `yaml:"tracer_retry_base_delay"`
	TracerRetryMaxDelay  string `yaml:"tracer_retry_max_delay"`

	// Models
	ModelsEndpointEnabled bool   `yaml:"models_endpoint_enabled"`
	ModelsCacheTTL        string `yaml:"models_cache_ttl"`

	// Model aliasing (issue #1184)
	ModelAliases       map[string]string `yaml:"model_aliases"`
	ModelAliasesStrict bool              `yaml:"model_aliases_strict"`

	// Built-in web dashboard (issue #1182)
	DashboardEndpointEnabled bool   `yaml:"dashboard_endpoint_enabled"`
	DashboardEndpoint        string `yaml:"dashboard_endpoint"`
	DashboardPublic          bool   `yaml:"dashboard_public"`

	// Trusted proxies
	TrustedProxies    string `yaml:"trusted_proxies"`
	RateLimitRPM      int    `yaml:"rate_limit_rpm"`
	RateLimitBurst    int    `yaml:"rate_limit_burst"`
	RateLimitByAPIKey bool   `yaml:"rate_limit_by_api_key"`

	// Auth brute-force protection (issue #840)
	AuthRateLimitRPM    int    `yaml:"auth_rate_limit_rpm"`
	AuthRateLimitBurst  int    `yaml:"auth_rate_limit_burst"`
	AuthRateLimitWindow string `yaml:"auth_rate_limit_window"`

	// Tracing
	TracingEndpoint   string  `yaml:"tracing_endpoint"`
	TracingTimeout    string  `yaml:"tracing_timeout"`
	TracingQueueSize  int     `yaml:"tracing_queue_size"`
	TracingBatchSize  int     `yaml:"tracing_batch_size"`
	TracingSampleRate float64 `yaml:"tracing_sample_rate"`

	// SSRF egress guard (issue #1174)
	EgressGuardEnabled bool   `yaml:"egress_block_private"`
	EgressAllowCIDRs   string `yaml:"egress_allow"`

	// Secret management (issue #1173)
	SecretBackend string `yaml:"secret_backend"`
	VaultAddr     string `yaml:"vault_addr"`
	VaultToken    string `yaml:"vault_token"`
	VaultRole     string `yaml:"vault_role"`
	VaultPath     string `yaml:"vault_path"`
	AWSSMPrefix   string `yaml:"awssm_prefix"`
	SecretRefresh string `yaml:"secret_refresh"`
}

// LoadYAML reads configuration from a YAML file at path, then overlays
// environment variables so env always wins. This lets operators manage
// a config.yaml in K8s ConfigMaps while still overriding individual values
// via Secrets or env vars in the pod spec.
//
// The YAML file may omit any field; missing values fall back to the
// same defaults as Load. An error is returned when the file cannot be
// read or contains malformed YAML.
func LoadYAML(path string) (Config, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Config{}, fmt.Errorf("config: cannot read config.yaml %q: %w", path, err)
	}

	// Pre-pass: inspect raw YAML values for boolean fields that must be
	// valid bools. This catches "toon_unfenced: maybe" before yaml.Unmarshal
	// silently leaves the field at its zero value (false), which would then
	// be incorrectly interpreted as the default.
	if err := validateRawBoolFields(data); err != nil {
		return Config{}, err
	}

	var yc YAMLConfig
	if err := yaml.Unmarshal(data, &yc); err != nil {
		return Config{}, fmt.Errorf("config: cannot unmarshal config.yaml %q: %w", path, err)
	}

	// Validate YAML-sourced fields that have constraints. These are
	// checked here so the error is returned before any env override runs.
	if err := yc.validate(); err != nil {
		return Config{}, err
	}

	// Seed from YAML values (these are the "soft defaults").
	cfg, err := yc.toConfig()
	if err != nil {
		return Config{}, err
	}

	// Now apply env overrides on top of the YAML seed.
	// The logic mirrors Load() but reads from os.Getenv directly
	// so that an explicitly set env var overrides the YAML value.

	// Server
	if v := os.Getenv("NEXUS_ADDR"); v != "" {
		cfg.Addr = v
	}
	if v := os.Getenv("NEXUS_SERVER_READ_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SERVER_READ_TIMEOUT: %w; see .env.example", err)
		}
		if d < 0 {
			return cfg, configError("NEXUS_SERVER_READ_TIMEOUT", "must not be negative", v, DefaultServerReadTimeout.String())
		}
		cfg.ReadTimeout = d
	}
	if v := os.Getenv("NEXUS_SERVER_WRITE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SERVER_WRITE_TIMEOUT: %w; see .env.example", err)
		}
		if d < 0 {
			return cfg, configError("NEXUS_SERVER_WRITE_TIMEOUT", "must not be negative", v, DefaultServerWriteTimeout.String())
		}
		cfg.WriteTimeout = d
	}
	if v := os.Getenv("NEXUS_SERVER_IDLE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SERVER_IDLE_TIMEOUT: %w; see .env.example", err)
		}
		if d < 0 {
			return cfg, configError("NEXUS_SERVER_IDLE_TIMEOUT", "must not be negative", v, DefaultServerIdleTimeout.String())
		}
		cfg.IdleTimeout = d
	}
	if v := os.Getenv("NEXUS_SERVER_MAX_HEADER_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SERVER_MAX_HEADER_BYTES: %w; see .env.example", err)
		}
		if n < 0 {
			return cfg, configError("NEXUS_SERVER_MAX_HEADER_BYTES", "must not be negative", v, strconv.Itoa(DefaultServerMaxHeaderBytes))
		}
		cfg.MaxHeaderBytes = n
	}
	if v := os.Getenv("NEXUS_MAX_BODY_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_MAX_BODY_BYTES: %w; see .env.example", err)
		}
		cfg.MaxBodyBytes = n
	}
	if v := os.Getenv("NEXUS_MAX_RESPONSE_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_MAX_RESPONSE_BYTES: %w; see .env.example", err)
		}
		cfg.MaxResponseBytes = n
	}
	if v := os.Getenv("NEXUS_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SHUTDOWN_TIMEOUT: %w; see .env.example", err)
		}
		if d < 0 {
			return cfg, configError("NEXUS_SHUTDOWN_TIMEOUT", "must not be negative", v, DefaultShutdownTimeout.String())
		}
		if d == 0 {
			d = DefaultShutdownTimeout
		}
		cfg.ShutdownTimeout = d
	}
	if v := os.Getenv("NEXUS_TRACING_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_TRACING_TIMEOUT: %w; see .env.example", err)
		}
		if d < 0 {
			return cfg, configError("NEXUS_TRACING_TIMEOUT", "must not be negative", v, DefaultTracingTimeout.String())
		}
		if d == 0 {
			d = DefaultTracingTimeout
		}
		cfg.TracingTimeout = d
	}
	if v := os.Getenv("NEXUS_TLS_ENABLED"); v != "" {
		cfg.TLSEnabled = parseBoolEnvStr(v, false)
	}

	// Logging
	if v := os.Getenv("NEXUS_LOG_LEVEL"); v != "" {
		logLevel, logLevelErr := parseLogLevel(v)
		if logLevelErr != nil {
			return cfg, logLevelErr
		}
		cfg.LogLevel = logLevel
	}
	if v := os.Getenv("NEXUS_LOG_FORMAT"); v != "" {
		cfg.LogFormat = parseLogFormat(v)
	}

	// Debug
	if v := os.Getenv("NEXUS_DEBUG"); v != "" {
		cfg.Debug = parseBoolEnvStr(v, false)
	}
	if v := os.Getenv("NEXUS_DEBUG_BODY_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_DEBUG_BODY_BYTES: %w; see .env.example", err)
		}
		cfg.DebugBodyBytes = n
	}

	// Ollama
	if v := os.Getenv("NEXUS_OLLAMA_URL"); v != "" {
		cfg.OllamaURL = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("NEXUS_ROUTER_MODEL"); v != "" {
		cfg.RouterModel = v
	}
	if v := os.Getenv("NEXUS_LOCAL_MODEL"); v != "" {
		cfg.LocalModel = v
	}
	if v := os.Getenv("NEXUS_EMBEDDING_MODEL"); v != "" {
		cfg.EmbeddingModel = v
	}

	// Frontier
	if v := os.Getenv("NEXUS_FRONTIER_URL"); v != "" {
		cfg.FrontierURL = v
	}
	if v := os.Getenv("NEXUS_FRONTIER_MODEL"); v != "" {
		cfg.FrontierModel = v
	}
	if v := os.Getenv("NEXUS_FRONTIER_API_KEY"); v != "" {
		cfg.FrontierKey = v
	}
	if v := os.Getenv("NEXUS_FRONTIER_COST_PER_1K"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_FRONTIER_COST_PER_1K: %w; see .env.example", err)
		}
		if f < 0 {
			f = 0
		}
		cfg.FrontierCostPer1K = f
	}

	// Z.ai
	if v := os.Getenv("NEXUS_ZAI_URL"); v != "" {
		cfg.ZAIURL = v
	}
	if v := os.Getenv("NEXUS_ZAI_MODEL"); v != "" {
		cfg.ZAIModel = v
	}
	if v := os.Getenv("NEXUS_ZAI_API_KEY"); v != "" {
		cfg.ZAIKey = v
	}
	if v := os.Getenv("NEXUS_ZAI_COST_PER_1K"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_ZAI_COST_PER_1K: %w; see .env.example", err)
		}
		if f < 0 {
			f = 0
		}
		cfg.ZAICostPer1K = f
	}

	// Auth
	if v := os.Getenv("NEXUS_PROXY_API_KEY"); v != "" {
		cfg.ProxyAPIKey = v
	}
	if v := os.Getenv("NEXUS_STATUS_PUBLIC"); v != "" {
		cfg.StatusPublic = parseBoolEnvStr(v, false)
	}

	// Cost baseline
	if v := os.Getenv("NEXUS_COST_BASELINE_PROVIDER"); v != "" {
		cfg.CostBaselineProvider = v
	}
	if v := os.Getenv("NEXUS_COST_BASELINE_MODEL"); v != "" {
		cfg.CostBaselineModel = v
	}
	if v := os.Getenv("NEXUS_COST_BASELINE_RATE_PER_1K"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_COST_BASELINE_RATE_PER_1K: %w; see .env.example", err)
		}
		if f < 0 {
			f = 0
		}
		cfg.CostBaselineRatePer1K = f
	}

	// Per-provider cost model with input/output token split (issue #1183).
	if v := os.Getenv("NEXUS_COST_USE_OUTPUT_TOKENS"); v != "" {
		cfg.CostUseOutputTokens = parseBoolEnvStr(v, false)
	}

	// Budget
	if v := os.Getenv("NEXUS_BUDGET_DAILY_LIMIT"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_BUDGET_DAILY_LIMIT: %w; see .env.example", err)
		}
		if f < 0 {
			f = 0
		}
		cfg.BudgetDailyLimit = f
	}
	if v := os.Getenv("NEXUS_BUDGET_ALERT_ENABLED"); v != "" {
		cfg.BudgetAlertEnabled = parseBoolEnvStr(v, false)
	}
	if v := os.Getenv("NEXUS_BUDGET_ALERT_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_BUDGET_ALERT_THRESHOLD: %w; see .env.example", err)
		}
		cfg.BudgetAlertThreshold = clampFloat(f, 0, 1)
	}
	if v := os.Getenv("NEXUS_BUDGET_ALERT_WEBHOOK_URL"); v != "" {
		cfg.BudgetAlertWebhookURL = v
	}

	// Selector
	if v := os.Getenv("NEXUS_SELECTOR_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SELECTOR_WINDOW: %w; see .env.example", err)
		}
		if d < 0 {
			d = 0
		}
		cfg.SelectorWindow = d
	}
	if v := os.Getenv("NEXUS_SELECTOR_MIN_SAMPLES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SELECTOR_MIN_SAMPLES: %w; see .env.example", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.SelectorMinSamples = n
	}
	if v := os.Getenv("NEXUS_SELECTOR_REFRESH"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SELECTOR_REFRESH: %w; see .env.example", err)
		}
		if d < 0 {
			d = 0
		}
		cfg.SelectorRefreshInterval = d
	}
	if v := os.Getenv("NEXUS_PROVIDER_TAIL_WEIGHT"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_PROVIDER_TAIL_WEIGHT: %w; see .env.example", err)
		}
		if f < 0 || f > 1 {
			return cfg, configError("NEXUS_PROVIDER_TAIL_WEIGHT", "must be a number in [0,1]", v, "0")
		}
		cfg.ProviderTailWeight = f
	}

	// RAG
	if v := os.Getenv("NEXUS_EXAMPLES_DIR"); v != "" {
		cfg.ExamplesDir = v
	}
	if v := os.Getenv("NEXUS_RAG_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_RAG_THRESHOLD: %w; see .env.example", err)
		}
		cfg.RAGThreshold = f
	}
	if v := os.Getenv("NEXUS_EMBEDDER_TYPE"); v != "" {
		cfg.EmbedderType = ragpkg.EmbedderType(strings.ToLower(strings.TrimSpace(v)))
	}
	if v := os.Getenv("NEXUS_EMBEDDER_BASE_URL"); v != "" {
		cfg.EmbedderBaseURL = v
	}
	if v := os.Getenv("NEXUS_COHERE_API_KEY"); v != "" {
		cfg.CohereAPIKey = v
	}
	if v := os.Getenv("NEXUS_RAG_DB"); v != "" {
		cfg.RAGDBPath = v
	}
	if v := os.Getenv("NEXUS_RAG_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_RAG_POLL_INTERVAL: %w; see .env.example", err)
		}
		if d < 0 {
			d = 0
		}
		cfg.RAGPollInterval = d
	}
	if v := os.Getenv("NEXUS_RAG_EMBED_CACHE_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_RAG_EMBED_CACHE_SIZE: %w; see .env.example", err)
		}
		cfg.RAGEmbedCacheSize = n
	}
	if v := os.Getenv("NEXUS_RAG_EMBED_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_RAG_EMBED_CACHE_TTL: %w; see .env.example", err)
		}
		cfg.RAGEmbedCacheTTL = d
	}
	if v := os.Getenv("NEXUS_RAG_EMBED_CACHE_WAIT_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_RAG_EMBED_CACHE_WAIT_TIMEOUT: %w; see .env.example", err)
		}
		cfg.RAGEmbedCacheWaitTimeout = d
	}
	if v := os.Getenv("NEXUS_RAG_BATCH_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_RAG_BATCH_SIZE: %w; see .env.example", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.RAGBatchSize = n
	}

	// Routing
	if v := os.Getenv("NEXUS_TOKEN_GUARDRAIL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_TOKEN_GUARDRAIL: %w; see .env.example", err)
		}
		cfg.TokenGuardrail = n
	}
	if v := os.Getenv("NEXUS_SLM_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SLM_TIMEOUT: %w; see .env.example", err)
		}
		cfg.SLMTimeout = d
	}
	if v := os.Getenv("NEXUS_SLM_CACHE_MAX_ENTRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SLM_CACHE_MAX_ENTRIES: %w; see .env.example", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.SLMCacheMaxEntries = n
	}
	if v := os.Getenv("NEXUS_SLM_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SLM_CACHE_TTL: %w; see .env.example", err)
		}
		cfg.SLMCacheTTL = d
	}
	if v := os.Getenv("NEXUS_SLMCACHE_SIMILARITY_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SLMCACHE_SIMILARITY_THRESHOLD: %w; see .env.example", err)
		}
		cfg.SLMCacheSemanticThreshold = clampFloat(f, 0, 1)
	}
	if v := os.Getenv("NEXUS_SLMCACHE_MAX_STALE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SLMCACHE_MAX_STALE: %w; see .env.example", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.SLMCacheMaxStale = n
	}
	if v := os.Getenv("NEXUS_SLMCACHE_STALE_CLEANUP_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SLMCACHE_STALE_CLEANUP_THRESHOLD: %w; see .env.example", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.SLMCacheStaleCleanupThreshold = n
	}
	if v := os.Getenv("NEXUS_SLMCACHE_SEMANTIC_SCAN_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SLMCACHE_SEMANTIC_SCAN_LIMIT: %w; see .env.example", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.SLMCacheSemanticScanLimit = n
	}
	if v := os.Getenv("NEXUS_FUSION_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_FUSION_TIMEOUT: %w; see .env.example", err)
		}
		cfg.FusionTimeout = d
	}
	if v := os.Getenv("NEXUS_CASCADE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_CASCADE_TIMEOUT: %w; see .env.example", err)
		}
		cfg.CascadeTimeout = d
	}
	if v := os.Getenv("NEXUS_CASCADE_TIMEOUT_FLOOR"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_CASCADE_TIMEOUT_FLOOR: %w", err)
		}
		cfg.CascadeTimeoutFloor = d
	}
	if v := os.Getenv("NEXUS_CASCADE_TIMEOUT_CEILING"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_CASCADE_TIMEOUT_CEILING: %w", err)
		}
		cfg.CascadeTimeoutCeiling = d
	}
	if v := os.Getenv("NEXUS_CASCADE_TIMEOUT_PER_1K_TOKENS"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_CASCADE_TIMEOUT_PER_1K_TOKENS: %w", err)
		}
		cfg.CascadeTimeoutPer1kTokens = d
	}
	if v := os.Getenv("NEXUS_ARBITER_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_ARBITER_TIMEOUT: %w; see .env.example", err)
		}
		cfg.ArbiterTimeout = d
	}
	if v := os.Getenv("NEXUS_CASCADE_MAX_RESPONSE_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_CASCADE_MAX_RESPONSE_BYTES: %w; see .env.example", err)
		}
		cfg.CascadeMaxResponseBytes = n
	}
	if v := os.Getenv("NEXUS_POOL_BUFFER_MAX_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_POOL_BUFFER_MAX_BYTES: %w", err)
		}
		cfg.PoolBufferMaxBytes = n
	}

	// Fusion
	if v := os.Getenv("NEXUS_FUSION_PROGRESSIVE"); v != "" {
		cfg.FusionProgressiveDelivery = parseBoolEnvStr(v, true)
	}
	if v := os.Getenv("NEXUS_FUSION_AGREEMENT_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_FUSION_AGREEMENT_THRESHOLD: %w; see .env.example", err)
		}
		if f < 0 || f > 1 {
			return cfg, configError("NEXUS_FUSION_AGREEMENT_THRESHOLD", "must be a number in [0,1]", v, "0.85")
		}
		cfg.FusionAgreementThreshold = f
	}
	if v := os.Getenv("NEXUS_ARBITER_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_ARBITER_CACHE_TTL: %w; see .env.example", err)
		}
		cfg.ArbiterCacheTTL = d
	}
	if v := os.Getenv("NEXUS_ARBITER_CACHE_MAX_ENTRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_ARBITER_CACHE_MAX_ENTRIES: %w; see .env.example", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.ArbiterCacheMaxEntries = n
	}

	// Arbiter cache boot-time pre-warming (issue #1176)
	if v := os.Getenv("NEXUS_CACHE_WARM_ON_BOOT"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_CACHE_WARM_ON_BOOT: %w", err)
		}
		cfg.CacheWarmOnBoot = b
	}
	if v := os.Getenv("NEXUS_CACHE_WARM_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_CACHE_WARM_LIMIT: %w", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.CacheWarmLimit = n
	}

	// Health
	if v := os.Getenv("NEXUS_HEALTH_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_HEALTH_POLL_INTERVAL: %w; see .env.example", err)
		}
		cfg.HealthPollInterval = d
	}
	if v := os.Getenv("NEXUS_HEALTH_BREAKER_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_HEALTH_BREAKER_THRESHOLD: %w; see .env.example", err)
		}
		cfg.HealthBreakerThreshold = n
	}
	if v := os.Getenv("NEXUS_HEALTH_PROBE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_HEALTH_PROBE_TIMEOUT: %w; see .env.example", err)
		}
		cfg.HealthProbeTimeout = d
	}

	// Probe
	if v := os.Getenv("NEXUS_PROBE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_PROBE_INTERVAL: %w; see .env.example", err)
		}
		cfg.ProbePollInterval = d
		cfg.ProbeEnabled = cfg.ProbePollInterval > 0
	}
	if v := os.Getenv("NEXUS_PROBE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_PROBE_TIMEOUT: %w; see .env.example", err)
		}
		cfg.ProbeTimeout = d
	}
	if v := os.Getenv("NEXUS_PROBE_BYTES_PER_TOKEN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_PROBE_BYTES_PER_TOKEN: %w; see .env.example", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.ProbeBytesPerToken = n
	}
	if v := os.Getenv("NEXUS_PROBE_THERMAL_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_PROBE_THERMAL_THRESHOLD: %w; see .env.example", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.ProbeThermalThreshold = n
	}
	if v := os.Getenv("NEXUS_PROBE_NVIDIA_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_PROBE_NVIDIA_INTERVAL: %w", err)
		}
		if d < 0 {
			d = 0
		}
		cfg.ProbeNVIDIAInterval = d
	}

	// Local concurrency
	if v := os.Getenv("NEXUS_LOCAL_MAX_CONCURRENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_LOCAL_MAX_CONCURRENT: %w; see .env.example", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.LocalMaxConcurrent = n
	}
	if v := os.Getenv("NEXUS_LOCAL_VRAM_BYTES_PER_SLOT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_LOCAL_VRAM_BYTES_PER_SLOT: %w; see .env.example", err)
		}
		if n < 0 {
			n = int(DefaultLocalVRAMBytesPerSlot)
		}
		cfg.LocalVRAMBytesPerSlot = int64(n)
	}
	if v := os.Getenv("NEXUS_LOCAL_COOLDOWN"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_LOCAL_COOLDOWN: %w; see .env.example", err)
		}
		if d < 0 {
			d = 0
		}
		cfg.LocalCooldown = d
	}

	// Judge
	if v := os.Getenv("NEXUS_JUDGE_URL"); v != "" {
		cfg.JudgeURL = v
	}
	if v := os.Getenv("NEXUS_JUDGE_MODEL"); v != "" {
		cfg.JudgeModel = v
	}
	if v := os.Getenv("NEXUS_JUDGE_API_KEY"); v != "" {
		cfg.JudgeAPIKey = v
	}
	if v := os.Getenv("NEXUS_JUDGE_SAMPLE_RATE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_JUDGE_SAMPLE_RATE: %w; see .env.example", err)
		}
		cfg.JudgeSampleRate = f
	}
	if v := os.Getenv("NEXUS_JUDGE_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_JUDGE_CONCURRENCY: %w; see .env.example", err)
		}
		cfg.JudgeConcurrency = n
	}
	if v := os.Getenv("NEXUS_JUDGE_QUEUE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_JUDGE_QUEUE: %w; see .env.example", err)
		}
		cfg.JudgeQueueDepth = n
	}
	if v := os.Getenv("NEXUS_JUDGE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_JUDGE_TIMEOUT: %w; see .env.example", err)
		}
		cfg.JudgeTimeout = d
	}
	if v := os.Getenv("NEXUS_JUDGE_COST_PER_1K"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_JUDGE_COST_PER_1K: %w; see .env.example", err)
		}
		cfg.JudgeCostPer1KUSD = f
	}
	if v := os.Getenv("NEXUS_JUDGE_DB"); v != "" {
		cfg.JudgeDBPath = v
	}
	cfg.JudgeEnabled = cfg.JudgeSampleRate > 0 && cfg.JudgeURL != "" && cfg.JudgeModel != ""

	// Routing confidence
	if v := os.Getenv("NEXUS_ROUTING_CONFIDENCE_DB"); v != "" {
		cfg.RoutingConfidenceDB = v
	}
	if v := os.Getenv("NEXUS_ROUTING_CONFIDENCE_FLOOR"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_ROUTING_CONFIDENCE_FLOOR: %w; see .env.example", err)
		}
		cfg.RoutingConfidenceFloor = f
	}
	if v := os.Getenv("NEXUS_ROUTING_CONFIDENCE_CEILING"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_ROUTING_CONFIDENCE_CEILING: %w; see .env.example", err)
		}
		cfg.RoutingConfidenceCeiling = f
	}
	if v := os.Getenv("NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES: %w; see .env.example", err)
		}
		cfg.RoutingConfidenceMinSamples = n
	}
	if v := os.Getenv("NEXUS_ROUTING_CONFIDENCE_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_ROUTING_CONFIDENCE_WINDOW: %w; see .env.example", err)
		}
		cfg.RoutingConfidenceWindow = d
	}

	// Quality
	if v := os.Getenv("NEXUS_QUALITY_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_QUALITY_CONCURRENCY: %w; see .env.example", err)
		}
		cfg.QualityConcurrency = n
		cfg.QualityEnabled = cfg.QualityConcurrency > 0
	}
	if v := os.Getenv("NEXUS_QUALITY_QUEUE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_QUALITY_QUEUE: %w; see .env.example", err)
		}
		cfg.QualityQueueDepth = n
	}
	if v := os.Getenv("NEXUS_QUALITY_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_QUALITY_TIMEOUT: %w; see .env.example", err)
		}
		cfg.QualityTimeout = d
	}
	if v := os.Getenv("NEXUS_QUALITY_STDERR_CAP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_QUALITY_STDERR_CAP: %w; see .env.example", err)
		}
		cfg.QualityStderrCap = n
	}
	// Deprecated-key warnings are emitted centrally via the registry
	// (issue #1180). The #924 alias (NEXUS_QUALITY_DROPED_RING_SIZE)
	// is tracked there; value parsing still reads the current name.
	if v := os.Getenv("NEXUS_QUALITY_DROPPED_RING_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_QUALITY_DROPPED_RING_SIZE: %w; see .env.example", err)
		}
		cfg.QualityDroppedRingSize = n
	}

	// Middleware prompts
	if v := os.Getenv("NEXUS_META_PROMPT"); v != "" {
		cfg.MetaPrompt = v
	}
	if v := os.Getenv("NEXUS_TOON_NOTICE"); v != "" {
		cfg.TOONNotice = v
	}
	if v := os.Getenv("NEXUS_TOON_UNFENCED"); v != "" {
		cfg.TOONUnfenced = parseBoolEnvStr(v, true)
	}

	// Prompt injection
	if v := os.Getenv("NEXUS_PROMPT_INJECTION_MODE"); v != "" {
		cfg.PromptInjectionMode = middleware.ParseInjectionMode(v)
	}
	if v := os.Getenv("NEXUS_INJECTION_SCAN_ROLES"); v != "" {
		roles, unrecognized := parseInjectionScanRoles(v)
		cfg.InjectionScanRoles = roles
		if len(unrecognized) > 0 && len(roles) == 1 && roles[0] == "system" {
			slog.Warn("unrecognised injection scan role(s): falling back to [system]",
				slog.String("ignored", strings.Join(unrecognized, ",")),
			)
		}
	}

	// Telemetry
	if v := os.Getenv("NEXUS_TELEMETRY_PATH"); v != "" {
		cfg.TelemetryPath = v
	}
	if v := os.Getenv("NEXUS_TELEMETRY_MAX_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.TelemetryMaxBytes = n
		}
	}
	if v := os.Getenv("NEXUS_TELEMETRY_MAX_FILES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.TelemetryMaxFiles = n
		}
	}
	if v := os.Getenv("NEXUS_TELEMETRY_BUFFER_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.TelemetryBufferSize = n
		}
	}
	if v := os.Getenv("NEXUS_TELEMETRY_FLUSH_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.TelemetryFlushInterval = d
		}
	}
	if v := os.Getenv("NEXUS_METRICS_DB"); v != "" {
		cfg.MetricsDBPath = v
	}
	if v := os.Getenv("NEXUS_METRICS_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MetricsRetentionDays = n
		}
	}

	// OTLP retry/back-off parameters (issue #803).
	if v := os.Getenv("NEXUS_TRACING_MAX_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.TracerMaxRetries = n
		}
	}
	if v := os.Getenv("NEXUS_TRACING_RETRY_BASE_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.TracerRetryBaseDelay = d
		}
	}
	if v := os.Getenv("NEXUS_TRACING_RETRY_MAX_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.TracerRetryMaxDelay = d
		}
	}

	// Models
	if v := os.Getenv("NEXUS_MODELS_ENDPOINT"); v != "" {
		cfg.ModelsEndpointEnabled = parseBoolEnvStr(v, true)
	}
	if v := os.Getenv("NEXUS_MODELS_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_MODELS_CACHE_TTL: %w; see .env.example", err)
		}
		cfg.ModelsCacheTTL = d
	}

	// Model aliasing (issue #1184)
	if v := os.Getenv("NEXUS_MODEL_ALIASES"); v != "" {
		var aliases map[string]string
		if err := json.Unmarshal([]byte(v), &aliases); err != nil {
			return cfg, fmt.Errorf("config: NEXUS_MODEL_ALIASES: %w", err)
		}
		cfg.ModelAliases = aliases
	}
	if v := os.Getenv("NEXUS_MODEL_ALIASES_STRICT"); v != "" {
		cfg.ModelAliasesStrict = parseBoolEnvStr(v, false)
	}

	// Built-in web dashboard (issue #1182). Env overrides YAML; an
	// explicit empty NEXUS_DASHBOARD_PATH still falls back to the
	// /dashboard default so a blank value cannot unregister the route.
	if v := os.Getenv("NEXUS_DASHBOARD_ENDPOINT"); v != "" {
		cfg.DashboardEndpointEnabled = parseBoolEnvStr(v, false)
	}
	if v := os.Getenv("NEXUS_DASHBOARD_PATH"); v != "" {
		cfg.DashboardEndpoint = v
	}
	if v := os.Getenv("NEXUS_DASHBOARD_PUBLIC"); v != "" {
		cfg.DashboardPublic = parseBoolEnvStr(v, false)
	}

	// Trusted proxies
	if v := os.Getenv("NEXUS_TRUSTED_PROXIES"); v != "" {
		cfg.TrustedProxiesRaw = v
		parsed, err := parseTrustedProxies(v)
		if err != nil {
			return cfg, err
		}
		cfg.TrustedProxies = parsed
	}

	// Rate limit
	if v := os.Getenv("NEXUS_RATE_LIMIT_RPM"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_RATE_LIMIT_RPM: %w; see .env.example", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.RateLimitRPM = n
	}
	if v := os.Getenv("NEXUS_RATE_LIMIT_BURST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_RATE_LIMIT_BURST: %w; see .env.example", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.RateLimitBurst = n
	}
	if v := os.Getenv("NEXUS_RATE_LIMIT_BY_API_KEY"); v != "" {
		cfg.RateLimitByAPIKey = strings.ToLower(v) == "true" || v == "1"
	}

	// Auth brute-force protection (issue #840)
	if v := os.Getenv("NEXUS_AUTH_RATE_LIMIT_RPM"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_AUTH_RATE_LIMIT_RPM: %w; see .env.example", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.AuthRateLimitRPM = n
	}
	if v := os.Getenv("NEXUS_AUTH_RATE_LIMIT_BURST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_AUTH_RATE_LIMIT_BURST: %w; see .env.example", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.AuthRateLimitBurst = n
	}
	if v := os.Getenv("NEXUS_AUTH_RATE_LIMIT_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_AUTH_RATE_LIMIT_WINDOW: %w; see .env.example", err)
		}
		if d < 0 {
			d = 0
		}
		cfg.AuthRateLimitWindow = d
	}

	// Tracing
	if v := os.Getenv("NEXUS_TRACING_ENDPOINT"); v != "" {
		cfg.TracingEndpoint = v
	}
	if v := os.Getenv("NEXUS_TRACING_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_TRACING_TIMEOUT: %w; see .env.example", err)
		}
		if d < 0 {
			d = 10 * time.Second
		}
		cfg.TracingTimeout = d
	}
	if v := os.Getenv("NEXUS_TRACING_QUEUE_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_TRACING_QUEUE_SIZE: %w; see .env.example", err)
		}
		if n < 0 {
			n = 256
		}
		cfg.TracingQueueSize = n
	}
	if v := os.Getenv("NEXUS_TRACING_BATCH_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_TRACING_BATCH_SIZE: %w; see .env.example", err)
		}
		if n < 1 {
			n = 64
		}
		cfg.TracingBatchSize = n
	}
	if v := os.Getenv("NEXUS_TRACING_SAMPLE_RATE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_TRACING_SAMPLE_RATE: %w; see .env.example", err)
		}
		if f < 0 {
			f = 0
		}
		if f > 1 {
			f = 1
		}
		cfg.TracingSampleRate = f
	}

	// SSRF egress guard (issue #1174)
	if v := os.Getenv("NEXUS_EGRESS_BLOCK_PRIVATE"); v != "" {
		cfg.EgressGuardEnabled = parseBoolEnvStr(v, true)
	}
	if v := os.Getenv("NEXUS_EGRESS_ALLOW"); v != "" {
		cfg.EgressAllowCIDRs = v
	}

	// Secret-manager backend (issue #1173). Env overrides YAML.
	if v := os.Getenv("NEXUS_SECRET_BACKEND"); v != "" {
		cfg.SecretBackend = v
	}
	if v := os.Getenv("NEXUS_VAULT_ADDR"); v != "" {
		cfg.VaultAddr = v
	}
	if v := os.Getenv("NEXUS_VAULT_TOKEN"); v != "" {
		cfg.VaultToken = v
	}
	if v := os.Getenv("NEXUS_VAULT_ROLE"); v != "" {
		cfg.VaultRole = v
	}
	if v := os.Getenv("NEXUS_VAULT_PATH"); v != "" {
		cfg.VaultPath = v
	}
	if v := os.Getenv("NEXUS_AWSSM_PREFIX"); v != "" {
		cfg.AWSSMPrefix = v
	}
	if v := os.Getenv("NEXUS_SECRET_REFRESH"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SECRET_REFRESH: %w", err)
		}
		cfg.SecretRefresh = d
	}

	ValidateShutdownTimeout(cfg)

	// Emit structured warnings for deprecated env vars and YAML keys
	// (issue #1180). Advisory only — does not alter parsed values.
	WarnDeprecatedEnv()
	warnDeprecatedYAMLKeysFromData(data)

	return cfg, nil
}

// toConfig converts a YAMLConfig into a Config by applying the same
// defaults that Load() uses for fields not set in the YAML.
func (yc YAMLConfig) toConfig() (Config, error) {
	// Validate log level early so we can return error if invalid.
	logLevel, logLevelErr := parseLogLevel(yc.LogLevel)
	if logLevelErr != nil {
		return Config{}, logLevelErr
	}

	// InjectionScanRoles for yaml path: compute before composite literal so we can warn
	yamlRolesRaw := yc.InjectionScanRoles
	if yamlRolesRaw == "" {
		yamlRolesRaw = "system"
	}
	yamlRoles, yamlUnrecognized := parseInjectionScanRoles(yamlRolesRaw)

	cfg := Config{
		Addr:                   yc.stringDefault(yc.Addr, ":8000"),
		OllamaURL:              strings.TrimRight(yc.stringDefault(yc.OllamaURL, "http://localhost:11434"), "/"),
		RouterModel:            yc.stringDefault(yc.RouterModel, "qwen3-coder:4b"),
		LocalModel:             yc.stringDefault(yc.LocalModel, "qwen3-coder:8b"),
		EmbeddingModel:         yc.stringDefault(yc.EmbeddingModel, "nomic-embed-text"),
		FrontierURL:            yc.stringDefault(yc.FrontierURL, "https://api.openai.com/v1/chat/completions"),
		FrontierModel:          yc.stringDefault(yc.FrontierModel, "gpt-4o"),
		FrontierKey:            yc.FrontierKey,
		ZAIURL:                 yc.stringDefault(yc.ZAIURL, "https://api.z.ai/v1/chat/completions"),
		ZAIModel:               yc.stringDefault(yc.ZAIModel, "glm-4.6"),
		ZAIKey:                 yc.ZAIKey,
		ProxyAPIKey:            yc.ProxyAPIKey,
		StatusPublic:           yc.StatusPublic,
		ExamplesDir:            yc.stringDefault(yc.ExamplesDir, "./few_shot_examples"),
		MetaPrompt:             yc.stringDefault(yc.MetaPrompt, defaultMetaPrompt),
		TOONNotice:             yc.stringDefault(yc.TOONNotice, defaultTOONNotice),
		TOONUnfenced:           yc.boolFieldDefault(yc.TOONUnfenced, true),
		TelemetryPath:          yc.stringDefault(yc.TelemetryPath, "./nexus-telemetry.jsonl"),
		TelemetryMaxBytes:      yc.intDefault(yc.TelemetryMaxBytes, 0),
		TelemetryMaxFiles:      yc.intDefault(yc.TelemetryMaxFiles, 5),
		TelemetryBufferSize:    yc.intDefault(yc.TelemetryBufferSize, 64<<10),
		TelemetryFlushInterval: yc.durationDefault(yc.TelemetryFlushInterval, 5*time.Second),
		MetricsDBPath:          yc.stringDefault(yc.MetricsDBPath, DefaultMetricsDBPath()),
		MetricsRetentionDays:   yc.intDefault(yc.MetricsRetentionDays, 0),

		// OTLP retry/back-off parameters (issue #803).
		TracerMaxRetries:     yc.intDefault(yc.TracerMaxRetries, 0),
		TracerRetryBaseDelay: yc.durationDefault(yc.TracerRetryBaseDelay, 0),
		TracerRetryMaxDelay:  yc.durationDefault(yc.TracerRetryMaxDelay, 0),

		// Non-string fields with defaults
		RAGThreshold:                  yc.floatDefault(yc.RAGThreshold, 0.55),
		RAGDBPath:                     yc.stringDefault(yc.RAGDBPath, DefaultRAGDBPath()),
		RAGEmbedCacheSize:             yc.intDefault(yc.RAGEmbedCacheSize, 256),
		RAGEmbedCacheTTL:              yc.durationDefault(yc.RAGEmbedCacheTTL, 24*time.Hour),
		RAGEmbedCacheWaitTimeout:      yc.durationDefault(yc.RAGEmbedCacheWaitTimeout, 5*time.Second),
		RAGBatchSize:                  yc.intDefault(yc.RAGBatchSize, 32),
		TokenGuardrail:                yc.intDefault(yc.TokenGuardrail, 6000),
		SLMTimeout:                    yc.durationDefault(yc.SLMTimeout, 8*time.Second),
		SLMCacheMaxEntries:            yc.intDefault(yc.SLMCacheMaxEntries, 512),
		SLMCacheTTL:                   yc.durationDefault(yc.SLMCacheTTL, 30*time.Second),
		SLMCacheSemanticThreshold:     clampFloat(yc.floatDefault(yc.SLMCacheSemanticThreshold, 0.0), 0, 1),
		SLMCacheMaxStale:              yc.intDefault(yc.SLMCacheMaxStale, 0),              // issue #835
		SLMCacheStaleCleanupThreshold: yc.intDefault(yc.SLMCacheStaleCleanupThreshold, 0), // issue #1037
		SLMCacheSemanticScanLimit:     yc.intDefault(yc.SLMCacheSemanticScanLimit, 0),     // issue #933
		FusionTimeout:                 yc.durationDefault(yc.FusionTimeout, 120*time.Second),
		CascadeTimeout:                yc.durationDefault(yc.CascadeTimeout, 30*time.Second),
		CascadeTimeoutFloor:           yc.durationDefault(yc.CascadeTimeoutFloor, 5*time.Second),               // issue #1175
		CascadeTimeoutCeiling:         yc.durationDefault(yc.CascadeTimeoutCeiling, 120*time.Second),           // issue #1175
		CascadeTimeoutPer1kTokens:     yc.durationDefault(yc.CascadeTimeoutPer1kTokens, 1500*time.Millisecond), // issue #1175
		ArbiterTimeout:                yc.durationDefault(yc.ArbiterTimeout, 60*time.Second),
		CascadeMaxResponseBytes:       yc.intDefault(yc.CascadeMaxResponseBytes, DefaultMaxResponseBytes),
		MaxResponseBytes:              yc.intDefault(yc.MaxResponseBytes, DefaultMaxResponseBytes),
		PoolBufferMaxBytes:            yc.intDefault(yc.PoolBufferMaxBytes, DefaultPoolBufferMaxBytes),

		FusionProgressiveDelivery: yc.boolFieldDefault(yc.FusionProgressiveDelivery, true),
		FusionAgreementThreshold:  yc.floatDefault(yc.FusionAgreementThreshold, 0.85),
		ArbiterCacheTTL:           yc.durationDefault(yc.ArbiterCacheTTL, 5*time.Minute),
		ArbiterCacheMaxEntries:    yc.intDefault(yc.ArbiterCacheMaxEntries, 512),
		CacheWarmOnBoot:           yc.CacheWarmOnBoot, // default false (opt-in, issue #1176)
		CacheWarmLimit:            yc.intDefault(yc.CacheWarmLimit, 256),

		JudgeURL:          yc.stringDefault(yc.JudgeURL, "https://api.z.ai/v1/chat/completions"),
		JudgeModel:        yc.stringDefault(yc.JudgeModel, ""), // Falls back to FrontierModel later
		JudgeAPIKey:       yc.JudgeAPIKey,
		JudgeSampleRate:   yc.floatDefault(yc.JudgeSampleRate, 0.1),
		JudgeConcurrency:  yc.intDefault(yc.JudgeConcurrency, 2),
		JudgeQueueDepth:   yc.intDefault(yc.JudgeQueueDepth, 64),
		JudgeTimeout:      yc.durationDefault(yc.JudgeTimeout, 30*time.Second),
		JudgeCostPer1KUSD: yc.floatDefault(yc.JudgeCostPer1KUSD, 0.002),
		JudgeDBPath:       yc.stringDefault(yc.JudgeDBPath, DefaultJudgeDBPath()),

		RoutingConfidenceDB:         yc.stringDefault(yc.RoutingConfidenceDB, DefaultRoutingConfidenceDBPath()),
		RoutingConfidenceFloor:      clampFloat(yc.floatDefault(yc.RoutingConfidenceFloor, 0.4), 0, 1),
		RoutingConfidenceCeiling:    clampFloat(yc.floatDefault(yc.RoutingConfidenceCeiling, 0.85), 0, 1),
		RoutingConfidenceMinSamples: yc.intDefault(yc.RoutingConfidenceMinSamples, 5),
		RoutingConfidenceWindow:     yc.durationDefault(yc.RoutingConfidenceWindow, 168*time.Hour),

		HealthPollInterval:     yc.durationDefault(yc.HealthPollInterval, 30*time.Second),
		HealthBreakerThreshold: yc.intDefault(yc.HealthBreakerThreshold, 3),
		HealthProbeTimeout:     yc.durationDefault(yc.HealthProbeTimeout, 5*time.Second),

		ProbePollInterval:     yc.durationDefault(yc.ProbeInterval, 60*time.Second),
		ProbeTimeout:          yc.durationDefault(yc.ProbeTimeout, 5*time.Second),
		ProbeBytesPerToken:    yc.intDefault(yc.ProbeBytesPerToken, 256*1024),
		ProbeThermalThreshold: yc.intDefault(yc.ProbeThermalThreshold, 90),
		ProbeNVIDIAInterval:   yc.durationDefault(yc.ProbeNVIDIAInterval, 0),

		LocalMaxConcurrent:    yc.intDefault(yc.LocalMaxConcurrent, 0),
		LocalVRAMBytesPerSlot: yc.int64Default(yc.LocalVRAMBytesPerSlot, DefaultLocalVRAMBytesPerSlot),
		LocalCooldown:         yc.durationDefault(yc.LocalCooldown, 10*time.Second),

		MaxBodyBytes:    yc.intDefault(yc.MaxBodyBytes, DefaultMaxBodyBytes),
		ReadTimeout:     yc.durationDefault(yc.ReadTimeout, DefaultServerReadTimeout),
		WriteTimeout:    yc.durationDefault(yc.WriteTimeout, 0),
		IdleTimeout:     yc.durationDefault(yc.IdleTimeout, DefaultServerIdleTimeout),
		MaxHeaderBytes:  yc.intDefault(yc.MaxHeaderBytes, DefaultServerMaxHeaderBytes),
		ShutdownTimeout: yc.durationDefault(yc.ShutdownTimeout, DefaultShutdownTimeout),
		TLSEnabled:      yc.TLSEnabled,

		BudgetDailyLimit:      yc.floatDefault(yc.BudgetDailyLimit, 0),
		BudgetAlertEnabled:    yc.BudgetAlertEnabled,
		BudgetAlertThreshold:  clampFloat(yc.floatDefault(yc.BudgetAlertThreshold, 0.8), 0, 1),
		BudgetAlertWebhookURL: yc.BudgetAlertWebhookURL,

		SelectorWindow:          yc.durationDefault(yc.SelectorWindow, time.Hour),
		SelectorMinSamples:      yc.intDefault(yc.SelectorMinSamples, 5),
		SelectorRefreshInterval: yc.durationDefault(yc.SelectorRefreshInterval, 60*time.Second),
		ProviderTailWeight:      clampFloat(yc.floatDefault(yc.ProviderTailWeight, 0.0), 0, 1),
		FrontierCostPer1K:       yc.floatDefault(yc.FrontierCostPer1K, 0.005),
		ZAICostPer1K:            yc.floatDefault(yc.ZAICostPer1K, 0.002),

		CostBaselineProvider:  yc.stringDefault(yc.CostBaselineProvider, "frontier"),
		CostBaselineModel:     yc.stringDefault(yc.CostBaselineModel, ""),   // Falls back to FrontierModel later
		CostBaselineRatePer1K: yc.floatDefault(yc.CostBaselineRatePer1K, 0), // Falls back to FrontierCostPer1K later
		CostUseOutputTokens:   yc.boolFieldDefault(yc.CostUseOutputTokens, false),

		QualityConcurrency:     yc.intDefault(yc.QualityConcurrency, 2),
		QualityQueueDepth:      yc.intDefault(yc.QualityQueueDepth, 64),
		QualityTimeout:         yc.durationDefault(yc.QualityTimeout, 60*time.Second),
		QualityStderrCap:       yc.intDefault(yc.QualityStderrCap, 2*1024),
		QualityDroppedRingSize: yc.intDefault(yc.QualityDroppedRingSize, 256),
		QualityEnabled:         yc.intDefault(yc.QualityConcurrency, 2) > 0,

		PromptInjectionMode: middleware.ParseInjectionMode(yc.PromptInjectionMode),
		InjectionScanRoles:  yamlRoles,

		LogLevel:  logLevel,
		LogFormat: parseLogFormat(yc.LogFormat),

		Debug:          yc.Debug, // defaults to false in toConfig if not set
		DebugBodyBytes: yc.intDefault(yc.DebugBodyBytes, DefaultDebugBodyBytes),

		ModelsEndpointEnabled: yc.boolFieldDefault(yc.ModelsEndpointEnabled, true),
		ModelsCacheTTL:        yc.durationDefault(yc.ModelsCacheTTL, 5*time.Minute),

		ModelAliases:       yc.ModelAliases,
		ModelAliasesStrict: yc.ModelAliasesStrict,

		DashboardEndpointEnabled: yc.boolFieldDefault(yc.DashboardEndpointEnabled, false),
		DashboardEndpoint:        yc.stringDefault(yc.DashboardEndpoint, "/dashboard"),
		DashboardPublic:          yc.DashboardPublic,

		RAGPollInterval: yc.durationDefault(yc.RAGPollInterval, 30*time.Second),

		RateLimitRPM:      yc.intDefault(yc.RateLimitRPM, 0),
		RateLimitBurst:    yc.intDefault(yc.RateLimitBurst, 0),
		RateLimitByAPIKey: yc.RateLimitByAPIKey,

		AuthRateLimitRPM:    yc.intDefault(yc.AuthRateLimitRPM, 5),
		AuthRateLimitBurst:  yc.intDefault(yc.AuthRateLimitBurst, 3),
		AuthRateLimitWindow: yc.durationDefault(yc.AuthRateLimitWindow, 5*time.Minute),

		TracingEndpoint:   yc.stringDefault(yc.TracingEndpoint, ""),
		TracingTimeout:    yc.durationDefault(yc.TracingTimeout, 10*time.Second),
		TracingQueueSize:  yc.intDefault(yc.TracingQueueSize, 256),
		TracingBatchSize:  yc.intDefault(yc.TracingBatchSize, 64),
		TracingSampleRate: yc.floatDefault(yc.TracingSampleRate, 1.0),

		// SSRF egress guard (issue #1174). Default enabled so a stock
		// deployment is protected out of the box.
		EgressGuardEnabled: true,
		EgressAllowCIDRs:   yc.EgressAllowCIDRs,

		SecretBackend: yc.stringDefault(yc.SecretBackend, "env"),
		VaultAddr:     yc.VaultAddr,
		VaultToken:    yc.VaultToken,
		VaultRole:     yc.VaultRole,
		VaultPath:     yc.stringDefault(yc.VaultPath, "secret"),
		AWSSMPrefix:   yc.AWSSMPrefix,
		SecretRefresh: yc.durationDefault(yc.SecretRefresh, 0),
	}

	// Warn if yaml had unrecognized injection scan roles (issue #845)
	// Only warn when unrecognized tokens exist AND the fallback is ["system"] (issue #879).
	if len(yamlUnrecognized) > 0 && len(yamlRoles) == 1 && yamlRoles[0] == "system" {
		slog.Warn("unrecognised injection scan role(s) in config.yaml: falling back to [system]",
			slog.String("ignored", strings.Join(yamlUnrecognized, ",")),
		)
	}

	// Embedder type
	embedderType := yc.stringDefault(string(yc.EmbedderType), "ollama")
	cfg.EmbedderType = ragpkg.EmbedderType(strings.ToLower(strings.TrimSpace(embedderType)))
	switch cfg.EmbedderType {
	case ragpkg.EmbedderTypeOpenAI:
		cfg.EmbedderBaseURL = yc.stringDefault(yc.EmbedderBaseURL, "https://api.openai.com/v1")
	case ragpkg.EmbedderTypeCohere:
		cfg.EmbedderBaseURL = yc.stringDefault(yc.EmbedderBaseURL, "https://api.cohere.ai/v1")
	default:
		cfg.EmbedderBaseURL = cfg.OllamaURL
	}
	cfg.CohereAPIKey = yc.CohereAPIKey

	// Judge URL fallback: if not set in YAML, use FrontierURL
	if cfg.JudgeURL == "https://api.z.ai/v1/chat/completions" && yc.JudgeURL == "" {
		// YAML didn't override; use the hard default but check if FrontierURL was overridden
		if yc.FrontierURL != "" {
			cfg.JudgeURL = yc.FrontierURL
		}
	}
	if cfg.JudgeModel == "" {
		cfg.JudgeModel = cfg.FrontierModel
	}

	// Cost baseline rate fallback
	if cfg.CostBaselineRatePer1K == 0 && yc.CostBaselineRatePer1K == 0 {
		cfg.CostBaselineRatePer1K = cfg.FrontierCostPer1K
	}
	if cfg.CostBaselineModel == "" {
		cfg.CostBaselineModel = cfg.FrontierModel
	}

	// ProbeEnabled derived field
	cfg.ProbeEnabled = cfg.ProbePollInterval > 0

	// Judge enabled derived field
	cfg.JudgeEnabled = cfg.JudgeSampleRate > 0 && cfg.JudgeURL != "" && cfg.JudgeModel != ""

	// Trusted proxies
	if yc.TrustedProxies != "" {
		parsed, _ := parseTrustedProxies(yc.TrustedProxies)
		cfg.TrustedProxies = parsed
		cfg.TrustedProxiesRaw = yc.TrustedProxies
	}

	return cfg, nil
}

// boolFieldDefault applies a default when the YAML field was the zero value.
// Since we cannot distinguish unset from explicit false with a plain bool,
// this is only safe for fields whose default is also false. For fields with
// default=true (fusion_progressive_delivery, models_endpoint_enabled), the
// toConfig() caller must use the YAML value directly when non-zero (which
// in practice means: the YAML value was explicitly set to true).
func (yc YAMLConfig) boolFieldDefault(field, def bool) bool {
	// Go's zero value for bool is false. If field==false and def==true,
	// we cannot tell if YAML explicitly set it to false or left it unset.
	// We resolve this by treating false==def as "use default".
	if field == def {
		return def
	}
	return field
}

func (yc YAMLConfig) stringDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (yc YAMLConfig) intDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func (yc YAMLConfig) int64Default(v int64, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

func (yc YAMLConfig) floatDefault(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}

func (yc YAMLConfig) durationDefault(v string, def time.Duration) time.Duration {
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func clampFloat(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// validate checks YAML fields that have constraints (negative values rejected
// for durations and byte counts, matching Load()'s behaviour).
func (yc YAMLConfig) validate() error {
	if yc.ReadTimeout != "" {
		if d, err := time.ParseDuration(yc.ReadTimeout); err == nil && d < 0 {
			return configError("server_read_timeout", "must not be negative", yc.ReadTimeout, DefaultServerReadTimeout.String())
		}
	}
	if yc.WriteTimeout != "" {
		if d, err := time.ParseDuration(yc.WriteTimeout); err == nil && d < 0 {
			return configError("server_write_timeout", "must not be negative", yc.WriteTimeout, DefaultServerWriteTimeout.String())
		}
	}
	if yc.IdleTimeout != "" {
		if d, err := time.ParseDuration(yc.IdleTimeout); err == nil && d < 0 {
			return configError("server_idle_timeout", "must not be negative", yc.IdleTimeout, DefaultServerIdleTimeout.String())
		}
	}
	if yc.MaxHeaderBytes < 0 {
		return configError("server_max_header_bytes", "must not be negative", strconv.Itoa(yc.MaxHeaderBytes), strconv.Itoa(DefaultServerMaxHeaderBytes))
	}
	if yc.ShutdownTimeout != "" {
		if d, err := time.ParseDuration(yc.ShutdownTimeout); err == nil && d < 0 {
			return configError("shutdown_timeout", "must not be negative", yc.ShutdownTimeout, DefaultShutdownTimeout.String())
		}
	}

	// Fractional fields (0..1 range). Default values mirror the env-var
	// defaults documented in .env.example.
	if yc.BudgetAlertThreshold < 0 || yc.BudgetAlertThreshold > 1 {
		return configError("budget_alert_threshold", "must be in [0,1]", strconv.FormatFloat(yc.BudgetAlertThreshold, 'f', -1, 64), "0.8")
	}
	if yc.FusionAgreementThreshold < 0 || yc.FusionAgreementThreshold > 1 {
		return configError("fusion_agreement_threshold", "must be in [0,1]", strconv.FormatFloat(yc.FusionAgreementThreshold, 'f', -1, 64), "0.85")
	}
	if yc.ProviderTailWeight < 0 || yc.ProviderTailWeight > 1 {
		return configError("provider_tail_weight", "must be in [0,1]", strconv.FormatFloat(yc.ProviderTailWeight, 'f', -1, 64), "0")
	}
	if yc.TracingSampleRate < 0 || yc.TracingSampleRate > 1 {
		return configError("tracing_sample_rate", "must be in [0,1]", strconv.FormatFloat(yc.TracingSampleRate, 'f', -1, 64), "1")
	}
	if yc.RoutingConfidenceFloor < 0 || yc.RoutingConfidenceFloor > 1 {
		return configError("routing_confidence_floor", "must be in [0,1]", strconv.FormatFloat(yc.RoutingConfidenceFloor, 'f', -1, 64), "0.4")
	}
	if yc.RoutingConfidenceCeiling < 0 || yc.RoutingConfidenceCeiling > 1 {
		return configError("routing_confidence_ceiling", "must be in [0,1]", strconv.FormatFloat(yc.RoutingConfidenceCeiling, 'f', -1, 64), "0.85")
	}

	// Provider adapter types (issue #1185). Each entry's `type` must be
	// in the providers package's allowed set so a typo fails config
	// validation rather than producing a silent no-op at request time.
	for i, p := range yc.Providers {
		if p.Type == "" {
			continue // empty == openai default
		}
		if !providers.IsValidAdapterType(p.Type) {
			return fmt.Errorf("config: providers[%d] (%s): type %q is not valid (allowed: %s)",
				i, p.Name, p.Type, strings.Join(providers.ValidAdapterTypes(), ", "))
		}
	}

	return nil
}

// yamlProviderEntry is one element of the optional YAML `providers:` list
// (issue #1185). It mirrors the env-driven NEXUS_PROVIDER_<NAME>_* vars.
type yamlProviderEntry struct {
	Name   string `yaml:"name"`
	URL    string `yaml:"url"`
	Model  string `yaml:"model"`
	APIKey string `yaml:"api_key"`
	Type   string `yaml:"type"`
}

// validateRawBoolFields checks that boolean fields in the raw YAML data
// contain valid bool values. It uses a map unmarshal to inspect raw values
// before the real struct unmarshal, so invalid values like "maybe" for a
// bool field are caught and reported rather than silently becoming the zero
// value (which would then be misinterpreted as the default).
func validateRawBoolFields(data []byte) error {
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil // yaml parse error will be caught by the real unmarshal
	}

	// TOONUnfenced: reject non-bool values
	if v, ok := raw["toon_unfenced"]; ok {
		switch v := v.(type) {
		case bool:
			// valid
		case string:
			// Try to parse as bool; reject if unrecognized
			if _, err := parseYAMLBool(v); err != nil {
				return fmt.Errorf("config: toon_unfenced %s; see .env.example", err.Error())
			}
		default:
			// Also catch int/float etc that yaml.Unmarshal accepted
			return fmt.Errorf("config: toon_unfenced value %v is not a boolean; want true or false; see .env.example", v)
		}
	}

	return nil
}

// parseYAMLBool parses a boolean value from a YAML field string and returns
// an error for unrecognized strings (unlike parseBoolEnvStr which silently
// falls back to the default). Used for YAML fields where invalid values
// should fail-fast rather than silently passing.
func parseYAMLBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("value %q is not recognised; want true or false", v)
	}
}

// parseBoolEnvStr is like parseBoolEnv but takes the raw string directly.
func parseBoolEnvStr(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return def
	}
}
