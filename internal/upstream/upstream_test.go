package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func (f rtFunc) Do(r *http.Request) (*http.Response, error)        { return f(r) }

// recordingRW captures writes + flushes for streaming tests.
type recordingRW struct {
	header  http.Header
	status  int
	body    *strings.Builder
	flushes int
}

func newRW() *recordingRW {
	return &recordingRW{header: http.Header{}, body: &strings.Builder{}}
}

func (r *recordingRW) Header() http.Header         { return r.header }
func (r *recordingRW) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recordingRW) WriteHeader(s int)           { r.status = s }
func (r *recordingRW) Flush()                      { r.flushes++ }

func TestStreamForwardsChunksAndFlushes(t *testing.T) {
	chunks := []string{"data: {\"a\":1}\n\n", "data: {\"a\":2}\n\n"}
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(strings.Join(chunks, ""))),
		}, nil
	})}
	rw := newRW()
	if err := Stream(rw, client, "http://x", "", map[string]interface{}{"model": "m"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if rw.status != 200 {
		t.Errorf("status = %d", rw.status)
	}
	if rw.body.String() != strings.Join(chunks, "") {
		t.Errorf("body = %q", rw.body.String())
	}
	if rw.flushes < 2 {
		t.Errorf("expected >=2 flushes, got %d", rw.flushes)
	}
	if rw.header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("upstream Content-Type not preserved: %q", rw.header.Get("Content-Type"))
	}
}

func TestStreamSendsBearerWhenKeySet(t *testing.T) {
	var seenAuth string
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		seenAuth = r.Header.Get("Authorization")
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader("ok")),
		}, nil
	})}
	if err := Stream(newRW(), client, "http://x", "sk-test", nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if seenAuth != "Bearer sk-test" {
		t.Errorf("auth = %q", seenAuth)
	}
}

func TestStreamOmitsAuthWhenKeyEmpty(t *testing.T) {
	var seenAuth string
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		seenAuth = r.Header.Get("Authorization")
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader("ok")),
		}, nil
	})}
	if err := Stream(newRW(), client, "http://x", "", nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if seenAuth != "" {
		t.Errorf("auth should be empty, got %q", seenAuth)
	}
}

func TestStreamTransportError(t *testing.T) {
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("dial fail")
	})}
	if err := Stream(newRW(), client, "http://x", "", nil); err == nil {
		t.Error("expected error")
	}
}

func TestFetchPanelHappyPath(t *testing.T) {
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"hello"}}]}`)),
		}, nil
	})}
	got, err := FetchPanel(context.Background(), client, "http://x", "", "model-x", map[string]interface{}{"x": 1})
	if err != nil {
		t.Fatalf("FetchPanel: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q", got)
	}
}

func TestFetchPanelOverwritesModelAndStream(t *testing.T) {
	var seenBody string
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"x"}}]}`)),
		}, nil
	})}
	body := map[string]interface{}{
		"model":  "orig",
		"stream": true,
		"msg":    "keep",
	}
	if _, err := FetchPanel(context.Background(), client, "http://x", "", "override", body); err != nil {
		t.Fatalf("FetchPanel: %v", err)
	}
	for _, want := range []string{`"model":"override"`, `"stream":false`, `"msg":"keep"`} {
		if !strings.Contains(seenBody, want) {
			t.Errorf("body missing %s in %s", want, seenBody)
		}
	}
}

