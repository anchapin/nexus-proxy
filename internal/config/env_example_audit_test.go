package config

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestEnvExampleCoverage (issue #448) ensures every NEXUS_* env var parsed
// in config.go, yaml.go, and the providers package has a canonical entry
// in .env.example. It fails when a new env var is added to the code but
// not documented, catching drift before it reaches users.
//
// Issue #478 adds the reverse direction: any NEXUS_* entry in .env.example
// that the parser no longer references is reported as stale, so renamed or
// deleted vars cannot linger silently (operators copying the file would get
// a silent no-op). Both directions share the skip() filter so dynamic vars
// are exempted symmetrically.
func TestEnvExampleCoverage(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: cannot locate test file")
	}
	repoRoot := filepath.Join(filepath.Dir(here), "..", "..")

	// Scan these source files for NEXUS_* env var references. The set
	// covers the config parser (config.go, yaml.go) plus packages that
	// read NEXUS_* vars directly via os.Getenv (providers, quality) and
	// the tracing package (whose NEXUS_TRACING_ENDPOINT is referenced in
	// its package docs). Without these, the reverse-direction check
	// (issue #478) would false-positive on vars consumed outside the
	// parser. See AGENTS.md "Adding new env vars". cmd/nexus/init.go is
	// included so that init-specific vars (e.g. NEXUS_INIT_VERIFY_MODELS)
	// are covered by the audit (issue #1364).
	srcFiles := []string{
		filepath.Join(repoRoot, "internal", "config", "config.go"),
		filepath.Join(repoRoot, "internal", "config", "yaml.go"),
		filepath.Join(repoRoot, "internal", "providers", "providers.go"),
		filepath.Join(repoRoot, "internal", "providers", "registry.go"),
		filepath.Join(repoRoot, "internal", "providers", "frontier.go"),
		filepath.Join(repoRoot, "internal", "quality", "quality.go"),
		filepath.Join(repoRoot, "internal", "tracing", "tracing.go"),
		filepath.Join(repoRoot, "internal", "tracing", "exporter.go"),
		filepath.Join(repoRoot, "cmd", "nexus", "init.go"),
	}

	// Match NEXUS_ followed by at least two word chars (not ending with
	// _ to skip prefixes like NEXUS_PROVIDER_ from concatenation).
	nexusRe := regexp.MustCompile(`NEXUS_[A-Z][A-Z0-9_]{1,}`)
	envExamplePath := filepath.Join(repoRoot, ".env.example")

	exampleBytes, err := os.ReadFile(envExamplePath)
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}

	// Collect all uncommented NEXUS_* assignments from .env.example.
	// Lines starting with # are comments; lines like "NEXUS_FOO=" or
	// "NEXUS_FOO=value" are canonical entries.
	exampleVars := make(map[string]bool)
	for _, line := range strings.Split(string(exampleBytes), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || trimmed == "" {
			continue
		}
		for _, m := range nexusRe.FindAllString(trimmed, -1) {
			exampleVars[m] = true
		}
	}

	// Collect env var names from source code, excluding test files.
	codeVars := make(map[string]bool)
	for _, path := range srcFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range nexusRe.FindAllString(string(data), -1) {
			codeVars[m] = true
		}
	}

	// Skip vars that are constructed dynamically or are internal/test-only.
	skipPrefixes := []string{
		"NEXUS_PROVIDER_", // dynamic: NEXUS_PROVIDER_<NAME>_URL etc.
		"NEXUS_FRONTIER_", // legacy frontier vars already documented individually
		"NEXUS_ZAI_",      // legacy zai vars already documented individually
		"NEXUS_HTTP_",     // transport tuning vars (some dynamic)
	}
	// Exact-match skips for internal/test-only vars.
	skipExact := map[string]bool{
		"NEXUS_QUALITY_TEST_HOOK": true,
	}

	skip := func(v string) bool {
		if skipExact[v] {
			return true
		}
		for _, p := range skipPrefixes {
			if strings.HasPrefix(v, p) {
				return true
			}
		}
		return false
	}

	missing := []string{}
	for v := range codeVars {
		if skip(v) {
			continue
		}
		if !exampleVars[v] {
			missing = append(missing, v)
		}
	}

	if len(missing) > 0 {
		t.Errorf("NEXUS_* env vars in code but missing from .env.example: %s\n"+
			"Add canonical entries with defaults matching the parser (issue #448).",
			strings.Join(missing, ", "))
	}

	// Reverse direction (issue #478): flag .env.example entries that no
	// longer have a matching parser reference, so renamed/deleted vars do
	// not linger silently. Reuse skip() so dynamic-construction vars are
	// exempted in both directions.
	stale := []string{}
	for v := range exampleVars {
		if skip(v) {
			continue
		}
		if !codeVars[v] {
			stale = append(stale, v)
		}
	}

	if len(stale) > 0 {
		t.Errorf("NEXUS_* env vars in .env.example but no longer referenced by the config parser: %s\n"+
			"For each, either re-add the getEnv*(...) call in the parser, "+
			"or delete the line from .env.example (issue #478).",
			strings.Join(stale, ", "))
	}
}

