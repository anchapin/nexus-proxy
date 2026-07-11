package config

import (
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
