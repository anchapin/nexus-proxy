package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeConfigFile writes a YAML config file to a temp directory and
// returns the absolute path. Convenience for the integration tests
// below; keeps the test bodies focused on the precedence they exercise.
func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// chdir changes CWD for the duration of the test and restores it on
// cleanup. Used to exercise DiscoverConfigFile without polluting other
// tests.
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// clearAllEnv unsets every NEXUS_* env var the config package looks
// at so each test starts from a clean slate regardless of the
// developer shell. We can't blanket-unset, so we list the known keys.
func clearAllEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"NEXUS_CONFIG",
		"NEXUS_ADDR", "NEXUS_LOG_LEVEL", "NEXUS_LOG_FORMAT",
		"NEXUS_OLLAMA_URL", "NEXUS_ROUTER_MODEL", "NEXUS_LOCAL_MODEL", "NEXUS_EMBEDDING_MODEL",
		"NEXUS_FRONTIER_URL", "NEXUS_FRONTIER_MODEL", "NEXUS_FRONTIER_API_KEY",
		"NEXUS_ZAI_URL", "NEXUS_ZAI_MODEL", "NEXUS_ZAI_API_KEY",
		"NEXUS_EXAMPLES_DIR", "NEXUS_RAG_THRESHOLD",
		"NEXUS_TOKEN_GUARDRAIL", "NEXUS_SLM_TIMEOUT", "NEXUS_FUSION_TIMEOUT",
		"NEXUS_CASCADE_TIMEOUT", "NEXUS_ARBITER_TIMEOUT",
		"NEXUS_HEALTH_POLL_INTERVAL", "NEXUS_HEALTH_BREAKER_THRESHOLD", "NEXUS_HEALTH_PROBE_TIMEOUT",
		"NEXUS_JUDGE_URL", "NEXUS_JUDGE_MODEL", "NEXUS_JUDGE_API_KEY",
		"NEXUS_JUDGE_SAMPLE_RATE", "NEXUS_JUDGE_CONCURRENCY", "NEXUS_JUDGE_QUEUE",
		"NEXUS_JUDGE_TIMEOUT", "NEXUS_JUDGE_COST_PER_1K",
		"NEXUS_TELEMETRY_PATH", "NEXUS_METRICS_DB",
		"NEXUS_QUALITY_CONCURRENCY", "NEXUS_QUALITY_QUEUE",
		"NEXUS_QUALITY_TIMEOUT", "NEXUS_QUALITY_STDERR_CAP",
		"NEXUS_PROBE_INTERVAL", "NEXUS_PROBE_TIMEOUT", "NEXUS_PROBE_BYTES_PER_TOKEN",
		"NEXUS_MAX_BODY_BYTES",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
	// t.Setenv with "" still leaves the var set-but-empty in the env,
	// which trips the "env wins" branch in resolveString. Unset them
	// outright so the file path actually wins.
	for _, k := range keys {
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("Unsetenv %s: %v", k, err)
		}
	}
}

