package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/anchapin/nexus-proxy/internal/middleware"
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

	// Budget
	BudgetDailyLimit      float64 `yaml:"budget_daily_limit"`
	BudgetAlertEnabled    bool    `yaml:"budget_alert_enabled"`
	BudgetAlertThreshold  float64 `yaml:"budget_alert_threshold"`
	BudgetAlertWebhookURL string  `yaml:"budget_alert_webhook_url"`

	// Selector
	SelectorWindow          string `yaml:"selector_window"`
	SelectorMinSamples      int    `yaml:"selector_min_samples"`
	SelectorRefreshInterval string `yaml:"selector_refresh_interval"`

	// RAG
	ExamplesDir        string  `yaml:"examples_dir"`
	RAGThreshold       float64 `yaml:"rag_threshold"`
	EmbedderType       string  `yaml:"embedder_type"`
	EmbedderBaseURL    string  `yaml:"embedder_base_url"`
	CohereAPIKey       string  `yaml:"cohere_api_key"`
	RAGDBPath          string  `yaml:"rag_db_path"`
	RAGPollInterval    string  `yaml:"rag_poll_interval"`
	RAGWatcherDisabled bool    `yaml:"rag_watcher_disabled"`
	RAGEmbedCacheSize  int     `yaml:"rag_embed_cache_size"`
	RAGEmbedCacheTTL   string  `yaml:"rag_embed_cache_ttl"`

	// Routing
	TokenGuardrail            int     `yaml:"token_guardrail"`
	SLMTimeout                string  `yaml:"slm_timeout"`
	SLMCacheMaxEntries        int     `yaml:"slm_cache_max_entries"`
	SLMCacheTTL               string  `yaml:"slm_cache_ttl"`
	SLMCacheSemanticThreshold float64 `yaml:"slm_cache_similarity_threshold"`
	FusionTimeout             string  `yaml:"fusion_timeout"`
	CascadeTimeout            string  `yaml:"cascade_timeout"`
	ArbiterTimeout            string  `yaml:"arbiter_timeout"`

	// Fusion
	FusionProgressiveDelivery bool    `yaml:"fusion_progressive_delivery"`
	FusionAgreementThreshold  float64 `yaml:"fusion_agreement_threshold"`

	// Health
	HealthPollInterval     string `yaml:"health_poll_interval"`
	HealthBreakerThreshold int    `yaml:"health_breaker_threshold"`
	HealthProbeTimeout     string `yaml:"health_probe_timeout"`

	// Probe
	ProbeInterval      string `yaml:"probe_interval"`
	ProbeTimeout       string `yaml:"probe_timeout"`
	ProbeBytesPerToken int    `yaml:"probe_bytes_per_token"`

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
	QualityConcurrency int    `yaml:"quality_concurrency"`
	QualityQueueDepth  int    `yaml:"quality_queue"`
	QualityTimeout     string `yaml:"quality_timeout"`
	QualityStderrCap   int    `yaml:"quality_stderr_cap"`

	// Middleware
	MetaPrompt string `yaml:"meta_prompt"`
	TOONNotice string `yaml:"toon_notice"`

	// Prompt injection
	PromptInjectionMode string `yaml:"prompt_injection_mode"`

	// Telemetry
	TelemetryPath string `yaml:"telemetry_path"`
	MetricsDBPath string `yaml:"metrics_db_path"`

	// Models
	ModelsEndpointEnabled bool   `yaml:"models_endpoint_enabled"`
	ModelsCacheTTL        string `yaml:"models_cache_ttl"`

	// Trusted proxies
	TrustedProxies string `yaml:"trusted_proxies"`
	RateLimitRPM   int    `yaml:"rate_limit_rpm"`
	RateLimitBurst int    `yaml:"rate_limit_burst"`
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
	cfg := yc.toConfig()

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
			return cfg, fmt.Errorf("config: NEXUS_SERVER_READ_TIMEOUT: %w", err)
		}
		if d < 0 {
			return cfg, fmt.Errorf("config: NEXUS_SERVER_READ_TIMEOUT must not be negative, got %s", d)
		}
		cfg.ReadTimeout = d
	}
	if v := os.Getenv("NEXUS_SERVER_WRITE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SERVER_WRITE_TIMEOUT: %w", err)
		}
		if d < 0 {
			return cfg, fmt.Errorf("config: NEXUS_SERVER_WRITE_TIMEOUT must not be negative, got %s", d)
		}
		cfg.WriteTimeout = d
	}
	if v := os.Getenv("NEXUS_SERVER_IDLE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SERVER_IDLE_TIMEOUT: %w", err)
		}
		if d < 0 {
			return cfg, fmt.Errorf("config: NEXUS_SERVER_IDLE_TIMEOUT must not be negative, got %s", d)
		}
		cfg.IdleTimeout = d
	}
	if v := os.Getenv("NEXUS_SERVER_MAX_HEADER_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SERVER_MAX_HEADER_BYTES: %w", err)
		}
		if n < 0 {
			return cfg, fmt.Errorf("config: NEXUS_SERVER_MAX_HEADER_BYTES must not be negative, got %d", n)
		}
		cfg.MaxHeaderBytes = n
	}
	if v := os.Getenv("NEXUS_MAX_BODY_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_MAX_BODY_BYTES: %w", err)
		}
		cfg.MaxBodyBytes = n
	}
	if v := os.Getenv("NEXUS_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SHUTDOWN_TIMEOUT: %w", err)
		}
		if d < 0 {
			return cfg, fmt.Errorf("config: NEXUS_SHUTDOWN_TIMEOUT must not be negative, got %s", d)
		}
		if d == 0 {
			d = DefaultShutdownTimeout
		}
		cfg.ShutdownTimeout = d
	}

	// Logging
	if v := os.Getenv("NEXUS_LOG_LEVEL"); v != "" {
		cfg.LogLevel = parseLogLevel(v)
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
			return cfg, fmt.Errorf("config: NEXUS_DEBUG_BODY_BYTES: %w", err)
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
			return cfg, fmt.Errorf("config: NEXUS_FRONTIER_COST_PER_1K: %w", err)
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
			return cfg, fmt.Errorf("config: NEXUS_ZAI_COST_PER_1K: %w", err)
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
			return cfg, fmt.Errorf("config: NEXUS_COST_BASELINE_RATE_PER_1K: %w", err)
		}
		if f < 0 {
			f = 0
		}
		cfg.CostBaselineRatePer1K = f
	}

	// Budget
	if v := os.Getenv("NEXUS_BUDGET_DAILY_LIMIT"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_BUDGET_DAILY_LIMIT: %w", err)
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
			return cfg, fmt.Errorf("config: NEXUS_BUDGET_ALERT_THRESHOLD: %w", err)
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
			return cfg, fmt.Errorf("config: NEXUS_SELECTOR_WINDOW: %w", err)
		}
		if d < 0 {
			d = 0
		}
		cfg.SelectorWindow = d
	}
	if v := os.Getenv("NEXUS_SELECTOR_MIN_SAMPLES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SELECTOR_MIN_SAMPLES: %w", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.SelectorMinSamples = n
	}
	if v := os.Getenv("NEXUS_SELECTOR_REFRESH"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SELECTOR_REFRESH: %w", err)
		}
		if d < 0 {
			d = 0
		}
		cfg.SelectorRefreshInterval = d
	}

	// RAG
	if v := os.Getenv("NEXUS_EXAMPLES_DIR"); v != "" {
		cfg.ExamplesDir = v
	}
	if v := os.Getenv("NEXUS_RAG_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_RAG_THRESHOLD: %w", err)
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
			return cfg, fmt.Errorf("config: NEXUS_RAG_POLL_INTERVAL: %w", err)
		}
		if d < 0 {
			d = 0
		}
		if d == 0 {
			d = DefaultRAGPollInterval
		}
		cfg.RAGPollInterval = d
	}
	if v := os.Getenv("NEXUS_RAG_WATCHER_DISABLED"); v != "" {
		switch strings.ToLower(v) {
		case "true", "1", "yes":
			cfg.RAGWatcherDisabled = true
		default:
			cfg.RAGWatcherDisabled = false
		}
	}
	if v := os.Getenv("NEXUS_RAG_EMBED_CACHE_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_RAG_EMBED_CACHE_SIZE: %w", err)
		}
		cfg.RAGEmbedCacheSize = n
	}
	if v := os.Getenv("NEXUS_RAG_EMBED_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_RAG_EMBED_CACHE_TTL: %w", err)
		}
		cfg.RAGEmbedCacheTTL = d
	}

	// Routing
	if v := os.Getenv("NEXUS_TOKEN_GUARDRAIL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_TOKEN_GUARDRAIL: %w", err)
		}
		cfg.TokenGuardrail = n
	}
	if v := os.Getenv("NEXUS_SLM_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SLM_TIMEOUT: %w", err)
		}
		cfg.SLMTimeout = d
	}
	if v := os.Getenv("NEXUS_SLM_CACHE_MAX_ENTRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SLM_CACHE_MAX_ENTRIES: %w", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.SLMCacheMaxEntries = n
	}
	if v := os.Getenv("NEXUS_SLM_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SLM_CACHE_TTL: %w", err)
		}
		cfg.SLMCacheTTL = d
	}
	if v := os.Getenv("NEXUS_SLMCACHE_SIMILARITY_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_SLMCACHE_SIMILARITY_THRESHOLD: %w", err)
		}
		cfg.SLMCacheSemanticThreshold = clampFloat(f, 0, 1)
	}
	if v := os.Getenv("NEXUS_FUSION_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_FUSION_TIMEOUT: %w", err)
		}
		cfg.FusionTimeout = d
	}
	if v := os.Getenv("NEXUS_CASCADE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_CASCADE_TIMEOUT: %w", err)
		}
		cfg.CascadeTimeout = d
	}
	if v := os.Getenv("NEXUS_ARBITER_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_ARBITER_TIMEOUT: %w", err)
		}
		cfg.ArbiterTimeout = d
	}

	// Fusion
	if v := os.Getenv("NEXUS_FUSION_PROGRESSIVE"); v != "" {
		cfg.FusionProgressiveDelivery = parseBoolEnvStr(v, true)
	}
	if v := os.Getenv("NEXUS_FUSION_AGREEMENT_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_FUSION_AGREEMENT_THRESHOLD: %w", err)
		}
		cfg.FusionAgreementThreshold = f
	}

	// Health
	if v := os.Getenv("NEXUS_HEALTH_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_HEALTH_POLL_INTERVAL: %w", err)
		}
		cfg.HealthPollInterval = d
	}
	if v := os.Getenv("NEXUS_HEALTH_BREAKER_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_HEALTH_BREAKER_THRESHOLD: %w", err)
		}
		cfg.HealthBreakerThreshold = n
	}
	if v := os.Getenv("NEXUS_HEALTH_PROBE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_HEALTH_PROBE_TIMEOUT: %w", err)
		}
		cfg.HealthProbeTimeout = d
	}

	// Probe
	if v := os.Getenv("NEXUS_PROBE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_PROBE_INTERVAL: %w", err)
		}
		cfg.ProbePollInterval = d
		cfg.ProbeEnabled = cfg.ProbePollInterval > 0
	}
	if v := os.Getenv("NEXUS_PROBE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_PROBE_TIMEOUT: %w", err)
		}
		cfg.ProbeTimeout = d
	}
	if v := os.Getenv("NEXUS_PROBE_BYTES_PER_TOKEN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_PROBE_BYTES_PER_TOKEN: %w", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.ProbeBytesPerToken = n
	}

	// Local concurrency
	if v := os.Getenv("NEXUS_LOCAL_MAX_CONCURRENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_LOCAL_MAX_CONCURRENT: %w", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.LocalMaxConcurrent = n
	}
	if v := os.Getenv("NEXUS_LOCAL_VRAM_BYTES_PER_SLOT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_LOCAL_VRAM_BYTES_PER_SLOT: %w", err)
		}
		if n < 0 {
			n = int(DefaultLocalVRAMBytesPerSlot)
		}
		cfg.LocalVRAMBytesPerSlot = int64(n)
	}
	if v := os.Getenv("NEXUS_LOCAL_COOLDOWN"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_LOCAL_COOLDOWN: %w", err)
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
			return cfg, fmt.Errorf("config: NEXUS_JUDGE_SAMPLE_RATE: %w", err)
		}
		cfg.JudgeSampleRate = f
	}
	if v := os.Getenv("NEXUS_JUDGE_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_JUDGE_CONCURRENCY: %w", err)
		}
		cfg.JudgeConcurrency = n
	}
	if v := os.Getenv("NEXUS_JUDGE_QUEUE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_JUDGE_QUEUE: %w", err)
		}
		cfg.JudgeQueueDepth = n
	}
	if v := os.Getenv("NEXUS_JUDGE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_JUDGE_TIMEOUT: %w", err)
		}
		cfg.JudgeTimeout = d
	}
	if v := os.Getenv("NEXUS_JUDGE_COST_PER_1K"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_JUDGE_COST_PER_1K: %w", err)
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
			return cfg, fmt.Errorf("config: NEXUS_ROUTING_CONFIDENCE_FLOOR: %w", err)
		}
		cfg.RoutingConfidenceFloor = f
	}
	if v := os.Getenv("NEXUS_ROUTING_CONFIDENCE_CEILING"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_ROUTING_CONFIDENCE_CEILING: %w", err)
		}
		cfg.RoutingConfidenceCeiling = f
	}
	if v := os.Getenv("NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES: %w", err)
		}
		cfg.RoutingConfidenceMinSamples = n
	}
	if v := os.Getenv("NEXUS_ROUTING_CONFIDENCE_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_ROUTING_CONFIDENCE_WINDOW: %w", err)
		}
		cfg.RoutingConfidenceWindow = d
	}

	// Quality
	if v := os.Getenv("NEXUS_QUALITY_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_QUALITY_CONCURRENCY: %w", err)
		}
		cfg.QualityConcurrency = n
		cfg.QualityEnabled = cfg.QualityConcurrency > 0
	}
	if v := os.Getenv("NEXUS_QUALITY_QUEUE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_QUALITY_QUEUE: %w", err)
		}
		cfg.QualityQueueDepth = n
	}
	if v := os.Getenv("NEXUS_QUALITY_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_QUALITY_TIMEOUT: %w", err)
		}
		cfg.QualityTimeout = d
	}
	if v := os.Getenv("NEXUS_QUALITY_STDERR_CAP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_QUALITY_STDERR_CAP: %w", err)
		}
		cfg.QualityStderrCap = n
	}

	// Middleware prompts
	if v := os.Getenv("NEXUS_META_PROMPT"); v != "" {
		cfg.MetaPrompt = v
	}
	if v := os.Getenv("NEXUS_TOON_NOTICE"); v != "" {
		cfg.TOONNotice = v
	}

	// Prompt injection
	if v := os.Getenv("NEXUS_PROMPT_INJECTION_MODE"); v != "" {
		cfg.PromptInjectionMode = middleware.ParseInjectionMode(v)
	}

	// Telemetry
	if v := os.Getenv("NEXUS_TELEMETRY_PATH"); v != "" {
		cfg.TelemetryPath = v
	}
	if v := os.Getenv("NEXUS_METRICS_DB"); v != "" {
		cfg.MetricsDBPath = v
	}

	// Models
	if v := os.Getenv("NEXUS_MODELS_ENDPOINT"); v != "" {
		cfg.ModelsEndpointEnabled = parseBoolEnvStr(v, true)
	}
	if v := os.Getenv("NEXUS_MODELS_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_MODELS_CACHE_TTL: %w", err)
		}
		cfg.ModelsCacheTTL = d
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
			return cfg, fmt.Errorf("config: NEXUS_RATE_LIMIT_RPM: %w", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.RateLimitRPM = n
	}
	if v := os.Getenv("NEXUS_RATE_LIMIT_BURST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("config: NEXUS_RATE_LIMIT_BURST: %w", err)
		}
		if n < 0 {
			n = 0
		}
		cfg.RateLimitBurst = n
	}

	return cfg, nil
}

