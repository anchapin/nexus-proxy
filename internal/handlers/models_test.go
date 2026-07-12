package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/config"
)

// fixedTime gives every test a deterministic Created timestamp.
var fixedTime = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

func fixedClock() time.Time { return fixedTime }

// --- helpers ---------------------------------------------------------------

func mustDecode(t *testing.T, body string) ModelsListResponse {
	t.Helper()
	var resp ModelsListResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, body)
	}
	return resp
}

func mustDecodeModel(t *testing.T, body string) Model {
	t.Helper()
	var m Model
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode model: %v\nbody: %s", err, body)
	}
	return m
}

func modelIDs(resp ModelsListResponse) []string {
	ids := make([]string, len(resp.Data))
	for i, m := range resp.Data {
		ids[i] = m.ID
	}
	return ids
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// mockOllama starts an httptest server that responds to /api/tags
// with the given JSON body and status. Returns the server; the test
// plugs srv.URL into Config.OllamaURL.
func mockOllama(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- tests -----------------------------------------------------------------

func TestModelsList_ReturnsConfiguredModels(t *testing.T) {
	cfg := config.Config{
		LocalModel:            "qwen3-coder:8b",
		RouterModel:           "qwen3-coder:4b",
		FrontierModel:         "gpt-4o",
		ZAIModel:              "glm-4.6",
		ModelsEndpointEnabled: true,
		// ModelsCacheTTL = 0 → Ollama poll disabled, list is purely configured.
	}

	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	resp := mustDecode(t, rr.Body.String())

	if resp.Object != "list" {
		t.Errorf("object = %q, want \"list\"", resp.Object)
	}
	ids := modelIDs(resp)
	for _, want := range []string{"qwen3-coder:8b", "qwen3-coder:4b", "gpt-4o", "glm-4.6"} {
		if !contains(ids, want) {
			t.Errorf("models list missing %q; got %v", want, ids)
		}
	}
}

func TestModelsList_OpenAICompatibleObjectFields(t *testing.T) {
	cfg := config.Config{
		LocalModel:            "test-model",
		ModelsEndpointEnabled: true,
	}
	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	resp := mustDecode(t, rr.Body.String())
	if len(resp.Data) == 0 {
		t.Fatal("expected at least one model")
	}
	for _, m := range resp.Data {
		if m.Object != "model" {
			t.Errorf("model %q object = %q, want \"model\"", m.ID, m.Object)
		}
		if m.ID == "" {
			t.Error("model has empty id")
		}
	}
}

func TestModelsList_DataNeverNull(t *testing.T) {
	// Even if somehow no models are configured, data should be [] not null.
	cfg := config.Config{
		ModelsEndpointEnabled: true,
	}
	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// The raw JSON should contain "data":[ not "data":null.
	if strings.Contains(rr.Body.String(), "\"data\":null") {
		t.Errorf("data is null; should be empty array: %s", rr.Body.String())
	}
}

func TestModelsSingle_ReturnsKnownModel(t *testing.T) {
	cfg := config.Config{
		LocalModel:            "qwen3-coder:8b",
		FrontierModel:         "gpt-4o",
		ModelsEndpointEnabled: true,
	}
	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})

	req := httptest.NewRequest(http.MethodGet, "/v1/models/gpt-4o", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	m := mustDecodeModel(t, rr.Body.String())
	if m.ID != "gpt-4o" {
		t.Errorf("id = %q, want \"gpt-4o\"", m.ID)
	}
	if m.Object != "model" {
		t.Errorf("object = %q, want \"model\"", m.Object)
	}
}

func TestModelsSingle_UnknownReturns404(t *testing.T) {
	cfg := config.Config{
		LocalModel:            "qwen3-coder:8b",
		ModelsEndpointEnabled: true,
	}
	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})

	req := httptest.NewRequest(http.MethodGet, "/v1/models/nonexistent-xyz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "error") {
		t.Errorf("404 body should contain error JSON: %s", rr.Body.String())
	}
}

func TestModels_DisabledViaConfig(t *testing.T) {
	cfg := config.Config{
		LocalModel:            "qwen3-coder:8b",
		ModelsEndpointEnabled: false, // explicitly disabled
	}
	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})

	// Both endpoints should 404.
	for _, path := range []string{"/v1/models", "/v1/models/gpt-4o"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("path %s: status = %d, want 404 (endpoint disabled)", path, rr.Code)
		}
	}
}

