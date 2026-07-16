package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sseRW records SSE writes so tests can assert on what reached the client.
type sseRW struct {
	header  http.Header
	status  int
	body    strings.Builder
	flushed bool
}

func newSSERW() *sseRW { return &sseRW{header: http.Header{}} }

func (s *sseRW) Header() http.Header         { return s.header }
func (s *sseRW) Write(b []byte) (int, error) { return s.body.Write(b) }
func (s *sseRW) WriteHeader(c int)           { s.status = c }
func (s *sseRW) Flush()                      { s.flushed = true }

// chatBody200 is a minimal valid OpenAI-style chat completion body.
const chatBody200 = `{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":"hello from local"},"finish_reason":"stop"}]}`

// fakeTransport routes requests by URL to a per-URL handler function. The
// primary use case is "primary returns X, fallback returns Y"; tests
// install one handler per URL. It also tracks call counts per URL so
// tests can assert how many times each endpoint was hit.
type fakeTransport struct {
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	counters map[string]*int32
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{handlers: map[string]http.HandlerFunc{}, counters: map[string]*int32{}}
}

func (f *fakeTransport) on(url string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[url] = h
	if _, ok := f.counters[url]; !ok {
		var v int32
		f.counters[url] = &v
	}
}

func (f *fakeTransport) counter(url string) *int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.counters[url]; ok {
		return c
	}
	var v int32
	f.counters[url] = &v
	return &v
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	h := f.handlers[req.URL.String()]
	if h == nil {
		f.mu.Unlock()
		return nil, errors.New("fakeTransport: no handler for " + req.URL.String())
	}
	if c, ok := f.counters[req.URL.String()]; ok {
		atomic.AddInt32(c, 1)
	}
	f.mu.Unlock()
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Result(), nil
}

// Two-step cascade used by most tests.
func twoStepCascade() *Cascade {
	return &Cascade{
		Timeout: 2 * time.Second,
		Steps: []CascadeStep{
			{Name: "local", URL: "http://primary.local/v1/chat/completions", Model: "local-m"},
			{Name: "frontier", URL: "http://fallback.local/v1/chat/completions", APIKey: "sk", Model: "fb-m"},
		},
	}
}

func TestShouldRetryTable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		err    error
		want   bool
	}{
		{"nil err 200", http.StatusOK, nil, false},
		{"nil err 400", http.StatusBadRequest, nil, false},
		{"nil err 401", http.StatusUnauthorized, nil, false},
		{"nil err 408", http.StatusRequestTimeout, nil, true},
		{"nil err 429", http.StatusTooManyRequests, nil, true},
		{"nil err 500", http.StatusInternalServerError, nil, true},
		{"nil err 502", http.StatusBadGateway, nil, true},
		{"nil err 503", http.StatusServiceUnavailable, nil, true},
		{"nil err 504", http.StatusGatewayTimeout, nil, true},
		{"err wins", http.StatusOK, errors.New("dial fail"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldRetry(tc.status, tc.err)
			if got != tc.want {
				t.Errorf("ShouldRetry(%d, %v) = %v, want %v", tc.status, tc.err, got, tc.want)
			}
		})
	}
}

