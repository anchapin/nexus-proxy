package providers

import (
	"testing"
)

func TestParseAliasValue(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantProv  string
		wantModel string
		wantErr   bool
	}{
		{"standard", "anthropic/claude-3-5-sonnet", "anthropic", "claude-3-5-sonnet", false},
		{"local provider", "local/qwen3-coder", "local", "qwen3-coder", false},
		{"model with slash", "openai/gpt-4o-mini", "openai", "gpt-4o-mini", false},
		{"whitespace trimmed", " anthropic / claude-3-5-sonnet ", "anthropic", "claude-3-5-sonnet", false},
		{"no slash", "anthropic", "", "", true},
		{"empty provider", "/model", "", "", true},
		{"empty model", "provider/", "", "", true},
		{"empty string", "", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prov, model, err := ParseAliasValue(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if prov != tt.wantProv {
				t.Errorf("provider = %q, want %q", prov, tt.wantProv)
			}
			if model != tt.wantModel {
				t.Errorf("model = %q, want %q", model, tt.wantModel)
			}
		})
	}
}

func TestResolveAlias(t *testing.T) {
	reg := NewProviderRegistry()
	reg.Register(ProviderConfig{
		NameVal:    "anthropic",
		BaseURLVal: "https://api.anthropic.com/v1",
		ModelVal:   "claude-3-5-sonnet",
		APIKeyVal:  "sk-ant-xxx",
	})
	reg.Register(ProviderConfig{
		NameVal:    "local",
		BaseURLVal: "http://localhost:11434",
		ModelVal:   "qwen3-coder",
	})

	aliases := map[string]string{
		"gpt-4":         "anthropic/claude-3-5-sonnet",
		"gpt-3.5-turbo": "local/qwen3-coder",
	}

	t.Run("exact match resolves", func(t *testing.T) {
		target, ok := ResolveAlias(aliases, reg, "gpt-4")
		if !ok {
			t.Fatal("expected resolution to succeed")
		}
		if target.ProviderName != "anthropic" {
			t.Errorf("ProviderName = %q, want %q", target.ProviderName, "anthropic")
		}
		if target.Model != "claude-3-5-sonnet" {
			t.Errorf("Model = %q, want %q", target.Model, "claude-3-5-sonnet")
		}
		if target.BaseURL != "https://api.anthropic.com/v1" {
			t.Errorf("BaseURL = %q, want %q", target.BaseURL, "https://api.anthropic.com/v1")
		}
		if target.APIKey != "sk-ant-xxx" {
			t.Errorf("APIKey = %q, want %q", target.APIKey, "sk-ant-xxx")
		}
	})

	t.Run("second alias resolves", func(t *testing.T) {
		target, ok := ResolveAlias(aliases, reg, "gpt-3.5-turbo")
		if !ok {
			t.Fatal("expected resolution to succeed")
		}
		if target.ProviderName != "local" {
			t.Errorf("ProviderName = %q, want %q", target.ProviderName, "local")
		}
		if target.Model != "qwen3-coder" {
			t.Errorf("Model = %q, want %q", target.Model, "qwen3-coder")
		}
	})

	t.Run("non-matching model returns false", func(t *testing.T) {
		_, ok := ResolveAlias(aliases, reg, "unknown-model")
		if ok {
			t.Fatal("expected false for unknown model")
		}
	})

	t.Run("empty aliases returns false", func(t *testing.T) {
		_, ok := ResolveAlias(nil, reg, "gpt-4")
		if ok {
			t.Fatal("expected false for nil aliases")
		}
	})

	t.Run("empty model returns false", func(t *testing.T) {
		_, ok := ResolveAlias(aliases, reg, "")
		if ok {
			t.Fatal("expected false for empty model")
		}
	})

	t.Run("nil registry returns false", func(t *testing.T) {
		_, ok := ResolveAlias(aliases, nil, "gpt-4")
		if ok {
			t.Fatal("expected false for nil registry")
		}
	})

	t.Run("provider not in registry returns false", func(t *testing.T) {
		badAliases := map[string]string{
			"gpt-4": "nonexistent/gpt-4o",
		}
		_, ok := ResolveAlias(badAliases, reg, "gpt-4")
		if ok {
			t.Fatal("expected false when provider not in registry")
		}
	})

	t.Run("malformed alias value returns false", func(t *testing.T) {
		badAliases := map[string]string{
			"gpt-4": "no-slash-here",
		}
		_, ok := ResolveAlias(badAliases, reg, "gpt-4")
		if ok {
			t.Fatal("expected false for malformed alias value")
		}
	})
}

func TestHasProviderModel(t *testing.T) {
	reg := NewProviderRegistry()
	reg.Register(ProviderConfig{
		NameVal:    "openai",
		BaseURLVal: "https://api.openai.com/v1",
		ModelVal:   "gpt-4o",
	})

	t.Run("exact model match", func(t *testing.T) {
		if !HasProviderModel(reg, "gpt-4o") {
			t.Error("expected true for gpt-4o")
		}
	})

	t.Run("non-matching model", func(t *testing.T) {
		if HasProviderModel(reg, "gpt-4") {
			t.Error("expected false for gpt-4")
		}
	})

	t.Run("nil registry", func(t *testing.T) {
		if HasProviderModel(nil, "gpt-4o") {
			t.Error("expected false for nil registry")
		}
	})

	t.Run("empty model", func(t *testing.T) {
		if HasProviderModel(reg, "") {
			t.Error("expected false for empty model")
		}
	})
}
