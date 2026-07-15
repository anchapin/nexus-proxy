// Package providers provides a registry for multi-frontier provider routing
// (issue #223). Operators register providers via NEXUS_FRONTIERS env var
// and the chat handler looks up the appropriate provider by name at
// runtime, replacing the prior hardcoded frontier+z.ai-only path.
package providers

import (
	"fmt"
	"strconv"
	"strings"
)

// Provider is the interface all registered frontier providers must satisfy.
// The chat handler looks up providers by name when dispatching route=frontier
// and route=fusion requests.
type Provider interface {
	// Name returns the provider's unique identifier, e.g. "frontier",
	// "zai", "openrouter". Used as the lookup key and the cascade/fusion
	// step identifier in logs and telemetry.
	Name() string
	// BaseURL returns the upstream endpoint base, e.g.
	// "https://api.openai.com/v1/chat/completions". The chat handler
	// appends "/v1/chat/completions" when calling Ollama-backed local
	// routing; frontier providers are used as-is.
	BaseURL() string
	// Model returns the OpenAI-compatible model name, e.g. "gpt-4o".
	Model() string
	// APIKey returns the bearer token. Empty string means no auth
	// (used for the local Ollama endpoint which has no auth).
	APIKey() string
	// CostPer1K returns the USD cost per 1k input tokens, used as the
	// selector weight and for cost-avoidance tracking.
	CostPer1K() float64
}

// provider is the concrete implementation of Provider.
type provider struct {
	name      string
	baseURL   string
	model     string
	apiKey    string
	costPer1K float64
}

func (p *provider) Name() string   { return p.name }
func (p *provider) BaseURL() string { return p.baseURL }
func (p *provider) Model() string  { return p.model }
func (p *provider) APIKey() string { return p.apiKey }
func (p *provider) CostPer1K() float64 { return p.costPer1K }

// String returns a human-readable representation for debugging.
func (p *provider) String() string {
	return fmt.Sprintf("provider{name=%q, url=%q, model=%q, cost=%.6f}",
		p.name, p.baseURL, p.model, p.costPer1K)
}

// Registry holds the registered providers. It is safe for concurrent use.
type Registry struct {
	byName map[string]Provider
}

// NewRegistry constructs an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Provider)}
}

// Register adds p to the registry under p.Name(). Register panics if a
// provider with the same name is already registered — this catches config
// errors early at boot rather than returning a vague "provider not found"
// at request time. Callers that need idempotent registration (e.g. tests
// with overlapping setup) should check Get before Register.
func (r *Registry) Register(p Provider) {
	if p == nil {
		panic("providers.Registry: cannot register a nil Provider")
	}
	name := p.Name()
	if name == "" {
		panic("providers.Registry: cannot register a Provider with an empty Name()")
	}
	if _, exists := r.byName[name]; exists {
		panic(fmt.Sprintf("providers.Registry: provider %q already registered", name))
	}
	r.byName[name] = p
}

// Get returns the provider registered under name, or nil if none.
func (r *Registry) Get(name string) Provider {
	return r.byName[name]
}

// All returns a snapshot of all registered providers in registration order.
func (r *Registry) All() []Provider {
	out := make([]Provider, 0, len(r.byName))
	for _, p := range r.byName {
		out = append(out, p)
	}
	return out
}

// ParseProviders parses a NEXUS_FRONTIERS env value into a Registry.
// The format is one provider per line:
//
//	name|baseURL|model|apiKey|costPer1K
//
// Empty lines and lines starting with '#' are skipped. Lines with an
// insufficient number of fields are skipped and a warning is logged.
//
// Example NEXUS_FRONTIERS value:
//
//	frontier|https://api.openai.com/v1/chat/completions|gpt-4o|sk-xxx|0.005
//	zai|https://api.z.ai/v1/chat/completions|glm-4.6|zsk-xxx|0.002
//
// The apiKey field may be empty (providers without auth).
func ParseProviders(raw string) (*Registry, error) {
	reg := NewRegistry()
	if raw == "" {
		return reg, nil
	}
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 4 {
			return nil, fmt.Errorf("providers: line %d: expected 4-5 fields (name|baseURL|model|apiKey|costPer1K), got %d", i+1, len(fields))
		}
		name := strings.TrimSpace(fields[0])
		baseURL := strings.TrimSpace(fields[1])
		model := strings.TrimSpace(fields[2])
		apiKey := ""
		if len(fields) > 4 {
			apiKey = strings.TrimSpace(fields[3])
		}
		costStr := strings.TrimSpace(fields[len(fields)-1])
		cost := 0.0
		if costStr != "" {
			var err error
			cost, err = strconv.ParseFloat(costStr, 64)
			if err != nil {
				return nil, fmt.Errorf("providers: line %d: invalid cost %q: %w", i+1, costStr, err)
			}
		}
		if name == "" || baseURL == "" || model == "" {
			return nil, fmt.Errorf("providers: line %d: name, baseURL, and model are required", i+1)
		}
		reg.Register(&provider{
			name:      name,
			baseURL:   baseURL,
			model:    model,
			apiKey:   apiKey,
			costPer1K: cost,
		})
	}
	return reg, nil
}
