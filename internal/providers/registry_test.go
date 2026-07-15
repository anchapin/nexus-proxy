package providers

import (
	"testing"
)

func TestParseProviders(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		reg, err := ParseProviders("")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if got := len(reg.All()); got != 0 {
			t.Fatalf("expected 0 providers, got %d", got)
		}
	})

	t.Run("single provider", func(t *testing.T) {
		reg, err := ParseProviders("frontier|https://api.openai.com/v1|gpt-4o|sk-xxx|0.005")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		ps := reg.All()
		if len(ps) != 1 {
			t.Fatalf("expected 1 provider, got %d", len(ps))
		}
		if ps[0].Name() != "frontier" {
			t.Errorf("expected name frontier, got %q", ps[0].Name())
		}
		if ps[0].BaseURL() != "https://api.openai.com/v1" {
			t.Errorf("expected baseURL https://api.openai.com/v1, got %q", ps[0].BaseURL())
		}
		if ps[0].Model() != "gpt-4o" {
			t.Errorf("expected model gpt-4o, got %q", ps[0].Model())
		}
		if ps[0].APIKey() != "sk-xxx" {
			t.Errorf("expected apiKey sk-xxx, got %q", ps[0].APIKey())
		}
		if ps[0].CostPer1K() != 0.005 {
			t.Errorf("expected cost 0.005, got %f", ps[0].CostPer1K())
		}
	})

	t.Run("multiple providers", func(t *testing.T) {
		raw := `frontier|https://api.openai.com/v1|gpt-4o|sk-xxx|0.005
zai|https://api.z.ai/v1|glm-4.6|zsk-xxx|0.002
`
		reg, err := ParseProviders(raw)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		ps := reg.All()
		if len(ps) != 2 {
			t.Fatalf("expected 2 providers, got %d", len(ps))
		}
		// Frontier should be first
		if ps[0].Name() != "frontier" {
			t.Errorf("expected first provider frontier, got %q", ps[0].Name())
		}
		if ps[1].Name() != "zai" {
			t.Errorf("expected second provider zai, got %q", ps[1].Name())
		}
	})

	t.Run("skips empty lines and comments", func(t *testing.T) {
		raw := `# comment
frontier|https://api.openai.com/v1|gpt-4o|sk-xxx|0.005

zai|https://api.z.ai/v1|glm-4.6|zsk-xxx|0.002
`
		reg, err := ParseProviders(raw)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if got := len(reg.All()); got != 2 {
			t.Fatalf("expected 2 providers, got %d", got)
		}
	})

	t.Run("empty apiKey", func(t *testing.T) {
		reg, err := ParseProviders("local|http://localhost:11434|qwen3-coder:8b||0.0")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if got := reg.Get("local").APIKey(); got != "" {
			t.Errorf("expected empty apiKey, got %q", got)
		}
	})

	t.Run("invalid cost", func(t *testing.T) {
		_, err := ParseProviders("frontier|https://api.openai.com/v1|gpt-4o|sk-xxx|not-a-number")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("missing fields", func(t *testing.T) {
		_, err := ParseProviders("frontier|https://api.openai.com/v1")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestRegistry(t *testing.T) {
	t.Run("Register and Get", func(t *testing.T) {
		reg := NewRegistry()
		p := &provider{name: "test", baseURL: "http://test", model: "m", apiKey: "k", costPer1K: 0.001}
		reg.Register(p)
		got := reg.Get("test")
		if got == nil {
			t.Fatal("expected provider, got nil")
		}
		if got.Name() != "test" {
			t.Errorf("expected name test, got %q", got.Name())
		}
	})

	t.Run("Get non-existent", func(t *testing.T) {
		reg := NewRegistry()
		if got := reg.Get("nonexistent"); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("duplicate registration panics", func(t *testing.T) {
		reg := NewRegistry()
		p := &provider{name: "dup", baseURL: "http://test", model: "m", apiKey: "k", costPer1K: 0.001}
		reg.Register(p)
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic")
			}
		}()
		reg.Register(p)
	})

	t.Run("nil provider panics", func(t *testing.T) {
		reg := NewRegistry()
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic")
			}
		}()
		reg.Register(nil)
	})

	t.Run("empty name panics", func(t *testing.T) {
		reg := NewRegistry()
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic")
			}
		}()
		reg.Register(&provider{name: "", baseURL: "http://test", model: "m", apiKey: "k", costPer1K: 0.001})
	})

	t.Run("All returns in registration order", func(t *testing.T) {
		reg := NewRegistry()
		p1 := &provider{name: "a", baseURL: "http://a", model: "m", apiKey: "k", costPer1K: 0.001}
		p2 := &provider{name: "b", baseURL: "http://b", model: "m", apiKey: "k", costPer1K: 0.002}
		reg.Register(p1)
		reg.Register(p2)
		all := reg.All()
		if len(all) != 2 {
			t.Fatalf("expected 2, got %d", len(all))
		}
		if all[0].Name() != "a" {
			t.Errorf("expected first name a, got %q", all[0].Name())
		}
		if all[1].Name() != "b" {
			t.Errorf("expected second name b, got %q", all[1].Name())
		}
	})
}

func TestProviderInterface(t *testing.T) {
	// Verify that *provider satisfies the Provider interface at compile time.
	var _ Provider = (*provider)(nil)
}
