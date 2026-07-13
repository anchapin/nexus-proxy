// Package diag runs boot-time diagnostic checks for the Nexus Proxy
// (issue #32). The diagnostic surfaces the same signals the chat
// handler's hot path depends on — Ollama reachability, model
// availability, frontier key validity, VRAM probe budget, RAG
// directory state, on-disk path writability — so a new operator can
// verify their configuration with `nexus check` instead of waiting
// for the first proxied request to 401 or time out.
//
// The package is stdlib-only: every check uses net/http + os.Stat +
// probe.NewOllamaProbe, all of which are already on the proxy's
// dependency list. No new modules are introduced.
//
// Run takes a config.Config and returns a Result (slice of Check).
// Each Check carries a Name, a Status (pass/fail/warn/skip) and a
// human-readable Detail line. Run always returns every check — the
// caller decides how to render or whether to gate an exit code on
// the failed subset. This mirrors the way `internal/health` reports
// breaker state: collect everything, surface it all at once, let the
// operator decide.
package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/probe"
)

// Status is the outcome of a single diagnostic check. The values
// spell exactly as they appear in the human-readable output ("[PASS]",
// "[FAIL]") so a JSON consumer can match the rendered line by string
// comparison if needed.
type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	StatusWarn Status = "warn"
	StatusSkip Status = "skip"
)

// Check is a single diagnostic result. Detail is the operator-facing
// remediation hint (e.g. "Run: ollama pull qwen3-coder:8b") and is
// safe to print without further translation.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

// Result is the aggregated set of checks Run produced. Result
// satisfies sort.Interface (ordered by Name) so a UI can render the
// checks alphabetically; the run order is preserved in JSON output
// for diff-friendliness across runs.
type Result []Check

// Options tweaks the probe behaviour for diagnostic runs. The zero
// value uses sensible production defaults (5s HTTP timeouts, the
// real Ollama URL from cfg). Tests pass a stub HTTP client and a
// short timeout so a hung endpoint cannot stall the suite.
type Options struct {
	HTTPClient *http.Client  // nil falls back to http.DefaultClient
	OllamaURL  string        // overrides cfg.OllamaURL when non-empty
	SysfsRoot  string        // overrides probe default sysfs root
	Timeout    time.Duration // per-HTTP-call budget; 0 falls back to DefaultTimeout
}

// DefaultTimeout is the per-HTTP-call budget used when Options.Timeout
// is zero. Five seconds is long enough to ride out a slow Ollama
// /api/ps while still capping the whole Run() at ~10s (issue #32
// acceptance criteria).
const DefaultTimeout = 5 * time.Second

// Check names. Kept as constants so the JSON output and the
// human-readable table can never drift apart.
const (
	checkOllamaReachable = "ollama_reachable"
	checkRouterModel     = "ollama_router_model"
	checkLocalModel      = "ollama_local_model"
	checkEmbeddingModel  = "ollama_embedding_model"
	checkFrontierKey     = "frontier_api_key"
	checkZAIKey          = "zai_api_key"
	checkVRAMProbe       = "vram_probe"
	checkRAGDirectory    = "rag_directory"
	checkTelemetryPath   = "telemetry_path_writable"
	checkMetricsDBPath   = "metrics_db_writable"
	checkJudgeReadiness  = "judge_readiness"
)

// Run executes every diagnostic check against cfg and returns the
// aggregated result. Checks run sequentially; each one is bounded by
// Options.Timeout (DefaultTimeout when zero) so a single hung
// endpoint cannot stall the whole command past ~10s.
//
// The returned Result contains every check regardless of pass/fail
// status. Callers inspect the Status field and aggregate their own
// exit code (the CLI layer returns 1 when any Status == StatusFail).
func Run(ctx context.Context, cfg config.Config, opts Options) Result {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.OllamaURL == "" {
		opts.OllamaURL = cfg.OllamaURL
	}
	r := Result{}
	r = append(r, checkOllamaReachableFn(ctx, opts))
	models, modelsOK := fetchAvailableModels(ctx, opts)
	r = append(r, modelCheck(checkRouterModel, cfg.RouterModel, models, modelsOK))
	r = append(r, modelCheck(checkLocalModel, cfg.LocalModel, models, modelsOK))
	r = append(r, checkEmbeddingModelFn(ctx, cfg.EmbeddingModel, models, modelsOK, opts))
	r = append(r, checkFrontierKeyFn(ctx, cfg, opts))
	r = append(r, checkZAIKeyFn(cfg))
	r = append(r, checkVRAMProbeFn(ctx, cfg, opts))
	r = append(r, checkRAGDirectoryFn(cfg))
	r = append(r, checkWritablePathFn(checkTelemetryPath, cfg.TelemetryPath))
	r = append(r, checkWritablePathFn(checkMetricsDBPath, cfg.MetricsDBPath))
	r = append(r, checkJudgeReadinessFn(cfg))
	return r
}