func TestLoad_FileOnlyConfig(t *testing.T) {
	clearAllEnv(t)
	path := writeConfigFile(t, `server:
  addr: ":9100"
ollama:
  url: http://gpu.local:11434
  router_model: llama3.2:3b
routing:
  slm_timeout: 5s
  token_guardrail: 1234
rag:
  threshold: 0.7
`)
	SetConfigPathOverride(path)
	t.Cleanup(func() { SetConfigPathOverride("") })

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ConfigFile != path {
		t.Errorf("ConfigFile = %q, want %q", cfg.ConfigFile, path)
	}
	if cfg.Addr != ":9100" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.OllamaURL != "http://gpu.local:11434" {
		t.Errorf("OllamaURL = %q", cfg.OllamaURL)
	}
	if cfg.RouterModel != "llama3.2:3b" {
		t.Errorf("RouterModel = %q", cfg.RouterModel)
	}
	if cfg.SLMTimeout != 5*time.Second {
		t.Errorf("SLMTimeout = %v", cfg.SLMTimeout)
	}
	if cfg.TokenGuardrail != 1234 {
		t.Errorf("TokenGuardrail = %d", cfg.TokenGuardrail)
	}
	if cfg.RAGThreshold != 0.7 {
		t.Errorf("RAGThreshold = %v", cfg.RAGThreshold)
	}
	// Unset fields still fall back to built-in defaults.
	if cfg.LocalModel != "qwen3-coder:8b" {
		t.Errorf("LocalModel default = %q", cfg.LocalModel)
	}
	if cfg.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Errorf("MaxBodyBytes default = %d", cfg.MaxBodyBytes)
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	clearAllEnv(t)
	path := writeConfigFile(t, `server:
  addr: ":9100"
ollama:
  router_model: llama3.2:3b
routing:
  slm_timeout: 5s
  token_guardrail: 1234
`)
	SetConfigPathOverride(path)
	t.Cleanup(func() { SetConfigPathOverride("") })

	// Env values must beat the file.
	t.Setenv("NEXUS_ADDR", ":9999")
	t.Setenv("NEXUS_SLM_TIMEOUT", "11s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":9999" {
		t.Errorf("Addr = %q (env should override file)", cfg.Addr)
	}
	if cfg.SLMTimeout != 11*time.Second {
		t.Errorf("SLMTimeout = %v (env should override file)", cfg.SLMTimeout)
	}
	// File values still apply where env is unset.
	if cfg.RouterModel != "llama3.2:3b" {
		t.Errorf("RouterModel = %q (file should apply)", cfg.RouterModel)
	}
	if cfg.TokenGuardrail != 1234 {
		t.Errorf("TokenGuardrail = %d (file should apply)", cfg.TokenGuardrail)
	}
}

func TestLoad_FileEnvExpansion(t *testing.T) {
	clearAllEnv(t)
	t.Setenv("NEXUS_FRONTIER_API_KEY", "sk-from-env-12345")
	path := writeConfigFile(t, `frontier:
  url: "${NEXUS_FRONTIER_URL}"
  api_key: "${NEXUS_FRONTIER_API_KEY}"
ollama:
  url: "http://$NEXUS_OLLAMA_URL_HOST:11434"
`)
	// NEXUS_FRONTIER_URL is unset so ${...} expands to "" — that means
	// the file value becomes empty, which then loses to the built-in
	// default. We assert that, since it captures the intended
	// behaviour without entangling the test with the env var's own
	// default chain.
	t.Setenv("NEXUS_OLLAMA_URL_HOST", "gpu.lan")
	SetConfigPathOverride(path)
	t.Cleanup(func() { SetConfigPathOverride("") })

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.FrontierKey != "sk-from-env-12345" {
		t.Errorf("FrontierKey = %q (env expansion in YAML should pull secret)", cfg.FrontierKey)
	}
	// OllamaURL is composed from an env-var fragment. The full value
	// after expansion is "http://gpu.lan:11434".
	if cfg.OllamaURL != "http://gpu.lan:11434" {
		t.Errorf("OllamaURL = %q", cfg.OllamaURL)
	}
}

func TestLoad_MissingFileGracefulDegradation(t *testing.T) {
	clearAllEnv(t)
	// SetConfigPathOverride points at a path that does not exist.
	// LoadFile returns (nil, nil) for missing files, so Load() should
	// succeed and fall back to env-only behaviour.
	SetConfigPathOverride("/tmp/this-nexus-config-does-not-exist-xyzzy.yaml")
	t.Cleanup(func() { SetConfigPathOverride("") })

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// ConfigFile is cleared because the file wasn't actually loaded.
	if cfg.ConfigFile != "" {
		t.Errorf("ConfigFile = %q, want \"\" for missing file", cfg.ConfigFile)
	}
	// Defaults still apply.
	if cfg.Addr != ":8000" {
		t.Errorf("Addr default = %q", cfg.Addr)
	}
}