func TestFetchPanelEmptyChoices(t *testing.T) {
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"choices":[]}`)),
		}, nil
	})}
	_, err := FetchPanel(context.Background(), client, "http://x", "", "m", nil)
	if err == nil || !strings.Contains(err.Error(), "empty choice") {
		t.Errorf("got %v", err)
	}
}

func TestFetchPanelNon200(t *testing.T) {
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 502,
			Body:       io.NopCloser(strings.NewReader("bad gateway")),
		}, nil
	})}
	_, err := FetchPanel(context.Background(), client, "http://x", "", "m", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("err = %v", err)
	}
}

func TestSynthesisPrompt(t *testing.T) {
	prompt := SynthesisPrompt("the user said",
		PanelResult{Source: "local", Content: "L1"},
		PanelResult{Source: "frontier", Content: "F1"},
	)
	for _, want := range []string{"the user said", "L1", "F1", "Candidate 1", "Candidate 2"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("missing %q in %q", want, prompt)
		}
	}
}

func TestSynthesisPromptFormatsErrors(t *testing.T) {
	prompt := SynthesisPrompt("u",
		PanelResult{Source: "local", Err: errors.New("dead")},
		PanelResult{Source: "frontier", Content: "ok"},
	)
	if !strings.Contains(prompt, "[local failed: dead]") {
		t.Errorf("error not surfaced: %q", prompt)
	}
	if !strings.Contains(prompt, "ok") {
		t.Errorf("healthy candidate missing: %q", prompt)
	}
}

// TestStreamRequiresFlusher confirms we surface a useful error rather than
// silently truncating output if the ResponseWriter cannot flush.
type nonFlushRW struct {
	header http.Header
	body   *strings.Builder
}

func (r *nonFlushRW) Header() http.Header         { return r.header }
func (r *nonFlushRW) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *nonFlushRW) WriteHeader(int)             {}

func TestStreamNonFlusherWriterErrors(t *testing.T) {
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("x"))}, nil
	})}
	rw := &nonFlushRW{header: http.Header{}, body: &strings.Builder{}}
	if err := Stream(rw, client, "http://x", "", nil); err == nil {
		t.Error("expected error from non-flusher writer")
	}
}

// TestPanelArbiterTimeoutBoundsHangingCall is the acceptance test for
// issue #12: when the arbiter hangs, Panel must surface an error within
// ~arbiterTimeout rather than blocking on http.DefaultClient (which has
// no timeout). Panel members respond quickly; the arbiter blocks long
// enough that the client-side timeout fires. A real net/http transport
// is required here because the in-package fakeTransport returns
// successful responses from its httptest.Recorder even after context
// cancellation — that does not faithfully model what the real
// http.Transport does on Client.Timeout.
//
// Note on the safety timer in the arbiter handler: Go's net/http server
// does NOT reliably cancel r.Context() when the client closes the
// connection for POST requests whose body has been fully read. The
// handler therefore exits on whichever comes first — context
// cancellation (best case, fast cleanup) or the safety timer (worst
// case, bounded slow cleanup). The test's own assertion only depends on
// the client-side timeout firing within the elapsed budget; the safety
// timer is purely to let srv.Close() return promptly.
func TestPanelArbiterTimeoutBoundsHangingCall(t *testing.T) {
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"local reply"}}]}`)
	}))
	defer localSrv.Close()

	frontierSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"frontier reply"}}]}`)
	}))
	defer frontierSrv.Close()

	arbiterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer arbiterSrv.Close()

	const arbiterTO = 100 * time.Millisecond
	start := time.Now()
	err := Panel(
		newSSERW(), http.DefaultClient,
		localSrv.URL, "local-m",
		frontierSrv.URL, "frontier-m",
		arbiterSrv.URL+"/v1/chat/completions", "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"test prompt",
		5*time.Second, // perFetchTimeout (panel members)
		arbiterTO,     // arbiterTimeout
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from hanging arbiter, got nil")
	}
	// Allow some slack: arbiterTimeout * 5 ceiling plus scheduler
	// jitter on shared CI runners. The point is "did not block
	// indefinitely"; we explicitly avoid asserting the timeout fired
	// at exactly 100ms because context.WithTimeout resolution depends
	// on the runtime timer.
	if elapsed > 5*arbiterTO {
		t.Errorf("Panel took %v with arbiter timeout %v; expected <%v",
			elapsed, arbiterTO, 5*arbiterTO)
	}
}

// TestPanelArbiterHappyPathNoRegression guards against the timeout
// plumbing breaking the working path: a responsive arbiter must still
// stream its synthesis reply through unchanged.
func TestPanelArbiterHappyPathNoRegression(t *testing.T) {
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local/v1/chat/completions"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()
	ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"local reply"}}]}`)
	})
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"frontier reply"}}]}`)
	})
	ft.on(arbiterURL, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: {\"a\":1}\n\n")
		_, _ = io.WriteString(w, "data: {\"a\":2}\n\n")
	})
	client := &http.Client{Transport: ft}

	rw := newSSERW()
	if err := Panel(
		rw, client,
		"http://local.local", "local-m",
		"http://frontier.local", "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"test prompt",
		5*time.Second, // perFetchTimeout
		5*time.Second, // arbiterTimeout
	); err != nil {
		t.Fatalf("Panel: %v", err)
	}
	if !strings.Contains(rw.body.String(), `"a":1`) || !strings.Contains(rw.body.String(), `"a":2`) {
		t.Errorf("arbiter stream not forwarded to client: %q", rw.body.String())
	}
	if !rw.flushed {
		t.Error("expected Flush() to be called on arbiter stream")
	}
}

// avoid unused-import warning when only some tests run.
var _ = time.Second

// --- issue #10: BufferedFetch + arbiter stream-flag handling --------------

// jsonRW is a buffered response writer for BufferedFetch tests. Unlike
// sseRW it does not implement http.Flusher — BufferedFetch is a
// non-streaming helper, so the harness's recorder doesn't need to
// observe flushes.
type jsonRW struct {
	header http.Header
	status int
	body   strings.Builder
}

func newJSONRW() *jsonRW { return &jsonRW{header: http.Header{}} }

func (r *jsonRW) Header() http.Header         { return r.header }
func (r *jsonRW) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *jsonRW) WriteHeader(s int)           { r.status = s }

func TestBufferedFetchWritesSingleJSONObject(t *testing.T) {
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"hi"}}]}`)),
		}, nil
	})}
	rw := newJSONRW()
	if err := BufferedFetch(rw, client, "http://x", "", map[string]interface{}{"model": "m"}); err != nil {
		t.Fatalf("BufferedFetch: %v", err)
	}
	if rw.status != 200 {
		t.Errorf("status = %d, want 200", rw.status)
	}
	if rw.header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", rw.header.Get("Content-Type"))
	}
	// Body must be the verbatim single chatCompletionResponse, not SSE.
	if !strings.Contains(rw.body.String(), `"object":"chat.completion"`) {
		t.Errorf("body missing single chatCompletionResponse: %q", rw.body.String())
	}
	if strings.HasPrefix(strings.TrimSpace(rw.body.String()), "data:") {
		t.Errorf("body looks like SSE, want plain JSON: %q", rw.body.String())
	}
}