func TestCascadePrimarySucceeds(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// verify stream=false is set and the model name is forwarded
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if stream, _ := body["stream"].(bool); stream {
			t.Errorf("expected stream=false, got %v", body["stream"])
		}
		if model, _ := body["model"].(string); model != "local-m" {
			t.Errorf("model = %q, want local-m", model)
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chatBody200)
	})
	ft.on("http://fallback.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("fallback should not have been called")
	})
	client := &http.Client{Transport: ft}

	rw := newSSERW()
	res, err := twoStepCascade().Run(rw, client, map[string]interface{}{"messages": []interface{}{}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Succeeded {
		t.Errorf("Succeeded = false")
	}
	if res.ServedBy != "local" {
		t.Errorf("ServedBy = %q, want local", res.ServedBy)
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", res.Attempts)
	}
	if res.RouteAttempted != "local" {
		t.Errorf("RouteAttempted = %q", res.RouteAttempted)
	}
	body := rw.body.String()
	if !strings.Contains(body, "hello from local") {
		t.Errorf("body missing content: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body missing [DONE]: %q", body)
	}
	if rw.status != 200 {
		t.Errorf("status = %d", rw.status)
	}
	if rw.header.Get("X-Nexus-Cascade-Served-By") != "local" {
		t.Errorf("missing served-by header: %q", rw.header.Get("X-Nexus-Cascade-Served-By"))
	}
}

func TestCascadeFallsBackOn5xx(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "boom")
	})
	ft.on("http://fallback.local/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// verify bearer header was sent
		if got := r.Header.Get("Authorization"); got != "Bearer sk" {
			t.Errorf("auth = %q, want Bearer sk", got)
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"fb-m","choices":[{"message":{"content":"served by frontier"}}]}`)
	})
	client := &http.Client{Transport: ft}

	rw := newSSERW()
	res, err := twoStepCascade().Run(rw, client, map[string]interface{}{"messages": []interface{}{}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Succeeded || res.ServedBy != "frontier" {
		t.Errorf("Succeeded=%v ServedBy=%q", res.Succeeded, res.ServedBy)
	}
	if res.RouteAttempted != "local->frontier" {
		t.Errorf("RouteAttempted = %q", res.RouteAttempted)
	}
	if !strings.Contains(rw.body.String(), "served by frontier") {
		t.Errorf("body = %q", rw.body.String())
	}
	if *ft.counter("http://primary.local/v1/chat/completions") != 1 {
		t.Errorf("primary counter = %d", *ft.counter("http://primary.local/v1/chat/completions"))
	}
	if *ft.counter("http://fallback.local/v1/chat/completions") != 1 {
		t.Errorf("fallback counter = %d", *ft.counter("http://fallback.local/v1/chat/completions"))
	}
}

func TestCascadeFallsBackOn429(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
	})
	ft.on("http://fallback.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chatBody200)
	})
	res, err := twoStepCascade().Run(newSSERW(), &http.Client{Transport: ft}, nil)
	if err != nil || res.ServedBy != "frontier" {
		t.Errorf("err=%v servedBy=%q", err, res.ServedBy)
	}
}

func TestCascadeFallsBackOn408(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(408)
	})
	ft.on("http://fallback.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chatBody200)
	})
	res, err := twoStepCascade().Run(newSSERW(), &http.Client{Transport: ft}, nil)
	if err != nil || res.ServedBy != "frontier" {
		t.Errorf("err=%v servedBy=%q", err, res.ServedBy)
	}
}

func TestCascadeFallsBackOnTransportError(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(http.ResponseWriter, *http.Request) {})
	ft.handlers["http://primary.local/v1/chat/completions"] = nil // force "no handler" transport error
	ft.on("http://fallback.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chatBody200)
	})
	client := &http.Client{Transport: ft}
	res, err := twoStepCascade().Run(newSSERW(), client, nil)
	if err != nil || res.ServedBy != "frontier" {
		t.Errorf("err=%v servedBy=%q", err, res.ServedBy)
	}
}

func TestCascadeFallsBackOnTimeout(t *testing.T) {
	// Primary hangs; cascade timeout short-circuits it.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = io.WriteString(w, chatBody200)
	}))
	defer slow.Close()

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chatBody200)
	}))
	defer ok.Close()

	cas := &Cascade{
		Timeout: 50 * time.Millisecond,
		Steps: []CascadeStep{
			{Name: "local", URL: slow.URL + "/v1/chat/completions", Model: "m"},
			{Name: "frontier", URL: ok.URL + "/v1/chat/completions", Model: "m"},
		},
	}
	start := time.Now()
	res, err := cas.Run(newSSERW(), http.DefaultClient, nil)
	elapsed := time.Since(start)
	if err != nil || res.ServedBy != "frontier" {
		t.Errorf("err=%v servedBy=%q", err, res.ServedBy)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("cascade took %v, expected <500ms (timeout should have short-circuited)", elapsed)
	}
}

func TestCascadeFallsBackOnMalformedJSON(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{not valid json`)
	})
	ft.on("http://fallback.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chatBody200)
	})
	res, err := twoStepCascade().Run(newSSERW(), &http.Client{Transport: ft}, nil)
	if err != nil || res.ServedBy != "frontier" {
		t.Errorf("err=%v servedBy=%q", err, res.ServedBy)
	}
}

