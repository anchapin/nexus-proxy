// Subcommand: `nexus init` (issue #1156). Launches an interactive
// config wizard that walks a new operator through initial setup,
// reducing time-to-first-request. The wizard:
//
//  1. Detects Ollama reachability via GET /api/tags.
//  2. Validates model availability and prints exact `ollama pull`
//     commands for missing defaults.
//  3. Optionally tests a frontier API key against /v1/models.
//  4. Picks a routing profile preset (local-first / frontier-default /
//     fusion-balanced) that bundles sane knob defaults.
//  5. Writes either a `.env` file or `config.yaml` (when
//     `NEXUS_CONFIG_FILE` is set) and prints the verification command.
//
// `nexus init --non-interactive` skips all prompts and writes a config
// from `NEXUS_INIT_PROFILE` (or defaults), enabling unattended installs.
//
// This file is the CLI adapter; the HTTP probe logic mirrors the
// approach in internal/diag (GET /api/tags, GET /v1/models) but is
// kept self-contained so the wizard does not pull in the full diag
// suite's dependencies.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// initUsage is shown on -h / bad flags.
const initUsage = `nexus init — interactive config wizard (issue #1156).

Usage:
  nexus init [--non-interactive] [--output <path>]

Walks a new operator through initial configuration, then writes a .env
file (or config.yaml when NEXUS_CONFIG_FILE is set) that is ready to
serve traffic. Reduces time-to-first-request by detecting Ollama,
validating the frontier key, and bundling routing-knob presets.

Flags:
  --non-interactive   Write config from NEXUS_INIT_PROFILE without
                      prompting. Use for unattended installs / CI.
  --output <path>     Override the output file path. Defaults to .env
                      (or the NEXUS_CONFIG_FILE value).

Env vars:
  NEXUS_INIT_PROFILE  Preset for non-interactive mode: "local-first",
                      "frontier-default", or "fusion-balanced".
  NEXUS_CONFIG_FILE   When set, the wizard writes config.yaml instead
                      of .env.
  NEXUS_OLLAMA_URL    Ollama base URL (default http://localhost:11434).

Examples:
  nexus init
  nexus init --non-interactive
  NEXUS_INIT_PROFILE=frontier-default nexus init --non-interactive
`

// Routing profiles offered by the wizard. Each preset bundles a set
// of routing-knob defaults so an operator does not have to learn the
// individual env vars before the first request.
const (
	profileLocalFirst      = "local-first"
	profileFrontierDefault = "frontier-default"
	profileFusionBalanced  = "fusion-balanced"
)

// defaultModels are the three Ollama models the proxy needs by default.
// The wizard checks each against GET /api/tags and prints an exact
// `ollama pull` command for any that are missing.
var defaultModels = []string{
	"qwen3-coder:4b",   // router SLM
	"qwen3-coder:8b",   // local route
	"nomic-embed-text", // RAG embeddings
}

// initAnswers holds the configuration collected during the wizard.
// Every field has a default so non-interactive mode can produce a
// valid config with zero prompts.
type initAnswers struct {
	OllamaURL      string
	FrontierURL    string
	FrontierModel  string
	FrontierKey    string
	LocalModel     string
	RouterModel    string
	EmbeddingModel string
	ProxyAPIKey    string
	Profile        string
}

// initWizard bundles the injectable dependencies so runInit can be
// unit-tested without spawning a subprocess or touching the real
// network. The zero value is NOT usable — always construct via
// newInitWizard.
type initWizard struct {
	stdout io.Writer
	stderr io.Writer
	in     *bufio.Reader
	client *http.Client
	// timeout bounds every HTTP probe so a hung endpoint cannot
	// stall the wizard.
	timeout time.Duration
}

// newInitWizard constructs a wizard wired to the given streams. When
// client is nil a 10s-timeout http.Client is used. The reader is
// bound to r so interactive tests can inject a strings.Reader.
func newInitWizard(stdout, stderr io.Writer, r io.Reader, client *http.Client, timeout time.Duration) *initWizard {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &initWizard{
		stdout:  stdout,
		stderr:  stderr,
		in:      bufio.NewReader(r),
		client:  client,
		timeout: timeout,
	}
}