func TestBufferedFetchForcesStreamFalseOnWire(t *testing.T) {
	var seenBody string
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`)),
		}, nil
	})}
	payload := map[string]interface{}{
		"model":  "x",
		"stream": true, // harness asked for stream=true; BufferedFetch must override
	}
	if err := BufferedFetch(newJSONRW(), client, "http://x", "", payload); err != nil {
		t.Fatalf("BufferedFetch: %v", err)
	}
	if !strings.Contains(seenBody, `"stream":false`) {
		t.Errorf("upstream body missing stream=false override: %s", seenBody)
	}
}

func TestBufferedFetchSetsBearerWhenKeySet(t *testing.T) {
	var seenAuth string
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		seenAuth = r.Header.Get("Authorization")
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	})}
	if err := BufferedFetch(newJSONRW(), client, "http://x", "sk-test", nil); err != nil {
		t.Fatalf("BufferedFetch: %v", err)
	}
	if seenAuth != "Bearer sk-test" {
		t.Errorf("auth = %q, want Bearer sk-test", seenAuth)
	}
}

func TestBufferedFetchForwardsNonOKStatus(t *testing.T) {
	// When the upstream returns non-200 (e.g. 429 rate limit), the
	// harness should see the same status and the upstream's error
	// body so its retry logic can react.
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"rate limited"}}`)),
		}, nil
	})}
	rw := newJSONRW()
	if err := BufferedFetch(rw, client, "http://x", "", map[string]interface{}{"model": "m"}); err != nil {
		t.Fatalf("BufferedFetch: %v", err)
	}
	if rw.status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rw.status)
	}
	if !strings.Contains(rw.body.String(), "rate limited") {
		t.Errorf("body missing upstream error: %q", rw.body.String())
	}
}