// Failed reports the number of checks with Status == StatusFail. The
// CLI uses this for the exit-code gate.
func (r Result) Failed() int {
	n := 0
	for _, c := range r {
		if c.Status == StatusFail {
			n++
		}
	}
	return n
}

// Warned reports the number of checks with Status == StatusWarn. The
// CLI footer uses this so operators see how many soft warnings their
// setup carries.
func (r Result) Warned() int {
	n := 0
	for _, c := range r {
		if c.Status == StatusWarn {
			n++
		}
	}
	return n
}

// --- Ollama reachability + model inventory ---------------------------------

// checkEmbeddingModelFn probes the configured embedding model with a
// trivial call to /api/embeddings to confirm end-to-end readiness.
// Unlike the chat/router model checks (which only verify the model is
// pulled), this check validates that the model can actually generate
// embeddings — catching corrupt or incompatible models that would
// otherwise silently fail at runtime (issue #199).
func checkEmbeddingModelFn(ctx context.Context, model string, available map[string]struct{}, inventoryOK bool, opts Options) Check {
	if model == "" {
		return Check{
			Name:   checkEmbeddingModel,
			Status: StatusFail,
			Detail: "no embedding model configured",
		}
	}
	if !inventoryOK {
		return Check{
			Name:   checkEmbeddingModel,
			Status: StatusSkip,
			Detail: fmt.Sprintf("could not list models — %q not verified", model),
		}
	}
	if _, ok := available[model]; !ok {
		return Check{
			Name:   checkEmbeddingModel,
			Status: StatusFail,
			Detail: fmt.Sprintf("model %q not pulled — Run: ollama pull %s", model, model),
		}
	}
	// Probe with a trivial prompt; a successful 200 with a non-empty
	// embedding vector confirms the model is functional.
	payload, _ := json.Marshal(map[string]string{"model": model, "prompt": "hello"})
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		opts.OllamaURL+"/api/embeddings", bytes.NewReader(payload))
	if err != nil {
		return Check{
			Name:   checkEmbeddingModel,
			Status: StatusFail,
			Detail: fmt.Sprintf("embedding model %q: bad request: %v", model, err),
		}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return Check{
			Name:   checkEmbeddingModel,
			Status: StatusFail,
			Detail: fmt.Sprintf("embedding model %q: cannot reach: %v", model, err),
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Check{
			Name:   checkEmbeddingModel,
			Status: StatusFail,
			Detail: fmt.Sprintf("embedding model %q returned status %d", model, resp.StatusCode),
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Check{
			Name:   checkEmbeddingModel,
			Status: StatusFail,
			Detail: fmt.Sprintf("embedding model %q: could not read response: %v", model, err),
		}
	}
	var raw struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Check{
			Name:   checkEmbeddingModel,
			Status: StatusFail,
			Detail: fmt.Sprintf("embedding model %q: decode error: %v", model, err),
		}
	}
	if len(raw.Embedding) == 0 {
		return Check{
			Name:   checkEmbeddingModel,
			Status: StatusFail,
			Detail: fmt.Sprintf("embedding model %q returned empty embedding vector", model),
		}
	}
	return Check{
		Name:   checkEmbeddingModel,
		Status: StatusPass,
		Detail: fmt.Sprintf("model %q functional (%d-dim vector)", model, len(raw.Embedding)),
	}
}

