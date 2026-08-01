package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/config"
)

func TestModelsList_IncludesAliases(t *testing.T) {
	cfg := config.Config{
		LocalModel:            "qwen3-coder:8b",
		FrontierModel:         "gpt-4o",
		ModelsEndpointEnabled: true,
		ModelAliases: map[string]string{
			"gpt-4":         "anthropic/claude-3-5-sonnet",
			"gpt-3.5-turbo": "local/qwen3-coder",
		},
	}

	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	resp := mustDecode(t, rr.Body.String())
	ids := modelIDs(resp)

	for _, want := range []string{"gpt-4", "gpt-3.5-turbo"} {
		if !contains(ids, want) {
			t.Errorf("models list missing alias %q; got %v", want, ids)
		}
	}
}

func TestModelsList_AliasesHaveOwnedByAlias(t *testing.T) {
	cfg := config.Config{
		FrontierModel:         "gpt-4o",
		ModelsEndpointEnabled: true,
		ModelAliases: map[string]string{
			"gpt-4": "anthropic/claude-3-5-sonnet",
		},
	}

	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	resp := mustDecode(t, rr.Body.String())
	for _, m := range resp.Data {
		if m.ID == "gpt-4" {
			if m.OwnedBy != "alias" {
				t.Errorf("alias model owned_by = %q, want \"alias\"", m.OwnedBy)
			}
			return
		}
	}
	t.Error("alias model 'gpt-4' not found in models list")
}

func TestModelsSingle_AliasResolvable(t *testing.T) {
	cfg := config.Config{
		FrontierModel:         "gpt-4o",
		ModelsEndpointEnabled: true,
		ModelAliases: map[string]string{
			"gpt-4": "anthropic/claude-3-5-sonnet",
		},
	}

	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})
	req := httptest.NewRequest(http.MethodGet, "/v1/models/gpt-4", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestModelsList_NoAliasesWhenEmpty(t *testing.T) {
	cfg := config.Config{
		FrontierModel:         "gpt-4o",
		ModelsEndpointEnabled: true,
	}

	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	resp := mustDecode(t, rr.Body.String())
	for _, m := range resp.Data {
		if m.OwnedBy == "alias" {
			t.Errorf("unexpected alias model when no aliases configured: %+v", m)
		}
	}
}
