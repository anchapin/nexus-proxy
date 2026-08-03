package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/middleware"
	ragpkg "github.com/anchapin/nexus-proxy/internal/rag"
)

// writeYAML writes content to a temp config.yaml and returns its path.
func writeYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// setAllEnvOverrides sets every NEXUS_ env var that LoadYAML honours to a
// valid, distinct value. This drives the happy-path branch of every
// env-override block in LoadYAML in a single call.
func setAllEnvOverrides(t *testing.T) {
	t.Helper()

	// Server
	t.Setenv("NEXUS_ADDR", ":7777")
	t.Setenv("NEXUS_SERVER_READ_TIMEOUT", "11s")
	t.Setenv("NEXUS_SERVER_WRITE_TIMEOUT", "12s")
	t.Setenv("NEXUS_SERVER_IDLE_TIMEOUT", "13s")
	t.Setenv("NEXUS_SERVER_MAX_HEADER_BYTES", "1048576")
	t.Setenv("NEXUS_MAX_BODY_BYTES", "2000000")
	t.Setenv("NEXUS_MAX_RESPONSE_BYTES", "30000000")
	t.Setenv("NEXUS_SHUTDOWN_TIMEOUT", "44s")
	t.Setenv("NEXUS_TRACING_TIMEOUT", "22s")
	t.Setenv("NEXUS_TLS_ENABLED", "true")
	// Logging
	t.Setenv("NEXUS_LOG_LEVEL", "debug")
	t.Setenv("NEXUS_LOG_FORMAT", "text")
	// Debug
	t.Setenv("NEXUS_DEBUG", "true")
	t.Setenv("NEXUS_DEBUG_BODY_BYTES", "1024")
	// Ollama
	t.Setenv("NEXUS_OLLAMA_URL", "http://ollama.example.com:11434/")
	t.Setenv("NEXUS_ROUTER_MODEL", "qwen3-coder:4b-env")
	t.Setenv("NEXUS_LOCAL_MODEL", "qwen3-coder:8b-env")
	t.Setenv("NEXUS_EMBEDDING_MODEL", "nomic-embed-env")
	// Frontier
	t.Setenv("NEXUS_FRONTIER_URL", "https://frontier.example.com/v1/chat/completions")
	t.Setenv("NEXUS_FRONTIER_MODEL", "gpt-4o-env")
	t.Setenv("NEXUS_FRONTIER_API_KEY", "sk-frontier-env")
	t.Setenv("NEXUS_FRONTIER_COST_PER_1K", "0.012")
	// Z.ai
	t.Setenv("NEXUS_ZAI_URL", "https://zai.example.com/v1/chat/completions")
	t.Setenv("NEXUS_ZAI_MODEL", "glm-4.6-env")
	t.Setenv("NEXUS_ZAI_API_KEY", "zai-env-key")
	t.Setenv("NEXUS_ZAI_COST_PER_1K", "0.0034")
	// Auth
	t.Setenv("NEXUS_PROXY_API_KEY", "proxy-env-key")
	t.Setenv("NEXUS_STATUS_PUBLIC", "true")
	// Cost baseline
	t.Setenv("NEXUS_COST_BASELINE_PROVIDER", "zai")
	t.Setenv("NEXUS_COST_BASELINE_MODEL", "glm-baseline-env")
	t.Setenv("NEXUS_COST_BASELINE_RATE_PER_1K", "0.0099")
	// Budget
	t.Setenv("NEXUS_BUDGET_DAILY_LIMIT", "5.5")
	t.Setenv("NEXUS_BUDGET_ALERT_ENABLED", "true")
	t.Setenv("NEXUS_BUDGET_ALERT_THRESHOLD", "0.66")
	t.Setenv("NEXUS_BUDGET_ALERT_WEBHOOK_URL", "https://hooks.example.com/budget")
	// Selector
	t.Setenv("NEXUS_SELECTOR_WINDOW", "2h")
	t.Setenv("NEXUS_SELECTOR_MIN_SAMPLES", "9")
	t.Setenv("NEXUS_SELECTOR_REFRESH", "90s")
	t.Setenv("NEXUS_PROVIDER_TAIL_WEIGHT", "0.42")
	// RAG
	t.Setenv("NEXUS_EXAMPLES_DIR", "./examples-env")
	t.Setenv("NEXUS_RAG_THRESHOLD", "0.77")
	t.Setenv("NEXUS_EMBEDDER_TYPE", "cohere")
	t.Setenv("NEXUS_EMBEDDER_BASE_URL", "https://api.cohere.ai/v1")
	t.Setenv("NEXUS_COHERE_API_KEY", "cohere-env-key")
	t.Setenv("NEXUS_RAG_DB", "/tmp/rag-env.db")
	t.Setenv("NEXUS_RAG_POLL_INTERVAL", "45s")
	t.Setenv("NEXUS_RAG_EMBED_CACHE_SIZE", "128")
	t.Setenv("NEXUS_RAG_EMBED_CACHE_TTL", "12h")
	t.Setenv("NEXUS_RAG_EMBED_CACHE_WAIT_TIMEOUT", "9s")
	t.Setenv("NEXUS_RAG_BATCH_SIZE", "64")
	// Routing
	t.Setenv("NEXUS_TOKEN_GUARDRAIL", "9000")
	t.Setenv("NEXUS_SLM_TIMEOUT", "17s")
	t.Setenv("NEXUS_SLM_CACHE_MAX_ENTRIES", "256")
	t.Setenv("NEXUS_SLM_CACHE_TTL", "60s")
	t.Setenv("NEXUS_SLMCACHE_SIMILARITY_THRESHOLD", "0.5")
	t.Setenv("NEXUS_SLMCACHE_MAX_STALE", "7")
	t.Setenv("NEXUS_SLMCACHE_STALE_CLEANUP_THRESHOLD", "3")
	t.Setenv("NEXUS_SLMCACHE_SEMANTIC_SCAN_LIMIT", "100")
	t.Setenv("NEXUS_FUSION_TIMEOUT", "200s")
	t.Setenv("NEXUS_CASCADE_TIMEOUT", "40s")
	t.Setenv("NEXUS_ARBITER_TIMEOUT", "50s")
	t.Setenv("NEXUS_CASCADE_MAX_RESPONSE_BYTES", "40000000")
	// Fusion
	t.Setenv("NEXUS_FUSION_PROGRESSIVE", "false")
	t.Setenv("NEXUS_FUSION_AGREEMENT_THRESHOLD", "0.92")
	t.Setenv("NEXUS_ARBITER_CACHE_TTL", "8m")
	t.Setenv("NEXUS_ARBITER_CACHE_MAX_ENTRIES", "128")
	// Health
	t.Setenv("NEXUS_HEALTH_POLL_INTERVAL", "15s")
	t.Setenv("NEXUS_HEALTH_BREAKER_THRESHOLD", "5")
	t.Setenv("NEXUS_HEALTH_PROBE_TIMEOUT", "8s")
	// Probe
	t.Setenv("NEXUS_PROBE_INTERVAL", "90s")
	t.Setenv("NEXUS_PROBE_TIMEOUT", "4s")
	t.Setenv("NEXUS_PROBE_BYTES_PER_TOKEN", "65536")
	t.Setenv("NEXUS_PROBE_THERMAL_THRESHOLD", "80")
	// Local
	t.Setenv("NEXUS_LOCAL_MAX_CONCURRENT", "4")
	t.Setenv("NEXUS_LOCAL_VRAM_BYTES_PER_SLOT", "5368709120")
	t.Setenv("NEXUS_LOCAL_COOLDOWN", "25s")
	// Judge
	t.Setenv("NEXUS_JUDGE_URL", "https://judge.example.com/v1/chat/completions")
	t.Setenv("NEXUS_JUDGE_MODEL", "judge-model-env")
	t.Setenv("NEXUS_JUDGE_API_KEY", "judge-env-key")
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "0.25")
	t.Setenv("NEXUS_JUDGE_CONCURRENCY", "3")
	t.Setenv("NEXUS_JUDGE_QUEUE", "128")
	t.Setenv("NEXUS_JUDGE_TIMEOUT", "45s")
	t.Setenv("NEXUS_JUDGE_COST_PER_1K", "0.0011")
	t.Setenv("NEXUS_JUDGE_DB", "/tmp/judge-env.db")
	// Routing confidence
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_DB", "/tmp/routing-env.db")
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_FLOOR", "0.45")
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_CEILING", "0.88")
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES", "7")
	t.Setenv("NEXUS_ROUTING_CONFIDENCE_WINDOW", "200h")
	// Quality
	t.Setenv("NEXUS_QUALITY_CONCURRENCY", "4")
	t.Setenv("NEXUS_QUALITY_QUEUE", "100")
	t.Setenv("NEXUS_QUALITY_TIMEOUT", "90s")
	t.Setenv("NEXUS_QUALITY_STDERR_CAP", "4096")
	t.Setenv("NEXUS_QUALITY_DROPPED_RING_SIZE", "300")
	// Deprecated backward-compat alias (issue #924) — triggers a slog.Warn.
	t.Setenv("NEXUS_QUALITY_DROPED_RING_SIZE", "300")
	// Middleware prompts
	t.Setenv("NEXUS_META_PROMPT", "[ENV META]")
	t.Setenv("NEXUS_TOON_NOTICE", "[ENV TOON]")
	t.Setenv("NEXUS_TOON_UNFENCED", "false")
	// Prompt injection
	t.Setenv("NEXUS_PROMPT_INJECTION_MODE", "strict")
	t.Setenv("NEXUS_INJECTION_SCAN_ROLES", "system,user")
	// Telemetry
	t.Setenv("NEXUS_TELEMETRY_PATH", "/tmp/tel-env.jsonl")
	t.Setenv("NEXUS_TELEMETRY_MAX_BYTES", "9999")
	t.Setenv("NEXUS_TELEMETRY_MAX_FILES", "11")
	t.Setenv("NEXUS_TELEMETRY_BUFFER_SIZE", "2222")
	t.Setenv("NEXUS_TELEMETRY_FLUSH_INTERVAL", "3s")
	t.Setenv("NEXUS_METRICS_DB", "/tmp/metrics-env.db")
	t.Setenv("NEXUS_METRICS_RETENTION_DAYS", "21")
	t.Setenv("NEXUS_METRICS_BATCH_SIZE", "128")
	t.Setenv("NEXUS_METRICS_BATCH_TIMEOUT", "50ms")
	// OTLP retry/back-off (issue #803)
	t.Setenv("NEXUS_TRACING_MAX_RETRIES", "5")
	t.Setenv("NEXUS_TRACING_RETRY_BASE_DELAY", "150ms")
	t.Setenv("NEXUS_TRACING_RETRY_MAX_DELAY", "3s")
	// Models
	t.Setenv("NEXUS_MODELS_ENDPOINT", "false")
	t.Setenv("NEXUS_MODELS_CACHE_TTL", "9m")
	// Trusted proxies
	t.Setenv("NEXUS_TRUSTED_PROXIES", "10.0.0.0/8")
	// Rate limit
	t.Setenv("NEXUS_RATE_LIMIT_RPM", "120")
	t.Setenv("NEXUS_RATE_LIMIT_BURST", "30")
	t.Setenv("NEXUS_RATE_LIMIT_BY_API_KEY", "true")
	// Auth brute-force protection (issue #840)
	t.Setenv("NEXUS_AUTH_RATE_LIMIT_RPM", "40")
	t.Setenv("NEXUS_AUTH_RATE_LIMIT_BURST", "20")
	t.Setenv("NEXUS_AUTH_RATE_LIMIT_WINDOW", "11m")
	// Tracing
	t.Setenv("NEXUS_TRACING_ENDPOINT", "https://otel.example.com/v1/traces")
	t.Setenv("NEXUS_TRACING_QUEUE_SIZE", "512")
	t.Setenv("NEXUS_TRACING_BATCH_SIZE", "128")
	t.Setenv("NEXUS_TRACING_SAMPLE_RATE", "0.5")
}

