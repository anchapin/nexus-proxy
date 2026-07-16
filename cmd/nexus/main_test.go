package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/probe"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/telemetry"
)

// TestBuildRecorder verifies the telemetry recorder constructor returns a
// Noop recorder when disabled and a JSONL recorder when enabled.
func TestBuildRecorder(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected bool // true if should be Noop
	}{
		{"Disabled", "", true},
		{"Enabled", "/tmp/nexus-telemetry.jsonl", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NEXUS_TELEMETRY_PATH", tt.path)
			cfg, _ := config.Load()
			rec := buildRecorder(cfg)

			_, isNoop := rec.(telemetry.Noop)
			if isNoop != tt.expected {
				t.Errorf("buildRecorder(%q) isNoop = %v, want %v", tt.path, isNoop, tt.expected)
			}
		})
	}
}

// TestBuildMetrics verifies the metrics store constructor returns nil
// store/observer when disabled and a valid store when enabled.
func TestBuildMetrics(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected bool // true if should be nil
	}{
		{"Disabled", "", true},
		{"Enabled", "metrics.db", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NEXUS_METRICS_DB", tt.path)
			cfg, _ := config.Load()
			store, obs := buildMetrics(cfg)

			if (store == nil && obs == nil) != tt.expected {
				t.Errorf("buildMetrics(%q) returns nil = %v, want %v", tt.path, store == nil, tt.expected)
			}
		})
	}
}

// TestBudgetObserver verifies the budget observer returns safe defaults
// when the probe manager is nil.
func TestBudgetObserver(t *testing.T) {
	t.Run("NilManager", func(t *testing.T) {
		obs := budgetObserver(nil)
		if obs.BudgetTokens() != 0 {
			t.Errorf("expected 0 tokens for nil manager, got %d", obs.BudgetTokens())
		}
		if obs.BudgetSource() != string(probe.SourceStatic) {
			t.Errorf("expected static source for nil manager, got %q", obs.BudgetSource())
		}
	})
}

// TestHealthzHandler verifies the /healthz endpoint returns a 200 with the
// expected JSON structure when no hpoller or manager is provided.
func TestHealthzHandler(t *testing.T) {
	cfg := config.Config{
		TokenGuardrail: 6000,
	}

	handler := healthzHandler(nil, nil, cfg)
	req := httptest.NewRequest("GET", "/healthz", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rr.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}

	if resp["status"] != "ok" {
		t.Errorf("expected status ok, got %v", resp["status"])
	}
	if resp["budget_tokens"].(float64) != 6000 {
		t.Errorf("expected fallback budget 6000, got %v", resp["budget_tokens"])
	}
}

// TestBuildRAGStore verifies the RAG store constructor falls back to an
// in-memory store when persistence is disabled.
func TestBuildRAGStore(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(tmpDir+"/example1.txt", []byte("test example"), 0644); err != nil {
		t.Fatal(err)
	}

	emb := rag.NewOllamaEmbedder("http://localhost:11434", "nomic-embed-text", nil, rag.BreakerConfig{})
	ctx := context.Background()

	cfg := config.Config{
		ExamplesDir:  tmpDir,
		RAGThreshold: 0.55,
	}
	// RAGDBPath is empty by default via config.Load, so RAGPersistentEnabled() is false.
	// We explicitly set it to empty to ensure the in-memory fallback.
	cfg.RAGDBPath = ""

	store, ps, watcher := buildRAGStore(cfg, emb, ctx)
	if ps != nil || watcher != nil {
		t.Error("expected nil persistentStore and watcher for in-memory store")
	}
	if store == nil {
		t.Error("expected non-nil store")
	}
}

// TestPrintVersionOutput verifies the version printer includes the
// binary name and version string.
func TestPrintVersionOutput(t *testing.T) {
	saved := version
	t.Cleanup(func() { version = saved })
	version = "v9.9.9-test"

	var buf bytes.Buffer
	printVersion(&buf)
	out := buf.String()
	if !strings.Contains(out, "nexus") {
		t.Errorf("output %q does not contain 'nexus'", out)
	}
	if !strings.Contains(out, "v9.9.9-test") {
		t.Errorf("output %q does not contain the version string", out)
	}
}
