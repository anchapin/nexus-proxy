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
	// parser. See AGENTS.md "Adding new env vars".
	srcFiles := []string{
		filepath.Join(repoRoot, "internal", "config", "config.go"),
		filepath.Join(repoRoot, "internal", "config", "yaml.go"),
		filepath.Join(repoRoot, "internal", "providers", "providers.go"),
		filepath.Join(repoRoot, "internal", "providers", "registry.go"),
		filepath.Join(repoRoot, "internal", "providers", "frontier.go"),
		filepath.Join(repoRoot, "internal", "quality", "quality.go"),
		filepath.Join(repoRoot, "internal", "tracing", "tracing.go"),
		filepath.Join(repoRoot, "internal", "tracing", "exporter.go"),
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