// checkOllamaReachableFn performs GET /api/tags and reports whether
// the endpoint answered. A 2xx response is pass; 5xx or any other
// transport error is fail; 4xx (auth layer in front of Ollama) is
// pass — same convention as internal/health. The /api/tags response
// body is cached in opts (via availableModels) so the per-model
// checks do not re-issue the request.
func checkOllamaReachableFn(ctx context.Context, opts Options) Check {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.OllamaURL+"/api/tags", nil)
	if err != nil {
		return Check{Name: checkOllamaReachable, Status: StatusFail, Detail: err.Error()}
	}
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return Check{
			Name:   checkOllamaReachable,
			Status: StatusFail,
			Detail: fmt.Sprintf("cannot reach %s: %v", opts.OllamaURL, err),
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return Check{
			Name:   checkOllamaReachable,
			Status: StatusFail,
			Detail: fmt.Sprintf("ollama returned status %d", resp.StatusCode),
		}
	}
	return Check{
		Name:   checkOllamaReachable,
		Status: StatusPass,
		Detail: fmt.Sprintf("reachable at %s", opts.OllamaURL),
	}
}

// fetchAvailableModels hits /api/tags and returns (set, ok). The
// bool is false when the request failed at the transport layer
// (Ollama down) or returned a non-200 status — in that case the
// caller should surface the failure as "skip" rather than "missing"
// so the operator gets one clear error per endpoint, not two.
func fetchAvailableModels(ctx context.Context, opts Options) (map[string]struct{}, bool) {
	out := map[string]struct{}{}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.OllamaURL+"/api/tags", nil)
	if err != nil {
		return out, false
	}
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return out, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, false
	}
	var raw struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return out, false
	}
	for _, m := range raw.Models {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			continue
		}
		out[name] = struct{}{}
		// Ollama sometimes returns the tag (qwen3-coder:8b) but the
		// operator may have only the bare family name on the wire.
		// Strip the tag variant so a check for "nomic-embed-text"
		// matches a server returning "nomic-embed-text:latest".
		if i := strings.IndexByte(name, ':'); i > 0 {
			out[name[:i]] = struct{}{}
		}
	}
	return out, true
}

// modelCheck reports whether the configured model name is present in
// the Ollama inventory. A missing model yields StatusFail with a
// remediation hint that names the exact `ollama pull <model>` command
// — copying that line into a terminal is the only step the operator
// has to take to recover. When the inventory call itself failed the
// reachable check has already surfaced the underlying transport
// problem; this check then reports StatusSkip to avoid double noise.
func modelCheck(name, model string, available map[string]struct{}, inventoryOK bool) Check {
	if model == "" {
		return Check{
			Name:   name,
			Status: StatusFail,
			Detail: "no model configured",
		}
	}
	if !inventoryOK {
		return Check{
			Name:   name,
			Status: StatusSkip,
			Detail: fmt.Sprintf("could not list models — %q not verified", model),
		}
	}
	if _, ok := available[model]; ok {
		return Check{
			Name:   name,
			Status: StatusPass,
			Detail: fmt.Sprintf("model %q available", model),
		}
	}
	return Check{
		Name:   name,
		Status: StatusFail,
		Detail: fmt.Sprintf("model %q not pulled — Run: ollama pull %s", model, model),
	}
}

// --- Frontier API key ------------------------------------------------------

