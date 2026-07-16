package router

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SLMClient talks to a local Ollama /api/chat endpoint and asks the small
// model to produce a routing decision. The HTTP layer is abstracted so
// tests can substitute a deterministic stub.
type SLMClient struct {
	BaseURL string        // e.g. "http://localhost:11434"
	Model   string        // e.g. "qwen3-coder:4b"
	Timeout time.Duration // per-call timeout (default 8s)
	Client  *http.Client

	// ConfidenceFloor / ConfidenceCeiling bound the neutral band for
	// judge-guided adaptive routing (issue #47). When the empirical
	// local confidence passed to DecideWithConfidence is below the floor
	// the system prompt is augmented with a frontier bias; above the
	// ceiling it gets a local bias; inside the band the request is
	// byte-for-byte identical to the pre-issue-47 path. Zero values fall
	// back to DefaultConfidenceFloor / DefaultConfidenceCeiling.
	ConfidenceFloor   float64
	ConfidenceCeiling float64

	// SLM routing decision cache (issue #162). Caches identical prompts
	// keyed by FNV-1a hash of (prompt, systemPrompt) so that repeated
	// prompts (e.g. looping coding agents, repeated RAG queries) avoid
	// a full 8s Ollama round-trip. cacheTTL controls entry expiry;
	// cacheMaxEntries caps memory use with LRU eviction.
	CacheTTL        time.Duration // default 5m
	CacheMaxEntries int           // default 512

	cacheMu   sync.RWMutex
	cacheList *list.List               // LRU list: front=MRU, back=LRU
	cacheMap  map[string]*list.Element // key -> list element for O(1) lookup
	hits      int64
	misses    int64
}

// cacheEntry pairs a route decision with its expiry time.
// Key is stored so we can delete from cacheMap when this entry is evicted.
type cacheEntry struct {
	Key       string
	Route     Route
	ExpiresAt time.Time
}

// NewSLMClient constructs a client. Pass nil for Client to use
// http.DefaultClient.
func NewSLMClient(baseURL, model string, timeout time.Duration, client *http.Client) *SLMClient {
	if client == nil {
		client = http.DefaultClient
	}
	return &SLMClient{
		BaseURL:         baseURL,
		Model:           model,
		Timeout:         timeout,
		Client:          client,
		CacheTTL:        5 * time.Minute,
		CacheMaxEntries: 512,
		cacheList:       list.New(),
		cacheMap:        make(map[string]*list.Element),
	}
}

// WithCache sets the cache TTL and max-entries cap and returns the
// same client for chaining. Call on the return value of NewSLMClient:
//   - WithCache(0, 0)  disables caching
//   - WithCache(10, 5*time.Minute) enables a 10-entry TTL cache
func (c *SLMClient) WithCache(maxEntries int, ttl time.Duration) *SLMClient {
	c.CacheMaxEntries = maxEntries
	c.CacheTTL = ttl
	if maxEntries > 0 {
		c.cacheList = list.New()
		c.cacheMap = make(map[string]*list.Element)
	}
	return c
}

// slmSystemPrompt is the static instruction we send to the routing SLM.
// Keeping it as a package var (not a config field) makes it trivial to grep
// and to snapshot in tests.
const slmSystemPrompt = `You are an intelligent routing assistant for a coding agent proxy. 
    Analyze the user's prompt. 
    - If it is a simple task (boilerplate, styling, small isolated functions), output {"route": "local"}. 
    - If it is a complex task (deep debugging, multi-file refactoring), output {"route": "frontier"}. 
    - If it requires extreme architectural deliberation and planning, output {"route": "fusion"}.
	Respond ONLY in valid JSON. No explanations.`

// negativeBiasNote is appended to slmSystemPrompt when empirical local
// confidence for the task category is below the floor. It nudges the SLM
// toward frontier without hard-overriding its judgement — the SLM may
// still pick local for a trivially simple prompt.
const negativeBiasNote = `

ADAPTIVE ROUTING CONTEXT: Historical quality evaluations show the LOCAL model has performed POORLY on tasks similar to this one. Strongly prefer {"route": "frontier"} unless the task is trivially simple.`

// positiveBiasNote is appended when empirical local confidence is above the
// ceiling: the local model has a strong track record on this kind of task,
// so favour it when the request is not clearly complex.
const positiveBiasNote = `

ADAPTIVE ROUTING CONTEXT: Historical quality evaluations show the LOCAL model handles tasks similar to this one WELL. Prefer {"route": "local"} when the task is not clearly complex.`

// Decide returns the routing decision for prompt. It is the neutral-path
// entry point: equivalent to DecideWithConfidence with NeutralConfidence,
// so the SLM request is byte-for-byte identical to the pre-issue-47
// behaviour. The fallback on any failure (transport, decode, parse,
// unknown value) is RouteFrontier — that is the safest default because it
// never silently drops a request to a non-existent local model.
func (c *SLMClient) Decide(ctx context.Context, prompt string) (Route, error) {
	return c.DecideWithConfidence(ctx, prompt, NeutralConfidence)
}