// TestLoadYAMLEnvOverridesComprehensive drives the happy-path branch of every
// env-override block in LoadYAML by setting all ~120 honoured env vars and
// asserting representative fields across every getEnv* helper type.
func TestLoadYAMLEnvOverridesComprehensive(t *testing.T) {
	path := writeYAML(t, "{}\n")
	setAllEnvOverrides(t)

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}

	// String fields
	checks := map[string]func() bool{
		"Addr":              func() bool { return cfg.Addr == ":7777" },
		"RouterModel":       func() bool { return cfg.RouterModel == "qwen3-coder:4b-env" },
		"ProxyAPIKey":       func() bool { return cfg.ProxyAPIKey == "proxy-env-key" },
		"FrontierKey":       func() bool { return cfg.FrontierKey == "sk-frontier-env" },
		"MetaPrompt":        func() bool { return cfg.MetaPrompt == "[ENV META]" },
		"TelemetryPath":     func() bool { return cfg.TelemetryPath == "/tmp/tel-env.jsonl" },
		"MetricsDBPath":     func() bool { return cfg.MetricsDBPath == "/tmp/metrics-env.db" },
		"TracingEndpoint":   func() bool { return cfg.TracingEndpoint == "https://otel.example.com/v1/traces" },
		"TrustedProxiesRaw": func() bool { return cfg.TrustedProxiesRaw == "10.0.0.0/8" },
		"EmbedderType":      func() bool { return cfg.EmbedderType == ragpkg.EmbedderTypeCohere },
	}
	for name, ok := range checks {
		if !ok() {
			t.Errorf("%s not overridden by env", name)
		}
	}

	// Int fields
	if cfg.TokenGuardrail != 9000 {
		t.Errorf("TokenGuardrail = %d, want 9000", cfg.TokenGuardrail)
	}
	if cfg.RateLimitRPM != 120 {
		t.Errorf("RateLimitRPM = %d, want 120", cfg.RateLimitRPM)
	}
	if cfg.DebugBodyBytes != 1024 {
		t.Errorf("DebugBodyBytes = %d, want 1024", cfg.DebugBodyBytes)
	}
	if cfg.SLMCacheMaxStale != 7 {
		t.Errorf("SLMCacheMaxStale = %d, want 7", cfg.SLMCacheMaxStale)
	}
	if cfg.QualityStderrCap != 4096 {
		t.Errorf("QualityStderrCap = %d, want 4096", cfg.QualityStderrCap)
	}
	// Int64
	if cfg.LocalVRAMBytesPerSlot != 5368709120 {
		t.Errorf("LocalVRAMBytesPerSlot = %d, want 5368709120", cfg.LocalVRAMBytesPerSlot)
	}
	// Float
	if cfg.RAGThreshold != 0.77 {
		t.Errorf("RAGThreshold = %v, want 0.77", cfg.RAGThreshold)
	}
	if cfg.ProviderTailWeight != 0.42 {
		t.Errorf("ProviderTailWeight = %v, want 0.42", cfg.ProviderTailWeight)
	}
	if cfg.BudgetDailyLimit != 5.5 {
		t.Errorf("BudgetDailyLimit = %v, want 5.5", cfg.BudgetDailyLimit)
	}
	if cfg.TracingSampleRate != 0.5 {
		t.Errorf("TracingSampleRate = %v, want 0.5", cfg.TracingSampleRate)
	}
	if cfg.FrontierCostPer1K != 0.012 {
		t.Errorf("FrontierCostPer1K = %v, want 0.012", cfg.FrontierCostPer1K)
	}
	// Bool
	if !cfg.TLSEnabled {
		t.Error("TLSEnabled = false, want true")
	}
	if !cfg.Debug {
		t.Error("Debug = false, want true")
	}
	if !cfg.StatusPublic {
		t.Error("StatusPublic = false, want true")
	}
	if cfg.FusionProgressiveDelivery {
		t.Error("FusionProgressiveDelivery = true, want false")
	}
	if !cfg.RateLimitByAPIKey {
		t.Error("RateLimitByAPIKey = false, want true")
	}
	if !cfg.BudgetAlertEnabled {
		t.Error("BudgetAlertEnabled = false, want true")
	}
	// Duration
	if cfg.SLMTimeout != 17*time.Second {
		t.Errorf("SLMTimeout = %v, want 17s", cfg.SLMTimeout)
	}
	if cfg.ShutdownTimeout != 44*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 44s", cfg.ShutdownTimeout)
	}
	if cfg.JudgeTimeout != 45*time.Second {
		t.Errorf("JudgeTimeout = %v, want 45s", cfg.JudgeTimeout)
	}
	if cfg.SelectorWindow != 2*time.Hour {
		t.Errorf("SelectorWindow = %v, want 2h", cfg.SelectorWindow)
	}
	if cfg.AuthRateLimitWindow != 11*time.Minute {
		t.Errorf("AuthRateLimitWindow = %v, want 11m", cfg.AuthRateLimitWindow)
	}
	// Log level / format
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
	if cfg.LogFormat != LogFormatText {
		t.Errorf("LogFormat = %v, want text", cfg.LogFormat)
	}
	// Injection scan roles ([]string)
	if !slicesEqual(cfg.InjectionScanRoles, []string{"system", "user"}) {
		t.Errorf("InjectionScanRoles = %v, want [system user]", cfg.InjectionScanRoles)
	}
	// Prompt injection mode
	if cfg.PromptInjectionMode != middleware.InjectionModeStrict {
		t.Errorf("PromptInjectionMode = %v, want strict", cfg.PromptInjectionMode)
	}
}

