package diag

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/config"
)

// ollamaFixture wires an httptest server with the endpoints the diag
// checks depend on (currently /api/tags and /api/embeddings). Tests
// register the canned model inventory via the Tags handler and the
// canned /api/ps body via PS; both default to "empty + healthy" so a
// happy-path test only needs to override the fields it cares about.
type ollamaFixture struct {
	*httptest.Server
	tagsBody   atomic.Value // string — JSON document for /api/tags
	psBody     atomic.Value // string — JSON document for /api/ps
	embedBody  atomic.Value // string — JSON document for /api/embeddings
	tagCalls   atomic.Int32
	psCalls    atomic.Int32
	embedCalls atomic.Int32
}

func newOllamaFixture(t *testing.T) *ollamaFixture {
	t.Helper()
	f := &ollamaFixture{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			f.tagCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			body, _ := f.tagsBody.Load().(string)
			if body == "" {
				body = `{"models":[]}`
			}
			_, _ = w.Write([]byte(body))
		case "/api/ps":
			f.psCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			body, _ := f.psBody.Load().(string)
			if body == "" {
				body = `{"models":[]}`
			}
			_, _ = w.Write([]byte(body))
		case "/api/embeddings":
			f.embedCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			body, _ := f.embedBody.Load().(string)
			if body == "" {
				// Default: return a valid embedding response so the
				// check passes without every test having to stub it.
				body = `{"embedding":[0.1,0.2,0.3]}`
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// frontierFixture records the Authorization header so tests can
// assert the Bearer token is forwarded without logging it. Returns a
// /v1/models endpoint that mirrors OpenAI's shape; tests override
// Status to drive the pass/fail paths.
type frontierFixture struct {
	*httptest.Server
	status int
	bearer atomic.Value // string — most recent Authorization header
	calls  atomic.Int32
}

func newFrontierFixture(t *testing.T, status int) *frontierFixture {
	t.Helper()
	f := &frontierFixture{status: status}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		// Accept either /v1/models or /models so the fixture works
		// when the test wires cfg.FrontierURL with or without the
		// /v1 prefix.
		if r.URL.Path != "/v1/models" && r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			f.bearer.Store(auth)
		}
		// Truncate the body so a successful response is a tiny,
		// recognisable JSON document.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(f.status))
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	t.Cleanup(f.Close)
	return f
}

// nexusFixture mocks the Nexus /v1/models endpoint for diagnostic
// testing. The zero value serves a minimal valid response with no
// models; set modelsBody to override.
type nexusFixture struct {
	*httptest.Server
	modelsBody atomic.Value // string — JSON for /v1/models
	status     int          // HTTP status; defaults to 200
	calls      atomic.Int32
}

func newNexusFixture(t *testing.T) *nexusFixture {
	t.Helper()
	f := &nexusFixture{status: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.URL.Path != "/v1/models" && r.URL.Path != "/v1/models/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		body, _ := f.modelsBody.Load().(string)
		if body == "" {
			// Minimal valid response with no models.
			body = `{"object":"list","data":[]}`
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.Close)
	return f
}

// fixtureConfig returns a config.Config pointed at the supplied
// test servers. Defaults are picked so a happy-path test passes with
// zero overrides.
func fixtureConfig(ollamaURL, frontierURL string) config.Config {
	cfg := config.Config{
		OllamaURL:          ollamaURL,
		RouterModel:        "qwen3-coder:4b",
		LocalModel:         "qwen3-coder:8b",
		EmbeddingModel:     "nomic-embed-text",
		FrontierURL:        frontierURL,
		FrontierModel:      "gpt-4o",
		FrontierKey:        "",
		ZAIURL:             "https://api.z.ai/v1/chat/completions",
		ZAIModel:           "glm-4.6",
		ZAIKey:             "",
		ExamplesDir:        "./few_shot_examples",
		TokenGuardrail:     6000,
		ProbeBytesPerToken: 256 * 1024,
		TelemetryPath:      "./nexus-telemetry.jsonl",
		MetricsDBPath:      "./.cache/nexus-proxy/metrics.db",
		JudgeSampleRate:    0, // judge disabled by default in fixtures
	}
	return cfg
}

// withOptions returns an Options struct pointed at the supplied
// fixtures with a generous but bounded timeout.
func withOptions(ollamaURL string) Options {
	return Options{
		OllamaURL:  ollamaURL,
		Timeout:    2 * time.Second,
		HTTPClient: http.DefaultClient,
	}
}

// --- Run() integration tests ----------------------------------------------

func TestRunHealthyOllamaEmptyFixtures(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	if len(res) == 0 {
		t.Fatal("Run returned no checks")
	}
	// Ollama reachable but every configured model is missing — the
	// three model checks must fail with the "Run: ollama pull ..."
	// remediation hint.
	wantFails := map[string]bool{
		checkRouterModel:    false,
		checkLocalModel:     false,
		checkEmbeddingModel: false,
	}
	for _, c := range res {
		if _, ok := wantFails[c.Name]; ok {
			if c.Status != StatusFail {
				t.Errorf("%s = %s, want fail (detail=%s)", c.Name, c.Status, c.Detail)
			}
			if !strings.Contains(c.Detail, "ollama pull") {
				t.Errorf("%s detail missing remediation hint: %q", c.Name, c.Detail)
			}
			wantFails[c.Name] = true
		}
	}
	for name, saw := range wantFails {
		if !saw {
			t.Errorf("missing check %s", name)
		}
	}
	// Frontier should be skipped (no key), Z.ai should warn (no key).
	if got := checkByName(res, checkFrontierKey); got.Status != StatusSkip {
		t.Errorf("frontier = %s, want skip", got.Status)
	}
	if got := checkByName(res, checkZAIKey); got.Status != StatusWarn {
		t.Errorf("zai = %s, want warn", got.Status)
	}
}

func TestRunAllModelsPresent(t *testing.T) {
	ollama := newOllamaFixture(t)
	ollama.tagsBody.Store(`{"models":[
		{"name":"qwen3-coder:4b"},
		{"name":"qwen3-coder:8b"},
		{"name":"nomic-embed-text"}
	]}`)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	for _, name := range []string{checkRouterModel, checkLocalModel, checkEmbeddingModel} {
		got := checkByName(res, name)
		if got.Status != StatusPass {
			t.Errorf("%s = %s (detail=%s), want pass", name, got.Status, got.Detail)
		}
	}
}

func TestRunModelMatchedByBaseName(t *testing.T) {
	// Ollama returns the bare family name; the config asks for the
	// tagged variant. The check should still pass — the operator
	// usually writes the tag, but Ollama accepts both.
	ollama := newOllamaFixture(t)
	ollama.tagsBody.Store(`{"models":[{"name":"qwen3-coder:8b"}]}`)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkLocalModel)
	if got.Status != StatusPass {
		t.Errorf("local model = %s (detail=%s), want pass", got.Status, got.Detail)
	}
}

func TestRunEmbeddingModelFunctional(t *testing.T) {
	// Embedding model is listed AND responds to /api/embeddings with
	// a non-empty vector — check should pass.
	ollama := newOllamaFixture(t)
	ollama.tagsBody.Store(`{"models":[{"name":"nomic-embed-text"}]}`)
	ollama.embedBody.Store(`{"embedding":[0.1,0.2,0.3,0.4,0.5]}`)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkEmbeddingModel)
	if got.Status != StatusPass {
		t.Errorf("embedding = %s (detail=%s), want pass", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "5-dim") {
		t.Errorf("detail should report vector dimensions: %s", got.Detail)
	}
	if ollama.embedCalls.Load() == 0 {
		t.Error("/api/embeddings was never called")
	}
}

func TestRunEmbeddingModelEmptyVectorIsFail(t *testing.T) {
	// Ollama responds with 200 but an empty embedding — the model
	// is corrupt or incompatible; should be a failure.
	ollama := newOllamaFixture(t)
	ollama.tagsBody.Store(`{"models":[{"name":"nomic-embed-text"}]}`)
	ollama.embedBody.Store(`{"embedding":[]}`)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkEmbeddingModel)
	if got.Status != StatusFail {
		t.Errorf("embedding = %s (detail=%s), want fail", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "empty embedding") {
		t.Errorf("detail should mention empty embedding: %s", got.Detail)
	}
}

func TestRunEmbeddingModelAPIFailure(t *testing.T) {
	// Embedding model is in the inventory but /api/embeddings returns
	// a non-200 status — the model is pulled but not functional.
	ollama := newOllamaFixture(t)
	ollama.tagsBody.Store(`{"models":[{"name":"nomic-embed-text"}]}`)
	// Override the embeddings handler to return 500 after the call is made.
	originalHandler := ollama.Server.Config.Handler
	ollama.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/embeddings" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"model not loaded"}`))
			return
		}
		originalHandler.ServeHTTP(w, r)
	})
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkEmbeddingModel)
	if got.Status != StatusFail {
		t.Errorf("embedding = %s (detail=%s), want fail", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "500") {
		t.Errorf("detail should mention status 500: %s", got.Detail)
	}
}

