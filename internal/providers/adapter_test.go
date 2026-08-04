package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewAdapter validates the registry lookup, including the default
// (empty -> openai) and rejection of unknown types (criterion #3).
func TestNewAdapter(t *testing.T) {
	t.Run("empty defaults to openai", func(t *testing.T) {
		a, err := NewAdapter("")
		if err != nil {
			t.Fatalf("NewAdapter(\"\"): %v", err)
		}
		if a.Type() != AdapterTypeOpenAI {
			t.Errorf("Type = %q, want %q", a.Type(), AdapterTypeOpenAI)
		}
	})

	cases := []string{AdapterTypeOpenAI, AdapterTypeAnthropic, AdapterTypeAzure, AdapterTypeGemini}
	for _, tc := range cases {
		tc := tc
		t.Run("known type "+tc, func(t *testing.T) {
			a, err := NewAdapter(tc)
			if err != nil {
				t.Fatalf("NewAdapter(%q): %v", tc, err)
			}
			if a.Type() != tc {
				t.Errorf("Type = %q, want %q", a.Type(), tc)
			}
		})
	}

	t.Run("unknown type errors", func(t *testing.T) {
		_, err := NewAdapter("cohere")
		if err == nil {
			t.Fatal("NewAdapter(\"cohere\"): expected error, got nil")
		}
		if !strings.Contains(err.Error(), "cohere") {
			t.Errorf("error should name the bad type: %v", err)
		}
	})
}

// TestValidAdapterTypes ensures the allowed set is stable so config
// validation and the adapter registry agree.
func TestValidAdapterTypes(t *testing.T) {
	got := ValidAdapterTypes()
	want := map[string]bool{
		"openai": true, "anthropic": true, "azure": true, "gemini": true,
	}
	if len(got) != len(want) {
		t.Fatalf("ValidAdapterTypes = %v, want %d entries", got, len(want))
	}
	for _, v := range got {
		if !want[v] {
			t.Errorf("unexpected type %q in ValidAdapterTypes", v)
		}
	}
}