// TestLoadYAMLEnvPrecedenceTable is a table-driven test asserting that an
// explicitly set env var overrides the corresponding YAML value. Each entry
// spans a distinct getEnv* helper type. All entries share a single LoadYAML
// call, so one assertion run covers every precedence branch.
func TestLoadYAMLEnvPrecedenceTable(t *testing.T) {
	cases := []struct {
		name    string
		yamlKey string
		yamlVal string
		envName string
		envVal  string
		check   func(*testing.T, Config)
	}{
		// getEnv (string)
		{"addr_string", "addr", ":1111", "NEXUS_ADDR", ":2222", func(t *testing.T, c Config) {
			if c.Addr != ":2222" {
				t.Errorf("Addr = %q, want :2222", c.Addr)
			}
		}},
		// getEnvAllowEmpty (string, may be empty)
		{"telemetry_path", "telemetry_path", "/yaml.jsonl", "NEXUS_TELEMETRY_PATH", "/env.jsonl", func(t *testing.T, c Config) {
			if c.TelemetryPath != "/env.jsonl" {
				t.Errorf("TelemetryPath = %q, want /env.jsonl", c.TelemetryPath)
			}
		}},
		// getEnvInt
		{"token_guardrail_int", "token_guardrail", "1234", "NEXUS_TOKEN_GUARDRAIL", "5678", func(t *testing.T, c Config) {
			if c.TokenGuardrail != 5678 {
				t.Errorf("TokenGuardrail = %d, want 5678", c.TokenGuardrail)
			}
		}},
		// getEnvBool
		{"tls_bool", "tls_enabled", "false", "NEXUS_TLS_ENABLED", "true", func(t *testing.T, c Config) {
			if !c.TLSEnabled {
				t.Error("TLSEnabled = false, want true")
			}
		}},
		// getEnvFloat
		{"rag_threshold_float", "rag_threshold", "0.1", "NEXUS_RAG_THRESHOLD", "0.88", func(t *testing.T, c Config) {
			if c.RAGThreshold != 0.88 {
				t.Errorf("RAGThreshold = %v, want 0.88", c.RAGThreshold)
			}
		}},
		// getEnvDuration
		{"slm_timeout_duration", "slm_timeout", "5s", "NEXUS_SLM_TIMEOUT", "33s", func(t *testing.T, c Config) {
			if c.SLMTimeout != 33*time.Second {
				t.Errorf("SLMTimeout = %v, want 33s", c.SLMTimeout)
			}
		}},
		// getEnvInt — server max header bytes
		{"max_header_bytes_int", "server_max_header_bytes", "1000", "NEXUS_SERVER_MAX_HEADER_BYTES", "9999", func(t *testing.T, c Config) {
			if c.MaxHeaderBytes != 9999 {
				t.Errorf("MaxHeaderBytes = %d, want 9999", c.MaxHeaderBytes)
			}
		}},
		// getEnvFloat — budget daily limit
		{"budget_daily_limit_float", "budget_daily_limit", "1.0", "NEXUS_BUDGET_DAILY_LIMIT", "9.9", func(t *testing.T, c Config) {
			if c.BudgetDailyLimit != 9.9 {
				t.Errorf("BudgetDailyLimit = %v, want 9.9", c.BudgetDailyLimit)
			}
		}},
		// getEnvDuration — selector window
		{"selector_window_duration", "selector_window", "30m", "NEXUS_SELECTOR_WINDOW", "3h", func(t *testing.T, c Config) {
			if c.SelectorWindow != 3*time.Hour {
				t.Errorf("SelectorWindow = %v, want 3h", c.SelectorWindow)
			}
		}},
		// getEnvBool — budget alert enabled
		{"budget_alert_enabled_bool", "budget_alert_enabled", "false", "NEXUS_BUDGET_ALERT_ENABLED", "true", func(t *testing.T, c Config) {
			if !c.BudgetAlertEnabled {
				t.Error("BudgetAlertEnabled = false, want true")
			}
		}},
		// getEnvFloat — provider tail weight (range-validated)
		{"provider_tail_weight_float", "provider_tail_weight", "0.1", "NEXUS_PROVIDER_TAIL_WEIGHT", "0.55", func(t *testing.T, c Config) {
			if c.ProviderTailWeight != 0.55 {
				t.Errorf("ProviderTailWeight = %v, want 0.55", c.ProviderTailWeight)
			}
		}},
		// getEnvInt — health breaker threshold
		{"health_breaker_threshold_int", "health_breaker_threshold", "2", "NEXUS_HEALTH_BREAKER_THRESHOLD", "8", func(t *testing.T, c Config) {
			if c.HealthBreakerThreshold != 8 {
				t.Errorf("HealthBreakerThreshold = %d, want 8", c.HealthBreakerThreshold)
			}
		}},
	}

	// Build a single YAML containing all the yaml-side values.
	var sb strings.Builder
	for _, tc := range cases {
		sb.WriteString(tc.yamlKey)
		sb.WriteString(": ")
		sb.WriteString(tc.yamlVal)
		sb.WriteByte('\n')
	}
	path := writeYAML(t, sb.String())

	// Set all the env-side overrides.
	for _, tc := range cases {
		t.Setenv(tc.envName, tc.envVal)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.check(t, cfg) })
	}
}