// checkFrontierKeyFn validates the configured frontier API key by
// issuing a GET /v1/models with a Bearer header. A 200 means the
// key is accepted; 401/403 means the key is bad; any other status
// is reported as-is so the operator can investigate.
//
// When cfg.FrontierKey is empty we skip with a hint: the proxy still
// runs without a frontier key (local + fusion degrade), but any
// frontier-bound request will 401.
func checkFrontierKeyFn(ctx context.Context, cfg config.Config, opts Options) Check {
	if !cfg.FrontierEnabled() {
		return Check{
			Name:   checkFrontierKey,
			Status: StatusSkip,
			Detail: "no NEXUS_FRONTIER_API_KEY set — frontier routing will return 401",
		}
	}
	base, err := frontierBaseURL(cfg.FrontierURL)
	if err != nil {
		return Check{
			Name:   checkFrontierKey,
			Status: StatusFail,
			Detail: fmt.Sprintf("invalid NEXUS_FRONTIER_URL %q: %v", cfg.FrontierURL, err),
		}
	}
	target := strings.TrimRight(base, "/") + "/models"
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Check{
			Name:   checkFrontierKey,
			Status: StatusFail,
			Detail: err.Error(),
		}
	}
	req.Header.Set("Authorization", "Bearer "+cfg.FrontierKey)
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return Check{
			Name:   checkFrontierKey,
			Status: StatusFail,
			Detail: fmt.Sprintf("cannot reach %s: %v", target, err),
		}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return Check{
			Name:   checkFrontierKey,
			Status: StatusPass,
			Detail: fmt.Sprintf("key accepted (endpoint: %s)", cfg.FrontierURL),
		}
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return Check{
			Name:   checkFrontierKey,
			Status: StatusFail,
			Detail: fmt.Sprintf("endpoint rejected the key (status %d)", resp.StatusCode),
		}
	default:
		return Check{
			Name:   checkFrontierKey,
			Status: StatusFail,
			Detail: fmt.Sprintf("unexpected status %d from %s", resp.StatusCode, target),
		}
	}
}

// frontierBaseURL strips the per-endpoint suffix (/chat/completions
// is the only supported shape today) from cfg.FrontierURL so we can
// hit /v1/models for the key-validation probe. Falls back to the raw
// URL when no recognised suffix is present — in that case we issue
// the probe against the configured URL itself and let the server
// reject it.
func frontierBaseURL(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("empty url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(u.Path, "/chat/completions") {
		return raw, nil
	}
	u.Path = strings.TrimSuffix(u.Path, "/chat/completions")
	return u.String(), nil
}

// --- Z.ai fallback key -----------------------------------------------------

// checkZAIKeyFn reports whether the Z.ai cascade key is set. The
// Z.ai endpoint is optional: when the key is empty the cascade
// fallback is disabled, which is a legitimate configuration in
// pure-Ollama deployments. We surface the missing-key case as a
// warning rather than a failure so a stock `.env.example` boots
// without red noise.
func checkZAIKeyFn(cfg config.Config) Check {
	if cfg.ZAIKey == "" {
		return Check{
			Name:   checkZAIKey,
			Status: StatusWarn,
			Detail: "no NEXUS_ZAI_API_KEY set — cascade fallback to z.ai disabled",
		}
	}
	return Check{
		Name:   checkZAIKey,
		Status: StatusPass,
		Detail: fmt.Sprintf("key present (endpoint: %s)", cfg.ZAIURL),
	}
}

// --- VRAM probe ------------------------------------------------------------

