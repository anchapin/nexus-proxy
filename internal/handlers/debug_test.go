package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/upstream"
)

// captureDebugSlog swaps slog.Default for a JSON handler bound to a
// buffer that captures every line. Returns the captured buffer
// contents as a JSON-decoded slice (one map per line) plus the raw
// string. Use findDebugLines to filter by msg prefix.
func captureDebugSlog(t *testing.T) func() ([]map[string]any, string) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() ([]map[string]any, string) {
		raw := buf.String()
		lines := strings.Split(strings.TrimSpace(raw), "\n")
		out := make([]map[string]any, 0, len(lines))
		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("invalid slog line %q: %v", line, err)
			}
			out = append(out, rec)
		}
		return out, raw
	}
}

// findDebugLines returns the debug-trace lines whose msg starts with
// the given prefix. Each trace sub-emit uses a "[DEBUG] <section>"
// msg so this is how tests assert which lifecycle stages fired.
func findDebugLines(lines []map[string]any, prefix string) []map[string]any {
	var out []map[string]any
	for _, line := range lines {
		msg, _ := line["msg"].(string)
		if strings.HasPrefix(msg, prefix) {
			out = append(out, line)
		}
	}
	return out
}

// debugDeps returns a Deps instance with Config.Debug = true. Other
// collaborators match baseDeps so the local Ollama mock is wired.
func debugDeps(t *testing.T) (Deps, *upstream.RecordingTransport) {
	t.Helper()
	deps, rt := baseDeps(t)
	deps.Config.Debug = true
	deps.Config.DebugBodyBytes = 64 // small cap so truncation tests are easy
	return deps, rt
}

// --- MaskAPIKey ---------------------------------------------------------

func TestMaskAPIKeyEmpty(t *testing.T) {
	if got := MaskAPIKey(""); got != "" {
		t.Errorf("MaskAPIKey(\"\") = %q, want empty", got)
	}
}

func TestMaskAPIKeyShort(t *testing.T) {
	if got := MaskAPIKey("abc"); got != "****" {
		t.Errorf("MaskAPIKey(\"abc\") = %q, want ****", got)
	}
}

func TestMaskAPIKeyTypical(t *testing.T) {
	got := MaskAPIKey("sk-proj-abcdefghij1234XYZ1")
	want := "sk-...XYZ1"
	if got != want {
		t.Errorf("MaskAPIKey(long) = %q, want %q", got, want)
	}
}

func TestMaskAPIKeyExactlyEight(t *testing.T) {
	// Boundary: input exactly 8 chars must produce a full mask
	// rather than a prefix+suffix split that leaks characters.
	got := MaskAPIKey("12345678")
	if got != "****" {
		t.Errorf("MaskAPIKey(\"12345678\") = %q, want **** (no leak)", got)
	}
}

// --- TruncateForDebug ---------------------------------------------------

func TestTruncateForDebugNoOp(t *testing.T) {
	got, trunc := TruncateForDebug("hello", 100)
	if got != "hello" || trunc {
		t.Errorf("short string should not truncate; got (%q, %v)", got, trunc)
	}
}

func TestTruncateForDebugTruncated(t *testing.T) {
	got, trunc := TruncateForDebug("hello world", 5)
	if !trunc {
		t.Errorf("long string must report truncation")
	}
	if !strings.HasPrefix(got, "hello") {
		t.Errorf("truncated string must start with the kept bytes; got %q", got)
	}
	if !strings.Contains(got, "...(truncated") {
		t.Errorf("truncated string must carry the truncation marker; got %q", got)
	}
	if !strings.Contains(got, "total=11") {
		t.Errorf("truncated string must report original length 11; got %q", got)
	}
}

func TestTruncateForDebugZeroCap(t *testing.T) {
	got, trunc := TruncateForDebug("hello", 0)
	if got != "hello" || trunc {
		t.Errorf("zero cap must disable truncation; got (%q, %v)", got, trunc)
	}
}

// --- HostOfURL ----------------------------------------------------------