// TestLoadYAMLEnvErrorPaths exercises the error-return branches of the
// env-override blocks in LoadYAML. Each subtest sets one malformed env var
// and asserts LoadYAML returns a non-nil error.
func TestLoadYAMLEnvErrorPaths(t *testing.T) {
	cases := []struct {
		name string
		env  string
		val  string
	}{
		// Malformed durations
		{"read_timeout_bad_duration", "NEXUS_SERVER_READ_TIMEOUT", "not-a-duration"},
		{"write_timeout_bad_duration", "NEXUS_SERVER_WRITE_TIMEOUT", "xyz"},
		{"idle_timeout_bad_duration", "NEXUS_SERVER_IDLE_TIMEOUT", "abc"},
		{"shutdown_timeout_negative", "NEXUS_SHUTDOWN_TIMEOUT", "-5s"},
		{"tracing_timeout_bad", "NEXUS_TRACING_TIMEOUT", "bad"},
		{"tracing_timeout_negative", "NEXUS_TRACING_TIMEOUT", "-1s"},
		{"slm_timeout_bad_duration", "NEXUS_SLM_TIMEOUT", "abc"},
		{"slm_cache_ttl_bad", "NEXUS_SLM_CACHE_TTL", "nope"},
		{"fusion_timeout_bad", "NEXUS_FUSION_TIMEOUT", "xx"},
		{"cascade_timeout_bad", "NEXUS_CASCADE_TIMEOUT", "xx"},
		{"arbiter_timeout_bad", "NEXUS_ARBITER_TIMEOUT", "xx"},
		{"health_poll_interval_bad", "NEXUS_HEALTH_POLL_INTERVAL", "xx"},
		{"health_probe_timeout_bad", "NEXUS_HEALTH_PROBE_TIMEOUT", "xx"},
		{"probe_interval_bad", "NEXUS_PROBE_INTERVAL", "xx"},
		{"probe_timeout_bad", "NEXUS_PROBE_TIMEOUT", "xx"},
		{"local_cooldown_bad", "NEXUS_LOCAL_COOLDOWN", "xx"},
		{"selector_window_bad", "NEXUS_SELECTOR_WINDOW", "xx"},
		{"selector_refresh_bad", "NEXUS_SELECTOR_REFRESH", "xx"},
		{"judge_timeout_bad", "NEXUS_JUDGE_TIMEOUT", "xx"},
		{"quality_timeout_bad", "NEXUS_QUALITY_TIMEOUT", "xx"},
		{"models_cache_ttl_bad", "NEXUS_MODELS_CACHE_TTL", "xx"},
		{"arbiter_cache_ttl_bad", "NEXUS_ARBITER_CACHE_TTL", "xx"},
		{"auth_rate_limit_window_bad", "NEXUS_AUTH_RATE_LIMIT_WINDOW", "xx"},
		{"rag_poll_interval_bad", "NEXUS_RAG_POLL_INTERVAL", "xx"},
		{"rag_embed_cache_ttl_bad", "NEXUS_RAG_EMBED_CACHE_TTL", "xx"},
		{"rag_embed_cache_wait_bad", "NEXUS_RAG_EMBED_CACHE_WAIT_TIMEOUT", "xx"},
		{"routing_confidence_window_bad", "NEXUS_ROUTING_CONFIDENCE_WINDOW", "xx"},
		// Malformed ints
		{"max_header_bytes_bad_int", "NEXUS_SERVER_MAX_HEADER_BYTES", "abc"},
		{"max_body_bytes_bad_int", "NEXUS_MAX_BODY_BYTES", "abc"},
		{"max_response_bytes_bad_int", "NEXUS_MAX_RESPONSE_BYTES", "abc"},
		{"token_guardrail_bad_int", "NEXUS_TOKEN_GUARDRAIL", "abc"},
		{"debug_body_bytes_bad_int", "NEXUS_DEBUG_BODY_BYTES", "abc"},
		{"rate_limit_rpm_bad_int", "NEXUS_RATE_LIMIT_RPM", "abc"},
		{"rate_limit_burst_bad_int", "NEXUS_RATE_LIMIT_BURST", "abc"},
		{"auth_rate_limit_rpm_bad_int", "NEXUS_AUTH_RATE_LIMIT_RPM", "abc"},
		{"auth_rate_limit_burst_bad_int", "NEXUS_AUTH_RATE_LIMIT_BURST", "abc"},
		{"tracing_queue_size_bad_int", "NEXUS_TRACING_QUEUE_SIZE", "abc"},
		{"tracing_batch_size_bad_int", "NEXUS_TRACING_BATCH_SIZE", "abc"},
		{"health_breaker_threshold_bad_int", "NEXUS_HEALTH_BREAKER_THRESHOLD", "abc"},
		{"probe_bytes_per_token_bad_int", "NEXUS_PROBE_BYTES_PER_TOKEN", "abc"},
		{"probe_thermal_bad_int", "NEXUS_PROBE_THERMAL_THRESHOLD", "abc"},
		{"local_max_concurrent_bad_int", "NEXUS_LOCAL_MAX_CONCURRENT", "abc"},
		{"local_vram_bad_int", "NEXUS_LOCAL_VRAM_BYTES_PER_SLOT", "abc"},
		{"slm_cache_max_entries_bad_int", "NEXUS_SLM_CACHE_MAX_ENTRIES", "abc"},
		{"slmcache_max_stale_bad_int", "NEXUS_SLMCACHE_MAX_STALE", "abc"},
		{"slmcache_cleanup_bad_int", "NEXUS_SLMCACHE_STALE_CLEANUP_THRESHOLD", "abc"},
		{"slmcache_scan_limit_bad_int", "NEXUS_SLMCACHE_SEMANTIC_SCAN_LIMIT", "abc"},
		{"arbiter_cache_max_entries_bad_int", "NEXUS_ARBITER_CACHE_MAX_ENTRIES", "abc"},
		{"selector_min_samples_bad_int", "NEXUS_SELECTOR_MIN_SAMPLES", "abc"},
		{"cascade_max_response_bad_int", "NEXUS_CASCADE_MAX_RESPONSE_BYTES", "abc"},
		{"quality_concurrency_bad_int", "NEXUS_QUALITY_CONCURRENCY", "abc"},
		{"quality_queue_bad_int", "NEXUS_QUALITY_QUEUE", "abc"},
		{"quality_stderr_cap_bad_int", "NEXUS_QUALITY_STDERR_CAP", "abc"},
		{"quality_dropped_ring_bad_int", "NEXUS_QUALITY_DROPPED_RING_SIZE", "abc"},
		{"judge_concurrency_bad_int", "NEXUS_JUDGE_CONCURRENCY", "abc"},
		{"judge_queue_bad_int", "NEXUS_JUDGE_QUEUE", "abc"},
		{"routing_confidence_min_samples_bad_int", "NEXUS_ROUTING_CONFIDENCE_MIN_SAMPLES", "abc"},
		// Malformed floats
		{"frontier_cost_bad_float", "NEXUS_FRONTIER_COST_PER_1K", "abc"},
		{"zai_cost_bad_float", "NEXUS_ZAI_COST_PER_1K", "abc"},
		{"cost_baseline_rate_bad_float", "NEXUS_COST_BASELINE_RATE_PER_1K", "abc"},
		{"budget_daily_limit_bad_float", "NEXUS_BUDGET_DAILY_LIMIT", "abc"},
		{"budget_alert_threshold_bad_float", "NEXUS_BUDGET_ALERT_THRESHOLD", "abc"},
		{"rag_threshold_bad_float", "NEXUS_RAG_THRESHOLD", "abc"},
		{"slmcache_similarity_bad_float", "NEXUS_SLMCACHE_SIMILARITY_THRESHOLD", "abc"},
		{"fusion_agreement_bad_float", "NEXUS_FUSION_AGREEMENT_THRESHOLD", "abc"},
		{"judge_sample_rate_bad_float", "NEXUS_JUDGE_SAMPLE_RATE", "abc"},
		{"judge_cost_bad_float", "NEXUS_JUDGE_COST_PER_1K", "abc"},
		{"routing_confidence_floor_bad_float", "NEXUS_ROUTING_CONFIDENCE_FLOOR", "abc"},
		{"routing_confidence_ceiling_bad_float", "NEXUS_ROUTING_CONFIDENCE_CEILING", "abc"},
		{"tracing_sample_rate_bad_float", "NEXUS_TRACING_SAMPLE_RATE", "abc"},
		// Out-of-range floats (rejected before assignment)
		{"provider_tail_weight_out_of_range", "NEXUS_PROVIDER_TAIL_WEIGHT", "2.5"},
		{"fusion_agreement_out_of_range", "NEXUS_FUSION_AGREEMENT_THRESHOLD", "9"},
		// Invalid log level
		{"log_level_bogus", "NEXUS_LOG_LEVEL", "bogus"},
		// Invalid trusted-proxy CIDR
		{"trusted_proxies_invalid_cidr", "NEXUS_TRUSTED_PROXIES", "not-a-cidr"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.val)
			path := writeYAML(t, "{}\n")
			_, err := LoadYAML(path)
			if err == nil {
				t.Errorf("LoadYAML with %s=%q: expected error, got nil", tc.env, tc.val)
			}
		})
	}
}

