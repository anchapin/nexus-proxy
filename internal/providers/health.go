// Package providers — health.go defines the HealthChecker interface
// that decouples the router selector (internal/router) from the health
// package (internal/health). The selector consults this interface to
// skip providers whose circuit is open; health.FrontierHealth is the
// canonical implementation.
//
// Keeping the interface here (in providers) rather than in the router
// package means the selector imports providers (which it already does
// for ProviderStats) but not health — preserving the dependency
// direction documented in AGENTS.md.
package providers

// HealthChecker reports whether a named provider is currently healthy.
// Implemented by health.FrontierHealth; consumed by the router
// ProviderSelector so it can exclude providers with open circuits
// before scoring them (issue #1158).
//
// A nil HealthChecker means "no health probing configured": the selector
// falls back to its existing error-rate-based exclusion.
type HealthChecker interface {
	// IsHealthy reports whether the named provider's circuit is closed.
	// Must return true for an unknown provider name so a programming
	// error does not block traffic.
	IsHealthy(name string) bool

	// UnhealthyProviders returns the names of all providers whose
	// circuit is currently open. Returns nil or an empty slice when
	// every provider is healthy.
	UnhealthyProviders() []string
}
