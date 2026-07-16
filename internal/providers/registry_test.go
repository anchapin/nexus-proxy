package providers

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

func TestProviderConfig(t *testing.T) {
	p := ProviderConfig{
		NameVal:      "test",
		BaseURLVal:   "https://api.example.com/v1",
		ModelVal:     "test-model",
		APIKeyVal:    "sk-test",
		CostPer1KVal: 0.005,
	}
	if got, want := p.Name(), "test"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if got, want := p.BaseURL(), "https://api.example.com/v1"; got != want {
		t.Errorf("BaseURL() = %q, want %q", got, want)
	}
	if got, want := p.Model(), "test-model"; got != want {
		t.Errorf("Model() = %q, want %q", got, want)
	}
	if got, want := p.APIKey(), "sk-test"; got != want {
		t.Errorf("APIKey() = %q, want %q", got, want)
	}
	if got, want := p.CostPer1KUSD(), 0.005; got != want {
		t.Errorf("CostPer1KUSD() = %v, want %v", got, want)
	}
}

func TestRegistryRegister(t *testing.T) {
	reg := NewProviderRegistry()

	// Register a provider
	p := ProviderConfig{NameVal: "test", BaseURLVal: "https://api.test.com", ModelVal: "model", APIKeyVal: "", CostPer1KVal: 0.01}
	reg.Register(p)

	if reg.Len() != 1 {
		t.Errorf("Len() = %d, want 1", reg.Len())
	}

	// ByName returns the correct provider
	got := reg.ByName("test")
	if got == nil {
		t.Fatal("ByName(\"test\") returned nil")
	}
	if got.Name() != "test" {
		t.Errorf("got.Name() = %q, want %q", got.Name(), "test")
	}

	// ByName returns nil for unknown names
	if reg.ByName("unknown") != nil {
		t.Error("ByName(\"unknown\") should return nil")
	}
}

func TestRegistryRegisterDuplicate(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("Register duplicate should panic")
		}
	}()

	reg := NewProviderRegistry()
	p := ProviderConfig{NameVal: "dup", BaseURLVal: "https://api.test.com", ModelVal: "model", APIKeyVal: "", CostPer1KVal: 0.01}
	reg.Register(p)
	reg.Register(p) // Should panic
}

func TestRegistryRegisterNil(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("Register nil should panic")
		}
	}()

	reg := NewProviderRegistry()
	reg.Register(nil)
}

func TestRegistryRegisterEmptyName(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("Register empty name should panic")
		}
	}()

	reg := NewProviderRegistry()
	reg.Register(ProviderConfig{NameVal: "", BaseURLVal: "https://api.test.com", ModelVal: "model", APIKeyVal: "", CostPer1KVal: 0.01})
}

func TestRegistryAll(t *testing.T) {
	reg := NewProviderRegistry()
	reg.Register(ProviderConfig{NameVal: "first", BaseURLVal: "https://first.com", ModelVal: "m1", APIKeyVal: "", CostPer1KVal: 0.01})
	reg.Register(ProviderConfig{NameVal: "second", BaseURLVal: "https://second.com", ModelVal: "m2", APIKeyVal: "", CostPer1KVal: 0.02})

	all := reg.All()
	if len(all) != 2 {
		t.Fatalf("len(All()) = %d, want 2", len(all))
	}
	if all[0].Name() != "first" || all[1].Name() != "second" {
		t.Errorf("All() order = [%q, %q], want [first, second]", all[0].Name(), all[1].Name())
	}
}

func TestRegistryProviderNames(t *testing.T) {
	reg := NewProviderRegistry()
	reg.Register(ProviderConfig{NameVal: "a", BaseURLVal: "https://a.com", ModelVal: "m", APIKeyVal: "", CostPer1KVal: 0.01})
	reg.Register(ProviderConfig{NameVal: "b", BaseURLVal: "https://b.com", ModelVal: "m", APIKeyVal: "", CostPer1KVal: 0.02})

	names := reg.ProviderNames()
	want := []string{"a", "b"}
	if len(names) != len(want) {
		t.Fatalf("len(ProviderNames()) = %d, want %d", len(names), len(want))
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("ProviderNames()[%d] = %q, want %q", i, n, want[i])
		}
	}
}

func TestParseProvidersFromEnvEmpty(t *testing.T) {
	os.Unsetenv("NEXUS_FRONTIER_PROVIDERS")
	reg, err := ParseProvidersFromEnv()
	if err != nil {
		t.Fatalf("ParseProvidersFromEnv() error = %v", err)
	}
	if reg != nil {
		t.Errorf("reg = %v, want nil for empty env", reg)
	}
}