// TestLoadYAMLValidateErrorPaths exercises the YAML validate() branches that
// are not otherwise covered: negative server timeouts and fractional fields
// outside [0,1].
func TestLoadYAMLValidateErrorPaths(t *testing.T) {
	cases := []struct {
		name    string
		yamlKey string
		yamlVal string
	}{
		{"read_timeout_negative", "server_read_timeout", "-1s"},
		{"write_timeout_negative", "server_write_timeout", "-2s"},
		{"idle_timeout_negative", "server_idle_timeout", "-3s"},
		{"max_header_bytes_negative", "server_max_header_bytes", "-1"},
		{"budget_alert_threshold_high", "budget_alert_threshold", "2.0"},
		{"fusion_agreement_threshold_high", "fusion_agreement_threshold", "1.5"},
		{"provider_tail_weight_high", "provider_tail_weight", "2.0"},
		{"tracing_sample_rate_high", "tracing_sample_rate", "3.0"},
		{"routing_confidence_floor_high", "routing_confidence_floor", "5.0"},
		{"routing_confidence_ceiling_high", "routing_confidence_ceiling", "5.0"},
		{"auth_mode_invalid", "auth_mode", "ldap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := tc.yamlKey + ": " + tc.yamlVal + "\n"
			path := writeYAML(t, content)
			_, err := LoadYAML(path)
			if err == nil {
				t.Errorf("LoadYAML with %s=%s: expected validation error, got nil", tc.yamlKey, tc.yamlVal)
			}
		})
	}
}

