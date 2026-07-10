// Package config loads runtime configuration from environment variables.
//
// All values have safe defaults so the binary boots in development with a
// local Ollama instance. Secrets (FRONTIER_API_KEY) must be supplied via env
// in any non-development deployment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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

	// Routing
	TokenGuardrail int           // estimated tokens above this force frontier (6000)
	SLMTimeout     time.Duration // Qwen3-Coder routing timeout (8s)
	FusionTimeout  time.Duration // per-panel-member fetch timeout (120s)
	CascadeTimeout time.Duration // per-attempt timeout for cascade fallback (30s)
	ArbiterTimeout time.Duration // per-call timeout for the fusion arbiter stream (60s)

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

	return cfg, nil
}

// FrontierEnabled reports whether a frontier API key is configured. The proxy
// still runs without one (fusion will degrade to local-only), but frontier
// routing will return 401s if attempted.
func (c Config) FrontierEnabled() bool { return c.FrontierKey != "" }

// TelemetryEnabled reports whether the on-disk recorder should be started.
// Disabled when TelemetryPath is empty.
func (c Config) TelemetryEnabled() bool { return c.TelemetryPath != "" }

// MetricsEnabled reports whether the SQLite metrics store should be
// opened. Disabled when MetricsDBPath is empty.
func (c Config) MetricsEnabled() bool { return c.MetricsDBPath != "" }

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

const (
	defaultMetaPrompt = `
[PROXY METADATA ENHANCEMENT]: 
- ROLE: You are an elite, autonomous Principal AI Software Engineer.
- REASONING (Chain-of-Thought): You must ALWAYS think step-by-step. Analyze the requirements, edge cases, and architectural impact before generating a single line of code.
- CONSTRAINTS: Prioritize modularity, memory efficiency, and strict security patterns. Do not silently ignore errors or swallow exceptions.
- FORMATTING: Provide clean, well-commented code. Do not use generic pleasantries.`

	defaultTOONNotice = "\n\n[PROXY SYSTEM NOTE]: Data arrays have been compressed using Token-Oriented Object Notation (TOON). The format is `object_name[count]{key1,key2}:\n  val1,val2`. Read the schema header to map the comma-separated rows."
)