func TestModels_SecretsNotLeaked(t *testing.T) {
	// Construct a config with a real-looking API key and verify
	// it never appears in any model response.
	secretKey := "sk-secret-DO-NOT-LEAK-12345"
	cfg := config.Config{
		LocalModel:            "qwen3-coder:8b",
		FrontierModel:         "gpt-4o",
		FrontierKey:           secretKey,
		ZAIKey:                "zai-secret-key-67890",
		ModelsEndpointEnabled: true,
	}
	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})

	// List endpoint.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if strings.Contains(rr.Body.String(), secretKey) {
		t.Errorf("API key leaked in models list response: %s", rr.Body.String())
	}

	// Single-model endpoint.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models/gpt-4o", nil)
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if strings.Contains(rr2.Body.String(), secretKey) {
		t.Errorf("API key leaked in single model response: %s", rr2.Body.String())
	}
	if strings.Contains(rr2.Body.String(), "zai-secret-key-67890") {
		t.Errorf("ZAI key leaked in single model response: %s", rr2.Body.String())
	}
}

func TestModels_MethodNotAllowed(t *testing.T) {
	cfg := config.Config{
		LocalModel:            "qwen3-coder:8b",
		ModelsEndpointEnabled: true,
	}
	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})

	req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
	if allow := rr.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow header = %q, want GET", allow)
	}
}

func TestModels_OllamaDown_ReturnsConfiguredModels(t *testing.T) {
	// Point at a closed server so /api/tags always fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	srv.Close() // immediately close → connection refused

	cfg := config.Config{
		LocalModel:            "qwen3-coder:8b",
		FrontierModel:         "gpt-4o",
		OllamaURL:             srv.URL,
		ModelsEndpointEnabled: true,
		ModelsCacheTTL:        5 * time.Minute,
	}
	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Ollama down should not fail the handler)", rr.Code)
	}
	resp := mustDecode(t, rr.Body.String())
	// Configured models must still be present.
	ids := modelIDs(resp)
	if !contains(ids, "qwen3-coder:8b") {
		t.Errorf("local model missing from list: %v", ids)
	}
	if !contains(ids, "gpt-4o") {
		t.Errorf("frontier model missing from list: %v", ids)
	}
}

func TestModels_OllamaTags_MergedAndDeduped(t *testing.T) {
	tagsJSON := `{"models":[
		{"name":"qwen3-coder:8b","model":"qwen3-coder:8b","modified_at":"2025-01-01T00:00:00Z"},
		{"name":"llama3:70b","model":"llama3:70b","modified_at":"2025-01-01T00:00:00Z"}
	]}`
	srv := mockOllama(t, http.StatusOK, tagsJSON)

	cfg := config.Config{
		LocalModel:            "qwen3-coder:8b", // also in Ollama tags → should dedupe
		FrontierModel:         "gpt-4o",
		OllamaURL:             srv.URL,
		ModelsEndpointEnabled: true,
		ModelsCacheTTL:        5 * time.Minute,
	}
	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	resp := mustDecode(t, rr.Body.String())
	ids := modelIDs(resp)

	// Ollama-only model must be present.
	if !contains(ids, "llama3:70b") {
		t.Errorf("Ollama tag llama3:70b missing from list: %v", ids)
	}
	// Dedup: qwen3-coder:8b should appear exactly once.
	count := 0
	for _, id := range ids {
		if id == "qwen3-coder:8b" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("qwen3-coder:8b appears %d times, want 1; ids: %v", count, ids)
	}
}

func TestModels_OllamaTags_CacheHitsAvoidExtraCalls(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3:70b"}]}`))
	}))
	t.Cleanup(srv.Close)

	cfg := config.Config{
		LocalModel:            "qwen3-coder:8b",
		OllamaURL:             srv.URL,
		ModelsEndpointEnabled: true,
		ModelsCacheTTL:        5 * time.Minute,
	}
	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})

	// First request populates the cache.
	req1 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	h.ServeHTTP(httptest.NewRecorder(), req1)
	// Second request should hit the cache.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	h.ServeHTTP(httptest.NewRecorder(), req2)

	if callCount != 1 {
		t.Errorf("Ollama called %d times, want 1 (cache should serve second request)", callCount)
	}
}