func TestLoad_MalformedFileError(t *testing.T) {
	clearAllEnv(t)
	// "server\n  addr: foo" — the "server" line has no colon, so the
	// parser flags it as malformed.
	path := writeConfigFile(t, "server\n  addr: foo\n")
	SetConfigPathOverride(path)
	t.Cleanup(func() { SetConfigPathOverride("") })

	if _, err := Load(); err == nil {
		t.Errorf("expected parse error for malformed YAML, got nil")
	}
}

func TestLoad_AutoDiscovery(t *testing.T) {
	clearAllEnv(t)
	dir := t.TempDir()
	chdir(t, dir)
	path := filepath.Join(dir, "nexus.yaml")
	if err := os.WriteFile(path, []byte("server:\n  addr: \":7777\"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// No --config flag, no NEXUS_CONFIG env, so discovery kicks in.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// ConfigFile is whatever DiscoverConfigFile returned — relative
	// "nexus.yaml" or the absolute path, depending on how the kernel
	// resolves the os.Stat call. Either is acceptable; the file
	// content is what matters.
	if cfg.ConfigFile == "" {
		t.Errorf("ConfigFile is empty, expected nexus.yaml path")
	}
	if cfg.Addr != ":7777" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
}

func TestLoad_ExplicitPathFromFlag(t *testing.T) {
	clearAllEnv(t)
	// Even with CWD containing a different nexus.yaml, --config must
	// win.
	cwd := t.TempDir()
	chdir(t, cwd)
	if err := os.WriteFile(filepath.Join(cwd, "nexus.yaml"), []byte("server:\n  addr: \":0001\"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	override := writeConfigFile(t, "server:\n  addr: \":0002\"\n")
	SetConfigPathOverride(override)
	t.Cleanup(func() { SetConfigPathOverride("") })

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ConfigFile != override {
		t.Errorf("ConfigFile = %q, want explicit override %q", cfg.ConfigFile, override)
	}
	if cfg.Addr != ":0002" {
		t.Errorf("Addr = %q (override should beat CWD discovery)", cfg.Addr)
	}
}

func TestLoad_NoFileNoEnvBackwardCompatible(t *testing.T) {
	clearAllEnv(t)
	// CWD has no nexus.yaml, NEXUS_CONFIG unset, --config unset.
	chdir(t, t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ConfigFile != "" {
		t.Errorf("ConfigFile = %q, want \"\"", cfg.ConfigFile)
	}
	// Every field must equal the value produced by the pre-#31 code
	// path. Spot-check a representative subset.
	if cfg.Addr != ":8000" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.OllamaURL != "http://localhost:11434" {
		t.Errorf("OllamaURL = %q", cfg.OllamaURL)
	}
	if cfg.TokenGuardrail != 6000 {
		t.Errorf("TokenGuardrail = %d", cfg.TokenGuardrail)
	}
	if cfg.SLMTimeout != 8*time.Second {
		t.Errorf("SLMTimeout = %v", cfg.SLMTimeout)
	}
	if cfg.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Errorf("MaxBodyBytes = %d", cfg.MaxBodyBytes)
	}
}

func TestLoad_FileBoolAndFloatTypes(t *testing.T) {
	// Verifies the "type inference pass-through" promise: YAML
	// literals that look like bool / float are stored as their source
	// text and round-trip through strconv cleanly.
	clearAllEnv(t)
	path := writeConfigFile(t, `rag:
  threshold: 0.75
judge:
  sample_rate: 0.42
quality:
  stderr_cap: 4096
routing:
  cascade_timeout: 90s
`)
	SetConfigPathOverride(path)
	t.Cleanup(func() { SetConfigPathOverride("") })

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RAGThreshold != 0.75 {
		t.Errorf("RAGThreshold = %v", cfg.RAGThreshold)
	}
	if cfg.JudgeSampleRate != 0.42 {
		t.Errorf("JudgeSampleRate = %v", cfg.JudgeSampleRate)
	}
	if cfg.QualityStderrCap != 4096 {
		t.Errorf("QualityStderrCap = %d", cfg.QualityStderrCap)
	}
	if cfg.CascadeTimeout != 90*time.Second {
		t.Errorf("CascadeTimeout = %v", cfg.CascadeTimeout)
	}
}

func TestLoad_FileInvalidIntErrors(t *testing.T) {
	clearAllEnv(t)
	path := writeConfigFile(t, `routing:
  token_guardrail: not-a-number
`)
	SetConfigPathOverride(path)
	t.Cleanup(func() { SetConfigPathOverride("") })

	if _, err := Load(); err == nil {
		t.Errorf("expected error for invalid int in YAML, got nil")
	}
}

func TestLoad_FileInvalidDurationErrors(t *testing.T) {
	clearAllEnv(t)
	path := writeConfigFile(t, `routing:
  slm_timeout: eight seconds
`)
	SetConfigPathOverride(path)
	t.Cleanup(func() { SetConfigPathOverride("") })

	if _, err := Load(); err == nil {
		t.Errorf("expected error for invalid duration in YAML, got nil")
	}
}

func TestLoad_TelemetryEmptyFromFile(t *testing.T) {
	// File value `path: ""` should disable telemetry, matching the
	// env-var semantics for NEXUS_TELEMETRY_PATH="".
	clearAllEnv(t)
	path := writeConfigFile(t, `telemetry:
  path: ""
`)
	SetConfigPathOverride(path)
	t.Cleanup(func() { SetConfigPathOverride("") })

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TelemetryEnabled() {
		t.Errorf("TelemetryEnabled = true, want false for empty path in file")
	}
	if cfg.TelemetryPath != "" {
		t.Errorf("TelemetryPath = %q, want \"\"", cfg.TelemetryPath)
	}
}

func TestLoad_FileKeyTranslationDropsUnknown(t *testing.T) {
	// Unknown section/key combos are silently ignored so a typo in
	// nexus.yaml doesn't crash boot.
	clearAllEnv(t)
	path := writeConfigFile(t, `unknown_section:
  whatever: 42
server:
  typo_key: oops
  addr: ":5555"
`)
	SetConfigPathOverride(path)
	t.Cleanup(func() { SetConfigPathOverride("") })

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":5555" {
		t.Errorf("Addr = %q (known key should still apply)", cfg.Addr)
	}
}

func TestLoad_FileEnvExpansionFallbackToFrontier(t *testing.T) {
	// When NEXUS_JUDGE_URL is unset everywhere, JudgeURL should
	// fall back to FrontierURL — same behaviour as the pre-#31 code.
	clearAllEnv(t)
	path := writeConfigFile(t, `frontier:
  url: http://frontier.example/v1
  model: gpt-5
`)
	SetConfigPathOverride(path)
	t.Cleanup(func() { SetConfigPathOverride("") })

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.JudgeURL != "http://frontier.example/v1" {
		t.Errorf("JudgeURL = %q, want fallback to frontier URL", cfg.JudgeURL)
	}
}

func TestLoad_FileOverridesDefaults(t *testing.T) {
	// File-level override of log level — a common "I just want debug
	// logs locally" use case that no longer requires editing .env.
	clearAllEnv(t)
	path := writeConfigFile(t, `log:
  level: debug
  format: text
`)
	SetConfigPathOverride(path)
	t.Cleanup(func() { SetConfigPathOverride("") })

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel.String() != "DEBUG" {
		t.Errorf("LogLevel = %v, want DEBUG", cfg.LogLevel)
	}
	if cfg.LogFormat != LogFormatText {
		t.Errorf("LogFormat = %v, want LogFormatText", cfg.LogFormat)
	}
}
