// Package providers holds the registry of OpenAI-compatible frontier
// endpoints configured for the proxy (issue #43).
//
// A Provider replaces the hardcoded single-frontier + single-z.ai config
// model with a uniform record carrying the endpoint URL, model name,
// bearer token, priority, and per-direction cost metadata. The Registry
// holds the ordered list of configured providers and exposes the
// accessors the rest of the codebase consumes:
//
//   - FrontierProviders() — ordered list used by the cascade builder
//     and the chat handler's route=frontier dispatch.
//   - Get(name) — lookup by the short Name an operator assigns.
//   - ByModel(model) — lookup by the upstream model name; the chat
//     handler uses this for per-provider cost estimation.
//   - Len() — convenience predicate for "any providers configured".
//
// LoadFromEnv is the single source of truth for translating the
// NEXUS_PROVIDER_<NAME>_* env-var surface into a Registry. It returns
// an empty Registry when NEXUS_PROVIDERS is unset; the config loader
// is responsible for the backward-compat fallback that rehydrates a
// synthetic Registry from the legacy NEXUS_FRONTIER_* / NEXUS_ZAI_*
// variables. That division keeps the providers package free of any
// knowledge about the legacy "frontier" vs "zai" naming.
package providers

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Provider represents one OpenAI-compatible frontier endpoint
// configured for the proxy (issue #43). A Provider is the
// generalised replacement for the pre-issue-#43 hardcoded
// NEXUS_FRONTIER_* / NEXUS_ZAI_* env vars; the registry builds a
// Provider slice from NEXUS_PROVIDERS + per-provider env vars.
//
// Fields are documented inline because every entry is part of the
// public operator contract; the cost and priority fields are
// optional and zero-valued when the operator omits them.
type Provider struct {
	// Name is a short identifier the operator assigns to this
	// endpoint (e.g. "openai", "anthropic", "zai"). Names are
	// case-sensitive and must be unique within a registry. They
	// appear in cascade route_attempted telemetry and in
	// X-Nexus-Cascade-Served-By response headers.
	Name string

	// URL is the upstream /v1/chat/completions endpoint the proxy
	// will POST to. Required — providers missing a URL are rejected
	// by LoadFromEnv with a clear error message.
	URL string

	// Model is the upstream model name inserted into the request
	// body. Required for the same reason as URL: the proxy cannot
	// construct a valid request without it.
	Model string

	// APIKey is the bearer token sent in the Authorization header.
	// Empty means "provider is not configured for use"; the cascade
	// builder skips empty-key providers while leaving them in the
	// registry so the operator can wire them up by setting the env
	// var without restarting the proxy semantics.
	APIKey string

	// Priority is the cascade ordering hint. Lower numbers are tried
	// first (matching the rest of the project where "0" is the
	// primary). Equal priorities preserve the order they were
	// declared in NEXUS_PROVIDERS. Zero is the default — operators
	// who only declare NEXUS_PROVIDERS get NEXUS_FRONTIER_*'s
	// first-then-z.ai ordering for free.
	Priority int

	// InputCostPer1K is the rough USD cost per 1,000 input tokens
	// (issue #43). Used by the chat handler's frontierCostEstimate
	// helper to project per-request spend onto the rolling daily
	// budget. Zero falls back to NEXUS_JUDGE_COST_PER_1K so a
	// provider without an explicit rate inherits the legacy default
	// rather than silently recording zero cost.
	InputCostPer1K float64

	// OutputCostPer1K mirrors InputCostPer1K for the model's output
	// tokens. Reserved for future cost-quality dashboards that need
	// input+output splits; the current per-request estimate only
	// considers input tokens.
	OutputCostPer1K float64

	// MaxTokens is the upstream's max context window. Zero means
	// "unspecified"; downstream code can use it for VRAM budgeting
	// or request-size validation without affecting existing callers.
	MaxTokens int
}

// Registry holds the ordered list of configured providers. The zero
// value is a valid (empty) registry; callers typically obtain a real
// value from LoadFromEnv or NewRegistry.
type Registry struct {
	providers []Provider
}

