package providers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/router"
)

type fixedStatsSource struct {
	stats []router.ProviderStats
}

func (s *fixedStatsSource) ProviderStats(_ context.Context, _ time.Time) ([]router.ProviderStats, error) {
	return s.stats, nil
}

func TestRegistry_Register(t *testing.T) {
	r := NewRegistry(nil, nil)
	p1 := NewFrontierProvider("https://api.openai.com/v1/chat/completions", "gpt-4o", "key1", 0.005)
	p2 := NewZAIProvider("https://api.z.ai/v1/chat/completions", "glm-4.6", "key2", 0.002)
	r.Register(p1)
	r.Register(p2)

	providers := r.Providers()
	if len(providers) != 2 {
		t.Errorf("Providers() len = %d, want 2", len(providers))
	}
	if providers[0].Name() != "frontier" {
		t.Errorf("providers[0].Name() = %q, want frontier", providers[0].Name())
	}
	if providers[1].Name() != "zai" {
		t.Errorf("providers[1].Name() = %q, want zai", providers[1].Name())
	}
}

func TestRegistry_Select_SingleProvider(t *testing.T) {
	r := NewRegistry(nil, nil)
	p := NewFrontierProvider("https://api.openai.com/v1/chat/completions", "gpt-4o", "key1", 0.005)
	r.Register(p)

	got, err := r.Select(context.Background())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.Name() != "frontier" {
		t.Errorf("got.Name() = %q, want frontier", got.Name())
	}
}

func TestRegistry_Select_NoProviders(t *testing.T) {
	r := NewRegistry(nil, nil)
	_, err := r.Select(context.Background())
	if !errors.Is(err, ErrNoProviders) {
		t.Errorf("err = %v, want ErrNoProviders", err)
	}
}

func TestRegistry_Select_NoStats_ReturnsFirst(t *testing.T) {
	r := NewRegistry(router.NewProviderSelector(), nil)
	p1 := NewFrontierProvider("https://api.openai.com/v1/chat/completions", "gpt-4o", "key1", 0.005)
	p2 := NewZAIProvider("https://api.z.ai/v1/chat/completions", "glm-4.6", "key2", 0.002)
	r.Register(p1)
	r.Register(p2)

	got, err := r.Select(context.Background())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.Name() != "frontier" {
		t.Errorf("got.Name() = %q, want frontier (first registered)", got.Name())
	}
}

func TestRegistry_Select_WithStats_ScoresCorrectly(t *testing.T) {
	src := &fixedStatsSource{
		stats: []router.ProviderStats{
			{Name: "frontier", SampleCount: 10, P50LatencyMs: 800, AvgCostUSD: 0.005},
			{Name: "zai", SampleCount: 20, P50LatencyMs: 400, AvgCostUSD: 0.002},
		},
	}
	cache := router.NewProviderStatsCache(router.NewProviderSelector(), src, time.Hour, time.Minute)
	r := NewRegistry(router.NewProviderSelector(), cache)
	p1 := NewFrontierProvider("https://api.openai.com/v1/chat/completions", "gpt-4o", "key1", 0.005)
	p2 := NewZAIProvider("https://api.z.ai/v1/chat/completions", "glm-4.6", "key2", 0.002)
	r.Register(p1)
	r.Register(p2)

	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got, err := r.Select(context.Background())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.Name() != "zai" {
		t.Errorf("got.Name() = %q, want zai (lower latency + lower cost)", got.Name())
	}
}

func TestRegistry_Select_WithStats_BelowMinSamples(t *testing.T) {
	src := &fixedStatsSource{
		stats: []router.ProviderStats{
			{Name: "frontier", SampleCount: 2, P50LatencyMs: 100, AvgCostUSD: 0.001},
			{Name: "zai", SampleCount: 20, P50LatencyMs: 400, AvgCostUSD: 0.002},
		},
	}
	cache := router.NewProviderStatsCache(router.NewProviderSelector(), src, time.Hour, time.Minute)
	r := NewRegistry(router.NewProviderSelector(), cache)
	p1 := NewFrontierProvider("https://api.openai.com/v1/chat/completions", "gpt-4o", "key1", 0.005)
	p2 := NewZAIProvider("https://api.z.ai/v1/chat/completions", "glm-4.6", "key2", 0.002)
	r.Register(p1)
	r.Register(p2)

	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got, err := r.Select(context.Background())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.Name() != "zai" {
		t.Errorf("got.Name() = %q, want zai (frontier below MinSamples=5, so zai wins)", got.Name())
	}
}

func TestRegistry_Select_WithStats_UnknownWinner(t *testing.T) {
	src := &fixedStatsSource{
		stats: []router.ProviderStats{
			{Name: "unknown", SampleCount: 10, P50LatencyMs: 100, AvgCostUSD: 0.001},
			{Name: "zai", SampleCount: 20, P50LatencyMs: 400, AvgCostUSD: 0.002},
		},
	}
	cache := router.NewProviderStatsCache(router.NewProviderSelector(), src, time.Hour, time.Minute)
	r := NewRegistry(router.NewProviderSelector(), cache)
	p1 := NewFrontierProvider("https://api.openai.com/v1/chat/completions", "gpt-4o", "key1", 0.005)
	p2 := NewZAIProvider("https://api.z.ai/v1/chat/completions", "glm-4.6", "key2", 0.002)
	r.Register(p1)
	r.Register(p2)

	got, err := r.Select(context.Background())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.Name() != "frontier" {
		t.Errorf("got.Name() = %q, want frontier (fallback when winner unknown)", got.Name())
	}
}

func TestFrontierProvider_Fields(t *testing.T) {
	p := NewFrontierProvider("https://api.openai.com/v1/chat/completions", "gpt-4o", "sk-test", 0.005)
	if p.Name() != "frontier" {
		t.Errorf("Name() = %q, want frontier", p.Name())
	}
	if p.BaseURL() != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("BaseURL() = %q, want https://api.openai.com/v1/chat/completions", p.BaseURL())
	}
	if p.Model() != "gpt-4o" {
		t.Errorf("Model() = %q, want gpt-4o", p.Model())
	}
	if p.CostPer1KUSD() != 0.005 {
		t.Errorf("CostPer1KUSD() = %f, want 0.005", p.CostPer1KUSD())
	}
}

func TestZAIProvider_Fields(t *testing.T) {
	p := NewZAIProvider("https://api.z.ai/v1/chat/completions", "glm-4.6", "sk-zai", 0.002)
	if p.Name() != "zai" {
		t.Errorf("Name() = %q, want zai", p.Name())
	}
	if p.BaseURL() != "https://api.z.ai/v1/chat/completions" {
		t.Errorf("BaseURL() = %q, want https://api.z.ai/v1/chat/completions", p.BaseURL())
	}
	if p.Model() != "glm-4.6" {
		t.Errorf("Model() = %q, want glm-4.6", p.Model())
	}
	if p.CostPer1KUSD() != 0.002 {
		t.Errorf("CostPer1KUSD() = %f, want 0.002", p.CostPer1KUSD())
	}
}
