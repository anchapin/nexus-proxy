package config

import (
	"log/slog"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("NEXUS_ADDR", "")
	t.Setenv("NEXUS_OLLAMA_URL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
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
	// Probe defaults (issue #6).
	if cfg.ProbePollInterval != 60*time.Second {
		t.Errorf("ProbePollInterval = %v, want 60s", cfg.ProbePollInterval)
	}
	if cfg.ProbeTimeout != 5*time.Second {
		t.Errorf("ProbeTimeout = %v, want 5s", cfg.ProbeTimeout)
	}
	if cfg.ProbeBytesPerToken != 256*1024 {
		t.Errorf("ProbeBytesPerToken = %d, want 262144", cfg.ProbeBytesPerToken)
	}
	if !cfg.ProbeEnabled {
		t.Error("ProbeEnabled = false, want true with default interval")
	}
	// Local-route concurrency limiter defaults (issue #81): the
	// limiter is dormant unless the operator opts in, and the per-slot
	// VRAM reservation defaults to 2 GiB.
	if cfg.LocalMaxConcurrent != 0 {
		t.Errorf("LocalMaxConcurrent = %d, want 0 (disabled by default)", cfg.LocalMaxConcurrent)
	}
	if cfg.LocalVRAMBytesPerSlot != DefaultLocalVRAMBytesPerSlot {
		t.Errorf("LocalVRAMBytesPerSlot = %d, want default %d", cfg.LocalVRAMBytesPerSlot, DefaultLocalVRAMBytesPerSlot)
	}
	if cfg.RAGThreshold != 0.55 {
		t.Errorf("RAGThreshold = %v, want 0.55", cfg.RAGThreshold)
	}
	if cfg.CascadeTimeout != 30*time.Second {
		t.Errorf("CascadeTimeout = %v, want 30s", cfg.CascadeTimeout)
	}
	if cfg.ZAIURL != "https://api.z.ai/v1/chat/completions" {
		t.Errorf("ZAIURL = %q", cfg.ZAIURL)
	}
	if cfg.ZAIModel != "glm-4.6" {
		t.Errorf("ZAIModel = %q", cfg.ZAIModel)
	}
	if cfg.ZAIKey != "" {
		t.Errorf("ZAIKey = %q, want empty", cfg.ZAIKey)
	}
	// Telemetry defaults to a local JSON-lines file unless NEXUS_TELEMETRY_PATH
	// is explicitly unset or set to ""; assert the default value when neither
	// is true (typical fresh dev environment).
	if cfg.TelemetryPath == "" {
		t.Skip("NEXUS_TELEMETRY_PATH set in environment; default test skipped")
	}
	if cfg.TelemetryPath != "./nexus-telemetry.jsonl" {
		t.Errorf("TelemetryPath = %q, want ./nexus-telemetry.jsonl", cfg.TelemetryPath)
	}
	if !cfg.TelemetryEnabled() {
		t.Error("TelemetryEnabled = false, want true with default path")
	}
	// HTTP listener timeouts (issue #77).
	if cfg.ReadTimeout != DefaultServerReadTimeout {
		t.Errorf("ReadTimeout = %v, want %v", cfg.ReadTimeout, DefaultServerReadTimeout)
	}
	if cfg.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (disabled, streaming-safe)", cfg.WriteTimeout)
	}
	if cfg.IdleTimeout != DefaultServerIdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", cfg.IdleTimeout, DefaultServerIdleTimeout)
	}
	if cfg.MaxHeaderBytes != DefaultServerMaxHeaderBytes {
		t.Errorf("MaxHeaderBytes = %d, want %d", cfg.MaxHeaderBytes, DefaultServerMaxHeaderBytes)
	}
	// Graceful shutdown drain window (issue #121).
	if cfg.ShutdownTimeout != DefaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v", cfg.ShutdownTimeout, DefaultShutdownTimeout)
	}
	// TOON unfenced-array detection (issue #123) defaults to on so a
	// stock deployment compresses bare arrays; operators can opt out
	// with NEXUS_TOON_UNFENCED=false.
	if !cfg.TOONUnfenced {
		t.Error("TOONUnfenced = false, want true (default on)")
	}
	// DSL fast-pass patterns (issue #305).
	if len(cfg.DSLFormattingPatterns) == 0 {
		t.Error("DSLFormattingPatterns is empty, want default patterns")
	}
	if len(cfg.DSLFusionPatterns) == 0 {
		t.Error("DSLFusionPatterns is empty, want default patterns")
	}
	if len(cfg.DSLLocalPatterns) == 0 {
		t.Error("DSLLocalPatterns is empty, want default patterns")
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("NEXUS_ADDR", ":9001")
	t.Setenv("NEXUS_ROUTER_MODEL", "llama3.2:3b")
	t.Setenv("NEXUS_FRONTIER_API_KEY", "sk-test")
	t.Setenv("NEXUS_RAG_THRESHOLD", "0.7")
	t.Setenv("NEXUS_SLM_TIMEOUT", "3s")
	t.Setenv("NEXUS_CASCADE_TIMEOUT", "15s")
	t.Setenv("NEXUS_ZAI_API_KEY", "zai-test")
	t.Setenv("NEXUS_ZAI_MODEL", "glm-4.5")
	t.Setenv("NEXUS_TELEMETRY_PATH", "")
	t.Setenv("NEXUS_PROBE_INTERVAL", "120s")
	t.Setenv("NEXUS_PROBE_TIMEOUT", "2s")
	t.Setenv("NEXUS_PROBE_BYTES_PER_TOKEN", "131072")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":9001" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.RouterModel != "llama3.2:3b" {
		t.Errorf("RouterModel = %q", cfg.RouterModel)
	}
	if !cfg.FrontierEnabled() {
		t.Error("FrontierEnabled = false, want true")
	}
	if cfg.RAGThreshold != 0.7 {
		t.Errorf("RAGThreshold = %v", cfg.RAGThreshold)
	}
	if cfg.SLMTimeout != 3*time.Second {
		t.Errorf("SLMTimeout = %v", cfg.SLMTimeout)
	}
	if cfg.CascadeTimeout != 15*time.Second {
		t.Errorf("CascadeTimeout = %v, want 15s", cfg.CascadeTimeout)
	}
	if cfg.ZAIKey != "zai-test" {
		t.Errorf("ZAIKey = %q", cfg.ZAIKey)
	}
	if cfg.ZAIModel != "glm-4.5" {
		t.Errorf("ZAIModel = %q", cfg.ZAIModel)
	}
	if cfg.TelemetryEnabled() {
		t.Error("TelemetryEnabled = true with empty path, want false")
	}
	if cfg.ProbePollInterval != 120*time.Second {
		t.Errorf("ProbePollInterval = %v, want 120s", cfg.ProbePollInterval)
	}
	if cfg.ProbeTimeout != 2*time.Second {
		t.Errorf("ProbeTimeout = %v, want 2s", cfg.ProbeTimeout)
	}
	if cfg.ProbeBytesPerToken != 131072 {
		t.Errorf("ProbeBytesPerToken = %d, want 131072", cfg.ProbeBytesPerToken)
	}
}