// toConfig converts a YAMLConfig into a Config by applying the same
// defaults that Load() uses for fields not set in the YAML.
func (yc YAMLConfig) toConfig() Config {
	cfg := Config{
		Addr:           yc.stringDefault(yc.Addr, ":8000"),
		OllamaURL:      strings.TrimRight(yc.stringDefault(yc.OllamaURL, "http://localhost:11434"), "/"),
		RouterModel:    yc.stringDefault(yc.RouterModel, "qwen3-coder:4b"),
		LocalModel:     yc.stringDefault(yc.LocalModel, "qwen3-coder:8b"),
		EmbeddingModel: yc.stringDefault(yc.EmbeddingModel, "nomic-embed-text"),
		FrontierURL:    yc.stringDefault(yc.FrontierURL, "https://api.openai.com/v1/chat/completions"),
		FrontierModel:  yc.stringDefault(yc.FrontierModel, "gpt-4o"),
		FrontierKey:    yc.FrontierKey,
		ZAIURL:         yc.stringDefault(yc.ZAIURL, "https://api.z.ai/v1/chat/completions"),
		ZAIModel:       yc.stringDefault(yc.ZAIModel, "glm-4.6"),
		ZAIKey:         yc.ZAIKey,
		ProxyAPIKey:    yc.ProxyAPIKey,
		StatusPublic:   yc.StatusPublic,
		ExamplesDir:    yc.stringDefault(yc.ExamplesDir, "./few_shot_examples"),
		MetaPrompt:     yc.stringDefault(yc.MetaPrompt, defaultMetaPrompt),
		TOONNotice:     yc.stringDefault(yc.TOONNotice, defaultTOONNotice),
		TelemetryPath:  yc.stringDefault(yc.TelemetryPath, "./nexus-telemetry.jsonl"),
		MetricsDBPath:  yc.stringDefault(yc.MetricsDBPath, DefaultMetricsDBPath()),

		// Non-string fields with defaults
		RAGThreshold:              yc.floatDefault(yc.RAGThreshold, 0.55),
		RAGDBPath:                 yc.stringDefault(yc.RAGDBPath, DefaultRAGDBPath()),
		RAGEmbedCacheSize:         yc.intDefault(yc.RAGEmbedCacheSize, 256),
		RAGEmbedCacheTTL:          yc.durationDefault(yc.RAGEmbedCacheTTL, 24*time.Hour),
		TokenGuardrail:            yc.intDefault(yc.TokenGuardrail, 6000),
		SLMTimeout:                yc.durationDefault(yc.SLMTimeout, 8*time.Second),
		SLMCacheMaxEntries:        yc.intDefault(yc.SLMCacheMaxEntries, 512),
		SLMCacheTTL:               yc.durationDefault(yc.SLMCacheTTL, 30*time.Second),
		SLMCacheSemanticThreshold: clampFloat(yc.floatDefault(yc.SLMCacheSemanticThreshold, 0.0), 0, 1),
		FusionTimeout:             yc.durationDefault(yc.FusionTimeout, 120*time.Second),
		CascadeTimeout:            yc.durationDefault(yc.CascadeTimeout, 30*time.Second),
		ArbiterTimeout:            yc.durationDefault(yc.ArbiterTimeout, 60*time.Second),

		FusionProgressiveDelivery: yc.boolFieldDefault(yc.FusionProgressiveDelivery, true),
		FusionAgreementThreshold:  yc.floatDefault(yc.FusionAgreementThreshold, 0.85),

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
		RoutingConfidenceFloor:      yc.floatDefault(yc.RoutingConfidenceFloor, 0.4),
		RoutingConfidenceCeiling:    yc.floatDefault(yc.RoutingConfidenceCeiling, 0.85),
		RoutingConfidenceMinSamples: yc.intDefault(yc.RoutingConfidenceMinSamples, 5),
		RoutingConfidenceWindow:     yc.durationDefault(yc.RoutingConfidenceWindow, 168*time.Hour),

		HealthPollInterval:     yc.durationDefault(yc.HealthPollInterval, 30*time.Second),
		HealthBreakerThreshold: yc.intDefault(yc.HealthBreakerThreshold, 3),
		HealthProbeTimeout:     yc.durationDefault(yc.HealthProbeTimeout, 5*time.Second),

		ProbePollInterval:  yc.durationDefault(yc.ProbeInterval, 60*time.Second),
		ProbeTimeout:       yc.durationDefault(yc.ProbeTimeout, 5*time.Second),
		ProbeBytesPerToken: yc.intDefault(yc.ProbeBytesPerToken, 256*1024),

		LocalMaxConcurrent:    yc.intDefault(yc.LocalMaxConcurrent, 0),
		LocalVRAMBytesPerSlot: yc.int64Default(yc.LocalVRAMBytesPerSlot, DefaultLocalVRAMBytesPerSlot),
		LocalCooldown:         yc.durationDefault(yc.LocalCooldown, 10*time.Second),

		MaxBodyBytes:    yc.intDefault(yc.MaxBodyBytes, DefaultMaxBodyBytes),
		ReadTimeout:     yc.durationDefault(yc.ReadTimeout, DefaultServerReadTimeout),
		WriteTimeout:    yc.durationDefault(yc.WriteTimeout, 0),
		IdleTimeout:     yc.durationDefault(yc.IdleTimeout, DefaultServerIdleTimeout),
		MaxHeaderBytes:  yc.intDefault(yc.MaxHeaderBytes, DefaultServerMaxHeaderBytes),
		ShutdownTimeout: yc.durationDefault(yc.ShutdownTimeout, DefaultShutdownTimeout),

		BudgetDailyLimit:      yc.floatDefault(yc.BudgetDailyLimit, 0),
		BudgetAlertEnabled:    yc.BudgetAlertEnabled,
		BudgetAlertThreshold:  clampFloat(yc.floatDefault(yc.BudgetAlertThreshold, 0.8), 0, 1),
		BudgetAlertWebhookURL: yc.BudgetAlertWebhookURL,

		SelectorWindow:          yc.durationDefault(yc.SelectorWindow, time.Hour),
		SelectorMinSamples:      yc.intDefault(yc.SelectorMinSamples, 5),
		SelectorRefreshInterval: yc.durationDefault(yc.SelectorRefreshInterval, 60*time.Second),
		FrontierCostPer1K:       yc.floatDefault(yc.FrontierCostPer1K, 0.005),
		ZAICostPer1K:            yc.floatDefault(yc.ZAICostPer1K, 0.002),

		CostBaselineProvider:  yc.stringDefault(yc.CostBaselineProvider, "frontier"),
		CostBaselineModel:     yc.stringDefault(yc.CostBaselineModel, ""),   // Falls back to FrontierModel later
		CostBaselineRatePer1K: yc.floatDefault(yc.CostBaselineRatePer1K, 0), // Falls back to FrontierCostPer1K later

		QualityConcurrency: yc.intDefault(yc.QualityConcurrency, 2),
		QualityQueueDepth:  yc.intDefault(yc.QualityQueueDepth, 64),
		QualityTimeout:     yc.durationDefault(yc.QualityTimeout, 60*time.Second),
		QualityStderrCap:   yc.intDefault(yc.QualityStderrCap, 2*1024),
		QualityEnabled:     yc.intDefault(yc.QualityConcurrency, 2) > 0,

		PromptInjectionMode: middleware.ParseInjectionMode(yc.PromptInjectionMode),

		LogLevel:  parseLogLevel(yc.LogLevel),
		LogFormat: parseLogFormat(yc.LogFormat),

		Debug:          yc.Debug, // defaults to false in toConfig if not set
		DebugBodyBytes: yc.intDefault(yc.DebugBodyBytes, DefaultDebugBodyBytes),

		ModelsEndpointEnabled: yc.boolFieldDefault(yc.ModelsEndpointEnabled, true),
		ModelsCacheTTL:        yc.durationDefault(yc.ModelsCacheTTL, 5*time.Minute),

		RAGPollInterval:    yc.durationDefault(yc.RAGPollInterval, DefaultRAGPollInterval),
		RAGWatcherDisabled: yc.boolFieldDefault(yc.RAGWatcherDisabled, false),

		RateLimitRPM:   yc.intDefault(yc.RateLimitRPM, 0),
		RateLimitBurst: yc.intDefault(yc.RateLimitBurst, 0),
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

	return cfg
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
			return fmt.Errorf("config: server_read_timeout must not be negative, got %s", d)
		}
	}
	if yc.WriteTimeout != "" {
		if d, err := time.ParseDuration(yc.WriteTimeout); err == nil && d < 0 {
			return fmt.Errorf("config: server_write_timeout must not be negative, got %s", d)
		}
	}
	if yc.IdleTimeout != "" {
		if d, err := time.ParseDuration(yc.IdleTimeout); err == nil && d < 0 {
			return fmt.Errorf("config: server_idle_timeout must not be negative, got %s", d)
		}
	}
	if yc.MaxHeaderBytes < 0 {
		return fmt.Errorf("config: server_max_header_bytes must not be negative, got %d", yc.MaxHeaderBytes)
	}
	if yc.ShutdownTimeout != "" {
		if d, err := time.ParseDuration(yc.ShutdownTimeout); err == nil && d < 0 {
			return fmt.Errorf("config: shutdown_timeout must not be negative, got %s", d)
		}
	}
	return nil
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
