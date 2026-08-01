// Package providers provides a registry for frontier API providers.
// It allows dynamic registration of providers via environment variables
// without recompiling the binary.
package providers

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Provider describes a frontier API endpoint the proxy can route to.
// The interface is minimal so it can be satisfied by configuration-only
// values (no external network calls at registration time).
type ProviderV2 interface {
	// Name returns a short identifier, e.g. "openrouter", "azure", "z".
	// Names are unique within a registry.
	Name() string
	// BaseURL returns the upstream endpoint base URL, e.g.
	// "https://api.openai.com/v1".
	BaseURL() string
	// Model returns the OpenAI-compatible model name, e.g. "gpt-4o".
	Model() string
	// APIKey returns the bearer token. May be empty for local endpoints.
	APIKey() string
	// CostPer1KUSD returns the USD cost per 1k input tokens, used
	// by the router.ProviderSelector as a cost weight.
	//
	// Deprecated: this flat rate ignores the input/output split that
	// real frontier pricing exposes. Prefer InputCostPer1KUSD() /
	// OutputCostPer1KUSD() for cost estimation (issue #1183). The
	// selector still calls CostPer1KUSD() for backward compatibility.
	CostPer1KUSD() float64
	// InputCostPer1KUSD returns the USD cost per 1k input tokens for
	// the serving provider (issue #1183). Callers that want accurate
	// per-request cost should use this together with OutputCostPer1KUSD.
	InputCostPer1KUSD() float64
	// OutputCostPer1KUSD returns the USD cost per 1k output tokens for
	// the serving provider (issue #1183). Frontier models typically
	// charge 3-5x for output vs input, so combining both streams yields
	// a materially more accurate estimate than the flat CostPer1KUSD.
	OutputCostPer1KUSD() float64
}

// AuthProviderV2 is a ProviderV2 that carries an API key for transport-level
// authentication. It is satisfied by the same concrete types as ProviderV2;
// the distinction lets callers that only need read-only metadata (URL, model,
// cost) accept the broader ProviderV2 interface while call sites that need to
// construct HTTP requests can assert AuthProviderV2 to access the key.
type AuthProviderV2 interface {
	ProviderV2
	// APIKey returns the bearer token required by the upstream.
	APIKey() string
}

// ProviderConfig is a plain-data struct that satisfies ProviderV2.
// It is the concrete type stored in the registry and parsed from
// NEXUS_FRONTIER_PROVIDERS.
//
// Cost fields (issue #1183): InputCostPer1KVal / OutputCostPer1KVal
// carry the per-direction rates. CostPer1KVal is retained for the
// selector weight and as the legacy single-rate value; when an
// operator only sets "costPer1K" in JSON it is mirrored into
// InputCostPer1KVal so the split estimate degrades gracefully.
type ProviderConfig struct {
	NameVal            string
	BaseURLVal         string
	ModelVal           string
	APIKeyVal          string
	CostPer1KVal       float64 // flat rate — selector weight / legacy
	InputCostPer1KVal  float64 // USD per 1k input tokens (issue #1183)
	OutputCostPer1KVal float64 // USD per 1k output tokens (issue #1183)
}

func (p ProviderConfig) Name() string                { return p.NameVal }
func (p ProviderConfig) BaseURL() string             { return p.BaseURLVal }
func (p ProviderConfig) Model() string               { return p.ModelVal }
func (p ProviderConfig) APIKey() string              { return p.APIKeyVal }
func (p ProviderConfig) CostPer1KUSD() float64       { return p.CostPer1KVal }
func (p ProviderConfig) InputCostPer1KUSD() float64  { return p.InputCostPer1KVal }
func (p ProviderConfig) OutputCostPer1KUSD() float64 { return p.OutputCostPer1KVal }

// ProviderRegistry holds registered providers and allows lookup by name.
// It is safe for concurrent use.
type ProviderRegistry struct {
	mu           sync.RWMutex
	providers    map[string]ProviderV2
	orderedNames []string // preserves insertion order for iteration
}

// NewProviderRegistry constructs an empty registry.
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{
		providers: make(map[string]ProviderV2),
	}
}

// Register adds p to the registry. If a provider with the same name is
// already registered, Register panics — provider names must be unique.
func (r *ProviderRegistry) Register(p ProviderV2) {
	if p == nil {
		panic("providers.Register: nil ProviderV2")
	}
	name := p.Name()
	if name == "" {
		panic("providers.Register: ProviderV2 with empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[name]; exists {
		panic(fmt.Sprintf("providers.Register: provider %q already registered", name))
	}
	r.providers[name] = p
	r.orderedNames = append(r.orderedNames, name)
}

// ByName returns the provider with the given name, or nil if none exists.
func (r *ProviderRegistry) ByName(name string) ProviderV2 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[name]
}