// DecideWithConfidence is Decide augmented with the empirical local
// confidence signal (issue #47). confidence is a 0.0..1.0 estimate of how
// well the local model performs on prompts like this one, derived from
// historical judge scores (see ConfidenceStore). Below the floor the
// system prompt gains a frontier bias; above the ceiling a local bias;
// inside the neutral band the request is unchanged from Decide.
func (c *SLMClient) DecideWithConfidence(ctx context.Context, prompt string, confidence float64) (Route, error) {
	return c.decide(ctx, prompt, c.systemPromptFor(confidence))
}

// systemPromptFor returns the SLM system prompt for the given confidence,
// applying the floor/ceiling bias notes. It is separated out so tests can
// assert the exact augmentation without an HTTP round-trip.
func (c *SLMClient) systemPromptFor(confidence float64) string {
	floor := c.ConfidenceFloor
	if floor <= 0 {
		floor = DefaultConfidenceFloor
	}
	ceiling := c.ConfidenceCeiling
	if ceiling <= 0 {
		ceiling = DefaultConfidenceCeiling
	}
	switch {
	case confidence < floor:
		return slmSystemPrompt + negativeBiasNote
	case confidence > ceiling:
		return slmSystemPrompt + positiveBiasNote
	default:
		return slmSystemPrompt
	}
}

// cacheKey returns the FNV-1a hash key for a (prompt, systemPrompt) pair.
// The same logical prompt always produces the same key regardless of
// SLMClient pointer equality, so callers can use this for pre-check.
func cacheKey(prompt, systemPrompt string) string {
	h := fnv.New64a()
	h.Write([]byte(prompt))
	h.Write([]byte(systemPrompt))
	return fmt.Sprintf("%x", h.Sum64())
}

// CacheStats returns the current cache hit and miss counts.
func (c *SLMClient) CacheStats() (hits, misses int64) {
	c.cacheMu.RLock()
	defer c.cacheMu.RUnlock()
	return c.hits, c.misses
}

// evictStale removes expired entries and enforces the max-entries cap via
// true LRU eviction: expired entries are always removed first; if the cache
// is still over capacity after expired entry removal, the least-recently-used
// entry (the back of the LRU list) is evicted. This ensures deterministic
// eviction order regardless of Go map iteration order (issue #275).
func (c *SLMClient) evictStale() {
	now := time.Now()
	max := c.CacheMaxEntries
	if max <= 0 {
		max = 512
	}

	// Phase 1: remove all expired entries by iterating the LRU list.
	// Iterate from back to front so we can safely remove while iterating.
	for e := c.cacheList.Back(); e != nil; e = e.Prev() {
		entry := e.Value.(*cacheEntry)
		if now.After(entry.ExpiresAt) {
			delete(c.cacheMap, entry.Key)
			c.cacheList.Remove(e)
		}
	}

	// Phase 2: if still over capacity, evict LRU entries from the back.
	if c.cacheList.Len() >= max {
		evict := c.cacheList.Len() - max + 1
		for i := 0; i < evict; i++ {
			lruElem := c.cacheList.Back()
			if lruElem == nil {
				break
			}
			entry := lruElem.Value.(*cacheEntry)
			delete(c.cacheMap, entry.Key)
			c.cacheList.Remove(lruElem)
		}
	}
}

// decide performs the HTTP round-trip with the supplied system prompt.
func (c *SLMClient) decide(ctx context.Context, prompt, systemPrompt string) (Route, error) {
	var key string
	caching := c.CacheMaxEntries > 0
	if caching {
		key = cacheKey(prompt, systemPrompt)
		// Fast path: check cache before HTTP call (issue #162).
		c.cacheMu.RLock()
		elem, ok := c.cacheMap[key]
		hit := ok && time.Now().Before(elem.Value.(*cacheEntry).ExpiresAt)
		if hit {
			c.hits++
			// Move to front (MRU position) since this entry was just accessed.
			c.cacheList.MoveToFront(elem)
		}
		c.cacheMu.RUnlock()
		if hit {
			return elem.Value.(*cacheEntry).Route, nil
		}
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": prompt},
		},
		"format":  "json",
		"stream":  false,
		"options": map[string]float64{"temperature": 0.1},
	})

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodPost,
		c.BaseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return RouteFrontier, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Client.Do(req)
	if err != nil {
		return RouteFrontier, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return RouteFrontier, err
	}
	if resp.StatusCode != http.StatusOK {
		return RouteFrontier, fmt.Errorf("slm: status %d: %s", resp.StatusCode, body)
	}

	var raw struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return RouteFrontier, fmt.Errorf("slm: decode: %w", err)
	}
	content := strings.TrimSpace(raw.Message.Content)
	if content == "" {
		return RouteFrontier, errors.New("slm: empty content")
	}
	decision, err := parseSLMDecision(content)
	if err != nil {
		return RouteFrontier, err
	}

	route := Route(strings.ToLower(decision.Route))
	if route != RouteLocal && route != RouteFusion {
		route = RouteFrontier
	}

	// Populate cache on successful HTTP response (issue #162).
	if caching {
		c.cacheMu.Lock()
		c.evictStale()
		ttl := c.CacheTTL
		if ttl <= 0 {
			ttl = 5 * time.Minute
		}
		entry := &cacheEntry{
			Key:       key,
			Route:     route,
			ExpiresAt: time.Now().Add(ttl),
		}
		// Add to front of list (MRU position) and map.
		elem := c.cacheList.PushFront(entry)
		c.cacheMap[key] = elem
		c.misses++
		c.cacheMu.Unlock()
	}

	return route, nil
}