// TestEnvExampleHotReloadAnnotations (issue #491) asserts that the env vars
// annotated `# hot-reloadable via SIGHUP` in .env.example exactly match the
// vars re-read by ReloadHotReloadable in config.go. The canonical set is
// derived directly from the source (the code region between the
// "// Hot-reloadable settings." marker and the function return), so renaming
// or removing a reloadable var without updating .env.example fails the test.
func TestEnvExampleHotReloadAnnotations(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: cannot locate test file")
	}
	repoRoot := filepath.Join(filepath.Dir(here), "..", "..")

	// Derive the canonical reloadable set from ReloadHotReloadable's body.
	// The hot-reloadable reads live between the "// Hot-reloadable settings."
	// comment and the "return next, result" statement.
	configPath := filepath.Join(repoRoot, "internal", "config", "config.go")
	configSrc, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	src := string(configSrc)
	const reloadMarker = "// Hot-reloadable settings."
	markIdx := strings.Index(src, reloadMarker)
	if markIdx < 0 {
		t.Fatal("cannot find '// Hot-reloadable settings.' marker in config.go")
	}
	const returnStmt = "\n\treturn next, result"
	retIdx := strings.Index(src[markIdx:], returnStmt)
	if retIdx < 0 {
		t.Fatal("cannot find 'return next, result' after hot-reloadable marker in config.go")
	}
	reloadRegion := src[markIdx : markIdx+retIdx]

	nexusRe := regexp.MustCompile(`NEXUS_[A-Z][A-Z0-9_]{1,}`)
	codeReloadable := make(map[string]bool)
	for _, m := range nexusRe.FindAllString(reloadRegion, -1) {
		codeReloadable[m] = true
	}

	// Collect annotated entries from .env.example: uncommented assignment lines
	// carrying the trailing "# hot-reloadable via SIGHUP" marker.
	envExamplePath := filepath.Join(repoRoot, ".env.example")
	exampleBytes, err := os.ReadFile(envExamplePath)
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	const annotation = "# hot-reloadable via SIGHUP"
	docReloadable := make(map[string]bool)
	for _, line := range strings.Split(string(exampleBytes), "\n") {
		if !strings.Contains(line, annotation) {
			continue
		}
		trimmed := strings.TrimSpace(line)
		// Skip comment-only lines (e.g. the header enumeration block).
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		for _, m := range nexusRe.FindAllString(trimmed, -1) {
			docReloadable[m] = true
		}
	}

	// Code → docs: every reloadable var must carry the annotation.
	var missing []string
	for v := range codeReloadable {
		if !docReloadable[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		t.Errorf("reloadable vars in ReloadHotReloadable lacking `# hot-reloadable via SIGHUP` in .env.example: %s\n"+
			"Add the trailing annotation (issue #491).", strings.Join(missing, ", "))
	}

	// Docs → code: no annotation should decorate a var that is not reloadable.
	var extra []string
	for v := range docReloadable {
		if !codeReloadable[v] {
			extra = append(extra, v)
		}
	}
	if len(extra) > 0 {
		t.Errorf("vars annotated `# hot-reloadable via SIGHUP` in .env.example but not re-read by ReloadHotReloadable: %s\n"+
			"Either add the var to ReloadHotReloadable or remove the annotation (issue #491).",
			strings.Join(extra, ", "))
	}
}