func TestHostOfURLValid(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"http://api.example.com/v1/chat/completions", "api.example.com"},
		{"https://api.openai.com:443/v1/chat", "api.openai.com:443"},
		{"", ""},
	}
	for _, c := range cases {
		if got := HostOfURL(c.in); got != c.want {
			t.Errorf("HostOfURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHostOfURLUnparseable(t *testing.T) {
	// Unparseable URLs (or those without a host) return "" so logs
	// stay quiet rather than print the broken input verbatim.
	if got := HostOfURL("not a url"); got != "" {
		t.Errorf("HostOfURL(bad) = %q, want empty", got)
	}
}

// --- RedactedHeaders ----------------------------------------------------

func TestRedactedHeadersStripsAuthorization(t *testing.T) {
	h := map[string][]string{
		"Authorization": {"Bearer sk-proj-abcdef1234XYZ1"},
		"Content-Type":  {"application/json"},
	}
	out := RedactedHeaders(h)
	if got := out["Authorization"]; len(got) != 1 || got[0] != "Bearer ****" {
		t.Errorf("Authorization leaked: %v", got)
	}
	if got := out["Content-Type"]; len(got) != 1 || got[0] != "application/json" {
		t.Errorf("Content-Type mangled: %v", got)
	}
}

func TestRedactedHeadersCaseInsensitive(t *testing.T) {
	h := map[string][]string{
		"authorization": {"Bearer sk-proj-abcdef1234XYZ1"},
	}
	out := RedactedHeaders(h)
	if got := out["authorization"]; got[0] != "Bearer ****" {
		t.Errorf("case-insensitive match failed: %v", got)
	}
}

func TestRedactedHeadersNil(t *testing.T) {
	if got := RedactedHeaders(nil); got != nil {
		t.Errorf("RedactedHeaders(nil) = %v, want nil", got)
	}
}

// --- Debug trace end-to-end --------------------------------------------

// TestChatDebugEmitsAllFiveTraceSubEvents is the headline acceptance
// test for issue #33: when NEXUS_DEBUG=true the handler emits
// exactly five [DEBUG] lines per request covering request, transforms,
// routing, upstream, response.
func TestChatDebugEmitsAllFiveTraceSubEvents(t *testing.T) {
	deps, rt := debugDeps(t)
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"local stream"},"finish_reason":"stop"}]}`)
	})

	stop := captureDebugSlog(t)
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rw := httptest.NewRecorder()
	Chat(deps).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rw.Code, rw.Body.String())
	}
	lines, _ := stop()

	required := []string{"request", "transforms", "routing", "upstream", "response"}
	for _, section := range required {
		matches := findDebugLines(lines, "[DEBUG] "+section)
		if len(matches) != 1 {
			t.Errorf("expected exactly one [DEBUG] %s line, got %d (lines=%v)", section, len(matches), debugMsg(lines))
		}
	}
}

// TestChatDebugRoutingIncludesReasonAndBudget asserts the routing
// trace carries the reason (dsl/guardrail/slm), budget source, and
// token estimate — three of the four acceptance criteria fields.
func TestChatDebugRoutingIncludesReasonAndBudget(t *testing.T) {
	deps, rt := debugDeps(t)
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"local stream"},"finish_reason":"stop"}]}`)
	})

	stop := captureDebugSlog(t)
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	Chat(deps).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	lines, _ := stop()

	matches := findDebugLines(lines, "[DEBUG] routing")
	if len(matches) != 1 {
		t.Fatalf("expected one routing trace, got %d", len(matches))
	}
	rec := matches[0]
	group, ok := rec["routing"].(map[string]any)
	if !ok {
		t.Fatalf("routing trace missing group: %v", rec)
	}
	if got, _ := group["reason"].(string); got != "dsl" {
		t.Errorf("routing.reason = %q, want dsl", got)
	}
	if got, _ := group["route"].(string); got != "local" {
		t.Errorf("routing.route = %q, want local", got)
	}
	if got, _ := group["budget_source"].(string); got != "static-fallback" {
		t.Errorf("routing.budget_source = %q, want static-fallback", got)
	}
	if _, ok := group["estimated_tokens"]; !ok {
		t.Errorf("routing.estimated_tokens missing")
	}
}