// ByModel returns the provider whose Model() matches model, or nil if
// none exists. Used by the chat handler's cost estimator to look up the
// serving provider's per-direction rates by the upstream model name
// (issue #1183). When two providers share the same model the first one
// registered wins; operators are expected to keep model names unique.
func (r *ProviderRegistry) ByModel(model string) ProviderV2 {
	if model == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, name := range r.orderedNames {
		if p := r.providers[name]; p.Model() == model {
			return p
		}
	}
	return nil
}

// All returns a slice of all registered providers in insertion order.
func (r *ProviderRegistry) All() []ProviderV2 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ProviderV2, 0, len(r.providers))
	for _, name := range r.orderedNames {
		out = append(out, r.providers[name])
	}
	return out
}

// ProviderNames returns the names of all registered providers in insertion order.
func (r *ProviderRegistry) ProviderNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.orderedNames))
	copy(out, r.orderedNames)
	return out
}

// Len returns the number of registered providers.
func (r *ProviderRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.providers)
}

// ParseProvidersFromEnv parses NEXUS_FRONTIER_PROVIDERS and registers
// each provider. The env value is a JSON array of objects:
//
//	NEXUS_FRONTIER_PROVIDERS='[{"name":"openrouter","url":"https://openrouter.ai/v1","model":"google/gemini-pro","apiKey":"sk-...","costPer1K":0.01}]'
//
// Fields:
//   - name: (required) unique provider identifier
//   - url: (required) base URL e.g. "https://api.openai.com/v1"
//   - model: (required) OpenAI-compatible model name
//   - apiKey: (optional) bearer token; empty strings are registered as-is
//   - costPer1K: (optional) flat USD cost per 1k tokens; legacy selector
//     weight. When set and inputCostPer1K is absent it is also used as
//     the input rate so the split estimate degrades gracefully.
//   - inputCostPer1K: (optional, issue #1183) USD cost per 1k input tokens
//   - outputCostPer1K: (optional, issue #1183) USD cost per 1k output tokens
//
// When the env var is empty, ParseProvidersFromEnv returns a nil registry
// and nil error (no providers registered).
func ParseProvidersFromEnv() (*ProviderRegistry, error) {
	raw := os.Getenv("NEXUS_FRONTIER_PROVIDERS")
	if raw == "" {
		return nil, nil
	}
	var configs []struct {
		Name            string  `json:"name"`
		URL             string  `json:"url"`
		Model           string  `json:"model"`
		APIKey          string  `json:"apiKey"`
		CostPer1K       float64 `json:"costPer1K"`
		InputCostPer1K  float64 `json:"inputCostPer1K"`
		OutputCostPer1K float64 `json:"outputCostPer1K"`
	}
	if err := json.Unmarshal([]byte(raw), &configs); err != nil {
		return nil, fmt.Errorf("NEXUS_FRONTIER_PROVIDERS: parse JSON: %w", err)
	}
	if len(configs) == 0 {
		return nil, nil
	}
	reg := NewProviderRegistry()
	for _, c := range configs {
		c := c // capture loop variable
		if c.Name == "" {
			return nil, fmt.Errorf("NEXUS_FRONTIER_PROVIDERS: entry missing required field 'name'")
		}
		if c.URL == "" {
			return nil, fmt.Errorf("NEXUS_FRONTIER_PROVIDERS: entry %q missing required field 'url'", c.Name)
		}
		if c.Model == "" {
			return nil, fmt.Errorf("NEXUS_FRONTIER_PROVIDERS: entry %q missing required field 'model'", c.Name)
		}
		// Legacy "costPer1K" is the flat selector weight. When an
		// operator does not set the per-direction "inputCostPer1K",
		// fall back to the flat rate so the split estimator still has
		// a non-zero input rate (issue #1183).
		inputRate := c.InputCostPer1K
		if inputRate == 0 {
			inputRate = c.CostPer1K
		}
		reg.Register(ProviderConfig{
			NameVal:            c.Name,
			BaseURLVal:         strings.TrimRight(c.URL, "/"),
			ModelVal:           c.Model,
			APIKeyVal:          c.APIKey,
			CostPer1KVal:       c.CostPer1K,
			InputCostPer1KVal:  inputRate,
			OutputCostPer1KVal: c.OutputCostPer1K,
		})
	}
	return reg, nil
}