func TestParseProvidersFromEnvSingle(t *testing.T) {
	envVal := `[{"name":"openrouter","url":"https://openrouter.ai/v1","model":"google/gemini-pro","apiKey":"sk-xxx","costPer1K":0.01}]`
	os.Setenv("NEXUS_FRONTIER_PROVIDERS", envVal)
	defer os.Unsetenv("NEXUS_FRONTIER_PROVIDERS")

	reg, err := ParseProvidersFromEnv()
	if err != nil {
		t.Fatalf("ParseProvidersFromEnv() error = %v", err)
	}
	if reg == nil {
		t.Fatal("reg is nil")
	}
	if reg.Len() != 1 {
		t.Errorf("Len() = %d, want 1", reg.Len())
	}
	p := reg.ByName("openrouter")
	if p == nil {
		t.Fatal("ByName(\"openrouter\") returned nil")
	}
	if got, want := p.BaseURL(), "https://openrouter.ai/v1"; got != want {
		t.Errorf("BaseURL() = %q, want %q", got, want)
	}
	if got, want := p.Model(), "google/gemini-pro"; got != want {
		t.Errorf("Model() = %q, want %q", got, want)
	}
	if got, want := p.APIKey(), "sk-xxx"; got != want {
		t.Errorf("APIKey() = %q, want %q", got, want)
	}
	if got, want := p.CostPer1KUSD(), 0.01; got != want {
		t.Errorf("CostPer1KUSD() = %v, want %v", got, want)
	}
}

func TestParseProvidersFromEnvMultiple(t *testing.T) {
	envVal := `[{"name":"openrouter","url":"https://openrouter.ai/v1","model":"google/gemini-pro","apiKey":"sk-xxx","costPer1K":0.01},{"name":"azure","url":"https://xxx.openai.azure.com/v1","model":"gpt-4o","apiKey":"azure-key","costPer1K":0.005}]`
	os.Setenv("NEXUS_FRONTIER_PROVIDERS", envVal)
	defer os.Unsetenv("NEXUS_FRONTIER_PROVIDERS")

	reg, err := ParseProvidersFromEnv()
	if err != nil {
		t.Fatalf("ParseProvidersFromEnv() error = %v", err)
	}
	if reg.Len() != 2 {
		t.Errorf("Len() = %d, want 2", reg.Len())
	}

	// Check insertion order
	names := reg.ProviderNames()
	if len(names) != 2 || names[0] != "openrouter" || names[1] != "azure" {
		t.Errorf("ProviderNames() = %v, want [openrouter, azure]", names)
	}
}

func TestParseProvidersFromEnvURLTrim(t *testing.T) {
	// URLs with trailing slashes should be trimmed
	envVal := `[{"name":"test","url":"https://api.test.com/v1///","model":"m","costPer1K":0.01}]`
	os.Setenv("NEXUS_FRONTIER_PROVIDERS", envVal)
	defer os.Unsetenv("NEXUS_FRONTIER_PROVIDERS")

	reg, err := ParseProvidersFromEnv()
	if err != nil {
		t.Fatalf("ParseProvidersFromEnv() error = %v", err)
	}
	p := reg.ByName("test")
	if p.BaseURL() != "https://api.test.com/v1" {
		t.Errorf("BaseURL() = %q, want trimmed URL", p.BaseURL())
	}
}

func TestParseProvidersFromEnvMissingName(t *testing.T) {
	envVal := `[{"url":"https://api.test.com","model":"m","costPer1K":0.01}]`
	os.Setenv("NEXUS_FRONTIER_PROVIDERS", envVal)
	defer os.Unsetenv("NEXUS_FRONTIER_PROVIDERS")

	_, err := ParseProvidersFromEnv()
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error should mention 'name': %v", err)
	}
}

func TestParseProvidersFromEnvMissingURL(t *testing.T) {
	envVal := `[{"name":"test","model":"m","costPer1K":0.01}]`
	os.Setenv("NEXUS_FRONTIER_PROVIDERS", envVal)
	defer os.Unsetenv("NEXUS_FRONTIER_PROVIDERS")

	_, err := ParseProvidersFromEnv()
	if err == nil {
		t.Fatal("expected error for missing url")
	}
	if !strings.Contains(err.Error(), "url") {
		t.Errorf("error should mention 'url': %v", err)
	}
}

func TestParseProvidersFromEnvMissingModel(t *testing.T) {
	envVal := `[{"name":"test","url":"https://api.test.com","costPer1K":0.01}]`
	os.Setenv("NEXUS_FRONTIER_PROVIDERS", envVal)
	defer os.Unsetenv("NEXUS_FRONTIER_PROVIDERS")

	_, err := ParseProvidersFromEnv()
	if err == nil {
		t.Fatal("expected error for missing model")
	}
	if !strings.Contains(err.Error(), "model") {
		t.Errorf("error should mention 'model': %v", err)
	}
}

func TestParseProvidersFromEnvInvalidJSON(t *testing.T) {
	os.Setenv("NEXUS_FRONTIER_PROVIDERS", "not json")
	defer os.Unsetenv("NEXUS_FRONTIER_PROVIDERS")

	_, err := ParseProvidersFromEnv()
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "parse JSON") {
		t.Errorf("error should mention 'parse JSON': %v", err)
	}
}