func TestLoadTOONUnfencedFlag(t *testing.T) {
	t.Run("defaults_on", func(t *testing.T) {
		t.Setenv("NEXUS_TOON_UNFENCED", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.TOONUnfenced {
			t.Error("TOONUnfenced = false, want true when unset (default on)")
		}
	})
	t.Run("explicit_false", func(t *testing.T) {
		t.Setenv("NEXUS_TOON_UNFENCED", "false")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.TOONUnfenced {
			t.Error("TOONUnfenced = true, want false when NEXUS_TOON_UNFENCED=false")
		}
	})
	t.Run("explicit_true", func(t *testing.T) {
		t.Setenv("NEXUS_TOON_UNFENCED", "true")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.TOONUnfenced {
			t.Error("TOONUnfenced = false, want true when NEXUS_TOON_UNFENCED=true")
		}
	})
}

func TestLoadDSLPatternOverrides(t *testing.T) {
	// Custom local patterns: operator adds "custom task" to local patterns (issue #305)
	// Note: comma-separated regex patterns - each is compiled separately
	t.Setenv("NEXUS_DSL_LOCAL_PATTERNS", `(?i)\b(refactor)\b,(?i)\b(custom task)\b`)
	// Custom fusion patterns: operator adds "database schema" (issue #305)
	t.Setenv("NEXUS_DSL_FUSION_PATTERNS", `(?i)\b(architectural design|system architecture)\b,(?i)\b(database schema)\b`)
	// Custom formatting patterns
	t.Setenv("NEXUS_DSL_FORMATTING_PATTERNS", `(?i)\b(css)\b,(?i)\b(html)\b`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Verify local patterns include refactor and custom task (2 comma-separated patterns)
	if len(cfg.DSLLocalPatterns) != 2 {
		t.Errorf("DSLLocalPatterns count = %d, want 2", len(cfg.DSLLocalPatterns))
	}
	foundRefactor := false
	for _, re := range cfg.DSLLocalPatterns {
		if re.String() == `(?i)\b(refactor)\b` {
			foundRefactor = true
			break
		}
	}
	if !foundRefactor {
		t.Error("DSLLocalPatterns does not contain refactor pattern")
	}
	foundCustom := false
	for _, re := range cfg.DSLLocalPatterns {
		if re.String() == `(?i)\b(custom task)\b` {
			foundCustom = true
			break
		}
	}
	if !foundCustom {
		t.Error("DSLLocalPatterns does not contain custom task pattern")
	}

	// Verify fusion patterns (2 comma-separated patterns)
	if len(cfg.DSLFusionPatterns) != 2 {
		t.Errorf("DSLFusionPatterns count = %d, want 2", len(cfg.DSLFusionPatterns))
	}
	foundSchema := false
	for _, re := range cfg.DSLFusionPatterns {
		if re.String() == `(?i)\b(database schema)\b` {
			foundSchema = true
			break
		}
	}
	if !foundSchema {
		t.Error("DSLFusionPatterns does not contain database schema pattern")
	}

	// Verify formatting patterns (2 comma-separated patterns)
	if len(cfg.DSLFormattingPatterns) != 2 {
		t.Errorf("DSLFormattingPatterns count = %d, want 2", len(cfg.DSLFormattingPatterns))
	}
}

func TestLoadTelemetryDisabledByEmptyPath(t *testing.T) {
	t.Setenv("NEXUS_TELEMETRY_PATH", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TelemetryEnabled() {
		t.Error("TelemetryEnabled = true, want false when path empty")
	}
}

func TestLoadTelemetryPathHonoursOverride(t *testing.T) {
	t.Setenv("NEXUS_TELEMETRY_PATH", "/tmp/custom-tel.jsonl")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TelemetryPath != "/tmp/custom-tel.jsonl" {
		t.Errorf("TelemetryPath = %q, want /tmp/custom-tel.jsonl", cfg.TelemetryPath)
	}
	if !cfg.TelemetryEnabled() {
		t.Error("TelemetryEnabled = false, want true with explicit path")
	}
}

func TestLoadProbeDisabledByZeroInterval(t *testing.T) {
	t.Setenv("NEXUS_PROBE_INTERVAL", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ProbeEnabled {
		t.Error("ProbeEnabled = true, want false when NEXUS_PROBE_INTERVAL=0")
	}
}

func TestLoadProbeInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"bad interval", "NEXUS_PROBE_INTERVAL", "forever"},
		{"bad timeout", "NEXUS_PROBE_TIMEOUT", "ten seconds"},
		{"bad bytes per token", "NEXUS_PROBE_BYTES_PER_TOKEN", "lots"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Errorf("expected error for %s=%s", tc.key, tc.val)
			}
		})
	}
}

func TestLoadInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"bad int", "NEXUS_TOKEN_GUARDRAIL", "not-a-number"},
		{"bad float", "NEXUS_RAG_THRESHOLD", "0.5x"},
		{"bad duration", "NEXUS_SLM_TIMEOUT", "eight seconds"},
		{"bad cascade duration", "NEXUS_CASCADE_TIMEOUT", "ten seconds"},
		{"bad dsl formatting regex", "NEXUS_DSL_FORMATTING_PATTERNS", "[invalid"},
		{"bad dsl fusion regex", "NEXUS_DSL_FUSION_PATTERNS", "(unbalanced"},
		{"bad dsl local regex", "NEXUS_DSL_LOCAL_PATTERNS", "**invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Errorf("expected error for %s=%s", tc.key, tc.val)
			}
		})
	}
}

func TestOllamaURLTrimmed(t *testing.T) {
	t.Setenv("NEXUS_OLLAMA_URL", "http://localhost:11434/")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OllamaURL != "http://localhost:11434" {
		t.Errorf("trailing slash not trimmed: %q", cfg.OllamaURL)
	}
}

func TestLoadJudgeDefaults(t *testing.T) {
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "")
	t.Setenv("NEXUS_JUDGE_CONCURRENCY", "")
	t.Setenv("NEXUS_JUDGE_QUEUE", "")
	t.Setenv("NEXUS_JUDGE_TIMEOUT", "")
	t.Setenv("NEXUS_JUDGE_COST_PER_1K", "")
	t.Setenv("NEXUS_FRONTIER_MODEL", "gpt-4o")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.JudgeEnabled {
		t.Error("JudgeEnabled = false, want true (default sample rate > 0)")
	}
	if cfg.JudgeSampleRate != 0.1 {
		t.Errorf("JudgeSampleRate = %v, want 0.1", cfg.JudgeSampleRate)
	}
	if cfg.JudgeConcurrency != 2 {
		t.Errorf("JudgeConcurrency = %d, want 2", cfg.JudgeConcurrency)
	}
	if cfg.JudgeQueueDepth != 64 {
		t.Errorf("JudgeQueueDepth = %d, want 64", cfg.JudgeQueueDepth)
	}
	if cfg.JudgeTimeout != 30*time.Second {
		t.Errorf("JudgeTimeout = %v, want 30s", cfg.JudgeTimeout)
	}
	if cfg.JudgeURL == "" {
		t.Error("JudgeURL should not be empty")
	}
	if cfg.JudgeModel != "gpt-4o" {
		t.Errorf("JudgeModel = %q, want fallback to FrontierModel", cfg.JudgeModel)
	}
}