// TestIsValidAdapterType covers whitespace and casing tolerance so
// NEXUS_PROVIDER_<NAME>_TYPE=Anthropic is accepted.
func TestIsValidAdapterType(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"openai", true},
		{"anthropic", true},
		{"Anthropic", true},
		{"  anthropic  ", true},
		{"", false},
		{"cohere", false},
	}
	for _, tc := range cases {
		if got := IsValidAdapterType(tc.in); got != tc.want {
			t.Errorf("IsValidAdapterType(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// --- OpenAI regression ----------------------------------------------------

// TestOpenAIAdapterByteIdentical asserts the openai adapter is a complete
// no-op (criterion #2): auth header unchanged, path unchanged, body
// unchanged, SSE reader returned verbatim.
func TestOpenAIAdapterByteIdentical(t *testing.T) {
	a, err := NewAdapter(AdapterTypeOpenAI)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}

	// Auth: Bearer header present when key set, absent when empty.
	h := a.AuthHeaders("sk-test")
	if got := h.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", got)
	}
	h2 := a.AuthHeaders("")
	if got := h2.Get("Authorization"); got != "" {
		t.Errorf("empty key Authorization = %q, want empty", got)
	}

	// Path: returned unchanged.
	base := "https://api.openai.com/v1/chat/completions"
	if got := a.RequestPath(base); got != base {
		t.Errorf("RequestPath = %q, want %q", got, base)
	}

	// Body: returned unchanged (byte-identical).
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	out, err := a.TransformRequest(body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	if !bytes.Equal(out, body) {
		t.Errorf("TransformRequest altered body:\n got: %s\nwant: %s", out, body)
	}

	// SSE: reader returned unchanged.
	r := strings.NewReader("data: {\"x\":1}\n\n")
	if a.NormalizeSSE(r) != io.Reader(r) {
		t.Errorf("NormalizeSSE did not return the same reader (identity required for openai)")
	}
}

// --- Anthropic auth + path ------------------------------------------------

func TestAnthropicAdapterAuthHeaders(t *testing.T) {
	a, err := NewAdapter(AdapterTypeAnthropic)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	h := a.AuthHeaders("sk-ant-test")
	if got := h.Get("x-api-key"); got != "sk-ant-test" {
		t.Errorf("x-api-key = %q, want sk-ant-test", got)
	}
	if got := h.Get("anthropic-version"); got != anthropicAPIVersion {
		t.Errorf("anthropic-version = %q, want %q", got, anthropicAPIVersion)
	}
	// No Authorization header for anthropic.
	if got := h.Get("Authorization"); got != "" {
		t.Errorf("anthropic adapter must not set Authorization, got %q", got)
	}

	// Empty key: still sets anthropic-version, no x-api-key.
	h2 := a.AuthHeaders("")
	if got := h2.Get("anthropic-version"); got == "" {
		t.Error("anthropic-version should be set even with empty key")
	}
	if got := h2.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key = %q, want empty for empty key", got)
	}
}

func TestAnthropicAdapterRequestPath(t *testing.T) {
	a, _ := NewAdapter(AdapterTypeAnthropic)
	cases := []struct {
		in, want string
	}{
		{"https://api.anthropic.com", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/v1", "https://api.anthropic.com/v1/v1/messages"},
	}
	for _, tc := range cases {
		if got := a.RequestPath(tc.in); got != tc.want {
			t.Errorf("RequestPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- Anthropic request transform ------------------------------------------

func TestAnthropicAdapterTransformRequest(t *testing.T) {
	a, _ := NewAdapter(AdapterTypeAnthropic)

	temp := 0.7
	req := map[string]any{
		"model":       "claude-opus-4",
		"stream":      true,
		"temperature": temp,
		"max_tokens":  256,
		"messages": []map[string]any{
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "Hello"},
			{"role": "assistant", "content": "Hi there"},
		},
	}
	body, _ := json.Marshal(req)

	out, err := a.TransformRequest(body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}

	var got anthropicRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal transformed body: %v", err)
	}

	if got.Model != "claude-opus-4" {
		t.Errorf("Model = %q, want claude-opus-4", got.Model)
	}
	if !got.Stream {
		t.Error("Stream lost in transform")
	}
	if got.MaxTokens != 256 {
		t.Errorf("MaxTokens = %d, want 256", got.MaxTokens)
	}
	if got.Temperature == nil || *got.Temperature != 0.7 {
		t.Errorf("Temperature = %v, want 0.7", got.Temperature)
	}
	// System hoisted out of messages array. Since System is now
	// json.RawMessage (to support either string or array form),
	// unmarshal it to compare.
	var sysStr string
	if err := json.Unmarshal(got.System, &sysStr); err != nil {
		t.Fatalf("unmarshal system as string: %v (raw: %s)", err, got.System)
	}
	if sysStr != "You are helpful." {
		t.Errorf("System = %q, want %q", sysStr, "You are helpful.")
	}
	// Messages array excludes the system message.
	if len(got.Messages) != 2 {
		t.Fatalf("len(Messages) = %d, want 2 (system hoisted)", len(got.Messages))
	}
	if got.Messages[0].Role != "user" || got.Messages[0].Content != "Hello" {
		t.Errorf("Messages[0] = %+v", got.Messages[0])
	}
	if got.Messages[1].Role != "assistant" || got.Messages[1].Content != "Hi there" {
		t.Errorf("Messages[1] = %+v", got.Messages[1])
	}
}

// TestAnthropicAdapterTransformRequestMultiPartContent verifies that an
// OpenAI multi-part content array is flattened to a string (defensive —
// the proxy's canonical requests use plain strings).
func TestAnthropicAdapterTransformRequestMultiPartContent(t *testing.T) {
	a, _ := NewAdapter(AdapterTypeAnthropic)
	req := map[string]any{
		"model": "claude-sonnet-4",
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": "part1 "},
				{"type": "text", "text": "part2"},
			}},
		},
	}
	body, _ := json.Marshal(req)
	out, err := a.TransformRequest(body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	var got anthropicRequest
	_ = json.Unmarshal(out, &got)
	if len(got.Messages) != 1 || got.Messages[0].Content != "part1 part2" {
		t.Errorf("flattened content = %q", got.Messages[0].Content)
	}
}

// TestAnthropicAdapterTransformRequestInvalidJSON ensures malformed input
// surfaces a clear error rather than a panic.
func TestAnthropicAdapterTransformRequestInvalidJSON(t *testing.T) {
	a, _ := NewAdapter(AdapterTypeAnthropic)
	_, err := a.TransformRequest([]byte("{not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// --- Anthropic SSE normalization (criterion #1) ---------------------------

// TestAnthropicNormalizeSSE runs a realistic Anthropic SSE stream through
// the adapter's NormalizeSSE and asserts the output is canonical OpenAI
// chat.completion.chunk frames ending in [DONE].
func TestAnthropicNormalizeSSE(t *testing.T) {
	a, _ := NewAdapter(AdapterTypeAnthropic)

	// A faithful (if abbreviated) Anthropic streaming transcript.
	anthropicStream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-4"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", world"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
		"",
	}, "\n")

	r := a.NormalizeSSE(strings.NewReader(anthropicStream))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	outStr := string(out)

	// Every non-terminator data line must be a valid OpenAI chunk.
	var contentText strings.Builder
	var sawFinish bool
	dataLines := 0
	for _, line := range strings.Split(outStr, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		dataLines++
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("output frame is not valid JSON: %v\nline: %s", err, line)
		}
		if obj, _ := chunk["object"].(string); obj != "chat.completion.chunk" {
			t.Errorf("object = %q, want chat.completion.chunk", obj)
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) != 1 {
			t.Fatalf("choices len = %d, want 1", len(choices))
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if txt, ok := delta["content"].(string); ok && txt != "" {
			contentText.WriteString(txt)
		}
		if fr, ok := choice["finish_reason"]; ok && fr != nil {
			sawFinish = true
			if fr != "stop" {
				t.Errorf("finish_reason = %v, want stop", fr)
			}
		}
	}

	if got := contentText.String(); got != "Hello, world" {
		t.Errorf("reassembled content = %q, want %q", got, "Hello, world")
	}
	if !sawFinish {
		t.Error("no chunk carried a finish_reason (stream must terminate with one)")
	}
	if !strings.Contains(outStr, "data: [DONE]") {
		t.Error("stream did not terminate with data: [DONE]")
	}
	if dataLines == 0 {
		t.Error("no OpenAI data frames emitted")
	}
}

// TestAnthropicNormalizeSSEStopReasonMapping verifies the Anthropic
// stop_reason -> OpenAI finish_reason mapping across the documented values.
func TestAnthropicNormalizeSSEStopReasonMapping(t *testing.T) {
	cases := []struct{ anthropic, openai string }{
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
		{"unknown_value", "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.anthropic, func(t *testing.T) {
			stream := strings.Join([]string{
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`,
				``,
				`data: {"type":"message_delta","delta":{"stop_reason":"` + tc.anthropic + `"}}`,
				``,
				"",
			}, "\n")
			a, _ := NewAdapter(AdapterTypeAnthropic)
			out, _ := io.ReadAll(a.NormalizeSSE(strings.NewReader(stream)))
			var finish string
			for _, line := range strings.Split(string(out), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				payload := strings.TrimPrefix(line, "data: ")
				if payload == "[DONE]" {
					continue
				}
				var chunk map[string]any
				_ = json.Unmarshal([]byte(payload), &chunk)
				choices, _ := chunk["choices"].([]any)
				if len(choices) == 0 {
					continue
				}
				choice, _ := choices[0].(map[string]any)
				if fr, ok := choice["finish_reason"].(string); ok {
					finish = fr
				}
			}
			if finish != tc.openai {
				t.Errorf("stop_reason %q -> finish_reason %q, want %q", tc.anthropic, finish, tc.openai)
			}
		})
	}
}

// TestAnthropicNormalizeSSEEmptyStream ensures an empty/short upstream
// stream does not panic or emit a spurious [DONE] without any content.
func TestAnthropicNormalizeSSEEmptyStream(t *testing.T) {
	a, _ := NewAdapter(AdapterTypeAnthropic)
	out, err := io.ReadAll(a.NormalizeSSE(strings.NewReader("")))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty stream produced %d bytes, want 0", len(out))
	}
}

// --- End-to-end with httptest mock (criterion #1) -------------------------

// TestAnthropicAdapterEndToEnd spins up an httptest server that speaks the
// Anthropic Messages API and verifies the adapter produces OpenAI-shaped
// output when wired into a realistic request/response flow.
func TestAnthropicAdapterEndToEnd(t *testing.T) {
	var seenHeaders http.Header
	var seenPath string
	var seenBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		seenPath = r.URL.Path
		seenBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		// Minimal Anthropic streaming transcript.
		frames := []string{
			`data: {"type":"message_start","message":{"model":"claude-opus-4"}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
			`data: {"type":"message_stop"}`,
		}
		for _, f := range frames {
			io.WriteString(w, f+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer srv.Close()

	adapter, _ := NewAdapter(AdapterTypeAnthropic)

	// 1) Auth headers carry x-api-key + anthropic-version.
	req, _ := http.NewRequest(http.MethodPost, adapter.RequestPath(srv.URL), nil)
	for k, vs := range adapter.AuthHeaders("sk-ant-e2e") {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}

	// 2) Request body transformed to Anthropic shape.
	origBody, _ := json.Marshal(map[string]any{
		"model": "claude-opus-4",
		"messages": []map[string]any{
			{"role": "user", "content": "ping"},
		},
		"stream": true,
	})
	transformed, err := adapter.TransformRequest(origBody)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(transformed))
	req.ContentLength = int64(len(transformed))
	req.Header.Set("Content-Type", "application/json")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	// 3) Server received x-api-key auth, NOT Bearer.
	if got := seenHeaders.Get("x-api-key"); got != "sk-ant-e2e" {
		t.Errorf("server saw x-api-key = %q, want sk-ant-e2e", got)
	}
	if got := seenHeaders.Get("Authorization"); got != "" {
		t.Errorf("server saw Authorization = %q, want empty (anthropic uses x-api-key)", got)
	}
	if got := seenHeaders.Get("anthropic-version"); got != anthropicAPIVersion {
		t.Errorf("server saw anthropic-version = %q, want %q", got, anthropicAPIVersion)
	}
	// 4) Server received the request at /v1/messages.
	if seenPath != "/v1/messages" {
		t.Errorf("server path = %q, want /v1/messages", seenPath)
	}
	// 5) Server received the transformed (Anthropic-shape) body.
	var srvReq anthropicRequest
	if err := json.Unmarshal(seenBody, &srvReq); err != nil {
		t.Fatalf("server body not anthropic shape: %v\nbody: %s", err, seenBody)
	}
	if len(srvReq.Messages) != 1 || srvReq.Messages[0].Content != "ping" {
		t.Errorf("server body messages = %+v", srvReq.Messages)
	}

	// 6) Response normalized to OpenAI chat.completion.chunk frames.
	normalized := adapter.NormalizeSSE(resp.Body)
	out, err := io.ReadAll(normalized)
	if err != nil {
		t.Fatalf("ReadAll normalized: %v", err)
	}
	if !strings.Contains(string(out), `"chat.completion.chunk"`) {
		t.Errorf("normalized output missing chat.completion.chunk:\n%s", out)
	}
	if !strings.Contains(string(out), `"Hi"`) {
		t.Errorf("normalized output missing content Hi:\n%s", out)
	}
	if !strings.Contains(string(out), "data: [DONE]") {
		t.Error("normalized output missing [DONE] terminator")
	}
}

// --- Azure + Gemini sanity ------------------------------------------------

func TestAzureAdapterAuthHeaders(t *testing.T) {
	a, _ := NewAdapter(AdapterTypeAzure)
	h := a.AuthHeaders("az-key")
	if got := h.Get("api-key"); got != "az-key" {
		t.Errorf("api-key = %q, want az-key", got)
	}
	if got := h.Get("Authorization"); got != "" {
		t.Errorf("azure must not set Authorization, got %q", got)
	}
	// Azure shares OpenAI's body/SSE schema -> no-op.
	body := []byte(`{"x":1}`)
	out, _ := a.TransformRequest(body)
	if !bytes.Equal(out, body) {
		t.Error("azure TransformRequest should be a no-op")
	}
	r := strings.NewReader("s")
	if a.NormalizeSSE(r) != io.Reader(r) {
		t.Error("azure NormalizeSSE should return the reader unchanged")
	}
}

func TestGeminiAdapterAuthAndPath(t *testing.T) {
	a, _ := NewAdapter(AdapterTypeGemini)
	h := a.AuthHeaders("gem-key")
	if got := h.Get("x-goog-api-key"); got != "gem-key" {
		t.Errorf("x-goog-api-key = %q, want gem-key", got)
	}
	// Path appends streamGenerateContent.
	if got := a.RequestPath("https://generativelanguage.googleapis.com/v1beta/models/gemini-pro"); !strings.HasSuffix(got, ":streamGenerateContent") {
		t.Errorf("RequestPath = %q, want suffix :streamGenerateContent", got)
	}
	// Complete URL is honoured verbatim.
	full := "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:streamGenerateContent"
	if got := a.RequestPath(full); got != full {
		t.Errorf("RequestPath(full) = %q, want %q", got, full)
	}
}

// TestGeminiAdapterTransformRequest verifies the TransformRequest method
// correctly maps OpenAI chat-completions requests to Gemini GenerateContent format.
func TestGeminiAdapterTransformRequest(t *testing.T) {
	a, _ := NewAdapter(AdapterTypeGemini)

	temp := 0.7
	req := map[string]any{
		"model":       "gemini-2.0-flash",
		"stream":      true,
		"temperature": temp,
		"max_tokens":  256,
		"messages": []map[string]any{
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "Hello"},
			{"role": "assistant", "content": "Hi there"},
		},
	}
	body, _ := json.Marshal(req)

	out, err := a.TransformRequest(body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}

	var got geminiRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal transformed body: %v", err)
	}

	if len(got.Contents) != 2 {
		t.Fatalf("len(Contents) = %d, want 2 (system hoisted)", len(got.Contents))
	}
	if got.Contents[0].Role != "user" || len(got.Contents[0].Parts) != 1 || got.Contents[0].Parts[0].Text != "Hello" {
		t.Errorf("Contents[0] = %+v", got.Contents[0])
	}
	if got.Contents[1].Role != "assistant" || len(got.Contents[1].Parts) != 1 || got.Contents[1].Parts[0].Text != "Hi there" {
		t.Errorf("Contents[1] = %+v", got.Contents[1])
	}

	// System instruction should be hoisted.
	if got.SystemInstruction == nil || len(got.SystemInstruction.Parts) != 1 {
		t.Fatalf("SystemInstruction = %v, want non-nil with 1 part", got.SystemInstruction)
	}
	if got.SystemInstruction.Parts[0].Text != "You are a helpful assistant." {
		t.Errorf("SystemInstruction.Parts[0].Text = %q, want %q", got.SystemInstruction.Parts[0].Text, "You are a helpful assistant.")
	}

	// GenerationConfig.
	if got.GenerationConfig == nil {
		t.Fatalf("GenerationConfig = nil, want non-nil")
	}
	if *got.GenerationConfig.Temperature != temp {
		t.Errorf("Temperature = %v, want %v", *got.GenerationConfig.Temperature, temp)
	}
	if got.GenerationConfig.MaxOutputTokens == nil || *got.GenerationConfig.MaxOutputTokens != 256 {
		t.Errorf("MaxOutputTokens = %v, want 256", got.GenerationConfig.MaxOutputTokens)
	}
}

// TestGeminiAdapterTransformRequestMultiTurn verifies multi-turn conversation
// mapping to Gemini Contents array.
func TestGeminiAdapterTransformRequestMultiTurn(t *testing.T) {
	a, _ := NewAdapter(AdapterTypeGemini)

	req := map[string]any{
		"model": "gemini-pro",
		"messages": []map[string]any{
			{"role": "user", "content": "First message"},
			{"role": "assistant", "content": "First response"},
			{"role": "user", "content": "Second message"},
		},
	}
	body, _ := json.Marshal(req)

	out, err := a.TransformRequest(body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}

	var got geminiRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal transformed body: %v", err)
	}

	if len(got.Contents) != 3 {
		t.Fatalf("len(Contents) = %d, want 3", len(got.Contents))
	}
	if got.Contents[0].Role != "user" || got.Contents[0].Parts[0].Text != "First message" {
		t.Errorf("Contents[0] = %+v", got.Contents[0])
	}
	if got.Contents[1].Role != "assistant" || got.Contents[1].Parts[0].Text != "First response" {
		t.Errorf("Contents[1] = %+v", got.Contents[1])
	}
	if got.Contents[2].Role != "user" || got.Contents[2].Parts[0].Text != "Second message" {
		t.Errorf("Contents[2] = %+v", got.Contents[2])
	}
}

// TestGeminiAdapterTransformRequestMaxCompletionTokens verifies that
// max_completion_tokens (OpenAI SDK alternative) is also mapped.
func TestGeminiAdapterTransformRequestMaxCompletionTokens(t *testing.T) {
	a, _ := NewAdapter(AdapterTypeGemini)

	req := map[string]any{
		"model":                 "gemini-2.0-flash",
		"max_completion_tokens": 512,
		"messages": []map[string]any{
			{"role": "user", "content": "Hello"},
		},
	}
	body, _ := json.Marshal(req)

	out, err := a.TransformRequest(body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}

	var got geminiRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal transformed body: %v", err)
	}

	if got.GenerationConfig == nil || got.GenerationConfig.MaxOutputTokens == nil || *got.GenerationConfig.MaxOutputTokens != 512 {
		t.Errorf("MaxOutputTokens = %v, want 512", got.GenerationConfig.MaxOutputTokens)
	}
}

// TestGeminiAdapterTransformRequestInvalidJSON ensures malformed input
// returns a descriptive error.
func TestGeminiAdapterTransformRequestInvalidJSON(t *testing.T) {
	a, _ := NewAdapter(AdapterTypeGemini)
	_, err := a.TransformRequest([]byte("{not json"))
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "gemini adapter") {
		t.Errorf("error should be prefixed with 'gemini adapter': %v", err)
	}
}

// TestGeminiNormalizeSSE runs a realistic Gemini SSE stream through the
// adapter's NormalizeSSE and asserts the output is canonical OpenAI
// chat.completion.chunk frames ending in [DONE].
func TestGeminiNormalizeSSE(t *testing.T) {
	a, _ := NewAdapter(AdapterTypeGemini)

	geminiStream := strings.Join([]string{
		`data: {"candidates": [{"content": {"parts": [{"text": "Hello"}]}}]}`,
		`data: {"candidates": [{"content": {"parts": [{"text": ", world"}]}}]}`,
		`data: {"candidates": [{"finishReason": "STOP"}]}`,
		"",
	}, "\n")

	r := a.NormalizeSSE(strings.NewReader(geminiStream))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	outStr := string(out)

	var contentText strings.Builder
	var sawFinish bool
	dataLines := 0
	for _, line := range strings.Split(outStr, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		dataLines++
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("output frame is not valid JSON: %v\nline: %s", err, line)
		}
		if obj, _ := chunk["object"].(string); obj != "chat.completion.chunk" {
			t.Errorf("object = %q, want chat.completion.chunk", obj)
		}
		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) != 1 {
			t.Fatalf("choices len = %d, want 1", len(choices))
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if txt, ok := delta["content"].(string); ok && txt != "" {
			contentText.WriteString(txt)
		}
		if fr, ok := choice["finish_reason"]; ok && fr != nil {
			sawFinish = true
			if fr != "stop" {
				t.Errorf("finish_reason = %v, want stop", fr)
			}
		}
	}

	if got := contentText.String(); got != "Hello, world" {
		t.Errorf("reassembled content = %q, want %q", got, "Hello, world")
	}
	if !sawFinish {
		t.Error("never saw finish_reason in output")
	}
	if dataLines < 2 {
		t.Errorf("dataLines = %d, want at least 2 content frames", dataLines)
	}
}

// TestGeminiNormalizeSSEFinishReasonMapping verifies that Gemini finish
// reasons are mapped to the correct OpenAI finish_reasons.
func TestGeminiNormalizeSSEFinishReasonMapping(t *testing.T) {
	a, _ := NewAdapter(AdapterTypeGemini)

	cases := []struct {
		geminiFR string
		openAIFR string
	}{
		{"STOP", "stop"},
		{"MAX_TOKENS", "length"},
		{"SAFETY", "content_filter"},
		{"RECITATION", "content_filter"},
		{"BLOCKLIST", "content_filter"},
		{"OTHER", "stop"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.geminiFR, func(t *testing.T) {
			stream := fmt.Sprintf(`data: {"candidates": [{"content": {"parts": [{"text": "hi"}]}}]}
data: {"candidates": [{"finishReason": "%s"}]}
`, tc.geminiFR)

			out, _ := io.ReadAll(a.NormalizeSSE(strings.NewReader(stream)))
			outStr := string(out)

			var gotFR string
			for _, line := range strings.Split(outStr, "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				payload := strings.TrimPrefix(line, "data: ")
				if payload == "[DONE]" || payload == "" {
					continue
				}
				var chunk map[string]any
				if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
					continue
				}
				choices, ok := chunk["choices"].([]any)
				if !ok || len(choices) == 0 {
					continue
				}
				choice, ok := choices[0].(map[string]any)
				if !ok {
					continue
				}
				if fr, ok := choice["finish_reason"].(string); ok {
					gotFR = fr
				}
			}
			if gotFR != tc.openAIFR {
				t.Errorf("finish_reason = %q, want %q", gotFR, tc.openAIFR)
			}
		})
	}
}

// TestGeminiNormalizeSSEEmptyStream ensures an empty upstream stream
// produces no output (no crash, no frames).
func TestGeminiNormalizeSSEEmptyStream(t *testing.T) {
	a, _ := NewAdapter(AdapterTypeGemini)
	out, err := io.ReadAll(a.NormalizeSSE(strings.NewReader("")))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty stream produced %d bytes, want 0", len(out))
	}
}

