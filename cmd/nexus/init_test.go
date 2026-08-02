package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Note: we use net/http/httptest for the probe servers.

// runInitInDir calls runInitWith with a temp dir as CWD so generated// files do not pollute the repo. It returns (exitCode, stdout, stderr).
// stdin is the interactive input (empty for non-interactive tests).
func runInitInDir(t *testing.T, args []string, stdin string, client *http.Client) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	var stdout, stderr bytes.Buffer
	code := runInitWith(args, &stdout, &stderr, strings.NewReader(stdin), client, 0)
	return code, stdout.String(), stderr.String()
}

// TestInitDispatchRoutes verifies dispatch wires "init" to runInit.
func TestInitDispatchRoutes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// --non-interactive with no Ollama running still exits 0 (writes config).
	dir := t.TempDir()
	t.Chdir(dir)
	code, handled := dispatch([]string{"nexus", "init", "--non-interactive"}, &stdout, &stderr)
	if !handled {
		t.Fatal("expected handled=true for init")
	}
	if code != 0 {
		t.Errorf("expected exit 0, got %d (stderr=%s)", code, stderr.String())
	}
}

// TestInitNonInteractiveWritesEnv verifies the --non-interactive path
// writes a .env file containing the seeded values + profile knobs.
func TestInitNonInteractiveWritesEnv(t *testing.T) {
	t.Setenv("NEXUS_INIT_PROFILE", "local-first")
	t.Setenv("NEXUS_FRONTIER_API_KEY", "sk-test-123")

	code, stdout, stderr := runInitInDir(t, []string{"--non-interactive"}, "", nil)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	data, err := os.ReadFile(".env")
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		"NEXUS_OLLAMA_URL=http://localhost:11434",
		"NEXUS_ROUTER_MODEL=qwen3-coder:4b",
		"NEXUS_LOCAL_MODEL=qwen3-coder:8b",
		"NEXUS_EMBEDDING_MODEL=nomic-embed-text",
		"NEXUS_FRONTIER_MODEL=gpt-4o",
		"NEXUS_FRONTIER_API_KEY=sk-test-123",
		"# profile: local-first",
		"NEXUS_SLM_CONFIDENCE_THRESHOLD=0.2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf(".env missing %q\nbody:\n%s", want, body)
		}
	}
	if !strings.Contains(stdout, "non-interactive mode") {
		t.Errorf("stdout should mention non-interactive mode, got: %s", stdout)
	}
}

// TestInitNonInteractiveWritesYAML verifies that when
// NEXUS_CONFIG_FILE points to a .yaml path, the wizard writes a
// config.yaml instead of .env.
func TestInitNonInteractiveWritesYAML(t *testing.T) {
	t.Setenv("NEXUS_CONFIG_FILE", "config.yaml")
	t.Setenv("NEXUS_INIT_PROFILE", "frontier-default")

	code, stdout, stderr := runInitInDir(t, []string{"--non-interactive"}, "", nil)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	data, err := os.ReadFile("config.yaml")
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		"ollama_url:",
		"router_model:",
		"frontier_api_key:",
		"# profile: frontier-default",
		"slm_confidence_threshold: 0.6",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config.yaml missing %q\nbody:\n%s", want, body)
		}
	}
	if !strings.Contains(stdout, "nexus config validate config.yaml") {
		t.Errorf("stdout should print yaml verify hint, got: %s", stdout)
	}
	// .env must NOT have been written.
	if _, err := os.Stat(".env"); !os.IsNotExist(err) {
		t.Errorf(".env should not exist when NEXUS_CONFIG_FILE is set")
	}
}

// TestInitNonInteractiveOutputFlag verifies --output overrides the
// destination path.
func TestInitNonInteractiveOutputFlag(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom.env")
	code, _, stderr := runInitInDir(t, []string{"--non-interactive", "--output", custom}, "", nil)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("custom output not written: %v", err)
	}
}