func TestModels_OllamaTags_CacheExpiresAndRefetches(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3:70b"}]}`))
	}))
	t.Cleanup(srv.Close)

	// Use a mutable clock so we can advance time past the TTL.
	now := fixedTime
	clock := func() time.Time { return now }

	cfg := config.Config{
		LocalModel:            "qwen3-coder:8b",
		OllamaURL:             srv.URL,
		ModelsEndpointEnabled: true,
		ModelsCacheTTL:        5 * time.Minute,
	}
	h := Models(ModelsDeps{Config: cfg, Now: clock})

	// First call → fetch.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if callCount != 1 {
		t.Fatalf("after first call: Ollama call count = %d, want 1", callCount)
	}

	// Second call within TTL → cache hit.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if callCount != 1 {
		t.Fatalf("within TTL: Ollama call count = %d, want 1 (cached)", callCount)
	}

	// Advance time past TTL → refetch.
	now = now.Add(6 * time.Minute)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if callCount != 2 {
		t.Errorf("after TTL expiry: Ollama call count = %d, want 2", callCount)
	}
}

func TestModels_OllamaTags_ServesStaleOnTransientFailure(t *testing.T) {
	// Phase 1: Ollama is up and returns tags.
	var status int
	var body string
	status = http.StatusOK
	body = `{"models":[{"name":"llama3:70b"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	cfg := config.Config{
		LocalModel:            "qwen3-coder:8b",
		OllamaURL:             srv.URL,
		ModelsEndpointEnabled: true,
		ModelsCacheTTL:        5 * time.Minute,
	}
	// Use a mutable clock so we can advance time.
	now := fixedTime
	clock := func() time.Time { return now }
	h := Models(ModelsDeps{Config: cfg, Now: clock})

	// Warm the cache.
	rr1 := httptest.NewRecorder()
	h.ServeHTTP(rr1, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	resp1 := mustDecode(t, rr1.Body.String())
	if !contains(modelIDs(resp1), "llama3:70b") {
		t.Fatal("expected llama3:70b from first fetch")
	}

	// Advance time past TTL, then make Ollama return an error.
	now = now.Add(6 * time.Minute)
	status = http.StatusInternalServerError

	// Should serve the stale cached entry (llama3:70b) rather than losing it.
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stale cache should still serve)", rr2.Code)
	}
	resp2 := mustDecode(t, rr2.Body.String())
	if !contains(modelIDs(resp2), "llama3:70b") {
		t.Errorf("stale cache lost llama3:70b after Ollama failure: %v", modelIDs(resp2))
	}
}

func TestModels_OllamaTags_CacheDisabledByZeroTTL(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3:70b"}]}`))
	}))
	t.Cleanup(srv.Close)

	cfg := config.Config{
		LocalModel:            "qwen3-coder:8b",
		OllamaURL:             srv.URL,
		ModelsEndpointEnabled: true,
		ModelsCacheTTL:        0, // disabled → no Ollama round-trip
	}
	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	resp := mustDecode(t, rr.Body.String())
	ids := modelIDs(resp)
	if contains(ids, "llama3:70b") {
		t.Errorf("Ollama model should not be present when cache TTL is 0: %v", ids)
	}
	if callCount != 0 {
		t.Errorf("Ollama called %d times, want 0 (cache disabled)", callCount)
	}
}

func TestModels_OllamaTags_ModifiedAtBecomesCreated(t *testing.T) {
	modified := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	tagsJSON := `{"models":[{"name":"llama3:70b","modified_at":"` + modified.Format(time.RFC3339) + `"}]}`
	srv := mockOllama(t, http.StatusOK, tagsJSON)

	cfg := config.Config{
		LocalModel:            "qwen3-coder:8b",
		OllamaURL:             srv.URL,
		ModelsEndpointEnabled: true,
		ModelsCacheTTL:        5 * time.Minute,
	}
	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})

	req := httptest.NewRequest(http.MethodGet, "/v1/models/llama3:70b", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	m := mustDecodeModel(t, rr.Body.String())
	if m.Created != modified.Unix() {
		t.Errorf("created = %d, want %d (from modified_at)", m.Created, modified.Unix())
	}
}

func TestModels_OllamaTags_UsesNameThenModelField(t *testing.T) {
	// Ollama /api/tags has both "name" and "model" fields; "name" wins.
	tagsJSON := `{"models":[{"name":"name-field","model":"model-field"}]}`
	srv := mockOllama(t, http.StatusOK, tagsJSON)

	cfg := config.Config{
		LocalModel:            "qwen3-coder:8b",
		OllamaURL:             srv.URL,
		ModelsEndpointEnabled: true,
		ModelsCacheTTL:        5 * time.Minute,
	}
	h := Models(ModelsDeps{Config: cfg, Now: fixedClock})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	resp := mustDecode(t, rr.Body.String())
	if !contains(modelIDs(resp), "name-field") {
		t.Errorf("expected name-field (not model-field): %v", modelIDs(resp))
	}
}
