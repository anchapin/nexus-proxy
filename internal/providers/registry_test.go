package providers

import (
	"testing"
)

func TestFrontierProvider_Fields(t *testing.T) {
	p := NewFrontierProvider("https://api.openai.com/v1/chat/completions", "gpt-4o", "sk-test", 0.005)
	if p.Name != "frontier" {
		t.Errorf("Name = %q, want frontier", p.Name)
	}
	if p.URL != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("URL = %q, want https://api.openai.com/v1/chat/completions", p.URL)
	}
	if p.Model != "gpt-4o" {
		t.Errorf("Model = %q, want gpt-4o", p.Model)
	}
	if p.InputCostPer1K != 0.005 {
		t.Errorf("InputCostPer1K = %f, want 0.005", p.InputCostPer1K)
	}
}

func TestZAIProvider_Fields(t *testing.T) {
	p := NewZAIProvider("https://api.z.ai/v1/chat/completions", "glm-4.6", "sk-zai", 0.002)
	if p.Name != "zai" {
		t.Errorf("Name = %q, want zai", p.Name)
	}
	if p.URL != "https://api.z.ai/v1/chat/completions" {
		t.Errorf("URL = %q, want https://api.z.ai/v1/chat/completions", p.URL)
	}
	if p.Model != "glm-4.6" {
		t.Errorf("Model = %q, want glm-4.6", p.Model)
	}
	if p.InputCostPer1K != 0.002 {
		t.Errorf("InputCostPer1K = %f, want 0.002", p.InputCostPer1K)
	}
}