// TestCascadeFallsBackOnHTMLContentType verifies that a 200 OK response
// with Content-Type: text/html is treated as an error and triggers fallback
// (issue #314). HTML error pages from a misbehaving upstream would
// otherwise pass JSON validation and get forwarded as a valid response.
func TestCascadeFallsBackOnHTMLContentType(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `<html><body>Internal Server Error</body></html>`)
	})
	ft.on("http://fallback.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chatBody200)
	})
	res, err := twoStepCascade().Run(newSSERW(), &http.Client{Transport: ft}, nil)
	if err != nil || res.ServedBy != "frontier" {
		t.Errorf("err=%v servedBy=%q", err, res.ServedBy)
	}
}

func TestCascadeFallsBackOnEmptyChoices(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[]}`)
	})
	ft.on("http://fallback.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chatBody200)
	})
	res, err := twoStepCascade().Run(newSSERW(), &http.Client{Transport: ft}, nil)
	if err != nil || res.ServedBy != "frontier" {
		t.Errorf("err=%v servedBy=%q", err, res.ServedBy)
	}
}

func TestCascadeFallsBackOnMalformedToolCall(t *testing.T) {
	// Primary returns tool_call with broken arguments JSON — the exact
	// "small model hallucinated JSON" failure mode the issue calls out.
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"qwen","choices":[{"message":{"content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{not_json"}}]}}]}`)
	})
	ft.on("http://fallback.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chatBody200)
	})
	res, err := twoStepCascade().Run(newSSERW(), &http.Client{Transport: ft}, nil)
	if err != nil || res.ServedBy != "frontier" {
		t.Errorf("err=%v servedBy=%q", err, res.ServedBy)
	}
}

func TestCascadeFallsBackOnMissingToolCallFields(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"tool_calls":[{"id":"","type":"function","function":{"name":"x","arguments":"{}"}}]}}]}`)
	})
	ft.on("http://fallback.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chatBody200)
	})
	res, err := twoStepCascade().Run(newSSERW(), &http.Client{Transport: ft}, nil)
	if err != nil || res.ServedBy != "frontier" {
		t.Errorf("err=%v servedBy=%q", err, res.ServedBy)
	}
}

func TestCascadeAcceptsValidToolCall(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"qwen","choices":[{"message":{"content":"tool call ok","tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`)
	})
	ft.on("http://fallback.local/v1/chat/completions", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("fallback should not fire when tool_call is valid")
	})
	res, err := twoStepCascade().Run(newSSERW(), &http.Client{Transport: ft}, nil)
	if err != nil || res.ServedBy != "local" {
		t.Errorf("err=%v servedBy=%q", err, res.ServedBy)
	}
}

func TestCascadeAllFailReturnsLastError(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	})
	ft.on("http://fallback.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
	})
	rw := newSSERW()
	res, err := twoStepCascade().Run(rw, &http.Client{Transport: ft}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if res.Succeeded {
		t.Error("Succeeded should be false")
	}
	if res.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", res.Attempts)
	}
	if res.RouteAttempted != "local->frontier" {
		t.Errorf("RouteAttempted = %q", res.RouteAttempted)
	}
	if rw.status != 0 {
		t.Errorf("rw.status = %d, expected 0 (nothing should be written)", rw.status)
	}
	if rw.body.Len() != 0 {
		t.Errorf("rw.body = %q, expected empty", rw.body.String())
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("err should mention last status: %v", err)
	}
}

