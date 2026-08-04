// Package providers provides the frontier provider registry and the
// ProviderAdapter translation layer introduced in issue #1185.
//
// A ProviderAdapter bridges between the proxy's canonical
// OpenAI-compatible request/response shape and a non-OpenAI provider's
// native API. The proxy's hot path (chat handler, upstream streaming)
// always speaks OpenAI internally; the adapter is the single place that
// knows how to:
//
//   - AuthHeaders: build the auth header set for the upstream request
//     (e.g. x-api-key + anthropic-version for Anthropic, vs the
//     Authorization: Bearer header OpenAI expects).
//   - RequestPath: compute the request URL from a base URL (Anthropic
//     POSTs to /v1/messages, OpenAI to /v1/chat/completions).
//   - TransformRequest: translate the canonical OpenAI request body
//     into the provider's native shape.
//   - NormalizeSSE: wrap the upstream SSE stream so the agent always
//     receives canonical OpenAI chat.completion.chunk events, regardless
//     of the provider's native event schema.
//
// The default adapter (type "openai") is a byte-for-byte no-op so the
// existing OpenAI-compatible path is unchanged.
package providers

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// AnthropicCacheMinSystemChars is the minimum system-field character
// count above which the Anthropic adapter injects a cache_control hint
// (issue #1245). Set via NEXUS_ANTHROPIC_CACHE_MIN_SYSTEM_CHARS; zero
// or negative disables cache-control injection entirely.
var AnthropicCacheMinSystemChars = func() int {
	v := os.Getenv("NEXUS_ANTHROPIC_CACHE_MIN_SYSTEM_CHARS")
	if v == "" {
		return 1024
	}
	n := 0
	for _, c := range v {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else {
			return 1024
		}
	}
	return n
}()

// AzureContentFilterEnabled reports whether the Azure adapter should
// detect content_filter finish reasons and map them to structured
// OpenAI error frames (issue #1245). Set via
// NEXUS_AZURE_CONTENT_FILTER_ENABLED (default false).
var AzureContentFilterEnabled = func() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("NEXUS_AZURE_CONTENT_FILTER_ENABLED")))
	return v == "true" || v == "1" || v == "yes"
}()

// AdapterTypeOpenAI is the canonical no-op adapter. It is the default
// for any provider that omits an explicit type.
const AdapterTypeOpenAI = "openai"

// AdapterTypeAnthropic selects the Anthropic Messages API adapter.
const AdapterTypeAnthropic = "anthropic"

// AdapterTypeAzure selects the Azure OpenAI adapter.
const AdapterTypeAzure = "azure"

// AdapterTypeGemini selects the Google Gemini adapter.
const AdapterTypeGemini = "gemini"

// DefaultAdapterType is the type used when a provider does not specify
// one. It preserves the pre-issue-#1185 byte-for-byte OpenAI path.
const DefaultAdapterType = AdapterTypeOpenAI

// anthropicAPIVersion is the Anthropic API version header value sent on
// every request. Pinned to a stable date-stamped release so a future
// Anthropic schema bump does not silently break the adapter.
const anthropicAPIVersion = "2023-06-01"

// validAdapterTypes is the closed set of adapter types the config layer
// accepts. Adding a value here without registering an implementation in
// NewAdapter would produce a runtime panic; NewAdapter guards against
// that by looking the type up in the adapter registry.
var validAdapterTypes = map[string]struct{}{
	AdapterTypeOpenAI:    {},
	AdapterTypeAnthropic: {},
	AdapterTypeAzure:     {},
	AdapterTypeGemini:    {},
}

// ValidAdapterTypes returns the set of adapter type names the proxy
// accepts in NEXUS_PROVIDER_<NAME>_TYPE / YAML `type`. Used by config
// validation to reject unknown types at boot.
func ValidAdapterTypes() []string {
	out := make([]string, 0, len(validAdapterTypes))
	for t := range validAdapterTypes {
		out = append(out, t)
	}
	return out
}