func TestRunEmbeddingModelNoModelConfigured(t *testing.T) {
	ollama := newOllamaFixture(t)
	ollama.tagsBody.Store(`{"models":[{"name":"qwen3-coder:8b"}]}`)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.EmbeddingModel = ""

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkEmbeddingModel)
	if got.Status != StatusFail {
		t.Errorf("embedding = %s, want fail when no model configured", got.Status)
	}
}

func TestRunOllamaDown(t *testing.T) {
	cfg := fixtureConfig("http://127.0.0.1:1", "https://api.openai.com/v1/chat/completions")
	// A short timeout ensures the test fails fast on platforms
	// where :1 may actually accept (then immediately reset).
	res := Run(context.Background(), cfg, Options{
		OllamaURL:  cfg.OllamaURL,
		HTTPClient: &http.Client{Timeout: 200 * time.Millisecond},
		Timeout:    200 * time.Millisecond,
	})
	got := checkByName(res, checkOllamaReachable)
	if got.Status != StatusFail {
		t.Errorf("ollama_reachable = %s, want fail (detail=%s)", got.Status, got.Detail)
	}
	// Model checks should be skipped because /api/tags could not
	// be enumerated.
	for _, name := range []string{checkRouterModel, checkLocalModel, checkEmbeddingModel} {
		got := checkByName(res, name)
		if got.Status != StatusSkip {
			t.Errorf("%s = %s, want skip when /api/tags unreachable", name, got.Status)
		}
	}
}

