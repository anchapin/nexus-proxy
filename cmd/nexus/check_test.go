package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/diag"
)

// runCheckTests drives the full check subcommand end-to-end. Each
// test wires its own httptest server for Ollama + frontier so the
// checks have predictable inputs; the runCheck helper under test
// reads cfg from env, so the tests set NEXUS_OLLAMA_URL etc. before
// invoking it.

func TestRunCheckHumanReadable(t *testing.T) {
	ollama := newCheckOllamaFixture(t)
	fr := newCheckFrontierFixture(t, http.StatusOK)

	t.Setenv("NEXUS_OLLAMA_URL", ollama.Server.URL)
	t.Setenv("NEXUS_FRONTIER_URL", fr.Server.URL+"/chat/completions")
	t.Setenv("NEXUS_FRONTIER_API_KEY", "sk-test")
	t.Setenv("NEXUS_ZAI_API_KEY", "")
	t.Setenv("NEXUS_TELEMETRY_PATH", filepath.Join(t.TempDir(), "telem.jsonl"))
	t.Setenv("NEXUS_METRICS_DB", "")
	t.Setenv("NEXUS_EXAMPLES_DIR", "./few_shot_examples")
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "0")
	// Clear legacy env vars that earlier tests may have set.
	for _, k := range []string{
		"NEXUS_ROUTING_CONFIDENCE_DB",
		"NEXUS_RAG_DB",
		"NEXUS_HEALTH_POLL_INTERVAL",
		"NEXUS_PROBE_INTERVAL",
	} {
		t.Setenv(k, "")
	}

	var stdout, stderr bytes.Buffer
	code := runCheck(nil, &stdout, &stderr)
	out := stdout.String()
	if code != 1 {
		t.Fatalf("runCheck = %d, want 1 (ollama missing models)", code)
	}
	if !strings.Contains(out, "Nexus Proxy — Configuration Check") {
		t.Errorf("missing banner; got:\n%s", out)
	}
	if !strings.Contains(out, "[FAIL]") {
		t.Errorf("missing fail row; got:\n%s", out)
	}
	if !strings.Contains(out, "ollama pull") {
		t.Errorf("missing ollama pull hint; got:\n%s", out)
	}
	if !strings.Contains(out, "[PASS] frontier_api_key") {
		t.Errorf("frontier_api_key should pass with a valid key; got:\n%s", out)
	}
}

func TestRunCheckJSON(t *testing.T) {
	ollama := newCheckOllamaFixture(t)
	t.Setenv("NEXUS_OLLAMA_URL", ollama.Server.URL)
	t.Setenv("NEXUS_FRONTIER_API_KEY", "")
	t.Setenv("NEXUS_TELEMETRY_PATH", "")
	t.Setenv("NEXUS_METRICS_DB", "")
	t.Setenv("NEXUS_EXAMPLES_DIR", "./few_shot_examples")
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "0")
	for _, k := range []string{
		"NEXUS_ROUTING_CONFIDENCE_DB",
		"NEXUS_RAG_DB",
		"NEXUS_HEALTH_POLL_INTERVAL",
		"NEXUS_PROBE_INTERVAL",
	} {
		t.Setenv(k, "")
	}

	var stdout, stderr bytes.Buffer
	code := runCheck([]string{"--json"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("runCheck --json = %d, want 1 (stderr=%s)", code, stderr.String())
	}
	var got []diag.Check
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\noutput:\n%s", err, stdout.String())
	}
	if len(got) == 0 {
		t.Fatal("expected at least one check in JSON output")
	}
	for _, c := range got {
		switch c.Status {
		case diag.StatusPass, diag.StatusFail, diag.StatusWarn, diag.StatusSkip:
			// ok
		default:
			t.Errorf("unknown status %q in row %+v", c.Status, c)
		}
	}
}

func TestRunCheckPassesWhenAllGreen(t *testing.T) {
	ollama := newCheckOllamaFixture(t)
	tags := `{"models":[{"name":"qwen3-coder:4b"},{"name":"qwen3-coder:8b"},{"name":"nomic-embed-text"}]}`
	ollama.tags = &tags
	fr := newCheckFrontierFixture(t, http.StatusOK)
	t.Setenv("NEXUS_OLLAMA_URL", ollama.Server.URL)
	t.Setenv("NEXUS_FRONTIER_URL", fr.Server.URL+"/chat/completions")
	t.Setenv("NEXUS_FRONTIER_API_KEY", "sk-test")
	t.Setenv("NEXUS_ZAI_API_KEY", "zai-test")
	t.Setenv("NEXUS_TELEMETRY_PATH", filepath.Join(t.TempDir(), "telem.jsonl"))
	t.Setenv("NEXUS_METRICS_DB", filepath.Join(t.TempDir(), "metrics.db"))
	t.Setenv("NEXUS_EXAMPLES_DIR", t.TempDir()) // empty -> warn, not fail
	t.Setenv("NEXUS_JUDGE_SAMPLE_RATE", "0")
	for _, k := range []string{
		"NEXUS_ROUTING_CONFIDENCE_DB",
		"NEXUS_RAG_DB",
		"NEXUS_HEALTH_POLL_INTERVAL",
		"NEXUS_PROBE_INTERVAL",
	} {
		t.Setenv(k, "")
	}

	var stdout, stderr bytes.Buffer
	code := runCheck(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runCheck = %d, want 0; stderr=%s\nstdout=%s",
			code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "All checks passed") {
		t.Errorf("missing 'All checks passed' footer; got:\n%s", stdout.String())
	}
}

func TestRunCheckUnknownFlagReturnsNonZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runCheck([]string{"--bogus"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("runCheck --bogus = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Errorf("stderr missing flag error: %q", stderr.String())
	}
}

func TestRunCheckHelpReturnsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runCheck([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runCheck --help = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("--help should print usage; got stderr=%q", stderr.String())
	}
}

// --- fixtures --------------------------------------------------------------

// checkOllamaFixture is the same minimal Ollama stub used in the
// diag-package tests but exposes mutable fields (no atomics) because
// runCheck is single-threaded and the convenience matters.
type checkOllamaFixture struct {
	*httptest.Server
	tags *string // raw JSON body for /api/tags
}

func newCheckOllamaFixture(t *testing.T) *checkOllamaFixture {
	t.Helper()
	f := &checkOllamaFixture{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			if f.tags != nil {
				_, _ = w.Write([]byte(*f.tags))
				return
			}
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/ps":
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/embeddings":
			// Return a valid embedding response so the embedding
			// model check passes without every test having to stub it.
			_, _ = w.Write([]byte(`{"embedding":[0.1,0.2,0.3]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

type checkFrontierFixture struct {
	*httptest.Server
	status int
}

func newCheckFrontierFixture(t *testing.T, status int) *checkFrontierFixture {
	t.Helper()
	f := &checkFrontierFixture{status: status}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(f.Close)
	return f
}