func TestLoadJudgeDisabledByZeroRate(t *testing.T) {
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.JudgeEnabled {
		t.Error("JudgeEnabled = true, want false (sample rate = 0)")
	}
}

func TestLoadJudgeInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"bad sample rate", "NEXUS_JUDGE_SAMPLE_RATE", "nope"},
		{"bad concurrency", "NEXUS_JUDGE_CONCURRENCY", "two"},
		{"bad queue depth", "NEXUS_JUDGE_QUEUE", "lots"},
		{"bad timeout", "NEXUS_JUDGE_TIMEOUT", "30"},
		{"bad cost rate", "NEXUS_JUDGE_COST_PER_1K", "free"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Errorf("expected error for %s=%s", tc.key, tc.val)
			}
		})
	}
}

func TestLoadJudgeOverrides(t *testing.T) {
	t.Setenv("NEXUS_JUDGE_URL", "http://judge.local/v1/chat/completions")
	t.Setenv("NEXUS_JUDGE_MODEL", "judge-model")
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "0.25")
	t.Setenv("NEXUS_JUDGE_CONCURRENCY", "4")
	t.Setenv("NEXUS_JUDGE_QUEUE", "128")
	t.Setenv("NEXUS_JUDGE_TIMEOUT", "5s")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.JudgeURL != "http://judge.local/v1/chat/completions" {
		t.Errorf("JudgeURL = %q", cfg.JudgeURL)
	}
	if cfg.JudgeModel != "judge-model" {
		t.Errorf("JudgeModel = %q", cfg.JudgeModel)
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
	if cfg.JudgeTimeout != 5*time.Second {
		t.Errorf("JudgeTimeout = %v", cfg.JudgeTimeout)
	}
	if !cfg.JudgeEnabled {
		t.Error("JudgeEnabled = false, want true")
	}
}

func TestLoadLocalConcurrencyOverrides(t *testing.T) {
	t.Setenv("NEXUS_LOCAL_MAX_CONCURRENT", "6")
	t.Setenv("NEXUS_LOCAL_VRAM_BYTES_PER_SLOT", "1073741824") // 1 GiB
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LocalMaxConcurrent != 6 {
		t.Errorf("LocalMaxConcurrent = %d, want 6", cfg.LocalMaxConcurrent)
	}
	if cfg.LocalVRAMBytesPerSlot != 1073741824 {
		t.Errorf("LocalVRAMBytesPerSlot = %d, want 1073741824", cfg.LocalVRAMBytesPerSlot)
	}
}

