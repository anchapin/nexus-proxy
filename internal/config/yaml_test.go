package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadYAMLDefaults(t *testing.T) {
	// Empty YAML — should fall back to same defaults as Load()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.Addr != ":8000" {
		t.Errorf("Addr = %q, want :8000", cfg.Addr)
	}
	if cfg.OllamaURL != "http://localhost:11434" {
		t.Errorf("OllamaURL = %q", cfg.OllamaURL)
	}
	if cfg.RouterModel != "qwen3-coder:4b" {
		t.Errorf("RouterModel = %q", cfg.RouterModel)
	}
	if cfg.TokenGuardrail != 6000 {
		t.Errorf("TokenGuardrail = %d, want 6000", cfg.TokenGuardrail)
	}
	if cfg.SLMTimeout != 8*time.Second {
		t.Errorf("SLMTimeout = %v, want 8s", cfg.SLMTimeout)
	}
	if cfg.FusionTimeout != 120*time.Second {
		t.Errorf("FusionTimeout = %v, want 120s", cfg.FusionTimeout)
	}
	if cfg.ArbiterTimeout != 60*time.Second {
		t.Errorf("ArbiterTimeout = %v, want 60s", cfg.ArbiterTimeout)
	}
	if cfg.RAGThreshold != 0.55 {
		t.Errorf("RAGThreshold = %v, want 0.55", cfg.RAGThreshold)
	}
	if cfg.ProbePollInterval != 60*time.Second {
		t.Errorf("ProbePollInterval = %v, want 60s", cfg.ProbePollInterval)
	}
	if !cfg.ProbeEnabled {
		t.Error("ProbeEnabled = false, want true")
	}
	if cfg.LocalCooldown != 10*time.Second {
		t.Errorf("LocalCooldown = %v, want 10s", cfg.LocalCooldown)
	}
}

