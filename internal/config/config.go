// Package config loads runtime configuration from environment variables.
//
// All values have safe defaults so the binary boots in development with a
// local Ollama instance. Secrets (FRONTIER_API_KEY) must be supplied via env
// in any non-development deployment.
package config

import (
	"fmt"
	"os"
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
	OllamaURL     string // "http://localhost:11434"
	RouterModel   string // "qwen3-coder:4b"
	LocalModel    string // "qwen3-coder:8b"
	EmbeddingModel string // "nomic-embed-text"

	// Frontier API (OpenAI-compatible)
	FrontierURL   string // "https://api.openai.com/v1/chat/completions"
	FrontierModel string // "gpt-4o"
	FrontierKey   string // required for actual frontier traffic; may be empty in dev

	// RAG
	ExamplesDir string // "./few_shot_examples"
	RAGThreshold float64 // cosine similarity cutoff for retrieval (0.55)

	// Routing
	TokenGuardrail int           // estimated tokens above this force frontier (6000)
	SLMTimeout     time.Duration // Qwen3-Coder routing timeout (8s)
	FusionTimeout  time.Duration // per-panel-member fetch timeout (120s)

	// Middleware prompts
	MetaPrompt string // appended to system prompt by prompt_engine
	TOONNotice string // appended when TOON compression is applied
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
		ExamplesDir:    getEnv("NEXUS_EXAMPLES_DIR", "./few_shot_examples"),
		MetaPrompt:     defaultMetaPrompt,
		TOONNotice:     defaultTOONNotice,
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

	return cfg, nil
}

// FrontierEnabled reports whether a frontier API key is configured. The proxy
// still runs without one (fusion will degrade to local-only), but frontier
// routing will return 401s if attempted.
func (c Config) FrontierEnabled() bool { return c.FrontierKey != "" }

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
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