package config

import (
	"testing"
)

func TestLoadModelAliases(t *testing.T) {
	t.Setenv("NEXUS_MODEL_ALIASES", `{"gpt-4":"anthropic/claude-3-5-sonnet","gpt-3.5-turbo":"local/qwen3-coder"}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ModelAliases) != 2 {
		t.Fatalf("ModelAliases len = %d, want 2", len(cfg.ModelAliases))
	}
	if v := cfg.ModelAliases["gpt-4"]; v != "anthropic/claude-3-5-sonnet" {
		t.Errorf("ModelAliases[gpt-4] = %q", v)
	}
	if v := cfg.ModelAliases["gpt-3.5-turbo"]; v != "local/qwen3-coder" {
		t.Errorf("ModelAliases[gpt-3.5-turbo] = %q", v)
	}
	if cfg.ModelAliasesStrict {
		t.Error("ModelAliasesStrict = true, want false (default)")
	}
}

func TestLoadModelAliasesStrict(t *testing.T) {
	t.Setenv("NEXUS_MODEL_ALIASES_STRICT", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.ModelAliasesStrict {
		t.Error("ModelAliasesStrict = false, want true")
	}
}

func TestLoadModelAliasesEmpty(t *testing.T) {
	t.Setenv("NEXUS_MODEL_ALIASES", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ModelAliases != nil {
		t.Errorf("ModelAliases = %v, want nil", cfg.ModelAliases)
	}
}

func TestLoadModelAliasesInvalidJSON(t *testing.T) {
	t.Setenv("NEXUS_MODEL_ALIASES", `{invalid json}`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}
