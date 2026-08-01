package config

import (
	"os"
	"path/filepath"
	"strings"
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
	if cfg.ProbeThermalThreshold != 90 {
		t.Errorf("ProbeThermalThreshold = %d, want 90", cfg.ProbeThermalThreshold)
	}
	if cfg.LocalCooldown != 10*time.Second {
		t.Errorf("LocalCooldown = %v, want 10s", cfg.LocalCooldown)
	}
	if cfg.AuthRateLimitRPM != 5 {
		t.Errorf("AuthRateLimitRPM = %d, want 5", cfg.AuthRateLimitRPM)
	}
	if cfg.AuthRateLimitBurst != 3 {
		t.Errorf("AuthRateLimitBurst = %d, want 3", cfg.AuthRateLimitBurst)
	}
	if cfg.AuthRateLimitWindow != 5*time.Minute {
		t.Errorf("AuthRateLimitWindow = %v, want 5m", cfg.AuthRateLimitWindow)
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
probe_thermal_threshold: 75
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
auth_rate_limit_rpm: 20
auth_rate_limit_burst: 10
auth_rate_limit_window: "3m"
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
	if cfg.ProbeThermalThreshold != 75 {
		t.Errorf("ProbeThermalThreshold = %d, want 75", cfg.ProbeThermalThreshold)
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
	if cfg.AuthRateLimitRPM != 20 {
		t.Errorf("AuthRateLimitRPM = %d, want 20", cfg.AuthRateLimitRPM)
	}
	if cfg.AuthRateLimitBurst != 10 {
		t.Errorf("AuthRateLimitBurst = %d, want 10", cfg.AuthRateLimitBurst)
	}
	if cfg.AuthRateLimitWindow != 3*time.Minute {
		t.Errorf("AuthRateLimitWindow = %v, want 3m", cfg.AuthRateLimitWindow)
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
		t.Fatal("expected error for negative shutdown timeout")
	}
	// issue #1181: the error must name the safe default and point to .env.example.
	msg := err.Error()
	if !strings.Contains(msg, "30s") {
		t.Errorf("error %q does not name the default 30s", msg)
	}
	if !strings.Contains(msg, ".env.example") {
		t.Errorf("error %q does not point to .env.example", msg)
	}
}

// issue #986: fractional fields must be validated in 0..1 range.
// issue #1181: error messages now carry actionable hints (default +
// .env.example pointer); the substring checks below target the
// stable "must be in [0,1]" constraint phrase.
func TestLoadYAMLFractionalFieldRangeValidation(t *testing.T) {
	fractionalFields := []struct {
		yamlKey string
		yamlVal string
		wantErr string
	}{
		{"budget_alert_threshold", "5.0", "budget_alert_threshold must be in [0,1]"},
		{"budget_alert_threshold", "-0.5", "budget_alert_threshold must be in [0,1]"},
		{"fusion_agreement_threshold", "2.0", "fusion_agreement_threshold must be in [0,1]"},
		{"fusion_agreement_threshold", "-0.1", "fusion_agreement_threshold must be in [0,1]"},
		{"provider_tail_weight", "1.5", "provider_tail_weight must be in [0,1]"},
		{"provider_tail_weight", "-0.1", "provider_tail_weight must be in [0,1]"},
		{"tracing_sample_rate", "3.0", "tracing_sample_rate must be in [0,1]"},
		{"tracing_sample_rate", "-0.1", "tracing_sample_rate must be in [0,1]"},
	}

	for _, tc := range fractionalFields {
		t.Run(tc.yamlKey+"_"+tc.yamlVal, func(t *testing.T) {
			tmp := t.TempDir()
			path := filepath.Join(tmp, "config.yaml")
			yamlContent := tc.yamlKey + ": " + tc.yamlVal + "\n"
			if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			_, err := LoadYAML(path)
			if err == nil {
				t.Errorf("LoadYAML: expected error for %s=%s, got nil", tc.yamlKey, tc.yamlVal)
			}
			if err != nil && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("LoadYAML error = %q, want containing %q", err.Error(), tc.wantErr)
			}
			// issue #1181: every range error must carry a remediation hint.
			if err != nil && !strings.Contains(err.Error(), ".env.example") {
				t.Errorf("LoadYAML error = %q, want .env.example hint", err.Error())
			}
		})
	}
}

func TestLoadYAMLFractionalFieldBoundaryValues(t *testing.T) {
	// Boundary values 0.0 and 1.0 should be accepted
	validValues := []string{"0.0", "0", "1.0", "1", "0.5", "0.85"}
	fractionalFields := []string{
		"budget_alert_threshold",
		"fusion_agreement_threshold",
		"provider_tail_weight",
		"tracing_sample_rate",
	}

	for _, field := range fractionalFields {
		for _, val := range validValues {
			t.Run(field+"_"+val, func(t *testing.T) {
				tmp := t.TempDir()
				path := filepath.Join(tmp, "config.yaml")
				yamlContent := field + ": " + val + "\n"
				if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				_, err := LoadYAML(path)
				if err != nil {
					t.Errorf("LoadYAML: unexpected error for %s=%s: %v", field, val, err)
				}
			})
		}
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
	// TrustedProxiesRaw is consumed by the rate_limit_proxy_config
	// diagnostic check (issue #603) — assert it is populated so the
	// field stays wired through the YAML loader.
	if cfg.TrustedProxiesRaw == "" {
		t.Error("TrustedProxiesRaw should be populated from YAML trusted_proxies")
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
provider_tail_weight: 0.5
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
	if cfg.ProviderTailWeight != 0.5 {
		t.Errorf("ProviderTailWeight = %v, want 0.5", cfg.ProviderTailWeight)
	}
	if cfg.FrontierCostPer1K != 0.01 {
		t.Errorf("FrontierCostPer1K = %v", cfg.FrontierCostPer1K)
	}
	if cfg.ZAICostPer1K != 0.005 {
		t.Errorf("ZAICostPer1K = %v", cfg.ZAICostPer1K)
	}
}

func TestLoadYAMLProviderTailWeightEnvOverride(t *testing.T) {
	// Issue #450: the env var must override the YAML default and
	// reject out-of-range values. Set a YAML file with the default
	// 0, then push NEXUS_PROVIDER_TAIL_WEIGHT through the loader's
	// env-override branch and assert it wins. A separate sub-test
	// pins the strict range check on the env-var path.
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(path, []byte("provider_tail_weight: 0.0\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("NEXUS_PROVIDER_TAIL_WEIGHT", "0.3")
	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.ProviderTailWeight != 0.3 {
		t.Errorf("ProviderTailWeight = %v, want 0.3 (env override)", cfg.ProviderTailWeight)
	}
}

func TestLoadYAMLProviderTailWeightEnvInvalid(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(path, []byte("provider_tail_weight: 0.0\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("NEXUS_PROVIDER_TAIL_WEIGHT", "1.5")
	if _, err := LoadYAML(path); err == nil {
		t.Error("LoadYAML: expected error for NEXUS_PROVIDER_TAIL_WEIGHT=1.5")
	}
}

func TestLoadYAMLFusionAgreementThresholdOutOfRange(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(path, []byte("fusion_agreement_threshold: 0.85\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("NEXUS_FUSION_AGREEMENT_THRESHOLD", "1.5")
	if _, err := LoadYAML(path); err == nil {
		t.Error("LoadYAML: expected error for NEXUS_FUSION_AGREEMENT_THRESHOLD=1.5")
	}

	t.Setenv("NEXUS_FUSION_AGREEMENT_THRESHOLD", "-0.1")
	if _, err := LoadYAML(path); err == nil {
		t.Error("LoadYAML: expected error for NEXUS_FUSION_AGREEMENT_THRESHOLD=-0.1")
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

// TestLoadYAMLTLSEnabled (issue #444) verifies the YAML mirror of
// NEXUS_TLS_ENABLED. Operators running behind a TLS-terminating reverse
// proxy can set the flag via config.yaml instead of an env var.
func TestLoadYAMLTLSEnabled(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(path, []byte("tls_enabled: true\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if !cfg.TLSEnabled {
		t.Error("TLSEnabled = false, want true when tls_enabled: true in YAML")
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

func TestLoadYAMLRoutingConfidenceOutOfRange(t *testing.T) {
	routingConfidenceFields := []struct {
		yamlKey string
		yamlVal string
		wantErr string
	}{
		{"routing_confidence_floor", "1.5", "routing_confidence_floor must be in [0,1]"},
		{"routing_confidence_floor", "-0.2", "routing_confidence_floor must be in [0,1]"},
		{"routing_confidence_ceiling", "1.5", "routing_confidence_ceiling must be in [0,1]"},
		{"routing_confidence_ceiling", "-0.2", "routing_confidence_ceiling must be in [0,1]"},
	}

	for _, tc := range routingConfidenceFields {
		t.Run(tc.yamlKey+"_"+tc.yamlVal, func(t *testing.T) {
			tmp := t.TempDir()
			path := filepath.Join(tmp, "config.yaml")
			yamlContent := tc.yamlKey + ": " + tc.yamlVal + "\n"
			if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			_, err := LoadYAML(path)
			if err == nil {
				t.Errorf("LoadYAML: expected error for %s=%s, got nil", tc.yamlKey, tc.yamlVal)
			}
			if err != nil && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("LoadYAML error = %q, want containing %q", err.Error(), tc.wantErr)
			}
		})
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

func TestLoadYAMLTOONUnfencedSettings(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
toon_unfenced: false
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.TOONUnfenced {
		t.Error("TOONUnfenced = true, want false from YAML")
	}
}

func TestLoadYAMLTOONUnfencedEnvOverridesYAML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
toon_unfenced: false
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("NEXUS_TOON_UNFENCED", "true")

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if !cfg.TOONUnfenced {
		t.Error("TOONUnfenced = false, want true (env overrides YAML)")
	}
}

func TestLoadYAMLTOONUnfencedInvalidValue(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
toon_unfenced: maybe
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadYAML(path)
	if err == nil {
		t.Fatal("LoadYAML: expected error for toon_unfenced: maybe, got nil")
	}
	if got := err.Error(); got != `config: toon_unfenced value "maybe" is not recognised; want true or false. See .env.example.` {
		t.Errorf("error = %q, want %q", got, `config: toon_unfenced value "maybe" is not recognised; want true or false. See .env.example.`)
	}
}

func TestParseYAMLBool(t *testing.T) {
	tests := []struct {
		input   string
		wantVal bool
		wantErr bool
	}{
		{"true", true, false},
		{"false", false, false},
		{"1", true, false},
		{"0", false, false},
		{"yes", true, false},
		{"no", false, false},
		{"on", true, false},
		{"off", false, false},
		{"True", true, false},
		{"FALSE", false, false},
		{"  yes  ", true, false},
		{"maybe", false, true},
		{"certainly", false, true},
		{"", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseYAMLBool(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseYAMLBool(%q) = _, nil; want error", tt.input)
				}
			} else {
				if err != nil {
					t.Errorf("parseYAMLBool(%q) = _, %v; want no error", tt.input, err)
				}
				if got != tt.wantVal {
					t.Errorf("parseYAMLBool(%q) = %v; want %v", tt.input, got, tt.wantVal)
				}
			}
		})
	}
}

func TestLoadYAMLCascadeMaxResponseBytesEnvOverridesYAML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
cascade_max_response_bytes: 12345
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("NEXUS_CASCADE_MAX_RESPONSE_BYTES", "67890")

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.CascadeMaxResponseBytes != 67890 {
		t.Errorf("CascadeMaxResponseBytes = %d, want 67890 (env overrides YAML 12345)", cfg.CascadeMaxResponseBytes)
	}
}

func TestLoadYAMLMaxResponseBytesEnvOverridesYAML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
max_response_bytes: 10000000
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("NEXUS_MAX_RESPONSE_BYTES", "20000000")

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.MaxResponseBytes != 20000000 {
		t.Errorf("MaxResponseBytes = %d, want 20000000 (env overrides YAML 10000000)", cfg.MaxResponseBytes)
	}
}

func TestLoadYAMLAuthRateLimitEnvOverridesYAML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yamlContent := `
auth_rate_limit_rpm: 10
auth_rate_limit_burst: 5
auth_rate_limit_window: 3m
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("NEXUS_AUTH_RATE_LIMIT_RPM", "20")
	t.Setenv("NEXUS_AUTH_RATE_LIMIT_BURST", "15")
	t.Setenv("NEXUS_AUTH_RATE_LIMIT_WINDOW", "7m")

	cfg, err := LoadYAML(path)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if cfg.AuthRateLimitRPM != 20 {
		t.Errorf("AuthRateLimitRPM = %d, want 20 (env overrides YAML 10)", cfg.AuthRateLimitRPM)
	}
	if cfg.AuthRateLimitBurst != 15 {
		t.Errorf("AuthRateLimitBurst = %d, want 15 (env overrides YAML 5)", cfg.AuthRateLimitBurst)
	}
	if cfg.AuthRateLimitWindow != 7*time.Minute {
		t.Errorf("AuthRateLimitWindow = %v, want 7m (env overrides YAML 3m)", cfg.AuthRateLimitWindow)
	}
}

func TestClampFloatBoundaryConditions(t *testing.T) {
	tests := []struct {
		name string
		v    float64
		min  float64
		max  float64
		want float64
	}{
		{"in_range", 0.5, 0.0, 1.0, 0.5},
		{"below_min", -0.5, 0.0, 1.0, 0.0},
		{"above_max", 1.5, 0.0, 1.0, 1.0},
		{"exact_min", 0.0, 0.0, 1.0, 0.0},
		{"exact_max", 1.0, 0.0, 1.0, 1.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := clampFloat(tc.v, tc.min, tc.max)
			if got != tc.want {
				t.Errorf("clampFloat(%v, %v, %v) = %v, want %v", tc.v, tc.min, tc.max, got, tc.want)
			}
		})
	}
}

func TestParseBoolEnvStr(t *testing.T) {
	// Tests verify that recognized boolean strings return the correct value,
	// and that unrecognized strings (typos) return the default.
	tests := []struct {
		name   string
		val    string
		def    bool
		want   bool
		isTypo bool // if true, this is a typo that should fall through to default
	}{
		// Recognized true values
		{"true_lower", "true", false, true, false},
		{"TRUE_UPPER", "TRUE", false, true, false},
		{"True_Mixed", "True", false, true, false},
		{"one", "1", false, true, false},
		{"yes_lower", "yes", false, true, false},
		{"YES_UPPER", "YES", false, true, false},
		{"on_lower", "on", false, true, false},
		{"ON_UPPER", "ON", false, true, false},
		// Recognized false values
		{"false_lower", "false", true, false, false},
		{"FALSE_UPPER", "FALSE", true, false, false},
		{"False_Mixed", "False", true, false, false},
		{"zero", "0", true, false, false},
		{"no_lower", "no", true, false, false},
		{"NO_UPPER", "NO", true, false, false},
		{"off_lower", "off", true, false, false},
		{"OFF_UPPER", "OFF", true, false, false},
		// Whitespace trimming
		{"spaces_around", "  true  ", false, true, false},
		{"tab_prefix", "\ton", false, true, false},
		// Default-return branch (typos / unrecognized values — should return default)
		{"typo_ture", "ture", false, false, true},
		{"typo_faalse", "faalse", true, true, true},
		{"typo_yess", "yess", false, false, true},
		{"typo_onn", "onn", true, true, true},
		{"random_string", "random", false, false, true},
		{"empty_string", "", false, false, true},
		{"non_boolean_number", "123", false, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseBoolEnvStr(tc.val, tc.def)
			want := tc.def
			if !tc.isTypo {
				want = tc.want
			}
			if got != want {
				t.Errorf("parseBoolEnvStr(%q, %v) = %v, want %v", tc.val, tc.def, got, want)
			}
		})
	}
}