func TestRunFrontierKeyValid(t *testing.T) {
	ollama := newOllamaFixture(t)
	fr := newFrontierFixture(t, http.StatusOK)
	cfg := fixtureConfig(ollama.URL, fr.URL+"/chat/completions")
	cfg.FrontierKey = "sk-test-secret-do-not-log"

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkFrontierKey)
	if got.Status != StatusPass {
		t.Fatalf("frontier = %s (detail=%s), want pass", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, fr.URL) {
		t.Errorf("detail missing endpoint URL: %q", got.Detail)
	}
	if fr.calls.Load() == 0 {
		t.Error("frontier endpoint was never called")
	}
	// The fixture recorded the Authorization header; assert the
	// bearer scheme was used without logging the secret.
	bearer, _ := fr.bearer.Load().(string)
	if !strings.HasPrefix(bearer, "Bearer ") {
		t.Errorf("Authorization = %q, want Bearer prefix", bearer)
	}
}

func TestRunFrontierKeyInvalid(t *testing.T) {
	ollama := newOllamaFixture(t)
	fr := newFrontierFixture(t, http.StatusUnauthorized)
	cfg := fixtureConfig(ollama.URL, fr.URL+"/chat/completions")
	cfg.FrontierKey = "sk-bad-key"

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkFrontierKey)
	if got.Status != StatusFail {
		t.Fatalf("frontier = %s, want fail", got.Status)
	}
	if !strings.Contains(got.Detail, "401") {
		t.Errorf("detail missing 401: %q", got.Detail)
	}
}