// TestChatDebugTransformsLogsTOONAndRAG ensures the transform trace
// surfaces both the RAG hit and the TOON compression result. We use
// a TOON-friendly payload so CompressJSONBlocks fires.
func TestChatDebugSLMErrorRoutingReason(t *testing.T) {
	deps, rt := debugDeps(t)
	rt.OnAny(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.String(), "ollama") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "frontier stream")
	})

	stop := captureDebugSlog(t)
	body := `{"messages":[{"role":"user","content":"review this unfamiliar design"}]}`
	Chat(deps).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	lines, _ := stop()

	matches := findDebugLines(lines, "[DEBUG] routing")
	if len(matches) != 1 {
		t.Fatalf("expected one routing trace, got %d", len(matches))
	}
	group, ok := matches[0]["routing"].(map[string]any)
	if !ok {
		t.Fatalf("routing trace missing group: %v", matches[0])
	}
	if got, _ := group["reason"].(string); got != "slm-error" {
		t.Errorf("routing.reason = %q, want slm-error", got)
	}
}

func TestChatDebugTransformsLogsTOONAndRAG(t *testing.T) {
	deps, rt := debugDeps(t)
	// The default stubEmbedder returns [0,0,0] which makes cosine
	// with any vector zero. Override with a non-zero vector and add
	// a matching example so RAG fires.
	deps.RAG = rag.NewStore(stubEmbedder{vec: []float64{1, 0, 0}}, 0.55)
	deps.RAG.Add("sample.go", "use this pattern", []float64{1, 0, 0})
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	})

	stop := captureDebugSlog(t)
	// Double-quoted string with escapes — the body contains
	// backticks (TOON-fence markers) which would terminate a raw
	// string literal prematurely.
	body := "{\"messages\":[{\"role\":\"user\",\"content\":\"sample prompt ```json\\n[{\\\"a\\\":1,\\\"b\\\":2},{\\\"a\\\":3,\\\"b\\\":4}]\\n``` end\"}]}"
	Chat(deps).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	lines, _ := stop()

	matches := findDebugLines(lines, "[DEBUG] transforms")
	if len(matches) != 1 {
		t.Fatalf("expected one transforms trace, got %d", len(matches))
	}
	group, ok := matches[0]["transforms"].(map[string]any)
	if !ok {
		t.Fatalf("transforms trace missing group: %v", matches[0])
	}
	rag, _ := group["rag"].(map[string]any)
	if rag == nil {
		t.Fatalf("rag sub-group missing: %v", group)
	}
	if injected, _ := rag["injected"].(bool); !injected {
		t.Errorf("rag.injected = false, want true")
	}
	if filename, _ := rag["filename"].(string); filename != "sample.go" {
		t.Errorf("rag.filename = %q, want sample.go", filename)
	}
	toon, _ := group["toon"].(map[string]any)
	if toon == nil {
		t.Fatalf("toon sub-group missing: %v", group)
	}
	if applied, _ := toon["applied"].(bool); !applied {
		t.Errorf("toon.applied = false, want true (json array was compressed)")
	}
	if _, ok := toon["tokens_saved"]; !ok {
		t.Errorf("toon.tokens_saved missing")
	}
}

// TestChatDebugUpstreamReportsHost verifies the upstream trace emits
// only the host (never the full URL with path/query) and the model
// name. The frontier URL we configure contains a path; the host-only
// extraction strips it.
func TestChatDebugUpstreamReportsHost(t *testing.T) {
	deps, rt := debugDeps(t)
	// 50000 chars forces the guardrail to fire (route=frontier)
	// which exercises the default frontier branch and the
	// single-endpoint upstream trace.
	large := strings.Repeat("a", 48500)
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"x"}}]}`))
	})

	stop := captureDebugSlog(t)
	body := `{"messages":[{"role":"user","content":"` + large + `"}]}`
	Chat(deps).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	lines, _ := stop()

	matches := findDebugLines(lines, "[DEBUG] upstream")
	if len(matches) != 1 {
		t.Fatalf("expected one upstream trace, got %d", len(matches))
	}
	group, ok := matches[0]["upstream"].(map[string]any)
	if !ok {
		t.Fatalf("upstream group missing: %v", matches[0])
	}
	if host, _ := group["target_host"].(string); host != "frontier.local" {
		t.Errorf("upstream.target_host = %q, want frontier.local", host)
	}
	if route, _ := group["route"].(string); route != "frontier" {
		t.Errorf("upstream.route = %q, want frontier", route)
	}
}

// TestChatDebugResponseIncludesBodyPreviewAndTruncation asserts the
// response trace carries a non-empty body preview AND reports
// truncation when the upstream body exceeds NEXUS_DEBUG_BODY_BYTES.
// debugDeps sets the cap to 64 bytes.
func TestChatDebugResponseIncludesBodyPreviewAndTruncation(t *testing.T) {
	deps, rt := debugDeps(t)
	long := strings.Repeat("x", 500)
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"`+long+`"},"finish_reason":"stop"}]}`)
	})

	stop := captureDebugSlog(t)
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	Chat(deps).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	lines, _ := stop()

	matches := findDebugLines(lines, "[DEBUG] response")
	if len(matches) != 1 {
		t.Fatalf("expected one response trace, got %d", len(matches))
	}
	group, ok := matches[0]["response"].(map[string]any)
	if !ok {
		t.Fatalf("response group missing: %v", matches[0])
	}
	preview, _ := group["body_preview"].(string)
	if preview == "" {
		t.Errorf("body_preview is empty; captureWriter should have buffered the response")
	}
	if len(preview) > 200 {
		t.Errorf("body_preview length = %d, want <= 200 (truncated, well below 500)", len(preview))
	}
	if truncated, _ := group["body_truncated"].(bool); !truncated {
		t.Errorf("body_truncated = false, want true (500-byte body, 64-byte cap)")
	}
}

