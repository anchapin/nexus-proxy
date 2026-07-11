package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestNexusYAMLExampleLoadsCleanly parses the committed nexus.yaml.example
// and asserts every knob that the example documents is actually known
// to configKeys. This guards against drift between the example file
// (the public surface) and the configKeys map (the private bridge).
//
// The example file lives at the repo root, two directories up from this
// test file (internal/config/...).
func TestNexusYAMLExampleLoadsCleanly(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("runtime.Caller unavailable")
	}
	examplePath := filepath.Join(filepath.Dir(thisFile), "..", "..", "nexus.yaml.example")
	if _, err := os.Stat(examplePath); err != nil {
		t.Skipf("nexus.yaml.example not found at %s: %v", examplePath, err)
	}

	parsed, err := LoadFile(examplePath)
	if err != nil {
		t.Fatalf("LoadFile(%s): %v", examplePath, err)
	}
	if len(parsed) == 0 {
		t.Fatalf("nexus.yaml.example parsed to an empty map — did the file get truncated?")
	}

	// Every parsed section.key must have a known mapping. Operators
	// see every typo immediately because the unknown key would be
	// silently dropped, so this test is the canonical guard.
	for k := range parsed {
		if _, ok := configKeys[k]; !ok {
			t.Errorf("nexus.yaml.example has unknown key %q — add it to configKeys (and the resolver that consumes it)", k)
		}
	}

	// Spot-check a few high-impact values to make sure the example
	// didn't accidentally flip a sensible default.
	wantPairs := map[string]string{
		"server.addr":             ":8000",
		"ollama.router_model":     "qwen3-coder:4b",
		"frontier.model":          "gpt-4o",
		"rag.threshold":           "0.55",
		"routing.token_guardrail": "6000",
		"routing.slm_timeout":     "8s",
	}
	for k, want := range wantPairs {
		if got := parsed[k]; got != want {
			t.Errorf("nexus.yaml.example[%s] = %q, want %q", k, got, want)
		}
	}
}
