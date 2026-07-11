// Package handlers — models.go serves the OpenAI-compatible
// GET /v1/models and GET /v1/models/{id} discovery endpoints (issue #78).
//
// The list always includes the configured local, router, and frontier
// models. When ModelsCacheTTL > 0 the handler also polls Ollama
// /api/tags on that cadence and merges any models Ollama reports that
// are not already in the configured set. Ollama failures are silent:
// the configured models are returned without the Ollama supplement so
// a local-down deployment still serves a useful list. No provider
// secrets (API keys, bearer tokens) ever appear in the response — the
// Model object carries only id / object / created / owned_by.

package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/anchapin/nexus-proxy/internal/config"
)

// ModelsDeps carries the collaborators the /v1/models handler needs.
type ModelsDeps struct {
	Config config.Config
	Client *http.Client    // shared HTTP client for Ollama /api/tags; nil → http.DefaultClient
	Now    func() time.Time // injectable clock for tests; nil → time.Now
}

// Model is one entry in the OpenAI-compatible /v1/models response.
// Mirrors https://platform.openai.com/docs/api-reference/models/object.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelsListResponse is the OpenAI-compatible list envelope returned
// by GET /v1/models.
type ModelsListResponse struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// ollamaTagsResponse is the subset of Ollama's /api/tags payload we read.
type ollamaTagsResponse struct {
	Models []struct {
		Name       string    `json:"name"`
		Model      string    `json:"model"`
		ModifiedAt time.Time `json:"modified_at"`
	} `json:"models"`
}

// tagsCache holds the last successful Ollama /api/tags fetch so the
// models list does not incur a round-trip on every request. Safe for
// concurrent use — the handler is hit from many goroutines.
type tagsCache struct {
	mu        sync.Mutex
	models    []Model
	fetchedAt time.Time
}

// Models returns an http.HandlerFunc that serves OpenAI-compatible
// GET /v1/models (list all) and GET /v1/models/{id} (single model).
// The same handler is wired under both "/v1/models" and "/v1/models/"
// in cmd/nexus/main.go; the path suffix distinguishes list vs single.
//
// When ModelsEndpointEnabled is false the handler returns 404 for
// every request so the endpoints effectively disappear.
func Models(deps ModelsDeps) http.HandlerFunc {
	if deps.Client == nil {
		deps.Client = http.DefaultClient
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	cache := &tagsCache{}

	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.Config.ModelsEndpointEnabled {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeModelsError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		// /v1/models       → id == "" → list
		// /v1/models/{id}  → id != "" → single
		id := strings.TrimPrefix(r.URL.Path, "/v1/models")
		id = strings.TrimPrefix(id, "/")

		all := buildModelsList(deps, cache)

		if id == "" {
			writeModelsJSON(w, http.StatusOK, ModelsListResponse{
				Object: "list",
				Data:   all,
			})
			return
		}

		for _, m := range all {
			if m.ID == id {
				writeModelsJSON(w, http.StatusOK, m)
				return
			}
		}
		writeModelsError(w, http.StatusNotFound,
			fmt.Sprintf("The model '%s' does not exist", id))
	}
}

// buildModelsList assembles the full model list: configured models
// always present, optionally supplemented by cached Ollama /api/tags.
// IDs are deduplicated so a model that is both configured and pulled
// in Ollama appears only once.
func buildModelsList(deps ModelsDeps, cache *tagsCache) []Model {
	now := deps.Now().Unix()
	seen := make(map[string]bool)
	out := make([]Model, 0, 4)

	// add appends a model unless its ID was already seen (dedup).
	// The caller supplies the created timestamp so Ollama-sourced
	// models can carry their real modified_at while configured
	// models fall back to the handler's clock.
	add := func(id, ownedBy string, created int64) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, Model{
			ID:      id,
			Object:  "model",
			Created: created,
			OwnedBy: ownedBy,
		})
	}

	// Configured models are always present, even when Ollama is down.
	add(deps.Config.LocalModel, "ollama", now)
	add(deps.Config.RouterModel, "ollama", now)
	add(deps.Config.FrontierModel, "frontier", now)
	add(deps.Config.ZAIModel, "zai", now)

	// Optional Ollama /api/tags supplement (cached per ModelsCacheTTL).
	if deps.Config.ModelsCacheEnabled() {
		for _, m := range cache.get(deps) {
			add(m.ID, m.OwnedBy, m.Created)
		}
	}

	return out
}

// get returns the cached Ollama tags, refreshing from /api/tags when
// the cache is older than ModelsCacheTTL. On fetch failure the stale
// cache is returned (if any) so a transient Ollama outage does not
// degrade the list. The mutex serialises refreshes; concurrent callers
// that arrive during a refresh simply wait and get the fresh result.
func (c *tagsCache) get(deps ModelsDeps) []Model {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := deps.Now()
	if !c.fetchedAt.IsZero() && now.Sub(c.fetchedAt) < deps.Config.ModelsCacheTTL {
		return c.models
	}

	fetched, err := fetchOllamaTags(deps)
	if err != nil {
		// Ollama is unreachable or returned an error. Serve the stale
		// cache if we have one; otherwise return nil so the caller
		// just sees the configured models.
		slog.Debug("ollama /api/tags fetch failed; serving configured models",
			slog.Any("err", err),
			slog.String("ollama_url", deps.Config.OllamaURL),
		)
		return c.models
	}
	c.models = fetched
	c.fetchedAt = now
	return fetched
}

// fetchOllamaTags issues a GET to {OllamaURL}/api/tags and decodes the
// model list into Model objects. The response body is capped at 4 MiB
// to prevent a runaway Ollama instance from exhausting proxy memory.
func fetchOllamaTags(deps ModelsDeps) ([]Model, error) {
	url := strings.TrimRight(deps.Config.OllamaURL, "/") + "/api/tags"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := deps.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama /api/tags: HTTP %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		return nil, err
	}

	var tags ollamaTagsResponse
	if err := json.Unmarshal(body, &tags); err != nil {
		return nil, fmt.Errorf("ollama /api/tags: decode: %w", err)
	}

	out := make([]Model, 0, len(tags.Models))
	for _, m := range tags.Models {
		id := m.Name
		if id == "" {
			id = m.Model
		}
		if id == "" {
			continue
		}
		var created int64
		if !m.ModifiedAt.IsZero() {
			created = m.ModifiedAt.Unix()
		}
		out = append(out, Model{
			ID:      id,
			Object:  "model",
			Created: created,
			OwnedBy: "ollama",
		})
	}
	return out, nil
}

// writeModelsJSON encodes v as JSON with a 200-class status. Used for
// successful list and single-model responses.
func writeModelsJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("models handler: json encode", slog.Any("err", err))
	}
}

// writeModelsError writes an OpenAI-compatible JSON error object with
// the given status code. This keeps error responses machine-parseable
// for clients that expect the OpenAI error envelope.
func writeModelsError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "invalid_request_error",
		},
	})
}