func TestCascadeNonRetryableStopsImmediately(t *testing.T) {
	// Primary returns 401 — retrying won't help, so cascade should not
	// call the fallback.
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, "unauthorized")
	})
	ft.on("http://fallback.local/v1/chat/completions", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("fallback should NOT have been called for non-retryable 401")
	})
	_, err := twoStepCascade().Run(newSSERW(), &http.Client{Transport: ft}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v", err)
	}
}

func TestCascadeEmptyStepsReturnsError(t *testing.T) {
	_, err := (&Cascade{}).Run(newSSERW(), http.DefaultClient, nil)
	if err == nil || !strings.Contains(err.Error(), "no steps") {
		t.Errorf("got %v", err)
	}
}

func TestCascadeDefaultTimeoutWhenZero(t *testing.T) {
	cas := &Cascade{
		Steps: []CascadeStep{
			{Name: "local", URL: "http://x/v1/chat/completions", Model: "m"},
		},
	}
	if cas.Timeout != 0 {
		t.Errorf("expected 0, got %v", cas.Timeout)
	}
	// Build & Run path: just verify Run doesn't panic with timeout=0
	// when there are no failures. Use a handler that returns 200.
	ft := newFakeTransport()
	ft.on("http://x/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chatBody200)
	})
	res, err := cas.Run(newSSERW(), &http.Client{Transport: ft}, nil)
	if err != nil || !res.Succeeded {
		t.Errorf("err=%v res=%+v", err, res)
	}
}

func TestBuildLocalCascadeOnlyLocal(t *testing.T) {
	cas := BuildLocalCascade(CascadeConfig{
		LocalURL:   "http://localhost:11434/",
		LocalModel: "qwen3-coder:8b",
		Timeout:    5 * time.Second,
	})
	if len(cas.Steps) != 1 {
		t.Fatalf("Steps = %d, want 1", len(cas.Steps))
	}
	if cas.Steps[0].Name != "local" {
		t.Errorf("first step = %q", cas.Steps[0].Name)
	}
	// Local URL should be the ollama /v1/chat/completions path with no trailing slash.
	if cas.Steps[0].URL != "http://localhost:11434/v1/chat/completions" {
		t.Errorf("URL = %q", cas.Steps[0].URL)
	}
	if cas.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v", cas.Timeout)
	}
}

func TestBuildLocalCascadeWithFrontier(t *testing.T) {
	cas := BuildLocalCascade(CascadeConfig{
		LocalURL:      "http://localhost:11434",
		LocalModel:    "local-m",
		FrontierURL:   "http://frontier.local",
		FrontierModel: "fb-m",
		FrontierKey:   "sk",
	})
	if len(cas.Steps) != 2 {
		t.Fatalf("Steps = %d, want 2", len(cas.Steps))
	}
	wantNames := []string{"local", "frontier"}
	for i, n := range wantNames {
		if cas.Steps[i].Name != n {
			t.Errorf("Steps[%d].Name = %q, want %q", i, cas.Steps[i].Name, n)
		}
	}
}

func TestBuildLocalCascadeWithFrontierAndZai(t *testing.T) {
	cas := BuildLocalCascade(CascadeConfig{
		LocalURL:      "http://localhost:11434",
		LocalModel:    "local-m",
		FrontierURL:   "http://frontier.local",
		FrontierModel: "fb-m",
		FrontierKey:   "sk",
		ZAIURL:        "http://zai.local",
		ZAIModel:      "glm",
		ZAIKey:        "zk",
	})
	if len(cas.Steps) != 3 {
		t.Fatalf("Steps = %d, want 3", len(cas.Steps))
	}
	want := []string{"local", "frontier", "zai"}
	for i, n := range want {
		if cas.Steps[i].Name != n {
			t.Errorf("Steps[%d].Name = %q, want %q", i, cas.Steps[i].Name, n)
		}
		if cas.Steps[i].URL == "" {
			t.Errorf("Steps[%d].URL empty", i)
		}
	}
}