func TestLoadYAMLYAMLOverrides(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
addr: ":9001"
ollama_url: "http://ollama.local:11434"
router_model: "llama3.2:3b"
local_model: "llama3.2:8b"
frontier_url: "https://api.frontier.example/v1/chat/completions"
frontier_model: "gpt-4.5"
frontier_api_key: "sk-yaml-key"
zai_url: "https://api.z.ai/v1/chat/completions"
zai_model: "glm-4.5"
zai_api_key: "zai-yaml-key"
token_guardrail: 8000
slm_timeout: "12s"
fusion_timeout: "180s"
cascade_timeout: "45s"
arbiter_timeout: "90s"
rag_threshold: 0.75
probe_interval: "90s"
probe_timeout: "3s"
probe_bytes_per_token: 131072
local_max_concurrent: 4
local_vram_bytes_per_slot: 1073741824
local_cooldown: "20s"
fusion_progressive_delivery: false
fusion_agreement_threshold: 0.9
models_endpoint_enabled: false
models_cache_ttl: "10m"
rate_limit_rpm: 120
rate_limit_burst: 30
trusted_proxies: "10.0.0.0/8"
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.Addr != ":9001" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.OllamaURL != "http://ollama.local:11434" {
		t.Errorf("OllamaURL = %q", cfg.OllamaURL)
	}
	if cfg.RouterModel != "llama3.2:3b" {
		t.Errorf("RouterModel = %q", cfg.RouterModel)
	}
	if cfg.LocalModel != "llama3.2:8b" {
		t.Errorf("LocalModel = %q", cfg.LocalModel)
	}
	if cfg.FrontierURL != "https://api.frontier.example/v1/chat/completions" {
		t.Errorf("FrontierURL = %q", cfg.FrontierURL)
	}
	if cfg.FrontierModel != "gpt-4.5" {
		t.Errorf("FrontierModel = %q", cfg.FrontierModel)
	}
	if cfg.FrontierKey != "sk-yaml-key" {
		t.Errorf("FrontierKey = %q", cfg.FrontierKey)
	}
	if cfg.ZAIModel != "glm-4.5" {
		t.Errorf("ZAIModel = %q", cfg.ZAIModel)
	}
	if cfg.ZAIKey != "zai-yaml-key" {
		t.Errorf("ZAIKey = %q", cfg.ZAIKey)
	}
	if cfg.TokenGuardrail != 8000 {
		t.Errorf("TokenGuardrail = %d", cfg.TokenGuardrail)
	}
	if cfg.SLMTimeout != 12*time.Second {
		t.Errorf("SLMTimeout = %v", cfg.SLMTimeout)
	}
	if cfg.FusionTimeout != 180*time.Second {
		t.Errorf("FusionTimeout = %v", cfg.FusionTimeout)
	}
	if cfg.CascadeTimeout != 45*time.Second {
		t.Errorf("CascadeTimeout = %v", cfg.CascadeTimeout)
	}
	if cfg.ArbiterTimeout != 90*time.Second {
		t.Errorf("ArbiterTimeout = %v", cfg.ArbiterTimeout)
	}
	if cfg.RAGThreshold != 0.75 {
		t.Errorf("RAGThreshold = %v", cfg.RAGThreshold)
	}
	if cfg.ProbePollInterval != 90*time.Second {
		t.Errorf("ProbePollInterval = %v", cfg.ProbePollInterval)
	}
	if cfg.ProbeTimeout != 3*time.Second {
		t.Errorf("ProbeTimeout = %v", cfg.ProbeTimeout)
	}
	if cfg.ProbeBytesPerToken != 131072 {
		t.Errorf("ProbeBytesPerToken = %d", cfg.ProbeBytesPerToken)
	}
	if cfg.LocalMaxConcurrent != 4 {
		t.Errorf("LocalMaxConcurrent = %d", cfg.LocalMaxConcurrent)
	}
	if cfg.LocalVRAMBytesPerSlot != 1073741824 {
		t.Errorf("LocalVRAMBytesPerSlot = %d", cfg.LocalVRAMBytesPerSlot)
	}
	if cfg.LocalCooldown != 20*time.Second {
		t.Errorf("LocalCooldown = %v", cfg.LocalCooldown)
	}
	if cfg.FusionProgressiveDelivery {
		t.Error("FusionProgressiveDelivery = true, want false from YAML")
	}
	if cfg.FusionAgreementThreshold != 0.9 {
		t.Errorf("FusionAgreementThreshold = %v, want 0.9", cfg.FusionAgreementThreshold)
	}
	if cfg.ModelsEndpointEnabled {
		t.Error("ModelsEndpointEnabled = true, want false from YAML")
	}
	if cfg.ModelsCacheTTL != 10*time.Minute {
		t.Errorf("ModelsCacheTTL = %v, want 10m", cfg.ModelsCacheTTL)
	}
	if cfg.RateLimitRPM != 120 {
		t.Errorf("RateLimitRPM = %d", cfg.RateLimitRPM)
	}
	if cfg.RateLimitBurst != 30 {
		t.Errorf("RateLimitBurst = %d", cfg.RateLimitBurst)
	}
	if len(cfg.TrustedProxies) != 1 {
		t.Errorf("TrustedProxies len = %d, want 1", len(cfg.TrustedProxies))
	}
}

func TestLoadYAMLEnvOverridesYAML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
addr: ":9001"
router_model: "llama3.2:3b"
token_guardrail: 8000
slm_timeout: "12s"
fusion_timeout: "180s"
frontier_api_key: "sk-yaml-key"
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Set env vars that override the YAML values
	t.Setenv("NEXUS_ADDR", ":9999")
	t.Setenv("NEXUS_ROUTER_MODEL", "codellama:7b")
	t.Setenv("NEXUS_TOKEN_GUARDRAIL", "10000")
	t.Setenv("NEXUS_SLM_TIMEOUT", "20s")
	t.Setenv("NEXUS_FUSION_TIMEOUT", "240s")
	t.Setenv("NEXUS_FRONTIER_API_KEY", "sk-env-key")

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.Addr != ":9999" {
		t.Errorf("Addr = %q, want :9999 (env overrides YAML :9001)", cfg.Addr)
	}
	if cfg.RouterModel != "codellama:7b" {
		t.Errorf("RouterModel = %q, want codellama:7b", cfg.RouterModel)
	}
	if cfg.TokenGuardrail != 10000 {
		t.Errorf("TokenGuardrail = %d, want 10000", cfg.TokenGuardrail)
	}
	if cfg.SLMTimeout != 20*time.Second {
		t.Errorf("SLMTimeout = %v, want 20s", cfg.SLMTimeout)
	}
	if cfg.FusionTimeout != 240*time.Second {
		t.Errorf("FusionTimeout = %v, want 240s", cfg.FusionTimeout)
	}
	if cfg.FrontierKey != "sk-env-key" {
		t.Errorf("FrontierKey = %q, want sk-env-key (env overrides YAML)", cfg.FrontierKey)
	}
}