// checkVRAMProbeFn drives the Ollama probe directly (no Manager —
// the diagnostic command is one-shot and should not spin up a
// goroutine) and reports the resulting budget. A non-zero budget is
// pass; zero budget (no signal) is warn — the handler falls back to
// the static NEXUS_TOKEN_GUARDRAIL in that case, which still serves
// traffic but loses the dynamic-aware behaviour the PRD promises.
func checkVRAMProbeFn(ctx context.Context, cfg config.Config, opts Options) Check {
	p := probe.NewOllamaProbe(opts.OllamaURL, opts.HTTPClient)
	p.BytesPerToken = cfg.ProbeBytesPerToken
	if opts.SysfsRoot != "" {
		p.SysfsRoot = opts.SysfsRoot
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	b, err := p.Budget(ctx)
	if err != nil {
		return Check{
			Name:   checkVRAMProbe,
			Status: StatusWarn,
			Detail: fmt.Sprintf("probe produced no budget: %v (falling back to NEXUS_TOKEN_GUARDRAIL=%d)", err, cfg.TokenGuardrail),
		}
	}
	if b.Disabled() {
		return Check{
			Name:   checkVRAMProbe,
			Status: StatusWarn,
			Detail: fmt.Sprintf("budget disabled (source=%s) — falling back to NEXUS_TOKEN_GUARDRAIL=%d", b.Source, cfg.TokenGuardrail),
		}
	}
	return Check{
		Name:   checkVRAMProbe,
		Status: StatusPass,
		Detail: fmt.Sprintf("budget: %d tokens (source: %s)", b.Tokens, b.Source),
	}
}

// --- RAG examples directory ------------------------------------------------

// checkRAGDirectoryFn inspects cfg.ExamplesDir. A missing directory
// is a warning (not a failure) because RAG silently no-ops on an
// empty store and the proxy still serves traffic; the operator
// simply will not get the few-shot boost.
func checkRAGDirectoryFn(cfg config.Config) Check {
	if cfg.ExamplesDir == "" {
		return Check{
			Name:   checkRAGDirectory,
			Status: StatusWarn,
			Detail: "no NEXUS_EXAMPLES_DIR configured",
		}
	}
	info, err := os.Stat(cfg.ExamplesDir)
	if err != nil {
		return Check{
			Name:   checkRAGDirectory,
			Status: StatusWarn,
			Detail: fmt.Sprintf("cannot stat %q: %v", cfg.ExamplesDir, err),
		}
	}
	if !info.IsDir() {
		return Check{
			Name:   checkRAGDirectory,
			Status: StatusWarn,
			Detail: fmt.Sprintf("%q is not a directory", cfg.ExamplesDir),
		}
	}
	entries, err := os.ReadDir(cfg.ExamplesDir)
	if err != nil {
		return Check{
			Name:   checkRAGDirectory,
			Status: StatusWarn,
			Detail: fmt.Sprintf("cannot read %q: %v", cfg.ExamplesDir, err),
		}
	}
	if len(entries) == 0 {
		return Check{
			Name:   checkRAGDirectory,
			Status: StatusWarn,
			Detail: fmt.Sprintf("%q is empty — RAG injection inactive", cfg.ExamplesDir),
		}
	}
	return Check{
		Name:   checkRAGDirectory,
		Status: StatusPass,
		Detail: fmt.Sprintf("%d file(s) in %q", len(entries), cfg.ExamplesDir),
	}
}

// --- Writable on-disk paths ------------------------------------------------

// checkWritablePathFn tries to os.CreateTemp in the parent directory
// of path. An empty path (operator opted out) is reported as skip
// — there is nothing to check.
func checkWritablePathFn(checkName, path string) Check {
	if path == "" {
		return Check{
			Name:   checkName,
			Status: StatusSkip,
			Detail: "path not configured",
		}
	}
	parent := filepath.Dir(path)
	// filepath.Dir of a bare filename (no slashes) returns "." —
	// the temp-file probe will use the cwd, which is what we want.
	f, err := os.CreateTemp(parent, "nexus-check-*")
	if err != nil {
		return Check{
			Name:   checkName,
			Status: StatusFail,
			Detail: fmt.Sprintf("cannot write to %q: %v", path, err),
		}
	}
	tmpName := f.Name()
	_ = f.Close()
	_ = os.Remove(tmpName)
	return Check{
		Name:   checkName,
		Status: StatusPass,
		Detail: fmt.Sprintf("writable (%s)", path),
	}
}

// --- Judge readiness -------------------------------------------------------

// checkJudgeReadinessFn inspects the judge configuration. The judge
// is dormant when sample rate is zero or required fields are empty;
// that is the default in `.env.example` and is reported as skip.
// A partially configured judge (URL set but API key missing) is
// reported as fail — the evaluator would panic on the first call.
func checkJudgeReadinessFn(cfg config.Config) Check {
	if !cfg.JudgeEnabled {
		return Check{
			Name:   checkJudgeReadiness,
			Status: StatusSkip,
			Detail: "judge disabled (NEXUS_JUDGE_SAMPLE_RATE <= 0)",
		}
	}
	if cfg.JudgeAPIKey == "" {
		return Check{
			Name:   checkJudgeReadiness,
			Status: StatusFail,
			Detail: "judge enabled but no API key configured — set NEXUS_JUDGE_API_KEY or NEXUS_FRONTIER_API_KEY",
		}
	}
	return Check{
		Name:   checkJudgeReadiness,
		Status: StatusPass,
		Detail: fmt.Sprintf("ready (model=%s, sample_rate=%.2f)", cfg.JudgeModel, cfg.JudgeSampleRate),
	}
}