func TestBufferedFetchRejectsInvalidJSON(t *testing.T) {
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader("<html>oops</html>")),
		}, nil
	})}
	rw := newJSONRW()
	if err := BufferedFetch(rw, client, "http://x", "", nil); err == nil {
		t.Fatal("expected error for non-JSON upstream response")
	}
	// Status must not have been written — the harness would otherwise
	// receive a 200 with an HTML body, which is the worst-case mix.
	if rw.status != 0 {
		t.Errorf("status written before validation: %d", rw.status)
	}
}

func TestBufferedFetchTransportError(t *testing.T) {
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("dial fail")
	})}
	if err := BufferedFetch(newJSONRW(), client, "http://x", "", nil); err == nil {
		t.Error("expected error")
	}
}

// TestPanelArbiterHonorsStreamFlagFalse is the issue #10 acceptance
// test for the fusion arbiter path: when the harness sets
// body["stream"]=false, the arbiter call must NOT stream — it must
// write a single JSON object with Content-Type application/json.
func TestPanelArbiterHonorsStreamFlagFalse(t *testing.T) {
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local/v1/chat/completions"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()
	ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"local reply"}}]}`)
	})
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"frontier reply"}}]}`)
	})
	var arbiterBody string
	ft.on(arbiterURL, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		arbiterBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"synthesized"}}]}`)
	})
	client := &http.Client{Transport: ft}

	rw := newJSONRW()
	if err := Panel(
		rw, client,
		"http://local.local", "local-m",
		"http://frontier.local", "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}, "stream": false},
		"test prompt",
		5*time.Second,
		5*time.Second,
	); err != nil {
		t.Fatalf("Panel: %v", err)
	}
	// Arbiter must have been called with stream=false on the wire.
	if !strings.Contains(arbiterBody, `"stream":false`) {
		t.Errorf("arbiter request missing stream=false: %s", arbiterBody)
	}
	// Harness must receive a single JSON object, not SSE chunks.
	if rw.status != 200 {
		t.Errorf("status = %d, want 200", rw.status)
	}
	if rw.header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", rw.header.Get("Content-Type"))
	}
	if !strings.Contains(rw.body.String(), `"content":"synthesized"`) {
		t.Errorf("arbiter JSON not forwarded: %q", rw.body.String())
	}
	if strings.HasPrefix(strings.TrimSpace(rw.body.String()), "data:") {
		t.Errorf("body looks like SSE, want plain JSON: %q", rw.body.String())
	}
}

// TestPanelArbiterHonorsStreamFlagTrueRegression guards the streaming
// path: when the harness did not set stream=false (default true), the
// arbiter must continue to stream SSE chunks. This is the no-regression
// companion to the buffered test above.
func TestPanelArbiterHonorsStreamFlagTrueRegression(t *testing.T) {
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local/v1/chat/completions"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()
	ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"local reply"}}]}`)
	})
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"frontier reply"}}]}`)
	})
	ft.on(arbiterURL, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: {\"a\":1}\n\n")
		_, _ = io.WriteString(w, "data: {\"a\":2}\n\n")
	})
	client := &http.Client{Transport: ft}

	// Explicit stream=true to mirror the OpenAI default.
	rw := newSSERW()
	if err := Panel(
		rw, client,
		"http://local.local", "local-m",
		"http://frontier.local", "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}, "stream": true},
		"test prompt",
		5*time.Second,
		5*time.Second,
	); err != nil {
		t.Fatalf("Panel: %v", err)
	}
	if !rw.flushed {
		t.Error("expected Flush() to be called on arbiter stream")
	}
	if !strings.Contains(rw.body.String(), `"a":1`) {
		t.Errorf("arbiter SSE not forwarded: %q", rw.body.String())
	}
}