// TestChatDebugNoTraceWhenOff verifies the production fast path:
// when Config.Debug is false the handler emits zero [DEBUG] lines.
// This is the acceptance criterion that guarantees zero overhead.
func TestChatDebugNoTraceWhenOff(t *testing.T) {
	deps, rt := baseDeps(t) // Debug defaults to false
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"local stream"},"finish_reason":"stop"}]}`)
	})

	stop := captureDebugSlog(t)
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	Chat(deps).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	lines, _ := stop()

	if matches := findDebugLines(lines, "[DEBUG] "); len(matches) != 0 {
		t.Errorf("expected zero [DEBUG] lines with Debug=false, got %d: %v", len(matches), debugMsg(matches))
	}
}

// TestChatDebugCaptureWriterInstalledWhenDebugOnly confirms that
// captureWriter is installed in debug mode even when no judge or
// quality observer is configured. The handler populates the trace's
// body preview from this writer, so without it the response trace
// would be useless.
func TestChatDebugCaptureWriterInstalledWhenDebugOnly(t *testing.T) {
	deps, rt := debugDeps(t)
	// Crucially, do NOT set JudgeObserver or QualityObserver — the
	// captureWriter should still be installed because Debug=true.
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	})

	stop := captureDebugSlog(t)
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	Chat(deps).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	lines, _ := stop()

	matches := findDebugLines(lines, "[DEBUG] response")
	if len(matches) != 1 {
		t.Fatalf("expected one response trace, got %d", len(matches))
	}
	group, _ := matches[0]["response"].(map[string]any)
	preview, _ := group["body_preview"].(string)
	if preview == "" {
		t.Errorf("captureWriter was not installed for debug-only mode; body_preview is empty")
	}
}

// TestChatDebugCascadeTraceReportsStepsAndServedBy is the cascade-
// specific assertion: the UpstreamTrace must include both the steps
// attempted and which one served the response. We force a fallback
// by making the local URL return invalid JSON so the cascade moves
// to the frontier step.
func TestChatDebugCascadeTraceReportsStepsAndServedBy(t *testing.T) {
	deps, rt := debugDeps(t)
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		// Invalid body — cascade retries.
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "not openai json")
	})
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"frontier fallback"},"finish_reason":"stop"}]}`)
	})

	stop := captureDebugSlog(t)
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	Chat(deps).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	lines, _ := stop()

	matches := findDebugLines(lines, "[DEBUG] upstream")
	if len(matches) != 1 {
		t.Fatalf("expected one upstream trace, got %d", len(matches))
	}
	group, ok := matches[0]["upstream"].(map[string]any)
	if !ok {
		t.Fatalf("upstream group missing: %v", matches[0])
	}
	cascade, ok := group["cascade"].(map[string]any)
	if !ok {
		t.Fatalf("cascade sub-group missing — got %v", group)
	}
	if served, _ := cascade["served_by"].(string); served != "frontier" {
		t.Errorf("cascade.served_by = %q, want frontier (cascade fell back)", served)
	}
	if success, _ := cascade["success"].(bool); !success {
		t.Errorf("cascade.success = false, want true (frontier step succeeded)")
	}
	steps, _ := cascade["steps"].([]any)
	if len(steps) < 2 {
		t.Errorf("cascade.steps = %v, want at least 2 (local + frontier)", steps)
	}
}