// TestInitMissingOllama verifies the wizard reports the unreachable
// endpoint but still writes a config (non-interactive must always
// produce output). Uses a live httptest server that returns 500 to
// simulate a broken Ollama.
func TestInitMissingOllama(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("NEXUS_OLLAMA_URL", srv.URL)

	code, stdout, stderr := runInitInDir(t, []string{"--non-interactive"}, "", srv.Client())
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "[FAIL]") {
		t.Errorf("stdout should report [FAIL] for broken Ollama, got: %s", stdout)
	}
	// Config must still be written despite the failure.
	if _, err := os.Stat(".env"); err != nil {
		t.Errorf(".env should be written even when Ollama is down: %v", err)
	}
}

// TestInitMissingOllamaConnectionError verifies the connection-refused
// path (no server at all) reports [FAIL] and still writes config.
func TestInitMissingOllamaConnectionError(t *testing.T) {
	// Point at a port that is guaranteed to refuse connections.
	t.Setenv("NEXUS_OLLAMA_URL", "http://127.0.0.1:1")
	code, stdout, stderr := runInitInDir(t, []string{"--non-interactive"}, "", &http.Client{})
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "[FAIL]") {
		t.Errorf("stdout should report [FAIL] for unreachable Ollama, got: %s", stdout)
	}
}

// TestInitOllamaReachableAndMissingModels verifies that when Ollama
// answers /api/tags, the interactive wizard reports missing models
// with exact `ollama pull` commands. The server returns an empty
// model list so all three defaults are flagged as missing.
func TestInitOllamaReachableAndMissingModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	t.Setenv("NEXUS_OLLAMA_URL", srv.URL)

	// Interactive: accept all defaults by feeding newlines.
	stdin := strings.Repeat("\n", 20)
	code, stdout, stderr := runInitInDir(t, nil, stdin, srv.Client())
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "[PASS] Ollama reachable") {
		t.Errorf("should report Ollama reachable, got: %s", stdout)
	}
	for _, m := range []string{"qwen3-coder:4b", "qwen3-coder:8b", "nomic-embed-text"} {
		if !strings.Contains(stdout, "ollama pull "+m) {
			t.Errorf("should suggest pulling %s, got: %s", m, stdout)
		}
	}
}

// TestInitFrontierKeyAccepted verifies the wizard probes the frontier
// key and reports [PASS] on a 200 response.
func TestInitFrontierKeyAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(`{"models":[]}`))
			return
		}
		// Frontier models endpoint.
		if r.Header.Get("Authorization") != "Bearer sk-good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("NEXUS_OLLAMA_URL", srv.URL)
	t.Setenv("NEXUS_FRONTIER_URL", srv.URL+"/v1/chat/completions")
	t.Setenv("NEXUS_FRONTIER_API_KEY", "sk-good")

	code, stdout, stderr := runInitInDir(t, []string{"--non-interactive"}, "", srv.Client())
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "[PASS] frontier key accepted") {
		t.Errorf("should report key accepted, got: %s", stdout)
	}
}