func TestRunFrontierKeyForbidden(t *testing.T) {
	// 403 is the OpenAI "key lacks scope" response; should still
	// be reported as a key failure, not a transport error.
	ollama := newOllamaFixture(t)
	fr := newFrontierFixture(t, http.StatusForbidden)
	cfg := fixtureConfig(ollama.URL, fr.URL+"/chat/completions")
	cfg.FrontierKey = "sk-scoped-wrong"

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkFrontierKey)
	if got.Status != StatusFail {
		t.Fatalf("frontier = %s, want fail", got.Status)
	}
}

func TestRunFrontierKeySkippedWhenEmpty(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.FrontierKey = ""

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkFrontierKey)
	if got.Status != StatusSkip {
		t.Errorf("frontier = %s, want skip", got.Status)
	}
}

func TestRunFrontierURLError(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "://bad-url")
	cfg.FrontierKey = "sk-test"

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkFrontierKey)
	if got.Status != StatusFail {
		t.Errorf("frontier = %s, want fail", got.Status)
	}
}

func TestRunZAIKeyPresent(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.ZAIKey = "zai-test"

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkZAIKey)
	if got.Status != StatusPass {
		t.Errorf("zai = %s (detail=%s), want pass", got.Status, got.Detail)
	}
}

func TestRunRAGDirectoryMissing(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.ExamplesDir = filepath.Join(t.TempDir(), "does-not-exist")

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkRAGDirectory)
	if got.Status != StatusWarn {
		t.Errorf("rag_directory = %s, want warn", got.Status)
	}
}

func TestRunRAGDirectoryEmpty(t *testing.T) {
	ollama := newOllamaFixture(t)
	dir := t.TempDir()
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.ExamplesDir = dir

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkRAGDirectory)
	if got.Status != StatusWarn {
		t.Errorf("rag_directory = %s, want warn", got.Status)
	}
	if !strings.Contains(got.Detail, "empty") {
		t.Errorf("detail should mention empty: %q", got.Detail)
	}
}

func TestRunRAGDirectoryPopulated(t *testing.T) {
	ollama := newOllamaFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "example.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.ExamplesDir = dir

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkRAGDirectory)
	if got.Status != StatusPass {
		t.Errorf("rag_directory = %s (detail=%s), want pass", got.Status, got.Detail)
	}
}

func TestRunTelemetryWritable(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.TelemetryPath = filepath.Join(t.TempDir(), "telem.jsonl")

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkTelemetryPath)
	if got.Status != StatusPass {
		t.Errorf("telemetry_path = %s (detail=%s), want pass", got.Status, got.Detail)
	}
}

func TestRunTelemetryNotWritable(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	// /proc on Linux is read-only; fall back to a path inside a
	// read-only directory if /proc is unavailable on the test host.
	cfg.TelemetryPath = "/proc/cmdline-fake/telem.jsonl"

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkTelemetryPath)
	if got.Status != StatusFail {
		t.Errorf("telemetry_path = %s, want fail", got.Status)
	}
}

func TestRunTelemetryEmptyIsSkip(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.TelemetryPath = ""

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkTelemetryPath)
	if got.Status != StatusSkip {
		t.Errorf("telemetry_path = %s, want skip", got.Status)
	}
}

func TestRunMetricsEmptyIsSkip(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.MetricsDBPath = ""

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkMetricsDBPath)
	if got.Status != StatusSkip {
		t.Errorf("metrics_db = %s, want skip", got.Status)
	}
}

func TestRunJudgeDisabledIsSkip(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.JudgeSampleRate = 0

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkJudgeReadiness)
	if got.Status != StatusSkip {
		t.Errorf("judge = %s, want skip", got.Status)
	}
}

func TestRunJudgeEnabledMissingKeyIsFail(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.JudgeSampleRate = 0.1
	cfg.JudgeURL = "https://judge.example/v1/chat/completions"
	cfg.JudgeModel = "judge-model"
	cfg.JudgeAPIKey = ""
	cfg.JudgeEnabled = true // populated by config.Load; set explicitly in fixtures

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkJudgeReadiness)
	if got.Status != StatusFail {
		t.Errorf("judge = %s, want fail", got.Status)
	}
}

