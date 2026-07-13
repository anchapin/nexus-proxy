package providers

import (
	"reflect"
	"testing"
)

// withEnv sets every key/value pair in the table for the duration of
// the test, then unsets anything it set so sibling tests are
// insulated. We cannot use t.Setenv with a loop because t.Setenv does
// not accept a list of pairs; the inline loop is the simplest
// stdlib-only equivalent.
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadFromEnvUnset(t *testing.T) {
	// NEXUS_PROVIDERS unset → empty registry, no error. The
	// backward-compat fallback is the config loader's job.
	t.Setenv("NEXUS_PROVIDERS", "")
	reg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if reg.Len() != 0 {
		t.Errorf("Len = %d, want 0", reg.Len())
	}
	if reg.FrontierProviders() != nil {
		t.Errorf("FrontierProviders = %v, want nil for empty registry", reg.FrontierProviders())
	}
}

func TestLoadFromEnvSingleProvider(t *testing.T) {
	// Verify the smallest valid NEXUS_PROVIDERS surface: one
	// provider with only the required fields.
	withEnv(t, map[string]string{
		"NEXUS_PROVIDERS":               "openai",
		"NEXUS_PROVIDER_OPENAI_URL":     "https://api.openai.com/v1/chat/completions",
		"NEXUS_PROVIDER_OPENAI_MODEL":   "gpt-4o",
		"NEXUS_PROVIDER_OPENAI_API_KEY": "sk-test",
	})
	reg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("Len = %d, want 1", reg.Len())
	}
	got, ok := reg.Get("openai")
	if !ok {
		t.Fatalf("Get(openai): not found")
	}
	want := Provider{
		Name:   "openai",
		URL:    "https://api.openai.com/v1/chat/completions",
		Model:  "gpt-4o",
		APIKey: "sk-test",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Get(openai) = %+v, want %+v", got, want)
	}
}

func TestLoadFromEnvTable(t *testing.T) {
	// Three providers exercising every optional field. Casing must
	// be preserved on Name even though the env-var lookups
	// upper-case the suffix.
	cases := []struct {
		name string
		env  map[string]string
		want []Provider
	}{
		{
			name: "three providers with full metadata",
			env: map[string]string{
				"NEXUS_PROVIDERS":                            "openai,anthropic,gemini",
				"NEXUS_PROVIDER_OPENAI_URL":                  "https://api.openai.com/v1/chat/completions",
				"NEXUS_PROVIDER_OPENAI_MODEL":                "gpt-4o",
				"NEXUS_PROVIDER_OPENAI_API_KEY":              "sk-openai",
				"NEXUS_PROVIDER_OPENAI_INPUT_COST_PER_1K":    "0.005",
				"NEXUS_PROVIDER_OPENAI_OUTPUT_COST_PER_1K":   "0.015",
				"NEXUS_PROVIDER_OPENAI_MAX_TOKENS":           "128000",
				"NEXUS_PROVIDER_OPENAI_PRIORITY":             "0",
				"NEXUS_PROVIDER_ANTHROPIC_URL":               "https://api.anthropic.com/v1/chat/completions",
				"NEXUS_PROVIDER_ANTHROPIC_MODEL":             "claude-opus-4-5",
				"NEXUS_PROVIDER_ANTHROPIC_API_KEY":           "sk-anthropic",
				"NEXUS_PROVIDER_ANTHROPIC_INPUT_COST_PER_1K": "0.015",
				"NEXUS_PROVIDER_ANTHROPIC_PRIORITY":          "10",
				"NEXUS_PROVIDER_GEMINI_URL":                  "https://generativelanguage.googleapis.com/v1/chat/completions",
				"NEXUS_PROVIDER_GEMINI_MODEL":                "gemini-2.0-flash",
				"NEXUS_PROVIDER_GEMINI_API_KEY":              "gem-key",
				"NEXUS_PROVIDER_GEMINI_MAX_TOKENS":           "1000000",
			},
			want: []Provider{
				{
					Name:            "openai",
					URL:             "https://api.openai.com/v1/chat/completions",
					Model:           "gpt-4o",
					APIKey:          "sk-openai",
					Priority:        0,
					InputCostPer1K:  0.005,
					OutputCostPer1K: 0.015,
					MaxTokens:       128000,
				},
				{
					Name:           "anthropic",
					URL:            "https://api.anthropic.com/v1/chat/completions",
					Model:          "claude-opus-4-5",
					APIKey:         "sk-anthropic",
					Priority:       10,
					InputCostPer1K: 0.015,
				},
				{
					Name:      "gemini",
					URL:       "https://generativelanguage.googleapis.com/v1/chat/completions",
					Model:     "gemini-2.0-flash",
					APIKey:    "gem-key",
					MaxTokens: 1000000,
				},
			},
		},
		{
			name: "mixed case names preserved on Provider",
			env: map[string]string{
				"NEXUS_PROVIDERS":                  "Anthropic,Gemini",
				"NEXUS_PROVIDER_ANTHROPIC_URL":     "https://api.anthropic.com/v1/chat/completions",
				"NEXUS_PROVIDER_ANTHROPIC_MODEL":   "claude-opus-4-5",
				"NEXUS_PROVIDER_ANTHROPIC_API_KEY": "sk-a",
				"NEXUS_PROVIDER_GEMINI_URL":        "https://generativelanguage.googleapis.com/v1/chat/completions",
				"NEXUS_PROVIDER_GEMINI_MODEL":      "gemini-2.0-flash",
				"NEXUS_PROVIDER_GEMINI_API_KEY":    "gk",
			},
			want: []Provider{
				{
					Name:   "Anthropic",
					URL:    "https://api.anthropic.com/v1/chat/completions",
					Model:  "claude-opus-4-5",
					APIKey: "sk-a",
				},
				{
					Name:   "Gemini",
					URL:    "https://generativelanguage.googleapis.com/v1/chat/completions",
					Model:  "gemini-2.0-flash",
					APIKey: "gk",
				},
			},
		},
		{
			name: "empty api key preserved (provider is skipped from cascade but stays in registry)",
			env: map[string]string{
				"NEXUS_PROVIDERS":            "zai",
				"NEXUS_PROVIDER_ZAI_URL":     "https://api.z.ai/v1/chat/completions",
				"NEXUS_PROVIDER_ZAI_MODEL":   "glm-4.6",
				"NEXUS_PROVIDER_ZAI_API_KEY": "",
			},
			want: []Provider{
				{
					Name:   "zai",
					URL:    "https://api.z.ai/v1/chat/completions",
					Model:  "glm-4.6",
					APIKey: "",
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withEnv(t, tc.env)
			reg, err := LoadFromEnv()
			if err != nil {
				t.Fatalf("LoadFromEnv: %v", err)
			}
			if reg.Len() != len(tc.want) {
				t.Fatalf("Len = %d, want %d", reg.Len(), len(tc.want))
			}
			got := reg.FrontierProviders()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("FrontierProviders mismatch:\n got=%+v\nwant=%+v", got, tc.want)
			}
		})
	}
}