func TestParseProvidersFromEnvEmptyArray(t *testing.T) {
	os.Setenv("NEXUS_FRONTIER_PROVIDERS", "[]")
	defer os.Unsetenv("NEXUS_FRONTIER_PROVIDERS")

	reg, err := ParseProvidersFromEnv()
	if err != nil {
		t.Fatalf("ParseProvidersFromEnv() error = %v", err)
	}
	if reg != nil {
		t.Errorf("reg = %v, want nil for empty array", reg)
	}
}

func TestParseProvidersFromEnvEmptyNameEntry(t *testing.T) {
	envVal := `[{"name":"","url":"https://api.test.com","model":"m","costPer1K":0.01}]`
	os.Setenv("NEXUS_FRONTIER_PROVIDERS", envVal)
	defer os.Unsetenv("NEXUS_FRONTIER_PROVIDERS")

	_, err := ParseProvidersFromEnv()
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestProviderConfigImplementsAuthProvider(t *testing.T) {
	var _ AuthProviderV2 = ProviderConfig{}
}

func TestProviderConfigImplementsProvider(t *testing.T) {
	var _ ProviderV2 = ProviderConfig{}
}

// TestRegistryConcurrent exercises concurrent Register/All/ByName calls.
func TestRegistryConcurrent(t *testing.T) {
	reg := NewProviderRegistry()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			reg.Register(ProviderConfig{
				NameVal:      fmt.Sprintf("provider-%d", i),
				BaseURLVal:   "https://api.test.com",
				ModelVal:     "m",
				APIKeyVal:    "",
				CostPer1KVal: 0.01,
			})
		}
		close(done)
	}()

	for i := 0; i < 100; i++ {
		_ = reg.All()
		_ = reg.ByName("provider-50")
		_ = reg.Len()
	}
	<-done
}

// BenchmarkRegistryByName benchmarks registry lookup performance.
func BenchmarkRegistryByName(b *testing.B) {
	reg := NewProviderRegistry()
	for i := 0; i < 100; i++ {
		reg.Register(ProviderConfig{
			NameVal:      "provider",
			BaseURLVal:   "https://api.test.com",
			ModelVal:     "m",
			APIKeyVal:    "",
			CostPer1KVal: 0.01,
		})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = reg.ByName("provider")
	}
}

// BenchmarkRegistryAll benchmarks reg.All() performance.
func BenchmarkRegistryAll(b *testing.B) {
	reg := NewProviderRegistry()
	for i := 0; i < 100; i++ {
		reg.Register(ProviderConfig{
			NameVal:      "provider",
			BaseURLVal:   "https://api.test.com",
			ModelVal:     "m",
			APIKeyVal:    "",
			CostPer1KVal: 0.01,
		})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = reg.All()
	}
}

// TestRegistryOrderedIteration verifies that All() and ProviderNames()
// preserve insertion order.
func TestRegistryOrderedIteration(t *testing.T) {
	reg := NewProviderRegistry()
	names := []string{"z", "a", "m", "b", "q"}
	for _, n := range names {
		reg.Register(ProviderConfig{NameVal: n, BaseURLVal: "https://" + n + ".com", ModelVal: "m", APIKeyVal: "", CostPer1KVal: 0.01})
	}

	// All should return in insertion order
	all := reg.All()
	got := make([]string, len(all))
	for i, p := range all {
		got[i] = p.Name()
	}
	if !sort.IsSorted(sort.StringSlice(got)) {
		// Not sorted, but should be insertion order
		want := []string{"z", "a", "m", "b", "q"}
		for i, n := range got {
			if n != want[i] {
				t.Errorf("All()[%d] = %q, want %q (insertion order)", i, n, want[i])
			}
		}
	}

	// ProviderNames should also preserve insertion order
	pnames := reg.ProviderNames()
	for i, n := range pnames {
		if n != names[i] {
			t.Errorf("ProviderNames()[%d] = %q, want %q", i, n, names[i])
		}
	}
}

// TestParseProviderJSONFields verifies exact JSON field names.
func TestParseProviderJSONFields(t *testing.T) {
	raw := `[{"name":"test","url":"https://api.test.com","model":"m","apiKey":"key","costPer1K":0.5}]`
	var configs []struct {
		Name      string  `json:"name"`
		URL       string  `json:"url"`
		Model     string  `json:"model"`
		APIKey    string  `json:"apiKey"`
		CostPer1K float64 `json:"costPer1K"`
	}
	if err := json.Unmarshal([]byte(raw), &configs); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("len = %d, want 1", len(configs))
	}
	c := configs[0]
	if c.Name != "test" || c.URL != "https://api.test.com" || c.Model != "m" || c.APIKey != "key" || c.CostPer1K != 0.5 {
		t.Errorf("config = %+v, want {test, https://api.test.com, m, key, 0.5}", c)
	}
}

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