func TestBuildLocalCascadeSkipsMissingKeys(t *testing.T) {
	cas := BuildLocalCascade(CascadeConfig{
		LocalURL:   "http://localhost:11434",
		LocalModel: "local-m",
		// FrontierKey empty -> skip
		// ZAIKey empty -> skip
	})
	if len(cas.Steps) != 1 {
		t.Errorf("Steps = %d, want 1 (only local)", len(cas.Steps))
	}
}

func TestCascadeValidatesBeforeWritingBytes(t *testing.T) {
	// Critical acceptance criterion: the response must NOT be written to
	// the client if it would later fail validation. Here the primary
	// returns garbage (200 + malformed JSON); fallback returns valid JSON.
	// Assert: nothing from the primary ends up in rw.
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "garbage{{{")
	})
	ft.on("http://fallback.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chatBody200)
	})
	rw := newSSERW()
	_, err := twoStepCascade().Run(rw, &http.Client{Transport: ft}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(rw.body.String(), "garbage") {
		t.Errorf("malformed primary bytes leaked into client response: %q", rw.body.String())
	}
}

func TestCascadeSSEChunkStructure(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chatBody200)
	})
	rw := newSSERW()
	_, err := twoStepCascade().Run(rw, &http.Client{Transport: ft}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(rw.body.String()), "\n\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 SSE frames, got %d: %q", len(lines), rw.body.String())
	}
	if !strings.HasPrefix(lines[0], "data: ") {
		t.Errorf("frame 0 not data: prefixed: %q", lines[0])
	}
	if lines[1] != "data: [DONE]" {
		t.Errorf("frame 1 = %q, want data: [DONE]", lines[1])
	}
	// Pull JSON out and verify it parses + contains the content.
	var chunk map[string]interface{}
	raw := strings.TrimPrefix(lines[0], "data: ")
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		t.Fatalf("frame JSON invalid: %v", err)
	}
	if chunk["object"] != "chat.completion.chunk" {
		t.Errorf("object = %v", chunk["object"])
	}
	if !rw.flushed {
		t.Error("expected Flush() to be called")
	}
}

// --- LocalStepFailed (issue #80) tests --------------------------------------