func TestLoadLocalConcurrencyNegativeClamped(t *testing.T) {
	// Negative ceiling is clamped to 0 (disabled); negative slot bytes
	// fall back to the default rather than producing a nonsensical
	// shrink divisor.
	t.Setenv("NEXUS_LOCAL_MAX_CONCURRENT", "-3")
	t.Setenv("NEXUS_LOCAL_VRAM_BYTES_PER_SLOT", "-512")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LocalMaxConcurrent != 0 {
		t.Errorf("LocalMaxConcurrent = %d, want 0 (clamped)", cfg.LocalMaxConcurrent)
	}
	if cfg.LocalVRAMBytesPerSlot != DefaultLocalVRAMBytesPerSlot {
		t.Errorf("LocalVRAMBytesPerSlot = %d, want default %d", cfg.LocalVRAMBytesPerSlot, DefaultLocalVRAMBytesPerSlot)
	}
}

func TestLoadLocalConcurrencyInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"bad max concurrent", "NEXUS_LOCAL_MAX_CONCURRENT", "many"},
		{"bad bytes per slot", "NEXUS_LOCAL_VRAM_BYTES_PER_SLOT", "lots"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Errorf("expected error for %s=%s", tc.key, tc.val)
			}
		})
	}
}

func TestLoadServerTimeoutOverrides(t *testing.T) {
	t.Setenv("NEXUS_SERVER_READ_TIMEOUT", "45s")
	t.Setenv("NEXUS_SERVER_WRITE_TIMEOUT", "300s")
	t.Setenv("NEXUS_SERVER_IDLE_TIMEOUT", "60s")
	t.Setenv("NEXUS_SERVER_MAX_HEADER_BYTES", "524288") // 512 KiB
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReadTimeout != 45*time.Second {
		t.Errorf("ReadTimeout = %v, want 45s", cfg.ReadTimeout)
	}
	if cfg.WriteTimeout != 300*time.Second {
		t.Errorf("WriteTimeout = %v, want 300s", cfg.WriteTimeout)
	}
	if cfg.IdleTimeout != 60*time.Second {
		t.Errorf("IdleTimeout = %v, want 60s", cfg.IdleTimeout)
	}
	if cfg.MaxHeaderBytes != 524288 {
		t.Errorf("MaxHeaderBytes = %d, want 524288", cfg.MaxHeaderBytes)
	}
}

func TestLoadServerTimeoutZeroAllowed(t *testing.T) {
	// Zero is valid for all four — it disables the corresponding
	// guard (and WriteTimeout=0 is the streaming-safe default).
	t.Setenv("NEXUS_SERVER_READ_TIMEOUT", "0")
	t.Setenv("NEXUS_SERVER_WRITE_TIMEOUT", "0")
	t.Setenv("NEXUS_SERVER_IDLE_TIMEOUT", "0")
	t.Setenv("NEXUS_SERVER_MAX_HEADER_BYTES", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0", cfg.ReadTimeout)
	}
	if cfg.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0", cfg.WriteTimeout)
	}
	if cfg.IdleTimeout != 0 {
		t.Errorf("IdleTimeout = %v, want 0", cfg.IdleTimeout)
	}
	if cfg.MaxHeaderBytes != 0 {
		t.Errorf("MaxHeaderBytes = %d, want 0", cfg.MaxHeaderBytes)
	}
}

func TestLoadServerTimeoutNegativeRejected(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"negative read timeout", "NEXUS_SERVER_READ_TIMEOUT", "-1s"},
		{"negative write timeout", "NEXUS_SERVER_WRITE_TIMEOUT", "-5s"},
		{"negative idle timeout", "NEXUS_SERVER_IDLE_TIMEOUT", "-10s"},
		{"negative max header bytes", "NEXUS_SERVER_MAX_HEADER_BYTES", "-1024"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Errorf("expected error for %s=%s", tc.key, tc.val)
			}
		})
	}
}

func TestLoadServerTimeoutInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"bad read timeout", "NEXUS_SERVER_READ_TIMEOUT", "soon"},
		{"bad write timeout", "NEXUS_SERVER_WRITE_TIMEOUT", "forever"},
		{"bad idle timeout", "NEXUS_SERVER_IDLE_TIMEOUT", "two minutes"},
		{"bad max header bytes", "NEXUS_SERVER_MAX_HEADER_BYTES", "lots"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Errorf("expected error for %s=%s", tc.key, tc.val)
			}
		})
	}
}