// TestEffectiveCascadeMaxResponseBytes covers the zero→default and
// explicit→explicit branches of the cascade response cap accessor.
func TestEffectiveCascadeMaxResponseBytes(t *testing.T) {
	if got := (Config{}).EffectiveCascadeMaxResponseBytes(); got != DefaultMaxResponseBytes {
		t.Errorf("zero Config EffectiveCascadeMaxResponseBytes = %d, want %d", got, DefaultMaxResponseBytes)
	}
	const want = 12345678
	cfg := Config{CascadeMaxResponseBytes: want}
	if got := cfg.EffectiveCascadeMaxResponseBytes(); got != want {
		t.Errorf("EffectiveCascadeMaxResponseBytes = %d, want %d", got, want)
	}
}

// TestConfigEnabledAccessors exercises the boolean accessor methods that gate
// optional subsystems. Each method has an enabled/disabled branch.
func TestConfigEnabledAccessors(t *testing.T) {
	t.Run("ModelsCacheEnabled", func(t *testing.T) {
		if (Config{}).ModelsCacheEnabled() {
			t.Error("zero Config ModelsCacheEnabled = true, want false")
		}
		if !(Config{ModelsCacheTTL: 5 * time.Minute}).ModelsCacheEnabled() {
			t.Error("ModelsCacheTTL>0 ModelsCacheEnabled = false, want true")
		}
	})
	t.Run("PromptInjectionIsolated", func(t *testing.T) {
		if (Config{}).PromptInjectionIsolated() {
			t.Error("off mode PromptInjectionIsolated = true, want false")
		}
		if !(Config{PromptInjectionMode: middleware.InjectionModeWarn}).PromptInjectionIsolated() {
			t.Error("warn mode PromptInjectionIsolated = false, want true")
		}
		if !(Config{PromptInjectionMode: middleware.InjectionModeStrict}).PromptInjectionIsolated() {
			t.Error("strict mode PromptInjectionIsolated = false, want true")
		}
	})
	t.Run("MetricsEnabled", func(t *testing.T) {
		if (Config{}).MetricsEnabled() {
			t.Error("empty MetricsDBPath MetricsEnabled = true, want false")
		}
		if !(Config{MetricsDBPath: "/tmp/m.db"}).MetricsEnabled() {
			t.Error("set MetricsDBPath MetricsEnabled = false, want true")
		}
	})
	t.Run("BudgetEnabled", func(t *testing.T) {
		if (Config{}).BudgetEnabled() {
			t.Error("zero BudgetDailyLimit BudgetEnabled = true, want false")
		}
		if !(Config{BudgetDailyLimit: 1.0}).BudgetEnabled() {
			t.Error("BudgetDailyLimit>0 BudgetEnabled = false, want true")
		}
	})
	t.Run("AuthRateLimitEnabled", func(t *testing.T) {
		if (Config{}).AuthRateLimitEnabled() {
			t.Error("zero AuthRateLimitRPM AuthRateLimitEnabled = true, want false")
		}
		if !(Config{AuthRateLimitRPM: 5}).AuthRateLimitEnabled() {
			t.Error("AuthRateLimitRPM>0 AuthRateLimitEnabled = false, want true")
		}
	})
	t.Run("AuthEnabled", func(t *testing.T) {
		if (Config{}).AuthEnabled() {
			t.Error("empty ProxyAPIKey AuthEnabled = true, want false")
		}
		if !(Config{ProxyAPIKey: "secret"}).AuthEnabled() {
			t.Error("set ProxyAPIKey AuthEnabled = false, want true")
		}
	})
	t.Run("SLMCacheEnabled", func(t *testing.T) {
		if (Config{}).SLMCacheEnabled() {
			t.Error("zero SLMCacheTTL SLMCacheEnabled = true, want false")
		}
		if !(Config{SLMCacheTTL: 30 * time.Second}).SLMCacheEnabled() {
			t.Error("SLMCacheTTL>0 SLMCacheEnabled = false, want true")
		}
	})
	t.Run("RAGPersistentEnabled", func(t *testing.T) {
		if (Config{}).RAGPersistentEnabled() {
			t.Error("empty RAGDBPath RAGPersistentEnabled = true, want false")
		}
		if !(Config{RAGDBPath: "/tmp/rag.db"}).RAGPersistentEnabled() {
			t.Error("set RAGDBPath RAGPersistentEnabled = false, want true")
		}
	})
	t.Run("RAGWatcherEnabled", func(t *testing.T) {
		if (Config{}).RAGWatcherEnabled() {
			t.Error("zero Config RAGWatcherEnabled = true, want false")
		}
		// Persistent store set but no poll interval → disabled.
		if (Config{RAGDBPath: "/tmp/rag.db"}).RAGWatcherEnabled() {
			t.Error("RAGPersistent + zero poll RAGWatcherEnabled = true, want false")
		}
		// Both set → enabled.
		if !(Config{RAGDBPath: "/tmp/rag.db", RAGPollInterval: 30 * time.Second}).RAGWatcherEnabled() {
			t.Error("RAGPersistent + poll>0 RAGWatcherEnabled = false, want true")
		}
	})
	t.Run("JudgeDBEnabled", func(t *testing.T) {
		if (Config{}).JudgeDBEnabled() {
			t.Error("empty JudgeDBPath JudgeDBEnabled = true, want false")
		}
		if !(Config{JudgeDBPath: "/tmp/judge.db"}).JudgeDBEnabled() {
			t.Error("set JudgeDBPath JudgeDBEnabled = false, want true")
		}
	})
}

// TestLogFormatString exercises all three branches of LogFormat.String.
func TestLogFormatString(t *testing.T) {
	tests := []struct {
		f    LogFormat
		want string
	}{
		{LogFormatJSON, "json"},
		{LogFormatText, "text"},
		{LogFormat(99), "unknown"},
	}
	for _, tc := range tests {
		if got := tc.f.String(); got != tc.want {
			t.Errorf("LogFormat(%d).String() = %q, want %q", tc.f, got, tc.want)
		}
	}
}