func TestLoadFromEnvErrors(t *testing.T) {
	// A declared provider missing the required URL or MODEL must
	// fail loud at boot, not silently dispatch to an empty endpoint
	// at request time.
	cases := []struct {
		name string
		env  map[string]string
		want string // substring of expected error
	}{
		{
			name: "missing URL",
			env: map[string]string{
				"NEXUS_PROVIDERS":             "openai",
				"NEXUS_PROVIDER_OPENAI_MODEL": "gpt-4o",
			},
			want: "URL is required",
		},
		{
			name: "missing MODEL",
			env: map[string]string{
				"NEXUS_PROVIDERS":           "openai",
				"NEXUS_PROVIDER_OPENAI_URL": "https://api.openai.com/v1/chat/completions",
			},
			want: "MODEL is required",
		},
		{
			name: "bad priority",
			env: map[string]string{
				"NEXUS_PROVIDERS":                "openai",
				"NEXUS_PROVIDER_OPENAI_URL":      "https://api.openai.com/v1/chat/completions",
				"NEXUS_PROVIDER_OPENAI_MODEL":    "gpt-4o",
				"NEXUS_PROVIDER_OPENAI_PRIORITY": "first",
			},
			want: "PRIORITY must be an integer",
		},
		{
			name: "bad input cost",
			env: map[string]string{
				"NEXUS_PROVIDERS":                         "openai",
				"NEXUS_PROVIDER_OPENAI_URL":               "https://api.openai.com/v1/chat/completions",
				"NEXUS_PROVIDER_OPENAI_MODEL":             "gpt-4o",
				"NEXUS_PROVIDER_OPENAI_INPUT_COST_PER_1K": "cheap",
			},
			want: "INPUT_COST_PER_1K must be a number",
		},
		{
			name: "bad max tokens",
			env: map[string]string{
				"NEXUS_PROVIDERS":                  "openai",
				"NEXUS_PROVIDER_OPENAI_URL":        "https://api.openai.com/v1/chat/completions",
				"NEXUS_PROVIDER_OPENAI_MODEL":      "gpt-4o",
				"NEXUS_PROVIDER_OPENAI_MAX_TOKENS": "lots",
			},
			want: "MAX_TOKENS must be an integer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withEnv(t, tc.env)
			_, err := LoadFromEnv()
			if err == nil {
				t.Fatalf("LoadFromEnv returned nil error")
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestRegistryAccessors(t *testing.T) {
	reg := NewRegistry([]Provider{
		{Name: "openai", Model: "gpt-4o", Priority: 10},
		{Name: "anthropic", Model: "claude-opus-4-5", Priority: 0},
		{Name: "gemini", Model: "gemini-2.0-flash", Priority: 5},
	})

	// FrontierProviders preserves declaration order (the registry
	// does not auto-sort — SortByPriority is the caller's escape
	// hatch).
	got := reg.FrontierProviders()
	wantNames := []string{"openai", "anthropic", "gemini"}
	for i, n := range wantNames {
		if got[i].Name != n {
			t.Errorf("FrontierProviders[%d].Name = %q, want %q", i, got[i].Name, n)
		}
	}

	// Get round-trips by Name.
	if _, ok := reg.Get("anthropic"); !ok {
		t.Error("Get(anthropic): not found")
	}
	if _, ok := reg.Get("missing"); ok {
		t.Error("Get(missing): unexpectedly found")
	}

	// ByModel round-trips by Model.
	if _, ok := reg.ByModel("claude-opus-4-5"); !ok {
		t.Error("ByModel(claude-opus-4-5): not found")
	}
	if _, ok := reg.ByModel("missing-model"); ok {
		t.Error("ByModel(missing-model): unexpectedly found")
	}
	if _, ok := reg.ByModel(""); ok {
		t.Error("ByModel(empty): unexpectedly found")
	}

	// FrontierProviders returns a fresh slice on every call so
	// callers can mutate without poisoning the registry.
	first := reg.FrontierProviders()
	first[0].Name = "mutated"
	again := reg.FrontierProviders()
	if again[0].Name != "openai" {
		t.Errorf("registry mutated through FrontierProviders(): %q", again[0].Name)
	}
}

func TestNewRegistryDedup(t *testing.T) {
	// NewRegistry de-duplicates by Name, keeping the first
	// occurrence, so an operator who accidentally lists the same
	// name twice (perhaps via an include-and-override pattern)
	// doesn't get a phantom second cascade step.
	reg := NewRegistry([]Provider{
		{Name: "openai", Model: "gpt-4o", APIKey: "first"},
		{Name: "openai", Model: "gpt-4o-mini", APIKey: "second"},
		{Name: "anthropic", Model: "claude-opus-4-5"},
	})
	if reg.Len() != 2 {
		t.Fatalf("Len = %d, want 2", reg.Len())
	}
	got, _ := reg.Get("openai")
	if got.APIKey != "first" {
		t.Errorf("Get(openai).APIKey = %q, want %q", got.APIKey, "first")
	}
}

func TestNewRegistryNilSafe(t *testing.T) {
	// Passing a nil slice should produce an empty registry without
	// panicking. We do this rather than panicking because the
	// fallback path in config.Load builds a Registry from a
	// possibly-nil slice.
	reg := NewRegistry(nil)
	if reg.Len() != 0 {
		t.Errorf("Len = %d, want 0", reg.Len())
	}
	if reg.FrontierProviders() != nil {
		t.Errorf("FrontierProviders = %v, want nil", reg.FrontierProviders())
	}
}

func TestSortByPriority(t *testing.T) {
	// Stable sort by Priority; ties keep declaration order. Used
	// by callers that want priority-overrides without changing
	// NEXUS_PROVIDERS ordering semantics.
	in := []Provider{
		{Name: "openai", Priority: 10},
		{Name: "anthropic", Priority: 0},
		{Name: "gemini", Priority: 5},
		{Name: "zai", Priority: 5}, // tie with gemini
	}
	got := SortByPriority(in)
	want := []string{"anthropic", "gemini", "zai", "openai"}
	for i, n := range want {
		if got[i].Name != n {
			t.Errorf("SortByPriority[%d].Name = %q, want %q", i, got[i].Name, n)
		}
	}

	// SortByPriority does not mutate its input.
	if in[0].Name != "openai" {
		t.Errorf("SortByPriority mutated input: %q", in[0].Name)
	}
}

func TestFrontierProvidersEmpty(t *testing.T) {
	// FrontierProviders on an empty registry returns nil, not an
	// empty slice. Tests and callers can rely on `len(x) == 0`
	// without having to also check for a non-nil-but-empty slice.
	reg := NewRegistry([]Provider{})
	if got := reg.FrontierProviders(); got != nil {
		t.Errorf("FrontierProviders = %v, want nil", got)
	}
}

// contains is a tiny strings.Contains shim so the test file stays
// stdlib-only without dragging strings into every error-message
// assertion.
func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