// slmDecision is the parsed route-decision object returned by the routing
// SLM. The model is instructed to emit {"route":"local|frontier|fusion"},
// but it frequently wraps that in markdown fences or prose (issue #79).
// parseSLMDecision below tolerates those shapes so a usable decision is
// not discarded just because of formatting noise.
type slmDecision struct {
	Route string `json:"route"`
}

// parseSLMDecision extracts the route-decision JSON object from the SLM's
// raw response content. It tolerates three common SLM output shapes:
//  1. Bare JSON object: {"route":"local"}
//  2. Markdown-fenced JSON: ```json\n{"route":"local"}\n```
//  3. Prose before/after the first JSON object: Here: {"route":"local"} !
//
// It returns an error only when no valid JSON object can be found, so the
// caller (decide) can fall back to RouteFrontier — the safe default. The
// first balanced JSON object wins: if there are multiple objects only the
// first is considered, so ambiguous/conflicting responses do not silently
// pick a later object (issue #79).
//
// Defaults preserved for the caller: an unspecified or unknown route still
// resolves to RouteFrontier unless the object explicitly names local or
// fusion; confidence clamping and task-type defaults live in the planner
// and are untouched here.
func parseSLMDecision(content string) (slmDecision, error) {
	var d slmDecision
	content = strings.TrimSpace(content)

	// 1. Fast path: the whole content is a bare JSON object.
	if err := json.Unmarshal([]byte(content), &d); err == nil {
		return d, nil
	}

	// 2. Markdown-fenced JSON block (```json ... ``` or ``` ... ```).
	if fenced := extractFencedJSON(content); fenced != "" {
		if err := json.Unmarshal([]byte(fenced), &d); err == nil {
			return d, nil
		}
	}

	// 3. First balanced {...} substring, tolerating prose wrappers and
	// braces that appear inside JSON string literals.
	if obj := extractFirstJSONObject(content); obj != "" {
		if err := json.Unmarshal([]byte(obj), &d); err == nil {
			return d, nil
		}
	}

	return slmDecision{}, fmt.Errorf("slm: no parseable JSON decision in %q", content)
}

// extractFencedJSON returns the trimmed contents of the first markdown
// fenced code block in content, preferring a ```json opener and falling
// back to a bare ``` opener. It returns "" when no complete fenced block
// is present. Matching on the opener is case-insensitive so ```JSON and
// ```Json also work.
func extractFencedJSON(content string) string {
	lower := strings.ToLower(content)
	for _, opener := range []string{"```json", "```"} {
		idx := strings.Index(lower, opener)
		if idx < 0 {
			continue
		}
		start := idx + len(opener)
		// Skip a single trailing newline immediately after the opener so
		// it is not treated as part of the JSON payload.
		if rest := content[start:]; strings.HasPrefix(rest, "\r\n") {
			start += 2
		} else if strings.HasPrefix(rest, "\n") {
			start++
		}
		if start > len(content) {
			continue
		}
		end := strings.Index(content[start:], "```")
		if end < 0 {
			continue
		}
		return strings.TrimSpace(content[start : start+end])
	}
	return ""
}

// extractFirstJSONObject returns the first brace-balanced JSON object
// substring of content, starting at the first '{'. It is string-literal
// aware so braces that appear inside JSON string values do not affect
// balancing. It returns "" when content has no '{' or the braces never
// balance to zero.
func extractFirstJSONObject(content string) string {
	start := strings.IndexByte(content, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(content); i++ {
		c := content[i]
		if escaped {
			escaped = false
			continue
		}
		if inString {
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return content[start : i+1]
			}
		}
	}
	return ""
}

// HTTPPoster is the minimal interface SLMClient needs from an http.Client.
// It exists so tests can swap in fakes without depending on *http.Client.
type HTTPPoster interface {
	Do(req *http.Request) (*http.Response, error)
}
