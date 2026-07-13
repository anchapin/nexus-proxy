package providers

import (
	"context"
	"errors"
	"sync"

	"github.com/anchapin/nexus-proxy/internal/router"
)

var ErrNoProviders = errors.New("providers: no providers registered")

type Registry struct {
	selector *router.ProviderSelector
	cache    *router.ProviderStatsCache
	providers []Provider
	mu        sync.RWMutex
}

func NewRegistry(selector *router.ProviderSelector, cache *router.ProviderStatsCache) *Registry {
	if selector == nil {
		selector = router.NewProviderSelector()
	}
	return &Registry{
		selector:  selector,
		cache:     cache,
		providers: []Provider{},
	}
}

func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers = append(r.providers, p)
}

func (r *Registry) Providers() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Provider, len(r.providers))
	copy(out, r.providers)
	return out
}

func (r *Registry) Select(ctx context.Context) (Provider, error) {
	r.mu.RLock()
	providers := r.providers
	r.mu.RUnlock()

	if len(providers) == 0 {
		return nil, ErrNoProviders
	}

	if len(providers) == 1 {
		return providers[0], nil
	}

	var stats []router.ProviderStats
	if r.cache != nil {
		stats = r.cache.Snapshot()
	}

	var name string
	var winner router.ProviderStats
	if len(stats) > 0 {
		name, winner = r.selector.SelectFrontier(stats)
	}

	if name == "" {
		return providers[0], nil
	}

	for _, p := range providers {
		if p.Name() == name {
			return p, nil
		}
	}

	_ = winner
	return providers[0], nil
}