func TestLoadYAMLFileNotFound(t *testing.T) {
	_, err := LoadYAML("/nonexistent/config.yaml")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestLoadYAMLMalformedYAML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(path, []byte("addr: [unclosed"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadYAML(path)
	if err == nil {
		t.Error("expected error for malformed YAML")
	}
}

func TestLoadYAMLNegativeTimeoutRejected(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
shutdown_timeout: "-5s"
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := LoadYAML(path)
	if err == nil {
		t.Error("expected error for negative shutdown timeout")
	}
}

func TestLoadYAMLTrustedProxiesYAML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
trusted_proxies: "10.0.0.0/8, 172.16.0.0/12"
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if len(cfg.TrustedProxies) != 2 {
		t.Errorf("TrustedProxies len = %d, want 2", len(cfg.TrustedProxies))
	}
}

func TestLoadYAMLRAGSettings(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
examples_dir: "/var/nexus/examples"
rag_threshold: 0.8
embedder_type: "openai"
embedder_base_url: "https://api.openai.com/v1"
rag_db_path: "/var/nexus/rag.db"
rag_poll_interval: "60s"
rag_watcher_disabled: true
rag_embed_cache_size: 512
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.ExamplesDir != "/var/nexus/examples" {
		t.Errorf("ExamplesDir = %q", cfg.ExamplesDir)
	}
	if cfg.RAGThreshold != 0.8 {
		t.Errorf("RAGThreshold = %v", cfg.RAGThreshold)
	}
	if string(cfg.EmbedderType) != "openai" {
		t.Errorf("EmbedderType = %q", cfg.EmbedderType)
	}
	if cfg.EmbedderBaseURL != "https://api.openai.com/v1" {
		t.Errorf("EmbedderBaseURL = %q", cfg.EmbedderBaseURL)
	}
	if cfg.RAGDBPath != "/var/nexus/rag.db" {
		t.Errorf("RAGDBPath = %q", cfg.RAGDBPath)
	}
	if cfg.RAGPollInterval != 60*time.Second {
		t.Errorf("RAGPollInterval = %v", cfg.RAGPollInterval)
	}
	if cfg.RAGWatcherDisabled != true {
		t.Errorf("RAGWatcherDisabled = %v, want true", cfg.RAGWatcherDisabled)
	}
	if cfg.RAGEmbedCacheSize != 512 {
		t.Errorf("RAGEmbedCacheSize = %d", cfg.RAGEmbedCacheSize)
	}
}

func TestLoadYAMLBudgetSettings(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
budget_daily_limit: 50.0
budget_alert_enabled: true
budget_alert_threshold: 0.9
budget_alert_webhook_url: "https://alert.example/hook"
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.BudgetDailyLimit != 50.0 {
		t.Errorf("BudgetDailyLimit = %v", cfg.BudgetDailyLimit)
	}
	if !cfg.BudgetAlertEnabled {
		t.Error("BudgetAlertEnabled = false, want true")
	}
	if cfg.BudgetAlertThreshold != 0.9 {
		t.Errorf("BudgetAlertThreshold = %v", cfg.BudgetAlertThreshold)
	}
	if cfg.BudgetAlertWebhookURL != "https://alert.example/hook" {
		t.Errorf("BudgetAlertWebhookURL = %q", cfg.BudgetAlertWebhookURL)
	}
}

func TestLoadYAMLJudgeSettings(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
judge_url: "https://judge.example/v1/chat/completions"
judge_model: "gpt-4o"
judge_api_key: "sk-judge-key"
judge_sample_rate: 0.25
judge_concurrency: 4
judge_queue: 128
judge_timeout: "60s"
judge_cost_per_1k: 0.003
judge_db_path: "/var/nexus/judge.db"
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.JudgeURL != "https://judge.example/v1/chat/completions" {
		t.Errorf("JudgeURL = %q", cfg.JudgeURL)
	}
	if cfg.JudgeModel != "gpt-4o" {
		t.Errorf("JudgeModel = %q", cfg.JudgeModel)
	}
	if cfg.JudgeAPIKey != "sk-judge-key" {
		t.Errorf("JudgeAPIKey = %q", cfg.JudgeAPIKey)
	}
	if cfg.JudgeSampleRate != 0.25 {
		t.Errorf("JudgeSampleRate = %v", cfg.JudgeSampleRate)
	}
	if cfg.JudgeConcurrency != 4 {
		t.Errorf("JudgeConcurrency = %d", cfg.JudgeConcurrency)
	}
	if cfg.JudgeQueueDepth != 128 {
		t.Errorf("JudgeQueueDepth = %d", cfg.JudgeQueueDepth)
	}
	if cfg.JudgeTimeout != 60*time.Second {
		t.Errorf("JudgeTimeout = %v", cfg.JudgeTimeout)
	}
	if cfg.JudgeCostPer1KUSD != 0.003 {
		t.Errorf("JudgeCostPer1KUSD = %v", cfg.JudgeCostPer1KUSD)
	}
	if cfg.JudgeDBPath != "/var/nexus/judge.db" {
		t.Errorf("JudgeDBPath = %q", cfg.JudgeDBPath)
	}
	if !cfg.JudgeEnabled {
		t.Error("JudgeEnabled = false, want true")
	}
}

func TestLoadYAMLQualitySettings(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
quality_concurrency: 4
quality_queue: 128
quality_timeout: "90s"
quality_stderr_cap: 4096
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.QualityConcurrency != 4 {
		t.Errorf("QualityConcurrency = %d", cfg.QualityConcurrency)
	}
	if cfg.QualityQueueDepth != 128 {
		t.Errorf("QualityQueueDepth = %d", cfg.QualityQueueDepth)
	}
	if cfg.QualityTimeout != 90*time.Second {
		t.Errorf("QualityTimeout = %v", cfg.QualityTimeout)
	}
	if cfg.QualityStderrCap != 4096 {
		t.Errorf("QualityStderrCap = %d", cfg.QualityStderrCap)
	}
	if !cfg.QualityEnabled {
		t.Error("QualityEnabled = false, want true")
	}
}

func TestLoadYAMLSelectorSettings(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
selector_window: "2h"
selector_min_samples: 10
selector_refresh_interval: "120s"
frontier_cost_per_1k: 0.01
zai_cost_per_1k: 0.005
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.SelectorWindow != 2*time.Hour {
		t.Errorf("SelectorWindow = %v", cfg.SelectorWindow)
	}
	if cfg.SelectorMinSamples != 10 {
		t.Errorf("SelectorMinSamples = %d", cfg.SelectorMinSamples)
	}
	if cfg.SelectorRefreshInterval != 120*time.Second {
		t.Errorf("SelectorRefreshInterval = %v", cfg.SelectorRefreshInterval)
	}
	if cfg.FrontierCostPer1K != 0.01 {
		t.Errorf("FrontierCostPer1K = %v", cfg.FrontierCostPer1K)
	}
	if cfg.ZAICostPer1K != 0.005 {
		t.Errorf("ZAICostPer1K = %v", cfg.ZAICostPer1K)
	}
}

func TestLoadYAMLHealthSettings(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
health_poll_interval: "60s"
health_breaker_threshold: 5
health_probe_timeout: "10s"
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.HealthPollInterval != 60*time.Second {
		t.Errorf("HealthPollInterval = %v", cfg.HealthPollInterval)
	}
	if cfg.HealthBreakerThreshold != 5 {
		t.Errorf("HealthBreakerThreshold = %d", cfg.HealthBreakerThreshold)
	}
	if cfg.HealthProbeTimeout != 10*time.Second {
		t.Errorf("HealthProbeTimeout = %v", cfg.HealthProbeTimeout)
	}
}

func TestLoadYAMLCostBaseline(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
cost_baseline_provider: "zai"
cost_baseline_model: "glm-4.6"
cost_baseline_rate_per_1k: 0.003
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.CostBaselineProvider != "zai" {
		t.Errorf("CostBaselineProvider = %q", cfg.CostBaselineProvider)
	}
	if cfg.CostBaselineModel != "glm-4.6" {
		t.Errorf("CostBaselineModel = %q", cfg.CostBaselineModel)
	}
	if cfg.CostBaselineRatePer1K != 0.003 {
		t.Errorf("CostBaselineRatePer1K = %v", cfg.CostBaselineRatePer1K)
	}
}

func TestLoadYAMLServerTimeouts(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
server_read_timeout: "45s"
server_write_timeout: "300s"
server_idle_timeout: "60s"
server_max_header_bytes: 524288
shutdown_timeout: "60s"
max_body_bytes: 2097152
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.ReadTimeout != 45*time.Second {
		t.Errorf("ReadTimeout = %v", cfg.ReadTimeout)
	}
	if cfg.WriteTimeout != 300*time.Second {
		t.Errorf("WriteTimeout = %v", cfg.WriteTimeout)
	}
	if cfg.IdleTimeout != 60*time.Second {
		t.Errorf("IdleTimeout = %v", cfg.IdleTimeout)
	}
	if cfg.MaxHeaderBytes != 524288 {
		t.Errorf("MaxHeaderBytes = %d", cfg.MaxHeaderBytes)
	}
	if cfg.ShutdownTimeout != 60*time.Second {
		t.Errorf("ShutdownTimeout = %v", cfg.ShutdownTimeout)
	}
	if cfg.MaxBodyBytes != 2097152 {
		t.Errorf("MaxBodyBytes = %d", cfg.MaxBodyBytes)
	}
}

func TestLoadYAMLCLoudEndpoint(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
models_endpoint_enabled: true
models_cache_ttl: "3m"
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if !cfg.ModelsEndpointEnabled {
		t.Error("ModelsEndpointEnabled = false, want true")
	}
	if cfg.ModelsCacheTTL != 3*time.Minute {
		t.Errorf("ModelsCacheTTL = %v", cfg.ModelsCacheTTL)
	}
}

func TestLoadYAMLRoutingConfidence(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
routing_confidence_db: "/var/nexus/routing.db"
routing_confidence_floor: 0.3
routing_confidence_ceiling: 0.9
routing_confidence_min_samples: 3
routing_confidence_window: "336h"
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.RoutingConfidenceDB != "/var/nexus/routing.db" {
		t.Errorf("RoutingConfidenceDB = %q", cfg.RoutingConfidenceDB)
	}
	if cfg.RoutingConfidenceFloor != 0.3 {
		t.Errorf("RoutingConfidenceFloor = %v", cfg.RoutingConfidenceFloor)
	}
	if cfg.RoutingConfidenceCeiling != 0.9 {
		t.Errorf("RoutingConfidenceCeiling = %v", cfg.RoutingConfidenceCeiling)
	}
	if cfg.RoutingConfidenceMinSamples != 3 {
		t.Errorf("RoutingConfidenceMinSamples = %d", cfg.RoutingConfidenceMinSamples)
	}
	if cfg.RoutingConfidenceWindow != 336*time.Hour {
		t.Errorf("RoutingConfidenceWindow = %v", cfg.RoutingConfidenceWindow)
	}
}

func TestLoadYAMLSLMCacheSettings(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
slm_cache_max_entries: 1024
slm_cache_ttl: "5m"
slm_cache_similarity_threshold: 0.5
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.SLMCacheMaxEntries != 1024 {
		t.Errorf("SLMCacheMaxEntries = %d", cfg.SLMCacheMaxEntries)
	}
	if cfg.SLMCacheTTL != 5*time.Minute {
		t.Errorf("SLMCacheTTL = %v", cfg.SLMCacheTTL)
	}
	if cfg.SLMCacheSemanticThreshold != 0.5 {
		t.Errorf("SLMCacheSemanticThreshold = %v", cfg.SLMCacheSemanticThreshold)
	}
}
