// Package providers — aliases.go implements model name aliasing
// (issue #1184). An operator configures a JSON map via
// NEXUS_MODEL_ALIASES that translates client-requested model names
// (e.g. "gpt-4") into "providerName/upstreamModel" targets. The chat
// handler consults the resolver before routing so an aliased request
// is dispatched directly to the target provider with the upstream
// model name rewritten into the request body.

package providers

import (
	"fmt"
	"strings"
)

// AliasTarget is the resolved result of a model alias lookup. It
// carries the concrete endpoint details the chat handler needs to
// dispatch the request.
type AliasTarget struct {
	// ProviderName is the short identifier matching a registered
	// provider (e.g. "anthropic").
	ProviderName string
	// Model is the upstream model name the request body should carry
	// after rewriting (e.g. "claude-3-5-sonnet").
	Model string
	// BaseURL is the provider's upstream endpoint base URL.
	BaseURL string
	// APIKey is the provider's bearer token (may be empty for local
	// endpoints).
	APIKey string
}

// ParseAliasValue splits a "providerName/upstreamModel" alias value
// into its two components. The provider name is everything before the
// first "/"; the model is everything after. A value without a "/" is
// treated as a provider name with an empty model (which the caller
// can choose to reject). Returns an error when either part is empty.
func ParseAliasValue(v string) (provider, model string, err error) {
	v = strings.TrimSpace(v)
	idx := strings.Index(v, "/")
	if idx < 0 {
		return "", "", fmt.Errorf("alias value %q: missing '/' separator (expected \"providerName/upstreamModel\")", v)
	}
	provider = strings.TrimSpace(v[:idx])
	model = strings.TrimSpace(v[idx+1:])
	if provider == "" {
		return "", "", fmt.Errorf("alias value %q: empty provider name", v)
	}
	if model == "" {
		return "", "", fmt.Errorf("alias value %q: empty model name", v)
	}
	return provider, model, nil
}

// ResolveAlias looks up the requested model in the alias map and, if
// found, resolves the target provider from the registry. Returns the
// AliasTarget and true when the alias resolves; returns AliasTarget{}
// and false when no alias matches or the target provider is not
// registered.
//
// When the alias value references a provider name that is not in the
// registry, ResolveAlias returns false — the caller (chat handler)
// decides whether to pass through (non-strict) or reject (strict).
func ResolveAlias(aliases map[string]string, reg *ProviderRegistry, requestedModel string) (AliasTarget, bool) {
	if len(aliases) == 0 || requestedModel == "" {
		return AliasTarget{}, false
	}
	raw, ok := aliases[requestedModel]
	if !ok {
		return AliasTarget{}, false
	}
	providerName, model, err := ParseAliasValue(raw)
	if err != nil {
		return AliasTarget{}, false
	}
	if reg == nil {
		return AliasTarget{}, false
	}
	p := reg.ByName(providerName)
	if p == nil {
		return AliasTarget{}, false
	}
	return AliasTarget{
		ProviderName: providerName,
		Model:        model,
		BaseURL:      p.BaseURL(),
		APIKey:       p.APIKey(),
	}, true
}

// HasProviderModel reports whether any provider in the registry
// serves the exact model name. Used by the strict-mode path to
// distinguish "unknown model" (should reject) from "exact provider
// model match" (should pass through).
func HasProviderModel(reg *ProviderRegistry, model string) bool {
	if reg == nil || model == "" {
		return false
	}
	for _, p := range reg.All() {
		if p.Model() == model {
			return true
		}
	}
	return false
}