func TestRunJudgeReady(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.JudgeSampleRate = 0.1
	cfg.JudgeURL = "https://judge.example/v1/chat/completions"
	cfg.JudgeModel = "judge-model"
	cfg.JudgeAPIKey = "judge-key"
	cfg.JudgeEnabled = true

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkJudgeReadiness)
	if got.Status != StatusPass {
		t.Errorf("judge = %s (detail=%s), want pass", got.Status, got.Detail)
	}
}

func TestRunVRAMProbeFromOllamaPS(t *testing.T) {
	ollama := newOllamaFixture(t)
	ollama.tagsBody.Store(`{"models":[{"name":"qwen3-coder:8b"}]}`)
	ollama.psBody.Store(`{"models":[{"name":"qwen3-coder:8b","context_length":32768}]}`)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	// Force the probe to ignore sysfs by pointing at an empty root
	// so the budget is sourced purely from /api/ps.
	res := Run(context.Background(), cfg, Options{
		OllamaURL:  ollama.URL,
		HTTPClient: ollama.Client(),
		Timeout:    2 * time.Second,
		SysfsRoot:  t.TempDir(),
	})
	got := checkByName(res, checkVRAMProbe)
	if got.Status != StatusPass {
		t.Fatalf("vram = %s (detail=%s), want pass", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "ollama-ps") {
		t.Errorf("detail should report source ollama-ps: %q", got.Detail)
	}
}

func TestRunVRAMProbeDisabledWhenNoSignal(t *testing.T) {
	ollama := newOllamaFixture(t)
	ollama.tagsBody.Store(`{"models":[]}`)
	ollama.psBody.Store(`{"models":[]}`)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	res := Run(context.Background(), cfg, Options{
		OllamaURL:  ollama.URL,
		HTTPClient: ollama.Client(),
		Timeout:    2 * time.Second,
		SysfsRoot:  t.TempDir(),
	})
	got := checkByName(res, checkVRAMProbe)
	if got.Status != StatusWarn {
		t.Errorf("vram = %s, want warn when no signal", got.Status)
	}
}

func TestRunRespectsContextCancellation(t *testing.T) {
	// A pre-cancelled context must short-circuit Run() without
	// blocking on the http roundtrip.
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		_ = Run(ctx, cfg, withOptions(ollama.URL))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run blocked past the cancelled context deadline")
	}
}

func TestResultAggregates(t *testing.T) {
	r := Result{
		{Status: StatusPass},
		{Status: StatusFail},
		{Status: StatusWarn},
		{Status: StatusSkip},
		{Status: StatusFail},
	}
	if got := r.Failed(); got != 2 {
		t.Errorf("Failed = %d, want 2", got)
	}
	if got := r.Warned(); got != 1 {
		t.Errorf("Warned = %d, want 1", got)
	}
}