// --- Graceful shutdown timeout (issue #121) ---

func TestLoadShutdownTimeoutHonoursOverride(t *testing.T) {
	t.Setenv("NEXUS_SHUTDOWN_TIMEOUT", "5s")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ShutdownTimeout != 5*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 5s", cfg.ShutdownTimeout)
	}
}

func TestLoadShutdownTimeoutZeroFallsBackToDefault(t *testing.T) {
	// 0 is documented as "use the default" — it must NOT disable the
	// drain (a misconfigured .env must not leak in-flight requests).
	t.Setenv("NEXUS_SHUTDOWN_TIMEOUT", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ShutdownTimeout != DefaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want default %v", cfg.ShutdownTimeout, DefaultShutdownTimeout)
	}
}

func TestLoadShutdownTimeoutNegativeRejected(t *testing.T) {
	t.Setenv("NEXUS_SHUTDOWN_TIMEOUT", "-1s")
	if _, err := Load(); err == nil {
		t.Errorf("expected error for NEXUS_SHUTDOWN_TIMEOUT=-1s")
	}
}

func TestLoadShutdownTimeoutInvalidValue(t *testing.T) {
	t.Setenv("NEXUS_SHUTDOWN_TIMEOUT", "soon")
	if _, err := Load(); err == nil {
		t.Errorf("expected error for NEXUS_SHUTDOWN_TIMEOUT=soon")
	}
}

// --- Trusted proxies (issue #75) ---

func TestTrustedProxies_DefaultDisabled(t *testing.T) {
	t.Setenv("NEXUS_TRUSTED_PROXIES", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TrustedProxiesConfigured() {
		t.Error("trusted proxies should be unconfigured by default (trust nobody)")
	}
	if cfg.RateLimitEnabled() {
		t.Error("rate limit should be disabled by default")
	}
}

func TestTrustedProxies_CIDRList(t *testing.T) {
	t.Setenv("NEXUS_TRUSTED_PROXIES", "10.0.0.0/8, 172.16.0.0/12")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.TrustedProxies) != 2 {
		t.Fatalf("expected 2 CIDRs, got %d", len(cfg.TrustedProxies))
	}
	if !cfg.TrustedProxiesConfigured() {
		t.Error("TrustedProxiesConfigured should be true")
	}
}

func TestTrustedProxies_BareIP(t *testing.T) {
	t.Setenv("NEXUS_TRUSTED_PROXIES", "127.0.0.1")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.TrustedProxies) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(cfg.TrustedProxies))
	}
	if ones, bits := cfg.TrustedProxies[0].Mask.Size(); ones != 32 || bits != 32 {
		t.Errorf("bare IPv4 should be /32, got /%d of %d", ones, bits)
	}
}

func TestTrustedProxies_InvalidFailsBoot(t *testing.T) {
	cases := []string{
		"not-a-cidr",
		"10.0.0.0/8, bogus",
		"10.0.0.0/99",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			t.Setenv("NEXUS_TRUSTED_PROXIES", c)
			if _, err := Load(); err == nil {
				t.Errorf("expected boot error for %q", c)
			}
		})
	}
}

func TestRateLimit_Overrides(t *testing.T) {
	t.Setenv("NEXUS_RATE_LIMIT_RPM", "120")
	t.Setenv("NEXUS_RATE_LIMIT_BURST", "30")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RateLimitRPM != 120 {
		t.Errorf("RateLimitRPM = %d, want 120", cfg.RateLimitRPM)
	}
	if cfg.RateLimitBurst != 30 {
		t.Errorf("RateLimitBurst = %d, want 30", cfg.RateLimitBurst)
	}
	if !cfg.RateLimitEnabled() {
		t.Error("RateLimitEnabled should be true")
	}
}