// IsValidAdapterType reports whether name is a recognised adapter type.
func IsValidAdapterType(name string) bool {
	_, ok := validAdapterTypes[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// ProviderAdapter translates between the proxy's canonical OpenAI
// request/response shape and a non-OpenAI provider's native API. The
// interface is deliberately small: four methods cover the full surface
// the hot path needs, and each method is pure (no I/O, no shared state)
// so adapters are safe to invoke concurrently from multiple goroutines.
//
// Implementations must be safe for concurrent use.
type ProviderAdapter interface {
	// Type returns the adapter's canonical name (one of the
	// AdapterType* constants). Used for logging and telemetry.
	Type() string

	// AuthHeaders returns the HTTP headers the proxy must attach to the
	// upstream request for authentication (and any required metadata,
	// e.g. anthropic-version). The proxy merges these with its own
	// Content-Type / traceparent headers; adapter headers take
	// precedence for keys they set. An empty apiKey yields whatever
	// headers the provider needs for an unauthenticated request (often
	// none).
	AuthHeaders(apiKey string) http.Header

	// RequestPath returns the full upstream request URL given a base
	// URL. For OpenAI this is the baseURL unchanged; for Anthropic it
	// appends /v1/messages, for Gemini it appends the model-specific
	// streamGenerateContent path.
	RequestPath(baseURL string) string

	// TransformRequest translates the canonical OpenAI chat-completions
	// request body into the provider's native shape. The input is the
	// raw JSON the proxy would have POSTed to an OpenAI-compatible
	// endpoint. Adapters that share OpenAI's schema (openai, azure)
	// return the body unchanged.
	TransformRequest(body []byte) ([]byte, error)

	// NormalizeSSE wraps the upstream response body so the caller reads
	// a stream of canonical OpenAI chat.completion.chunk SSE frames,
	// regardless of the provider's native event schema. The OpenAI
	// adapter returns the reader unchanged (byte-identical passthrough).
	NormalizeSSE(r io.Reader) io.Reader
}

// NewAdapter returns the adapter for the given type name. An empty name
// resolves to the default (openai). An unknown name returns an error so
// misconfiguration fails at boot rather than producing a silent no-op.
func NewAdapter(typeName string) (ProviderAdapter, error) {
	name := strings.ToLower(strings.TrimSpace(typeName))
	if name == "" {
		name = DefaultAdapterType
	}
	switch name {
	case AdapterTypeOpenAI:
		return openAIAdapter{}, nil
	case AdapterTypeAnthropic:
		return anthropicAdapter{}, nil
	case AdapterTypeAzure:
		return azureAdapter{}, nil
	case AdapterTypeGemini:
		return geminiAdapter{}, nil
	default:
		return nil, fmt.Errorf("providers: unknown adapter type %q (valid: %s)",
			typeName, strings.Join(ValidAdapterTypes(), ", "))
	}
}

// --- OpenAI (no-op) -------------------------------------------------------

// openAIAdapter is the byte-for-byte no-op adapter. It preserves the
// pre-issue-#1185 behaviour exactly: Authorization: Bearer auth, the
// baseURL as the request path, the body unchanged, and the SSE stream
// passed through verbatim.
type openAIAdapter struct{}

func (openAIAdapter) Type() string { return AdapterTypeOpenAI }

// AuthHeaders returns the Authorization: Bearer header. Empty apiKey
// yields no auth header (matches the existing "skip when key empty"
// behaviour in the upstream layer).
func (openAIAdapter) AuthHeaders(apiKey string) http.Header {
	h := http.Header{}
	if apiKey != "" {
		h.Set("Authorization", "Bearer "+apiKey)
	}
	return h
}

func (openAIAdapter) RequestPath(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/v1/chat/completions"
}

func (openAIAdapter) TransformRequest(body []byte) ([]byte, error) { return body, nil }

func (openAIAdapter) NormalizeSSE(r io.Reader) io.Reader { return r }

// --- Anthropic ------------------------------------------------------------

// anthropicAdapter translates to/from the Anthropic Messages API
// (https://docs.anthropic.com/en/api/messages). Anthropic authenticates
// with x-api-key + anthropic-version headers (not Bearer), POSTs to
// /v1/messages, and streams a distinct SSE event schema that this
// adapter normalises back to OpenAI chat.completion.chunk frames.
type anthropicAdapter struct{}

func (anthropicAdapter) Type() string { return AdapterTypeAnthropic }

// AuthHeaders returns the x-api-key + anthropic-version headers.
func (anthropicAdapter) AuthHeaders(apiKey string) http.Header {
	h := http.Header{}
	if apiKey != "" {
		h.Set("x-api-key", apiKey)
	}
	h.Set("anthropic-version", anthropicAPIVersion)
	return h
}

// RequestPath appends the Messages API path to the base URL.
func (anthropicAdapter) RequestPath(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/v1/messages"
}

// anthropicRequest is the subset of the Anthropic Messages API request
// body TransformRequest emits. It mirrors the OpenAI fields the proxy
// already populates (model, messages, stream, temperature, max_tokens).
//
// The System field uses json.RawMessage so TransformRequest can emit
// either a plain string (the default) or an array of content blocks
// carrying cache_control hints (issue #1245).
type anthropicRequest struct {
	Model       string             `json:"model"`
	Messages    []anthropicMessage `json:"messages"`
	System      json.RawMessage    `json:"system,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	MaxTokens   int                `json:"max_tokens,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicSystemBlock is a single block in Anthropic's array-form system
// field (issue #1245). The cache_control field is only set on the final
// block to enable Anthropic prompt caching on the entire system prefix.
type anthropicSystemBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicCacheControl represents Anthropic's cache_control object.
type anthropicCacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// openAIRequest is the subset of the OpenAI chat-completions request
// body TransformRequest reads. Only the fields the proxy populates are
// decoded; unknown fields are dropped (the proxy owns the request body).
type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Stream      bool            `json:"stream"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	// MaxTokens alternatives across SDKs.
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// stringContent flattens an OpenAI message content field (which may be a
// plain string or an array of content parts) into a single string. The
// proxy's canonical requests use plain strings; the array form is
// supported defensively so a client sending multi-part content does not
// lose data.
func stringContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if m, ok := part.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

// TransformRequest converts an OpenAI chat-completions request body into
// the Anthropic Messages API shape. System messages are hoisted into the
// top-level `system` field (Anthropic does not accept a system role in
// the messages array); all other messages are passed through with their
// content flattened to a string.
func (anthropicAdapter) TransformRequest(body []byte) ([]byte, error) {
	var req openAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("anthropic adapter: parse request: %w", err)
	}

	out := anthropicRequest{
		Model:       req.Model,
		Stream:      req.Stream,
		Temperature: req.Temperature,
	}
	maxTok := req.MaxTokens
	if maxTok == 0 {
		maxTok = req.MaxCompletionTokens
	}
	out.MaxTokens = maxTok

	var sysParts []string
	for _, m := range req.Messages {
		text := stringContent(m.Content)
		if strings.EqualFold(m.Role, "system") {
			if text != "" {
				sysParts = append(sysParts, text)
			}
			continue
		}
		role := m.Role
		if role == "" {
			role = "user"
		}
		out.Messages = append(out.Messages, anthropicMessage{Role: role, Content: text})
	}
	if len(sysParts) > 0 {
		systemText := strings.Join(sysParts, "\n\n")
		if AnthropicCacheMinSystemChars > 0 && len(systemText) >= AnthropicCacheMinSystemChars {
			// Emit system as an array of content blocks with
			// cache_control: {type: ephemeral} on the final block
			// (issue #1245). Anthropic caches everything up to and
			// including the cache_control breakpoint.
			blocks := make([]anthropicSystemBlock, 0, len(sysParts))
			for i, part := range sysParts {
				block := anthropicSystemBlock{Type: "text", Text: part}
				if i == len(sysParts)-1 {
					block.CacheControl = &anthropicCacheControl{Type: "ephemeral"}
				}
				blocks = append(blocks, block)
			}
			raw, err := json.Marshal(blocks)
			if err != nil {
				return nil, fmt.Errorf("anthropic adapter: marshal system blocks: %w", err)
			}
			out.System = raw
		} else {
			// Below threshold: emit as plain string (backward
			// compatible and avoids the array-form overhead).
			raw, _ := json.Marshal(systemText)
			out.System = raw
		}
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("anthropic adapter: marshal request: %w", err)
	}
	return encoded, nil
}

// NormalizeSSE wraps an Anthropic SSE stream and yields canonical OpenAI
// chat.completion.chunk frames. Anthropic emits typed events
// (content_block_delta, message_delta, message_stop); the adapter maps
// the text deltas into OpenAI choice deltas and synthesises a final
// chunk carrying the finish_reason plus the [DONE] sentinel.
func (a anthropicAdapter) NormalizeSSE(r io.Reader) io.Reader {
	return &anthropicSSENormalizer{
		source:  bufio.NewReader(r),
		chunkID: fmt.Sprintf("chatcmpl-nexus-%d", time.Now().UnixNano()),
		created: time.Now().Unix(),
	}
}

// anthropicSSENormalizer reads Anthropic SSE line-by-line and writes
// OpenAI SSE frames to an internal pipe. The first read on the pipe
// blocks until the goroutine fills it; subsequent reads stream. This
// keeps the public surface a plain io.Reader (no caller-visible
// buffering) while still doing incremental conversion.
type anthropicSSENormalizer struct {
	source  *bufio.Reader
	chunkID string
	created int64
	model   string

	// pipe pair; lazily initialised on first Read so the converter
	// goroutine starts exactly once.
	pr   *io.PipeReader
	pw   *io.PipeWriter
	once bool
}

func (n *anthropicSSENormalizer) start() {
	n.pr, n.pw = io.Pipe()
	go n.convert()
}

// convert reads the Anthropic stream and writes OpenAI SSE frames to the
// pipe writer. It closes the pipe when the source is exhausted or on the
// first conversion error (errors are terminal — the agent receives a
// clean EOF).
func (n *anthropicSSENormalizer) convert() {
	defer func() { _ = n.pw.Close() }()
	finishReason := "stop"
	emitted := false
	for {
		line, err := n.source.ReadString('\n')
		if line != "" {
			if frame, ok := n.translateLine(line, &finishReason); ok {
				if _, werr := io.WriteString(n.pw, frame); werr != nil {
					return
				}
				emitted = true
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return
		}
	}
	// Emit a final empty-delta chunk carrying the finish_reason, then the
	// OpenAI [DONE] sentinel — but only if we emitted at least one content
	// chunk. This matches the OpenAI stream terminator shape.
	if emitted {
		final := n.finalChunk(finishReason)
		if _, werr := io.WriteString(n.pw, final); werr != nil {
			return
		}
		_, _ = io.WriteString(n.pw, "data: [DONE]\n\n")
	}
}

// translateLine inspects a single Anthropic SSE line and, when it is a
// data: line carrying a JSON payload, returns the equivalent OpenAI SSE
// frame. Lines that do not produce content (event: lines, comments, or
// Anthropic control events like message_start) are silently consumed.
func (n *anthropicSSENormalizer) translateLine(line string, finishReason *string) (string, bool) {
	trimmed := strings.TrimRight(line, "\r\n")
	if trimmed == "" || strings.HasPrefix(trimmed, ":") || strings.HasPrefix(trimmed, "event:") {
		return "", false
	}
	if !strings.HasPrefix(trimmed, "data:") {
		return "", false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return "", false
	}
	var ev anthropicEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return "", false
	}
	if ev.Model != "" {
		n.model = ev.Model
	}
	switch ev.Type {
	case "content_block_delta":
		if ev.Delta == nil {
			return "", false
		}
		if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
			return n.contentChunk(ev.Delta.Text), true
		}
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			*finishReason = mapAnthropicStopReason(ev.Delta.StopReason)
		}
	}
	return "", false
}

// mapAnthropicStopReason converts Anthropic's stop_reason into the
// OpenAI finish_reason vocabulary. Unrecognised values map to "stop"
// (the safe default).
func mapAnthropicStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// contentChunk builds an OpenAI chat.completion.chunk SSE frame for a
// text content delta.
func (n *anthropicSSENormalizer) contentChunk(text string) string {
	return n.chunk([]map[string]any{{
		"index":         0,
		"delta":         map[string]any{"content": text},
		"finish_reason": nil,
	}})
}

// finalChunk builds the terminal OpenAI chunk carrying the finish_reason
// with an empty delta.
func (n *anthropicSSENormalizer) finalChunk(reason string) string {
	return n.chunk([]map[string]any{{
		"index":         0,
		"delta":         map[string]any{},
		"finish_reason": reason,
	}})
}

func (n *anthropicSSENormalizer) chunk(choices any) string {
	obj := map[string]any{
		"id":      n.chunkID,
		"object":  "chat.completion.chunk",
		"created": n.created,
		"model":   n.model,
		"choices": choices,
	}
	encoded, _ := json.Marshal(obj)
	return "data: " + string(encoded) + "\n\n"
}

// Read implements io.Reader, lazily starting the converter goroutine on
// the first call.
func (n *anthropicSSENormalizer) Read(p []byte) (int, error) {
	if !n.once {
		n.once = true
		n.start()
	}
	return n.pr.Read(p)
}

// anthropicEvent is the subset of Anthropic SSE event payloads the
// normalizer inspects.
type anthropicEvent struct {
	Type  string               `json:"type"`
	Model string               `json:"model"`
	Delta *anthropicEventDelta `json:"delta"`
}

type anthropicEventDelta struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	StopReason string `json:"stop_reason"`
}

// --- Azure OpenAI ---------------------------------------------------------

// azureAdapter targets Azure OpenAI. Azure authenticates with an
// `api-key` header (not Bearer) and uses deployment-specific paths, but
// otherwise shares OpenAI's request/response schema — so TransformRequest
// and NormalizeSSE are no-ops.
type azureAdapter struct{}

func (azureAdapter) Type() string { return AdapterTypeAzure }

// AuthHeaders returns the api-key header. Empty apiKey yields no auth
// header.
func (azureAdapter) AuthHeaders(apiKey string) http.Header {
	h := http.Header{}
	if apiKey != "" {
		h.Set("api-key", apiKey)
	}
	return h
}

func (azureAdapter) RequestPath(baseURL string) string { return baseURL }

func (azureAdapter) TransformRequest(body []byte) ([]byte, error) { return body, nil }

// NormalizeSSE wraps the Azure SSE stream to detect content_filter
// finish reasons and map them to structured OpenAI error frames
// (issue #1245). When NEXUS_AZURE_CONTENT_FILTER_ENABLED is false (the
// default), the reader is returned unchanged.
func (azureAdapter) NormalizeSSE(r io.Reader) io.Reader {
	if !AzureContentFilterEnabled {
		return r
	}
	return &azureContentFilterNormalizer{
		source: bufio.NewReader(r),
	}
}

// azureContentFilterNormalizer reads an Azure OpenAI SSE stream and
// rewrites finish_reason "content_filter" into an OpenAI-shaped error
// frame. All other frames pass through unchanged.
type azureContentFilterNormalizer struct {
	source *bufio.Reader
	pr     *io.PipeReader
	pw     *io.PipeWriter
	once   bool
}

func (n *azureContentFilterNormalizer) Read(p []byte) (int, error) {
	if !n.once {
		n.once = true
		n.pr, n.pw = io.Pipe()
		go n.convert()
	}
	return n.pr.Read(p)
}

// convert reads the Azure SSE stream line-by-line. When it encounters
// a chunk with finish_reason "content_filter", it emits an OpenAI error
// frame instead. The stream continues after the error frame so the
// caller's SSE loop can terminate cleanly.
func (n *azureContentFilterNormalizer) convert() {
	defer func() { _ = n.pw.Close() }()

	for {
		line, err := n.source.ReadString('\n')
		if line != "" {
			// Check for content_filter finish reason.
			if rewritten, ok := n.rewriteContentFilter(line); ok {
				if _, werr := io.WriteString(n.pw, rewritten); werr != nil {
					return
				}
			} else {
				if _, werr := io.WriteString(n.pw, line); werr != nil {
					return
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return
		}
	}
}

// rewriteContentFilter inspects an SSE data line for a content_filter
// finish_reason. When detected, it returns an error frame and a [DONE]
// sentinel; otherwise it returns ("", false).
func (n *azureContentFilterNormalizer) rewriteContentFilter(line string) (string, bool) {
	trimmed := strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(trimmed, "data: ") {
		return "", false
	}
	payload := strings.TrimPrefix(trimmed, "data: ")
	if payload == "[DONE]" || payload == "" {
		return "", false
	}

	var chunk map[string]any
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return "", false
	}

	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return "", false
	}
	choice, _ := choices[0].(map[string]any)
	fr, _ := choice["finish_reason"].(string)
	if fr != "content_filter" {
		return "", false
	}

	// Log the content_filter detection at debug level.
	slog.Debug("azure adapter: detected content_filter finish_reason, mapping to error frame",
		"model", chunk["model"],
	)

	// Emit an OpenAI error frame matching the OpenAI error shape.
	errFrame := map[string]any{
		"error": map[string]any{
			"message": "Response was filtered due to content policy. The model's output was blocked by Azure content filtering.",
			"type":    "content_filter",
			"code":    "content_filter",
		},
		"object": "error",
	}
	errBody, _ := json.Marshal(errFrame)

	// Emit a stop chunk with finish_reason "content_filter" (so the
	// caller's SSE loop sees a proper finish), then the error frame,
	// then [DONE].
	var b strings.Builder
	b.WriteString(line) // original content_filter chunk (transparent)
	b.WriteString("\n")
	b.WriteString("data: " + string(errBody) + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String(), true
}

// --- Google Gemini --------------------------------------------------------

// geminiAdapter targets the Google Gemini native API. Gemini authenticates
// with an `x-goog-api-key` header (query-string keys are also supported by
// the API but a header avoids logging the key in access logs) and POSTs to
// a model-scoped streamGenerateContent path.
type geminiAdapter struct{}

func (geminiAdapter) Type() string { return AdapterTypeGemini }

// AuthHeaders returns the x-goog-api-key header.
func (geminiAdapter) AuthHeaders(apiKey string) http.Header {
	h := http.Header{}
	if apiKey != "" {
		h.Set("x-goog-api-key", apiKey)
	}
	return h
}

// RequestPath appends the Gemini streamGenerateContent path. Gemini's
// native path embeds the model name; the adapter expects the baseURL to
// carry the model in its path (operators configure the full base
// including the model). When the base already ends with a
// streamGenerateContent call we leave it untouched so an operator who
// supplied a complete URL is honoured verbatim.
func (geminiAdapter) RequestPath(baseURL string) string {
	if strings.Contains(baseURL, ":streamGenerateContent") ||
		strings.Contains(baseURL, ":generateContent") {
		return baseURL
	}
	return strings.TrimRight(baseURL, "/") + ":streamGenerateContent"
}

// TransformRequest is a no-op for now: Gemini's native schema differs
// significantly from OpenAI's and a faithful translation is tracked
// separately. The adapter is registered so operators can select
// type=gemini for auth + path while the request body translation lands
// incrementally. The body is passed through unchanged so an
// OpenAI-compatible Gemini gateway still works.
func (geminiAdapter) TransformRequest(body []byte) ([]byte, error) { return body, nil }

// NormalizeSSE is a no-op (see TransformRequest note).
func (geminiAdapter) NormalizeSSE(r io.Reader) io.Reader { return r }