// TestChatDebugNoAPIKeyInLogs is the redaction acceptance test: the
// FrontierKey configured for the proxy must NEVER appear verbatim
// in any debug trace line. The trace should not contain the literal
// string; only the masked form may appear.
func TestChatDebugNoAPIKeyInLogs(t *testing.T) {
	deps, rt := debugDeps(t)
	const key = "sk-proj-supersecrettest1234XYZ1"
	deps.Config.FrontierKey = key
	deps.Config.DebugBodyBytes = 512

	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	})

	stop := captureDebugSlog(t)
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	Chat(deps).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	_, raw := stop()

	if strings.Contains(raw, key) {
		t.Errorf("debug logs leaked the API key verbatim; raw output:\n%s", raw)
	}
}

// TestChatDebugGuardrailRoutingReason ensures the guardrail path
// stamps reason="guardrail" so operators can distinguish it from
// DSL/SLM-driven routes.
func TestChatDebugGuardrailRoutingReason(t *testing.T) {
	deps, rt := debugDeps(t)
	// 50000 char prompt ≈ 6250 tokens > 6000 guardrail
	large := strings.Repeat("a", 48500)
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"x"}}]}`))
	})

	stop := captureDebugSlog(t)
	body := `{"messages":[{"role":"user","content":"` + large + `"}]}`
	Chat(deps).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	lines, _ := stop()

	matches := findDebugLines(lines, "[DEBUG] routing")
	if len(matches) != 1 {
		t.Fatalf("expected one routing trace, got %d", len(matches))
	}
	group, _ := matches[0]["routing"].(map[string]any)
	if reason, _ := group["reason"].(string); reason != "guardrail" {
		t.Errorf("routing.reason = %q, want guardrail", reason)
	}
	if route, _ := group["route"].(string); route != "frontier" {
		t.Errorf("routing.route = %q, want frontier (guardrail always forces frontier)", route)
	}
}

// TestChatDebugErrorPathStillEmits asserts the trace fires on the
// error path too — operators need the trace when something breaks.
func TestChatDebugErrorPathStillEmits(t *testing.T) {
	deps, rt := debugDeps(t)
	rt.On("POST", "http://ollama.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	rt.On("POST", "http://frontier.local", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})

	stop := captureDebugSlog(t)
	body := `{"messages":[{"role":"user","content":"please fix the css"}]}`
	Chat(deps).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	lines, _ := stop()

	for _, section := range []string{"request", "transforms", "routing", "upstream", "response"} {
		if matches := findDebugLines(lines, "[DEBUG] "+section); len(matches) != 1 {
			t.Errorf("expected one [DEBUG] %s trace on error path, got %d", section, len(matches))
		}
	}
}

// debugMsg returns a compact "msg=X, msg=Y" list of all captured slog
// messages — handy for diagnostic t.Errorf messages so a test
// failure shows exactly what was emitted.
func debugMsg(lines []map[string]any) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, asString(line["msg"]))
	}
	return out
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// Verify the routing constants the trace uses line up with the
// router package. Catches a typo at compile time.
var _ = []router.Route{router.RouteLocal, router.RouteFrontier, router.RouteFusion}

// Verify EffectiveDebugBodyBytes honours the configured cap and
// falls back to DefaultDebugBodyBytes when zero/negative. This
// guarantees the trace uses the operator's NEXUS_DEBUG_BODY_BYTES.
func TestEffectiveDebugBodyBytes(t *testing.T) {
	cfg := config.Config{DebugBodyBytes: 256}
	if got := cfg.EffectiveDebugBodyBytes(); got != 256 {
		t.Errorf("EffectiveDebugBodyBytes = %d, want 256", got)
	}
	cfg = config.Config{DebugBodyBytes: 0}
	if got := cfg.EffectiveDebugBodyBytes(); got != config.DefaultDebugBodyBytes {
		t.Errorf("EffectiveDebugBodyBytes(0) = %d, want default %d", got, config.DefaultDebugBodyBytes)
	}
	cfg = config.Config{DebugBodyBytes: -1}
	if got := cfg.EffectiveDebugBodyBytes(); got != config.DefaultDebugBodyBytes {
		t.Errorf("EffectiveDebugBodyBytes(-1) = %d, want default %d", got, config.DefaultDebugBodyBytes)
	}
}