// --- Provider.Adapter() method --------------------------------------------

// TestProviderAdapterMethod verifies the convenience method on Provider
// resolves the configured Type, and that an unconfigured (zero) Provider
// yields the openai no-op.
func TestProviderAdapterMethod(t *testing.T) {
	p := Provider{Name: "claude", Type: AdapterTypeAnthropic}
	a, err := p.Adapter()
	if err != nil {
		t.Fatalf("Adapter: %v", err)
	}
	if a.Type() != AdapterTypeAnthropic {
		t.Errorf("Type = %q, want anthropic", a.Type())
	}

	// Zero-value Provider -> openai default.
	zero := Provider{}
	az, err := zero.Adapter()
	if err != nil {
		t.Fatalf("zero Adapter: %v", err)
	}
	if az.Type() != AdapterTypeOpenAI {
		t.Errorf("zero Type = %q, want openai", az.Type())
	}

	// Bad type surfaces an error.
	bad := Provider{Type: "nope"}
	if _, err := bad.Adapter(); err == nil {
		t.Error("expected error for bad type")
	}
}

// --- Anthropic cache-control hints (issue #1245) ------------------------

// TestAnthropicAdapterCacheControlBelowThreshold verifies that when the
// system field is shorter than the cache threshold, the adapter emits the
// system as a plain string (no cache_control array form).
func TestAnthropicAdapterCacheControlBelowThreshold(t *testing.T) {
	orig := AnthropicCacheMinSystemChars
	AnthropicCacheMinSystemChars = 1024
	defer func() { AnthropicCacheMinSystemChars = orig }()

	a, _ := NewAdapter(AdapterTypeAnthropic)
	req := map[string]any{
		"model": "claude-sonnet-4",
		"messages": []map[string]any{
			{"role": "system", "content": "Short."},
		},
	}
	body, _ := json.Marshal(req)
	out, err := a.TransformRequest(body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}

	// System should be a plain string, not an array.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var sysStr string
	if err := json.Unmarshal(raw["system"], &sysStr); err != nil {
		// It's not a plain string — could be an array (wrong).
		t.Errorf("system should be a plain string when below threshold, got %s", raw["system"])
	}
	if sysStr != "Short." {
		t.Errorf("system = %q, want %q", sysStr, "Short.")
	}
}