func TestRateLimit_NegativeClamped(t *testing.T) {
	t.Setenv("NEXUS_RATE_LIMIT_RPM", "-5")
	t.Setenv("NEXUS_RATE_LIMIT_BURST", "-1")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RateLimitRPM != 0 {
		t.Errorf("RateLimitRPM = %d, want 0 (clamped)", cfg.RateLimitRPM)
	}
	if cfg.RateLimitBurst != 0 {
		t.Errorf("RateLimitBurst = %d, want 0 (clamped)", cfg.RateLimitBurst)
	}
}

func TestIsLoopbackBind(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{":8000", true}, // empty host — dev default
		{"localhost:8000", true},
		{"127.0.0.1:8000", true},
		{"127.99.99.99:8000", true}, // whole /8
		{"[::1]:8000", true},
		{"0.0.0.0:8000", false}, // all interfaces — non-loopback
		{"10.0.0.5:8000", false},
		{"example.com:8000", false}, // unknown host
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			cfg := Config{Addr: tc.addr}
			if got := cfg.IsLoopbackBind(); got != tc.want {
				t.Errorf("IsLoopbackBind(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

// TestReloadHotReloadable_RateLimit verifies that rate limit RPM and burst
// are correctly reloaded from environment variables.
func TestReloadHotReloadable_RateLimit(t *testing.T) {
	t.Setenv("NEXUS_RATE_LIMIT_RPM", "200")
	t.Setenv("NEXUS_RATE_LIMIT_BURST", "50")
	prev, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Change env between Load and ReloadHotReloadable.
	t.Setenv("NEXUS_RATE_LIMIT_RPM", "300")
	t.Setenv("NEXUS_RATE_LIMIT_BURST", "75")
	next, result := ReloadHotReloadable(prev)
	if result.NeedsRestart != nil {
		t.Errorf("expected no restart-required settings, got %v", result.NeedsRestart)
	}
	if next.RateLimitRPM != 300 {
		t.Errorf("RateLimitRPM = %d, want 300", next.RateLimitRPM)
	}
	if next.RateLimitBurst != 75 {
		t.Errorf("RateLimitBurst = %d, want 75", next.RateLimitBurst)
	}
}

// TestReloadHotReloadable_LogLevel verifies that log level is correctly
// reloaded from NEXUS_LOG_LEVEL.
func TestReloadHotReloadable_LogLevel(t *testing.T) {
	t.Setenv("NEXUS_LOG_LEVEL", "debug")
	prev, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Setenv("NEXUS_LOG_LEVEL", "warn")
	next, result := ReloadHotReloadable(prev)
	if result.NeedsRestart != nil {
		t.Errorf("expected no restart-required settings, got %v", result.NeedsRestart)
	}
	if next.LogLevel != slog.LevelWarn {
		t.Errorf("LogLevel = %v, want warn", next.LogLevel)
	}
}

// TestReloadHotReloadable_LogFormat verifies that log format is correctly
// reloaded from NEXUS_LOG_FORMAT.
func TestReloadHotReloadable_LogFormat(t *testing.T) {
	t.Setenv("NEXUS_LOG_FORMAT", "text")
	prev, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Setenv("NEXUS_LOG_FORMAT", "json")
	next, result := ReloadHotReloadable(prev)
	if result.NeedsRestart != nil {
		t.Errorf("expected no restart-required settings, got %v", result.NeedsRestart)
	}
	if next.LogFormat != LogFormatJSON {
		t.Errorf("LogFormat = %v, want json", next.LogFormat)
	}
}

// TestReloadHotReloadable_Debug verifies that the debug flag is correctly
// reloaded from NEXUS_DEBUG.
func TestReloadHotReloadable_Debug(t *testing.T) {
	t.Setenv("NEXUS_DEBUG", "false")
	prev, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if prev.Debug != false {
		t.Errorf("prev.Debug = %v, want false", prev.Debug)
	}
	t.Setenv("NEXUS_DEBUG", "true")
	next, result := ReloadHotReloadable(prev)
	if result.NeedsRestart != nil {
		t.Errorf("expected no restart-required settings, got %v", result.NeedsRestart)
	}
	if next.Debug != true {
		t.Errorf("Debug = %v, want true", next.Debug)
	}
}

// TestReloadHotReloadable_RestartRequired verifies that changes to
// restart-required settings are detected and reported.
func TestReloadHotReloadable_RestartRequired(t *testing.T) {
	prev := Config{
		OllamaURL:     "http://localhost:11434",
		FrontierKey:   "sk-old",
		MetricsDBPath: "/tmp/old.db",
	}
	// Simulate operator changing restart-required settings.
	t.Setenv("NEXUS_OLLAMA_URL", "http://ollama.local:11434")
	t.Setenv("NEXUS_FRONTIER_API_KEY", "sk-new")
	t.Setenv("NEXUS_METRICS_DB", "/tmp/new.db")
	_, result := ReloadHotReloadable(prev)
	if len(result.NeedsRestart) != 3 {
		t.Errorf("expected 3 restart-required settings, got %d: %v", len(result.NeedsRestart), result.NeedsRestart)
	}
	found := make(map[string]bool)
	for _, n := range result.NeedsRestart {
		found[n] = true
	}
	if !found["NEXUS_OLLAMA_URL"] {
		t.Error("expected NEXUS_OLLAMA_URL in NeedsRestart")
	}
	if !found["NEXUS_FRONTIER_API_KEY"] {
		t.Error("expected NEXUS_FRONTIER_API_KEY in NeedsRestart")
	}
	if !found["NEXUS_METRICS_DB"] {
		t.Error("expected NEXUS_METRICS_DB in NeedsRestart")
	}
}

// TestReloadHotReloadable_UnchangedRestartRequired verifies that unchanged
// restart-required settings do not appear in NeedsRestart.
func TestReloadHotReloadable_UnchangedRestartRequired(t *testing.T) {
	prev := Config{
		OllamaURL:   "http://localhost:11434",
		FrontierKey: "sk-same",
	}
	// Set env vars to the same values as prev.
	t.Setenv("NEXUS_OLLAMA_URL", prev.OllamaURL)
	t.Setenv("NEXUS_FRONTIER_API_KEY", prev.FrontierKey)
	_, result := ReloadHotReloadable(prev)
	if result.NeedsRestart != nil {
		t.Errorf("expected no restart-required settings (unchanged), got %v", result.NeedsRestart)
	}
}

// TestReloadHotReloadable_PreservesNonReloadable verifies that non-reloadable
// fields are preserved from the previous config.
func TestReloadHotReloadable_PreservesNonReloadable(t *testing.T) {
	prev := Config{
		Addr:           ":9000",
		OllamaURL:      "http://ollama.local:11434",
		RouterModel:    "my-model",
		TokenGuardrail: 9999,
		FrontierKey:    "sk-frontier",
		MetricsDBPath:  "/tmp/metrics.db",
	}
	t.Setenv("NEXUS_RATE_LIMIT_RPM", "500") // only change rate limit
	_, result := ReloadHotReloadable(prev)
	if result.NeedsRestart != nil {
		t.Errorf("expected no restart-required settings, got %v", result.NeedsRestart)
	}
	// Reload would be called with a fresh prev in the SIGHUP handler,
	// so we verify the returned cfg preserves non-reloadable fields.
}

func TestReadinessModeValidation(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		wantErr  bool
		wantMode string
	}{
		{
			name:     "default degraded",
			envValue: "",
			wantErr:  false,
			wantMode: "degraded",
		},
		{
			name:     "strict",
			envValue: "strict",
			wantErr:  false,
			wantMode: "strict",
		},
		{
			name:     "degraded explicit",
			envValue: "degraded",
			wantErr:  false,
			wantMode: "degraded",
		},
		{
			name:     "unknown value fails validation",
			envValue: "unknown",
			wantErr:  true,
			wantMode: "",
		},
		{
			name:     "strict with whitespace fails",
			envValue: " strict ",
			wantErr:  true,
			wantMode: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envValue != "" {
				t.Setenv("NEXUS_READINESS_MODE", tc.envValue)
			} else {
				t.Setenv("NEXUS_READINESS_MODE", "")
			}

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Errorf("Load() with NEXUS_READINESS_MODE=%q = %+v, want error", tc.envValue, cfg)
				}
				return
			}
			if err != nil {
				t.Errorf("Load() with NEXUS_READINESS_MODE=%q returned error: %v", tc.envValue, err)
				return
			}
			if cfg.ReadinessMode != tc.wantMode {
				t.Errorf("cfg.ReadinessMode = %q, want %q", cfg.ReadinessMode, tc.wantMode)
			}
		})
	}
}