func TestCheckJSONShape(t *testing.T) {
	// Snapshot the wire shape so a downstream consumer (CI scripts
	// in particular) can rely on the field names.
	c := Check{Name: "x", Status: StatusPass, Detail: "y"}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"name":"x","status":"pass","detail":"y"}`; string(data) != want {
		t.Errorf("JSON = %s, want %s", string(data), want)
	}
}

// --- front-end helpers -----------------------------------------------------

func checkByName(r Result, name string) Check {
	for _, c := range r {
		if c.Name == name {
			return c
		}
	}
	return Check{Name: name, Status: StatusFail, Detail: "missing"}
}

// --- New diagnostic checks -------------------------------------------------

func TestRunRAGCircuitBreakerDisabled(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.RAGCircuitBreakerThreshold = 0

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkRAGCircuitBreaker)
	if got.Status != StatusWarn {
		t.Errorf("rag_circuit_breaker = %s, want warn", got.Status)
	}
	if !strings.Contains(got.Detail, "disabled") {
		t.Errorf("detail should mention disabled: %q", got.Detail)
	}
}

func TestRunRAGCircuitBreakerEnabled(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.RAGCircuitBreakerThreshold = 5

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkRAGCircuitBreaker)
	if got.Status != StatusPass {
		t.Errorf("rag_circuit_breaker = %s, want pass", got.Status)
	}
	if !strings.Contains(got.Detail, "5") {
		t.Errorf("detail should mention threshold: %q", got.Detail)
	}
}

func TestRunQualityVerifierDisabled(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.QualityConcurrency = 0

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkQualityVerifier)
	if got.Status != StatusWarn {
		t.Errorf("quality_verifier = %s, want warn", got.Status)
	}
	if !strings.Contains(got.Detail, "dormant") {
		t.Errorf("detail should mention dormant: %q", got.Detail)
	}
}

func TestRunQualityVerifierEnabled(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.QualityConcurrency = 4

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkQualityVerifier)
	if got.Status != StatusPass {
		t.Errorf("quality_verifier = %s, want pass", got.Status)
	}
	if !strings.Contains(got.Detail, "4") {
		t.Errorf("detail should mention concurrency: %q", got.Detail)
	}
}

func TestRunBudgetGuardDisabled(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.BudgetDailyLimit = 0

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkBudgetGuard)
	if got.Status != StatusWarn {
		t.Errorf("budget_guard = %s, want warn", got.Status)
	}
	if !strings.Contains(got.Detail, "disabled") {
		t.Errorf("detail should mention disabled: %q", got.Detail)
	}
}

func TestRunBudgetGuardEnabled(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.BudgetDailyLimit = 10.50

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkBudgetGuard)
	if got.Status != StatusPass {
		t.Errorf("budget_guard = %s, want pass", got.Status)
	}
	if !strings.Contains(got.Detail, "10.50") {
		t.Errorf("detail should mention limit: %q", got.Detail)
	}
}

func TestRunRateLimitProxyConfigRPMZeroIsSkip(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.RateLimitRPM = 0

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkRateLimitProxyConfig)
	if got.Status != StatusSkip {
		t.Errorf("rate_limit_proxy_config = %s, want skip", got.Status)
	}
}

func TestRunRateLimitProxyConfigRPMPositiveNoTrustedProxiesIsFail(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.RateLimitRPM = 100
	cfg.TrustedProxies = nil // no trusted proxies

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkRateLimitProxyConfig)
	if got.Status != StatusFail {
		t.Errorf("rate_limit_proxy_config = %s, want fail", got.Status)
	}
	if !strings.Contains(got.Detail, "spoofing") {
		t.Errorf("detail should mention spoofing vulnerability: %q", got.Detail)
	}
}

func TestRunRateLimitProxyConfigRPMPositiveWithTrustedProxiesIsPass(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.RateLimitRPM = 100
	// Add a trusted proxy
	cfg.TrustedProxies = make([]*net.IPNet, 1)
	_, cfg.TrustedProxies[0], _ = net.ParseCIDR("10.0.0.0/8")

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkRateLimitProxyConfig)
	if got.Status != StatusPass {
		t.Errorf("rate_limit_proxy_config = %s (detail=%s), want pass", got.Status, got.Detail)
	}
}

func TestRunProviderRegistryMalformedJSON(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	t.Setenv("NEXUS_FRONTIER_PROVIDERS", `{invalid json`)

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkProviderRegistry)
	if got.Status != StatusFail {
		t.Errorf("provider_registry = %s, want fail", got.Status)
	}
	if !strings.Contains(got.Detail, "malformed") {
		t.Errorf("detail should mention malformed: %q", got.Detail)
	}
}

func TestRunProviderRegistryValidJSON(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	t.Setenv("NEXUS_FRONTIER_PROVIDERS", `[{"name":"test","url":"https://test.com/v1","model":"test-model","costPer1K":0.01}]`)

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkProviderRegistry)
	if got.Status != StatusPass {
		t.Errorf("provider_registry = %s, want pass", got.Status)
	}
}

func TestRunMiddlewareChainUnknownMiddlewareIsFail(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.MiddlewareChain = "promptEngineering,unknownMiddleware,rag"

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkMiddlewareChain)
	if got.Status != StatusFail {
		t.Errorf("middleware_chain = %s, want fail", got.Status)
	}
	if !strings.Contains(got.Detail, "unknown") {
		t.Errorf("detail should mention unknown middleware: %q", got.Detail)
	}
}

func TestRunMiddlewareChainValidIsPass(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.MiddlewareChain = "promptEngineering,rag,compressJSONBlocks,appendSystemNote"

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkMiddlewareChain)
	if got.Status != StatusPass {
		t.Errorf("middleware_chain = %s (detail=%s), want pass", got.Status, got.Detail)
	}
}

func TestRunMiddlewareChainEmptyUsesDefault(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.MiddlewareChain = "" // empty should use default

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkMiddlewareChain)
	if got.Status != StatusPass {
		t.Errorf("middleware_chain = %s (detail=%s), want pass", got.Status, got.Detail)
	}
}

// --- models_endpoint tests -------------------------------------------------

func TestRunModelsEndpointDisabledIsSkip(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.ModelsEndpointEnabled = false

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkModelsEndpoint)
	if got.Status != StatusSkip {
		t.Errorf("models_endpoint = %s, want skip", got.Status)
	}
}

func TestRunModelsEndpointServerNotRunningIsSkip(t *testing.T) {
	ollama := newOllamaFixture(t)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.ModelsEndpointEnabled = true
	// Use a server that is not running (connection refused).
	// The addr format ":1" with no scheme will fail immediately.
	res := Run(context.Background(), cfg, Options{
		OllamaURL:  ollama.URL,
		HTTPClient: &http.Client{Timeout: 200 * time.Millisecond},
		Timeout:    200 * time.Millisecond,
	})
	got := checkByName(res, checkModelsEndpoint)
	if got.Status != StatusSkip {
		t.Errorf("models_endpoint = %s (detail=%s), want skip when server not running", got.Status, got.Detail)
	}
}

func TestRunModelsEndpointReturnsErrorIsFail(t *testing.T) {
	ollama := newOllamaFixture(t)
	nexus := newNexusFixture(t)
	nexus.status = http.StatusInternalServerError
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.ModelsEndpointEnabled = true
	cfg.Addr = strings.TrimPrefix(nexus.URL, "http://")

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkModelsEndpoint)
	if got.Status != StatusFail {
		t.Errorf("models_endpoint = %s (detail=%s), want fail when server returns error", got.Status, got.Detail)
	}
}

func TestRunModelsEndpointReturnsInvalidJSONIsFail(t *testing.T) {
	ollama := newOllamaFixture(t)
	nexus := newNexusFixture(t)
	nexus.modelsBody.Store("not json")
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.ModelsEndpointEnabled = true
	cfg.Addr = strings.TrimPrefix(nexus.URL, "http://")

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkModelsEndpoint)
	if got.Status != StatusFail {
		t.Errorf("models_endpoint = %s (detail=%s), want fail on invalid JSON", got.Status, got.Detail)
	}
}

func TestRunModelsEndpointMissingModelsIsFail(t *testing.T) {
	ollama := newOllamaFixture(t)
	nexus := newNexusFixture(t)
	nexus.modelsBody.Store(`{"object":"list","data":[
		{"id": "some-other-model", "object":"model","created":0,"owned_by":"ollama"}
	]}`)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.ModelsEndpointEnabled = true
	cfg.Addr = strings.TrimPrefix(nexus.URL, "http://")

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkModelsEndpoint)
	if got.Status != StatusFail {
		t.Errorf("models_endpoint = %s (detail=%s), want fail when models missing", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "qwen3-coder") {
		t.Errorf("detail should mention missing model: %s", got.Detail)
	}
}

func TestRunModelsEndpointAllModelsPresentIsPass(t *testing.T) {
	ollama := newOllamaFixture(t)
	nexus := newNexusFixture(t)
	nexus.modelsBody.Store(`{"object":"list","data":[
		{"id":"qwen3-coder:4b","object":"model","created":0,"owned_by":"ollama"},
		{"id":"qwen3-coder:8b","object":"model","created":0,"owned_by":"ollama"}
	]}`)
	cfg := fixtureConfig(ollama.URL, "https://api.openai.com/v1/chat/completions")
	cfg.ModelsEndpointEnabled = true
	cfg.Addr = strings.TrimPrefix(nexus.URL, "http://")

	res := Run(context.Background(), cfg, withOptions(ollama.URL))
	got := checkByName(res, checkModelsEndpoint)
	if got.Status != StatusPass {
		t.Errorf("models_endpoint = %s (detail=%s), want pass", got.Status, got.Detail)
	}