// TestAnthropicAdapterCacheControlAboveThreshold verifies that when the
// system field exceeds the cache threshold, the adapter emits it as an
// array of content blocks with cache_control: {type: "ephemeral"} on the
// final block.
func TestAnthropicAdapterCacheControlAboveThreshold(t *testing.T) {
	orig := AnthropicCacheMinSystemChars
	AnthropicCacheMinSystemChars = 50
	defer func() { AnthropicCacheMinSystemChars = orig }()

	a, _ := NewAdapter(AdapterTypeAnthropic)
	// Build a system prompt that exceeds the threshold.
	longSystem := strings.Repeat("A", 100)
	req := map[string]any{
		"model": "claude-sonnet-4",
		"messages": []map[string]any{
			{"role": "system", "content": longSystem},
			{"role": "user", "content": "Hello"},
		},
	}
	body, _ := json.Marshal(req)
	out, err := a.TransformRequest(body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// System should be a JSON array.
	var blocks []anthropicSystemBlock
	if err := json.Unmarshal(got["system"], &blocks); err != nil {
		t.Fatalf("system should be an array of blocks, got %s: %v", got["system"], err)
	}
	if len(blocks) != 1 {
		t.Fatalf("len(system blocks) = %d, want 1", len(blocks))
	}
	if blocks[0].Type != "text" {
		t.Errorf("block type = %q, want text", blocks[0].Type)
	}
	if blocks[0].Text != longSystem {
		t.Errorf("block text length = %d, want %d", len(blocks[0].Text), len(longSystem))
	}
	if blocks[0].CacheControl == nil {
		t.Fatal("final block missing cache_control")
	}
	if blocks[0].CacheControl.Type != "ephemeral" {
		t.Errorf("cache_control type = %q, want ephemeral", blocks[0].CacheControl.Type)
	}
}

// TestAnthropicAdapterCacheControlMultipleSystemParts verifies that
// multiple system messages produce multiple blocks with cache_control
// only on the final block.
func TestAnthropicAdapterCacheControlMultipleSystemParts(t *testing.T) {
	orig := AnthropicCacheMinSystemChars
	AnthropicCacheMinSystemChars = 10
	defer func() { AnthropicCacheMinSystemChars = orig }()

	a, _ := NewAdapter(AdapterTypeAnthropic)
	req := map[string]any{
		"model": "claude-sonnet-4",
		"messages": []map[string]any{
			{"role": "system", "content": "First instruction"},
			{"role": "system", "content": "Second instruction"},
			{"role": "system", "content": "Third instruction"},
			{"role": "user", "content": "Go"},
		},
	}
	body, _ := json.Marshal(req)
	out, err := a.TransformRequest(body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}

	var got anthropicRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var blocks []anthropicSystemBlock
	if err := json.Unmarshal(got.System, &blocks); err != nil {
		t.Fatalf("system blocks: %v", err)
	}
	if len(blocks) != 3 {
		t.Fatalf("len(system blocks) = %d, want 3", len(blocks))
	}
	// First two blocks must NOT have cache_control.
	for i := 0; i < 2; i++ {
		if blocks[i].CacheControl != nil {
			t.Errorf("block[%d] should not have cache_control, got %+v", i, blocks[i].CacheControl)
		}
	}
	// Final block must have cache_control.
	if blocks[2].CacheControl == nil {
		t.Error("final block missing cache_control")
	}
	if blocks[2].CacheControl.Type != "ephemeral" {
		t.Errorf("final block cache_control type = %q, want ephemeral", blocks[2].CacheControl.Type)
	}
}

// TestAnthropicAdapterCacheControlDisabled verifies that setting the
// threshold to zero disables cache-control injection entirely.
func TestAnthropicAdapterCacheControlDisabled(t *testing.T) {
	orig := AnthropicCacheMinSystemChars
	AnthropicCacheMinSystemChars = 0
	defer func() { AnthropicCacheMinSystemChars = orig }()

	a, _ := NewAdapter(AdapterTypeAnthropic)
	longSystem := strings.Repeat("X", 5000)
	req := map[string]any{
		"model": "claude-sonnet-4",
		"messages": []map[string]any{
			{"role": "system", "content": longSystem},
		},
	}
	body, _ := json.Marshal(req)
	out, err := a.TransformRequest(body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}

	var got map[string]json.RawMessage
	_ = json.Unmarshal(out, &got)

	// Should be a plain string, not an array.
	var sysStr string
	err = json.Unmarshal(got["system"], &sysStr)
	if err != nil {
		t.Errorf("system should be a plain string when cache disabled, got %s", got["system"])
	}
}

// --- Azure content filter detection (issue #1245) ---------------------

// TestAzureAdapterContentFilterDisabled verifies that the Azure adapter
// returns the reader unchanged when content filter detection is disabled
// (the default).
func TestAzureAdapterContentFilterDisabled(t *testing.T) {
	orig := AzureContentFilterEnabled
	AzureContentFilterEnabled = false
	defer func() { AzureContentFilterEnabled = orig }()

	a, _ := NewAdapter(AdapterTypeAzure)
	r := strings.NewReader("test data")
	if a.NormalizeSSE(r) != io.Reader(r) {
		t.Error("azure NormalizeSSE should return reader unchanged when disabled")
	}
}

// TestAzureAdapterContentFilterEnabled verifies that the Azure adapter
// detects content_filter finish_reason and emits an error frame.
func TestAzureAdapterContentFilterEnabled(t *testing.T) {
	orig := AzureContentFilterEnabled
	AzureContentFilterEnabled = true
	defer func() { AzureContentFilterEnabled = orig }()

	a, _ := NewAdapter(AdapterTypeAzure)

	// Simulate an Azure SSE stream with content_filter finish_reason.
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"filtered"},"finish_reason":"content_filter"}]}`,
		``,
		"",
	}, "\n")

	r := a.NormalizeSSE(strings.NewReader(stream))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	outStr := string(out)

	// Should contain the original chunk (transparent passthrough).
	if !strings.Contains(outStr, `"content":"filtered"`) {
		t.Error("original content_filter chunk not preserved")
	}
	// Should contain the error frame.
	if !strings.Contains(outStr, `"content_filter"`) {
		t.Error("error frame missing content_filter type")
	}
	if !strings.Contains(outStr, `"object":"error"`) {
		t.Error("error frame missing object:error")
	}
	// Should contain [DONE].
	if !strings.Contains(outStr, "data: [DONE]") {
		t.Error("stream missing [DONE] after error frame")
	}
}

// TestAzureAdapterContentFilterNormalChunk verifies that normal finish
// reasons pass through unmodified.
func TestAzureAdapterContentFilterNormalChunk(t *testing.T) {
	orig := AzureContentFilterEnabled
	AzureContentFilterEnabled = true
	defer func() { AzureContentFilterEnabled = orig }()

	a, _ := NewAdapter(AdapterTypeAzure)

	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
		"",
	}, "\n")

	r := a.NormalizeSSE(strings.NewReader(stream))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	outStr := string(out)

	if !strings.Contains(outStr, `"hello"`) {
		t.Error("normal content lost")
	}
	if !strings.Contains(outStr, `"stop"`) {
		t.Error("normal finish_reason lost")
	}
	// No error frame for normal completion.
	if strings.Contains(outStr, `"object":"error"`) {
		t.Error("error frame should not be emitted for normal completion")
	}
}

// --- Provider cache cost fields (issue #1245) -------------------------

// TestProviderCacheCostFields verifies that the new cache cost fields
// round-trip through ToConfig correctly.
func TestProviderCacheCostFields(t *testing.T) {
	p := Provider{
		Name:                        "anthropic",
		InputCostPer1K:              0.003,
		OutputCostPer1K:             0.015,
		CacheReadInputCostPer1K:     0.0003,
		CacheCreationInputCostPer1K: 0.00375,
	}
	cfg := p.ToConfig()
	if cfg.CacheReadInputCostPer1KUSD() != 0.0003 {
		t.Errorf("CacheReadInputCostPer1KUSD = %v, want 0.0003", cfg.CacheReadInputCostPer1KUSD())
	}
	if cfg.CacheCreationInputCostPer1KUSD() != 0.00375 {
		t.Errorf("CacheCreationInputCostPer1KUSD = %v, want 0.00375", cfg.CacheCreationInputCostPer1KUSD())
	}
	// Zero defaults should propagate.
	zero := Provider{Name: "test"}
	zCfg := zero.ToConfig()
	if zCfg.CacheReadInputCostPer1KUSD() != 0 {
		t.Errorf("zero CacheReadInputCostPer1KUSD = %v, want 0", zCfg.CacheReadInputCostPer1KUSD())
	}
}