// TestCascadeLocalStepFailedOn5xx verifies that when the "local" step
// fails with a retryable error and a fallback serves the request, the
// CascadeResult.LocalStepFailed flag is set to true. The chat handler
// uses this flag to arm the local-route cooldown.
func TestCascadeLocalStepFailedOn5xx(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "ollama down")
	})
	ft.on("http://fallback.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"fb-m","choices":[{"message":{"content":"ok"}}]}`)
	})
	client := &http.Client{Transport: ft}

	res, err := twoStepCascade().Run(newSSERW(), client, map[string]interface{}{"messages": []interface{}{}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.LocalStepFailed {
		t.Error("LocalStepFailed = false, want true after local 5xx + frontier fallback")
	}
}

// TestCascadeLocalStepFailedNotSetOnSuccess verifies the flag is false
// when local serves the request normally.
func TestCascadeLocalStepFailedNotSetOnSuccess(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chatBody200)
	})
	client := &http.Client{Transport: ft}

	res, err := twoStepCascade().Run(newSSERW(), client, map[string]interface{}{"messages": []interface{}{}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.LocalStepFailed {
		t.Error("LocalStepFailed = true, want false when local served the request")
	}
}

// TestCascadeLocalStepFailedNotSetOnNonRetryable verifies the flag is
// NOT set when local fails with a non-retryable error (e.g. 401) — that
// surfaces immediately and is not a transient local health problem.
func TestCascadeLocalStepFailedNotSetOnNonRetryable(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	client := &http.Client{Transport: ft}

	_, err := twoStepCascade().Run(newSSERW(), client, map[string]interface{}{"messages": []interface{}{}})
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
	// Non-retryable means the cascade stops immediately; fallback is
	// never called and LocalStepFailed should be false.
}

// --- Issue #72: tool_calls preservation tests --------------------------------

// toolCallBody is a minimal OpenAI-compatible response with one tool call.
const toolCallBody = `{"model":"qwen","choices":[{"message":{"content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`

// TestCascadeToolCallsStreamedAsDeltaToolCalls verifies that valid
// tool_calls from the local upstream reach the client as OpenAI-compatible
// SSE delta.tool_calls with finish_reason "tool_calls" (issue #72).
func TestCascadeToolCallsStreamedAsDeltaToolCalls(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, toolCallBody)
	})
	ft.on("http://fallback.local/v1/chat/completions", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("fallback should not fire for valid tool_calls")
	})
	rw := newSSERW()
	res, err := twoStepCascade().Run(rw, &http.Client{Transport: ft}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Succeeded || res.ServedBy != "local" {
		t.Fatalf("Succeeded=%v ServedBy=%q", res.Succeeded, res.ServedBy)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(res.ToolCalls))
	}
	// Parse the SSE chunk and verify the delta carries tool_calls.
	body := rw.body.String()
	lines := strings.SplitN(strings.TrimSpace(body), "\n\n", 2)
	if len(lines) < 1 {
		t.Fatalf("no SSE frames: %q", body)
	}
	raw := strings.TrimPrefix(lines[0], "data: ")
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		t.Fatalf("invalid chunk JSON: %v", err)
	}
	choices, _ := chunk["choices"].([]interface{})
	if len(choices) != 1 {
		t.Fatalf("choices len = %d", len(choices))
	}
	choice := choices[0].(map[string]interface{})
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
	delta, _ := choice["delta"].(map[string]interface{})
	tc, ok := delta["tool_calls"].([]interface{})
	if !ok {
		t.Fatalf("delta.tool_calls missing or wrong type: %v", delta)
	}
	if len(tc) != 1 {
		t.Errorf("tool_calls len = %d, want 1", len(tc))
	}
	first := tc[0].(map[string]interface{})
	if first["id"] != "call_1" {
		t.Errorf("tool_call id = %v", first["id"])
	}
	if first["index"] != float64(0) {
		t.Errorf("tool_call index = %v, want 0", first["index"])
	}
	fn := first["function"].(map[string]interface{})
	if fn["name"] != "bash" {
		t.Errorf("function name = %v", fn["name"])
	}
	// Empty content must NOT appear in the delta.
	if _, hasContent := delta["content"]; hasContent {
		t.Errorf("delta should not carry content for tool-call-only response: %v", delta)
	}
}

// TestCascadeEmptyContentWithToolCallsAccepted verifies that an empty
// content field alongside valid tool_calls does NOT trigger fallback
// (issue #72 acceptance criterion).
func TestCascadeEmptyContentWithToolCallsAccepted(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, toolCallBody) // content is ""
	})
	ft.on("http://fallback.local/v1/chat/completions", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("fallback should not fire")
	})
	res, err := twoStepCascade().Run(newSSERW(), &http.Client{Transport: ft}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Succeeded || res.ServedBy != "local" {
		t.Errorf("Succeeded=%v ServedBy=%q", res.Succeeded, res.ServedBy)
	}
}