// runInit is the testable core of the `nexus init` subcommand. args
// and the IO streams are parameters so a test can drive the full CLI
// end-to-end. Returns a process exit code (0 on success) rather than
// calling os.Exit.
func runInit(args []string, stdout, stderr io.Writer) int {
	return runInitWith(args, stdout, stderr, os.Stdin, nil, 0)
}

// runInitWith is runInit with injectable dependencies. stdin is the
// interactive input source; client is the HTTP client used for the
// Ollama and frontier probes; timeout bounds each probe.
func runInitWith(args []string, stdout, stderr io.Writer, stdin io.Reader, client *http.Client, timeout time.Duration) int {
	fs := flag.NewFlagSet("nexus init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, initUsage) }

	var nonInteractive bool
	var output string
	fs.BoolVar(&nonInteractive, "non-interactive", false, "write config without prompting")
	fs.StringVar(&output, "output", "", "override the output file path")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	w := newInitWizard(stdout, stderr, stdin, client, timeout)

	// Seed answers from the environment so the wizard honours values
	// an operator has already exported (e.g. NEXUS_OLLAMA_URL).
	ans := defaultAnswers()

	if nonInteractive {
		return w.runNonInteractive(ans, output)
	}
	return w.runInteractive(ans, output)
}

// defaultAnswers seeds the wizard with env-resolved defaults. Every
// field is populated so non-interactive mode produces a valid config
// even when no env var is set.
func defaultAnswers() initAnswers {
	return initAnswers{
		OllamaURL:      envOrDefault("NEXUS_OLLAMA_URL", "http://localhost:11434"),
		FrontierURL:    envOrDefault("NEXUS_FRONTIER_URL", "https://api.openai.com/v1/chat/completions"),
		FrontierModel:  envOrDefault("NEXUS_FRONTIER_MODEL", "gpt-4o"),
		FrontierKey:    os.Getenv("NEXUS_FRONTIER_API_KEY"),
		LocalModel:     envOrDefault("NEXUS_LOCAL_MODEL", "qwen3-coder:8b"),
		RouterModel:    envOrDefault("NEXUS_ROUTER_MODEL", "qwen3-coder:4b"),
		EmbeddingModel: envOrDefault("NEXUS_EMBEDDING_MODEL", "nomic-embed-text"),
		ProxyAPIKey:    os.Getenv("NEXUS_PROXY_API_KEY"),
		Profile:        envOrDefault("NEXUS_INIT_PROFILE", profileFusionBalanced),
	}
}

// envOrDefault returns os.Getenv(key) when set and non-empty, else def.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// runNonInteractive writes a config from the seeded answers + the
// NEXUS_INIT_PROFILE preset without issuing any prompts. It still
// probes Ollama and the frontier key so the operator gets actionable
// feedback in the log, but a probe failure does not abort the write
// (non-interactive mode must produce a config regardless).
//
// When NEXUS_INIT_VERIFY_MODELS is true (the default), a final
// verification pass confirms all expected default models are present
// after the config is written. If any model is missing the process
// exits 1 with a [FAIL] line per missing model.
func (w *initWizard) runNonInteractive(ans initAnswers, output string) int {
	fmt.Fprintln(w.stdout, "nexus init — non-interactive mode")
	fmt.Fprintf(w.stdout, "profile: %s\n", ans.Profile)

	// Probe Ollama: report reachability + missing models but never abort.
	w.probeOllama(ans.OllamaURL, false)

	// Probe frontier key only when one is present.
	if ans.FrontierKey != "" {
		w.probeFrontierKey(ans.FrontierURL, ans.FrontierKey, false)
	} else {
		fmt.Fprintln(w.stdout, "[SKIP] no NEXUS_FRONTIER_API_KEY set — frontier traffic will 401 until set")
	}

	path, err := w.resolveOutputPath(output)
	if err != nil {
		fmt.Fprintf(w.stderr, "nexus init: %v\n", err)
		return 1
	}
	if err := w.writeConfig(path, ans); err != nil {
		fmt.Fprintf(w.stderr, "nexus init: write %s: %v\n", path, err)
		return 1
	}
	fmt.Fprintf(w.stdout, "\n✓ wrote %s\n", path)

	// Re-verify model availability after writing the config.
	if envOrDefault("NEXUS_INIT_VERIFY_MODELS", "true") == "true" {
		if !w.verifyModels(ans.OllamaURL) {
			return 1
		}
	}

	w.printVerifyHint(path)
	return 0
}

// runInteractive drives the full wizard: prompt for each field (with
// the seeded default as the parenthesised hint), probe Ollama and the
// frontier key, then write the config.
func (w *initWizard) runInteractive(ans initAnswers, output string) int {
	fmt.Fprintln(w.stdout, "Nexus Proxy — Interactive Config Wizard")
	fmt.Fprintln(w.stdout, "==========================================")
	fmt.Fprintln(w.stdout)

	// Step 1: Ollama URL + reachability.
	fmt.Fprintln(w.stdout, "Step 1: Ollama")
	fmt.Fprintln(w.stdout, "-----------")
	ans.OllamaURL = w.prompt("Ollama URL", ans.OllamaURL)
	reachable := w.probeOllama(ans.OllamaURL, true)
	if !reachable {
		fmt.Fprintln(w.stdout, "  (continuing — you can start Ollama later)")
	}
	fmt.Fprintln(w.stdout)

	// Step 2: Frontier provider.
	fmt.Fprintln(w.stdout, "Step 2: Frontier provider")
	fmt.Fprintln(w.stdout, "-------------------------")
	ans.FrontierURL = w.prompt("Frontier URL", ans.FrontierURL)
	ans.FrontierModel = w.prompt("Frontier model", ans.FrontierModel)
	ans.FrontierKey = w.promptSecret("Frontier API key (Enter to skip)")
	if ans.FrontierKey != "" {
		w.probeFrontierKey(ans.FrontierURL, ans.FrontierKey, true)
	} else {
		fmt.Fprintln(w.stdout, "  [SKIP] no key — frontier traffic will 401 until set")
	}
	fmt.Fprintln(w.stdout)

	// Step 3: Local models.
	fmt.Fprintln(w.stdout, "Step 3: Local models")
	fmt.Fprintln(w.stdout, "--------------------")
	ans.LocalModel = w.prompt("Local model", ans.LocalModel)
	ans.RouterModel = w.prompt("Router (SLM) model", ans.RouterModel)
	ans.EmbeddingModel = w.prompt("Embedding model", ans.EmbeddingModel)
	fmt.Fprintln(w.stdout)

	// Step 4: Routing profile.
	fmt.Fprintln(w.stdout, "Step 4: Routing profile")
	fmt.Fprintln(w.stdout, "-----------------------")
	ans.Profile = w.promptProfile(ans.Profile)
	fmt.Fprintln(w.stdout)

	// Step 5: Inbound auth (optional).
	fmt.Fprintln(w.stdout, "Step 5: Inbound auth")
	fmt.Fprintln(w.stdout, "--------------------")
	ans.ProxyAPIKey = w.promptSecret("Proxy API key (Enter to disable auth)")
	fmt.Fprintln(w.stdout)

	// Step 6: Write output.
	path, err := w.resolveOutputPath(output)
	if err != nil {
		fmt.Fprintf(w.stderr, "nexus init: %v\n", err)
		return 1
	}
	if err := w.writeConfig(path, ans); err != nil {
		fmt.Fprintf(w.stderr, "nexus init: write %s: %v\n", path, err)
		return 1
	}
	fmt.Fprintf(w.stdout, "✓ wrote %s\n", path)
	w.printVerifyHint(path)
	return 0
}

// prompt prints label + (default) and returns the trimmed line. An
// empty response yields the default.
func (w *initWizard) prompt(label, def string) string {
	fmt.Fprintf(w.stdout, "  %s [%s]: ", label, def)
	line, _ := w.in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// promptSecret reads a line without echoing the default in the
// bracketed hint (used for API keys). An empty response yields "".
func (w *initWizard) promptSecret(label string) string {
	fmt.Fprintf(w.stdout, "  %s: ", label)
	line, _ := w.in.ReadString('\n')
	return strings.TrimSpace(line)
}

// promptProfile offers the three presets and validates the choice.
func (w *initWizard) promptProfile(def string) string {
	fmt.Fprintf(w.stdout, "  Profiles:\n    %s — prefer local routing (lowest cost)\n    %s — prefer frontier (lowest latency variance)\n    %s — enable fusion panels (default)\n",
		profileLocalFirst, profileFrontierDefault, profileFusionBalanced)
	for {
		choice := w.prompt("Profile", def)
		switch choice {
		case profileLocalFirst, profileFrontierDefault, profileFusionBalanced:
			return choice
		default:
			fmt.Fprintf(w.stdout, "  unknown profile %q; choose one of %s, %s, %s\n", choice, profileLocalFirst, profileFrontierDefault, profileFusionBalanced)
		}
	}
}

// probeOllama hits GET <url>/api/tags. When interactive is true and
// the endpoint is reachable, it also reports missing default models
// with exact `ollama pull` commands. Returns whether the endpoint
// answered.
func (w *initWizard) probeOllama(url string, interactive bool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), w.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/api/tags", nil)
	if err != nil {
		fmt.Fprintf(w.stdout, "[FAIL] invalid Ollama URL %q: %v\n", url, err)
		return false
	}
	resp, err := w.client.Do(req)
	if err != nil {
		fmt.Fprintf(w.stdout, "[FAIL] cannot reach Ollama at %s: %v\n", url, err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		fmt.Fprintf(w.stdout, "[FAIL] Ollama returned status %d\n", resp.StatusCode)
		return false
	}
	fmt.Fprintf(w.stdout, "[PASS] Ollama reachable at %s\n", url)

	// List missing models — only in interactive mode (non-interactive
	// keeps the log concise; the operator runs `nexus check` for the
	// full report).
	if !interactive {
		return true
	}
	available, ok := parseTagsBody(resp.Body)
	if !ok {
		return true
	}
	missing := []string{}
	for _, m := range defaultModels {
		if !modelPresent(m, available) {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintln(w.stdout, "  Missing models — run:")
		for _, m := range missing {
			fmt.Fprintf(w.stdout, "    ollama pull %s\n", m)
		}
	} else {
		fmt.Fprintln(w.stdout, "  All default models present.")
	}
	return true
}

// verifyModels queries /api/tags and confirms all expected default
// models are present. Used after writeConfig in non-interactive mode
// to ensure the operator has actually pulled the required models before
// the proxy is started. Returns true when all models are present.
func (w *initWizard) verifyModels(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), w.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/api/tags", nil)
	if err != nil {
		fmt.Fprintf(w.stdout, "[FAIL] verify: invalid Ollama URL %q: %v\n", url, err)
		return false
	}
	resp, err := w.client.Do(req)
	if err != nil {
		fmt.Fprintf(w.stdout, "[FAIL] verify: cannot reach Ollama at %s: %v\n", url, err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		fmt.Fprintf(w.stdout, "[FAIL] verify: Ollama returned status %d\n", resp.StatusCode)
		return false
	}
	available, ok := parseTagsBody(resp.Body)
	if !ok {
		fmt.Fprintf(w.stdout, "[FAIL] verify: could not parse /api/tags response\n")
		return false
	}
	missing := []string{}
	for _, m := range defaultModels {
		if !modelPresent(m, available) {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		for _, m := range missing {
			fmt.Fprintf(w.stdout, "[FAIL] model %s still unavailable after pull\n", m)
		}
		return false
	}
	return true
}

// probeFrontierKey hits GET <base>/v1/models with the bearer key.
// Reports accept/reject. The probe never aborts the wizard.
func (w *initWizard) probeFrontierKey(frontierURL, key string, interactive bool) {
	base := frontierModelsURL(frontierURL)
	ctx, cancel := context.WithTimeout(context.Background(), w.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		fmt.Fprintf(w.stdout, "[FAIL] invalid frontier URL %q: %v\n", frontierURL, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := w.client.Do(req)
	if err != nil {
		fmt.Fprintf(w.stdout, "[FAIL] cannot reach frontier at %s: %v\n", base, err)
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		fmt.Fprintf(w.stdout, "[PASS] frontier key accepted (%s)\n", base)
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		fmt.Fprintf(w.stdout, "[FAIL] frontier key rejected (status %d)\n", resp.StatusCode)
	default:
		fmt.Fprintf(w.stdout, "[WARN] frontier returned unexpected status %d\n", resp.StatusCode)
	}
}

// frontierModelsURL strips the /chat/completions suffix so the probe
// can hit /v1/models. Falls back to the raw URL when no recognised
// suffix is present.
func frontierModelsURL(raw string) string {
	raw = strings.TrimSpace(raw)
	for _, suffix := range []string{"/chat/completions", "/completions"} {
		if strings.HasSuffix(raw, suffix) {
			return strings.TrimSuffix(raw, suffix) + "/models"
		}
	}
	if strings.HasSuffix(raw, "/models") {
		return raw
	}
	return strings.TrimRight(raw, "/") + "/models"
}

// parseTagsBody reads the /api/tags JSON and returns the model-name
// set plus an ok flag (false on any decode error).
func parseTagsBody(r io.Reader) (map[string]struct{}, bool) {
	body, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, false
	}
	var raw struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false
	}
	out := map[string]struct{}{}
	for _, m := range raw.Models {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			continue
		}
		out[name] = struct{}{}
		if i := strings.IndexByte(name, ':'); i > 0 {
			out[name[:i]] = struct{}{}
		}
	}
	return out, true
}

// modelPresent reports whether model (or its bare family name) is in
// the available set. Mirrors internal/diag.fetchAvailableModels tag
// stripping.
func modelPresent(model string, available map[string]struct{}) bool {
	if _, ok := available[model]; ok {
		return true
	}
	if i := strings.IndexByte(model, ':'); i > 0 {
		if _, ok := available[model[:i]]; ok {
			return true
		}
	}
	return false
}

// resolveOutputPath picks the destination file. Precedence:
//   - explicit --output flag
//   - NEXUS_CONFIG_FILE (writes config.yaml)
//   - default ./.env
func (w *initWizard) resolveOutputPath(output string) (string, error) {
	if output != "" {
		return output, nil
	}
	if cf := os.Getenv("NEXUS_CONFIG_FILE"); cf != "" {
		return cf, nil
	}
	return ".env", nil
}

// writeConfig dispatches to the .env or config.yaml writer based on
// the file extension / NEXUS_CONFIG_FILE hint.
func (w *initWizard) writeConfig(path string, ans initAnswers) error {
	if isYAMLPath(path) {
		return writeYAMLConfig(path, ans)
	}
	return writeEnvConfig(path, ans)
}

// isYAMLPath returns true for .yaml/.yml extensions.
func isYAMLPath(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml")
}

// printVerifyHint tells the operator how to validate the written file.
func (w *initWizard) printVerifyHint(path string) {
	if isYAMLPath(path) {
		fmt.Fprintf(w.stdout, "Verify with:  nexus config validate %s\n", path)
	} else {
		fmt.Fprintln(w.stdout, "Verify with:  nexus check")
	}
	fmt.Fprintln(w.stdout, "Start proxy: nexus")
}