// NewRegistry returns a registry wrapping the given providers. The
// registry does not mutate or copy the slice — callers should pass a
// stable slice they do not intend to modify. NewRegistry panics when
// p is nil (so callers don't accidentally depend on a nil-registry
// fallback path) and de-duplicates by Name, keeping the first
// occurrence.
func NewRegistry(p []Provider) Registry {
	if p == nil {
		// An explicitly nil slice means "no providers". We don't
		// panic — that's reserved for callers who reach in here
		// with a typed nil they'd forget to handle.
		return Registry{}
	}
	seen := make(map[string]struct{}, len(p))
	out := make([]Provider, 0, len(p))
	for _, prov := range p {
		if _, dup := seen[prov.Name]; dup {
			continue
		}
		seen[prov.Name] = struct{}{}
		out = append(out, prov)
	}
	return Registry{providers: out}
}

// FrontierProviders returns the registry's providers in priority
// order (ties broken by declaration order). Returns a fresh slice so
// callers can mutate it without disturbing the registry.
func (r Registry) FrontierProviders() []Provider {
	if len(r.providers) == 0 {
		return nil
	}
	out := make([]Provider, len(r.providers))
	copy(out, r.providers)
	return out
}

// Get returns the provider whose Name matches name (case-sensitive).
// Returns (Provider{}, false) when no match. Useful for handlers
// that want to look up a specific endpoint by its operator-assigned
// identifier (e.g. cost dashboards, debug logging).
func (r Registry) Get(name string) (Provider, bool) {
	for _, p := range r.providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// ByModel returns the provider whose Model field matches model.
// Returns (Provider{}, false) when no match. Used by the chat
// handler's frontierCostEstimate helper to look up per-provider
// pricing without coupling the cost logic to a specific name.
//
// When two providers share the same model name the first one in
// the registry wins; operators are expected to keep model names
// unique within a registry.
func (r Registry) ByModel(model string) (Provider, bool) {
	if model == "" {
		return Provider{}, false
	}
	for _, p := range r.providers {
		if p.Model == model {
			return p, true
		}
	}
	return Provider{}, false
}

// Len reports the number of providers in the registry.
func (r Registry) Len() int { return len(r.providers) }

// envProviderConfig is the per-provider env-var layout LoadFromEnv
// expects. Kept as a small struct so the parsing logic reads top-down
// without a wall of env-var name strings.
type envProviderConfig struct {
	suffix string // uppercased Name, ready to splice into NEXUS_PROVIDER_<suffix>_<field>
	name   string // original Name from NEXUS_PROVIDERS (lowercase preserved)
}

// LoadFromEnv reads NEXUS_PROVIDERS + the per-provider
// NEXUS_PROVIDER_<NAME>_* env vars and returns the parsed registry.
//
// NEXUS_PROVIDERS is a comma-separated list of provider names. For
// each name the function reads:
//
//	NEXUS_PROVIDER_<NAME>_URL                (required)
//	NEXUS_PROVIDER_<NAME>_MODEL              (required)
//	NEXUS_PROVIDER_<NAME>_API_KEY            (optional; "" == skip from cascade)
//	NEXUS_PROVIDER_<NAME>_PRIORITY           (optional; default 0)
//	NEXUS_PROVIDER_<NAME>_INPUT_COST_PER_1K  (optional; default 0)
//	NEXUS_PROVIDER_<NAME>_OUTPUT_COST_PER_1K (optional; default 0)
//	NEXUS_PROVIDER_<NAME>_MAX_TOKENS         (optional; default 0)
//
// NAME matching is case-insensitive (the env-var lookups uppercase
// the suffix); the original Name in the returned Provider preserves
// the casing the operator typed into NEXUS_PROVIDERS.
//
// A missing URL or MODEL for a declared name produces an error so a
// half-configured provider fails loud at boot instead of silently
// dispatching to /dev/null at request time.
//
// LoadFromEnv returns an empty Registry (no error) when NEXUS_PROVIDERS
// is unset or empty — the config loader is responsible for the
// backward-compat fallback that synthesises a registry from the
// legacy NEXUS_FRONTIER_* / NEXUS_ZAI_* vars in that case.
func LoadFromEnv() (Registry, error) {
	raw := strings.TrimSpace(os.Getenv("NEXUS_PROVIDERS"))
	if raw == "" {
		return Registry{}, nil
	}

	parts := strings.Split(raw, ",")
	configs := make([]envProviderConfig, 0, len(parts))
	for _, p := range parts {
		name := strings.TrimSpace(p)
		if name == "" {
			continue
		}
		configs = append(configs, envProviderConfig{
			suffix: strings.ToUpper(name),
			name:   name,
		})
	}
	if len(configs) == 0 {
		return Registry{}, nil
	}

	out := make([]Provider, 0, len(configs))
	for _, cfg := range configs {
		prov, err := loadOneProvider(cfg)
		if err != nil {
			return Registry{}, fmt.Errorf("providers: %s: %w", cfg.name, err)
		}
		out = append(out, prov)
	}
	return NewRegistry(out), nil
}

// loadOneProvider reads the per-provider env vars for cfg and returns
// the resulting Provider. Errors are returned with the field name
// prefixed so the caller can append the provider name without losing
// the underlying cause.
func loadOneProvider(cfg envProviderConfig) (Provider, error) {
	urlKey := "NEXUS_PROVIDER_" + cfg.suffix + "_URL"
	modelKey := "NEXUS_PROVIDER_" + cfg.suffix + "_MODEL"
	keyKey := "NEXUS_PROVIDER_" + cfg.suffix + "_API_KEY"
	priorityKey := "NEXUS_PROVIDER_" + cfg.suffix + "_PRIORITY"
	inCostKey := "NEXUS_PROVIDER_" + cfg.suffix + "_INPUT_COST_PER_1K"
	outCostKey := "NEXUS_PROVIDER_" + cfg.suffix + "_OUTPUT_COST_PER_1K"
	maxTokKey := "NEXUS_PROVIDER_" + cfg.suffix + "_MAX_TOKENS"

	url := strings.TrimSpace(os.Getenv(urlKey))
	if url == "" {
		return Provider{}, fmt.Errorf("%s is required", urlKey)
	}
	model := strings.TrimSpace(os.Getenv(modelKey))
	if model == "" {
		return Provider{}, fmt.Errorf("%s is required", modelKey)
	}

	p := Provider{
		Name:   cfg.name,
		URL:    url,
		Model:  model,
		APIKey: os.Getenv(keyKey),
	}

	if v := strings.TrimSpace(os.Getenv(priorityKey)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Provider{}, fmt.Errorf("%s must be an integer: %w", priorityKey, err)
		}
		p.Priority = n
	}

	if v := strings.TrimSpace(os.Getenv(inCostKey)); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return Provider{}, fmt.Errorf("%s must be a number: %w", inCostKey, err)
		}
		p.InputCostPer1K = f
	}
	if v := strings.TrimSpace(os.Getenv(outCostKey)); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return Provider{}, fmt.Errorf("%s must be a number: %w", outCostKey, err)
		}
		p.OutputCostPer1K = f
	}
	if v := strings.TrimSpace(os.Getenv(maxTokKey)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Provider{}, fmt.Errorf("%s must be an integer: %w", maxTokKey, err)
		}
		p.MaxTokens = n
	}
	return p, nil
}

// SortByPriority returns a copy of providers ordered by Priority (lower
// first), stable across ties. Exposed so callers building ad-hoc
// cascade lists (e.g. tests, debug CLI) can match the registry's
// canonical ordering without re-implementing the sort.
func SortByPriority(providers []Provider) []Provider {
	if len(providers) <= 1 {
		out := make([]Provider, len(providers))
		copy(out, providers)
		return out
	}
	out := make([]Provider, len(providers))
	copy(out, providers)
	// Insertion sort — providers lists are tiny (single-digit) so
	// the O(n²) cost is negligible and we avoid pulling sort.Slice
	//'s reflection overhead.
	for i := 1; i < len(out); i++ {
		cur := out[i]
		j := i - 1
		for j >= 0 && out[j].Priority > cur.Priority {
			out[j+1] = out[j]
			j--
		}
		out[j+1] = cur
	}
	return out
}