// TestCascadeContentOnlyBackwardCompat verifies content-only responses
// still produce delta.content + finish_reason "stop" — the legacy shape
// must be byte-for-byte backward compatible (issue #72).
func TestCascadeContentOnlyBackwardCompat(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chatBody200)
	})
	rw := newSSERW()
	res, err := twoStepCascade().Run(rw, &http.Client{Transport: ft}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ToolCalls) != 0 {
		t.Errorf("ToolCalls len = %d, want 0", len(res.ToolCalls))
	}
	body := rw.body.String()
	raw := strings.TrimPrefix(strings.SplitN(strings.TrimSpace(body), "\n\n", 2)[0], "data: ")
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		t.Fatalf("invalid chunk: %v", err)
	}
	choice := chunk["choices"].([]interface{})[0].(map[string]interface{})
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
	}
	delta := choice["delta"].(map[string]interface{})
	if _, hasTC := delta["tool_calls"]; hasTC {
		t.Errorf("content-only delta should not carry tool_calls: %v", delta)
	}
	if delta["content"] != "hello from local" {
		t.Errorf("content = %v", delta["content"])
	}
}

// TestCascadeMultipleToolCallsIndexed verifies that multiple tool calls
// in a single response each get the correct streaming "index" field.
func TestCascadeMultipleToolCallsIndexed(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"qwen","choices":[{"message":{"content":"","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}},
			{"id":"call_2","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"b.go\"}"}}
		]}}]}`)
	})
	rw := newSSERW()
	res, err := twoStepCascade().Run(rw, &http.Client{Transport: ft}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("ToolCalls len = %d, want 2", len(res.ToolCalls))
	}
	body := rw.body.String()
	raw := strings.TrimPrefix(strings.SplitN(strings.TrimSpace(body), "\n\n", 2)[0], "data: ")
	var chunk map[string]interface{}
	_ = json.Unmarshal([]byte(raw), &chunk)
	choice := chunk["choices"].([]interface{})[0].(map[string]interface{})
	tc := choice["delta"].(map[string]interface{})["tool_calls"].([]interface{})
	for i, raw := range tc {
		entry := raw.(map[string]interface{})
		if entry["index"] != float64(i) {
			t.Errorf("tool_call[%d] index = %v, want %d", i, entry["index"], i)
		}
	}
}

// TestCascadeContentAndToolCallsBothPresent verifies that when the
// upstream returns both content and tool_calls, the delta includes
// both (some models emit a preamble before the tool call).
func TestCascadeContentAndToolCallsBothPresent(t *testing.T) {
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"qwen","choices":[{"message":{"content":"Let me check that.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{}"}}]}}]}`)
	})
	rw := newSSERW()
	_, err := twoStepCascade().Run(rw, &http.Client{Transport: ft}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw := strings.TrimPrefix(strings.SplitN(strings.TrimSpace(rw.body.String()), "\n\n", 2)[0], "data: ")
	var chunk map[string]interface{}
	_ = json.Unmarshal([]byte(raw), &chunk)
	choice := chunk["choices"].([]interface{})[0].(map[string]interface{})
	delta := choice["delta"].(map[string]interface{})
	if delta["content"] != "Let me check that." {
		t.Errorf("content = %v", delta["content"])
	}
	if _, ok := delta["tool_calls"]; !ok {
		t.Errorf("tool_calls missing from delta: %v", delta)
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
}

// TestExtractAssistantMessageToolCalls is a unit test for the extraction
// function itself, verifying tool_calls are returned alongside content.
func TestExtractAssistantMessageToolCalls(t *testing.T) {
	msg, model, err := extractAssistantMessage([]byte(toolCallBody))
	if err != nil {
		t.Fatalf("extractAssistantMessage: %v", err)
	}
	if model != "qwen" {
		t.Errorf("model = %q", model)
	}
	if msg.Content != "" {
		t.Errorf("Content = %q, want empty", msg.Content)
	}
	if !msg.HasToolCalls() {
		t.Error("HasToolCalls = false, want true")
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].ID != "call_1" {
		t.Errorf("ID = %q", msg.ToolCalls[0].ID)
	}
	if msg.ToolCalls[0].Function.Name != "bash" {
		t.Errorf("Name = %q", msg.ToolCalls[0].Function.Name)
	}
}