// TestInitFrontierKeyRejected verifies the wizard reports [FAIL] when
// the endpoint returns 401.
func TestInitFrontierKeyRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(`{"models":[]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("NEXUS_OLLAMA_URL", srv.URL)
	t.Setenv("NEXUS_FRONTIER_URL", srv.URL+"/v1/chat/completions")
	t.Setenv("NEXUS_FRONTIER_API_KEY", "sk-bad")

	code, stdout, stderr := runInitInDir(t, []string{"--non-interactive"}, "", srv.Client())
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "[FAIL] frontier key rejected") {
		t.Errorf("should report key rejected, got: %s", stdout)
	}
}

// TestInitNoFrontierKeySkipsProbe verifies that when no key is set,
// the wizard skips the frontier probe rather than failing.
func TestInitNoFrontierKeySkipsProbe(t *testing.T) {
	t.Setenv("NEXUS_FRONTIER_API_KEY", "")
	code, stdout, stderr := runInitInDir(t, []string{"--non-interactive"}, "", nil)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "[SKIP]") {
		t.Errorf("should report [SKIP] for missing frontier key, got: %s", stdout)
	}
}

// TestInitInteractiveWritesConfig verifies the interactive wizard
// collects answers from stdin and writes them to .env.
func TestInitInteractiveWritesConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"name":"qwen3-coder:4b"},{"name":"qwen3-coder:8b"},{"name":"nomic-embed-text"}]}`))
	}))
	defer srv.Close()
	t.Setenv("NEXUS_OLLAMA_URL", srv.URL)

	// stdin: accept Ollama URL default, then frontier defaults, skip key,
	// accept model defaults, accept profile default, skip proxy key.
	stdin := strings.Repeat("\n", 20)
	code, stdout, stderr := runInitInDir(t, nil, stdin, srv.Client())
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "Interactive Config Wizard") {
		t.Errorf("should show wizard header, got: %s", stdout)
	}
	data, err := os.ReadFile(".env")
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	if !strings.Contains(string(data), "NEXUS_OLLAMA_URL="+srv.URL) {
		t.Errorf(".env should contain the Ollama URL, got: %s", data)
	}
}

// TestInitProfileValidation verifies an invalid profile in
// non-interactive mode still writes config (the profile is just a
// knob preset; an unknown value yields no extra knobs).
func TestInitProfileValidation(t *testing.T) {
	t.Setenv("NEXUS_INIT_PROFILE", "bogus")
	code, _, stderr := runInitInDir(t, []string{"--non-interactive"}, "", nil)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	data, err := os.ReadFile(".env")
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	if !strings.Contains(string(data), "# profile: bogus") {
		t.Errorf(".env should record the profile even when unknown")
	}
}

// TestInitHelpFlag verifies -h/--help print usage and exit 0.
func TestInitHelpFlag(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runInitWith([]string{flag}, &stdout, &stderr, strings.NewReader(""), nil, 0)
			if code != 0 {
				t.Errorf("expected exit 0, got %d", code)
			}
			if !strings.Contains(stderr.String(), "Usage:") {
				t.Errorf("stderr should contain Usage:, got: %s", stderr.String())
			}
		})
	}
}

// TestInitUsageMentionsSubcommand verifies the dispatch help text now
// lists `init`.
func TestInitUsageMentionsSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, _ = dispatch([]string{"nexus", "--help"}, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "nexus init") {
		t.Errorf("help text should list `nexus init`, got: %s", stderr.String())
	}
}

// TestFrontierModelsURL verifies the suffix-stripping helper.
func TestFrontierModelsURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://api.openai.com/v1/chat/completions", "https://api.openai.com/v1/models"},
		{"https://api.openai.com/v1/completions", "https://api.openai.com/v1/models"},
		{"https://api.openai.com/v1/models", "https://api.openai.com/v1/models"},
		{"https://example.com", "https://example.com/models"},
		{"https://example.com/", "https://example.com/models"},
	}
	for _, c := range cases {
		if got := frontierModelsURL(c.in); got != c.want {
			t.Errorf("frontierModelsURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestProfileKnobs verifies each preset returns the expected knobs.
func TestProfileKnobs(t *testing.T) {
	if v := profileKnobs(profileLocalFirst)["NEXUS_SLM_CONFIDENCE_THRESHOLD"]; v != "0.2" {
		t.Errorf("local-first threshold = %q, want 0.2", v)
	}
	if v := profileKnobs(profileFrontierDefault)["NEXUS_SLM_CONFIDENCE_THRESHOLD"]; v != "0.6" {
		t.Errorf("frontier-default threshold = %q, want 0.6", v)
	}
	if v := profileKnobs(profileFusionBalanced)["NEXUS_FUSION_AGREEMENT_THRESHOLD"]; v != "0.85" {
		t.Errorf("fusion-balanced threshold = %q, want 0.85", v)
	}
	if v := profileKnobs("bogus"); v != nil {
		t.Errorf("unknown profile should yield nil knobs, got %v", v)
	}
}
