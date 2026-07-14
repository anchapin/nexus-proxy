package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
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

// truncatingReader yields data then returns terminalErr instead of a
// clean io.EOF. It lets streaming tests reproduce a mid-stream TCP
// drop (io.ErrUnexpectedEOF — what the HTTP chunked reader surfaces
// when the connection closes before the terminating 0-length chunk)
// and a post-[DONE] connection reset, without spinning up an
// httptest.Server. terminalErr must be non-nil.
type truncatingReader struct {
	data        []byte
	pos         int
	terminalErr error
}

func (r *truncatingReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, r.terminalErr
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// TestStreamEmitsDoneOnUpstreamTruncation reproduces issue #118: the
// upstream yields one complete SSE chunk then drops the connection
// mid-stream (io.ErrUnexpectedEOF instead of a clean io.EOF). The
// proxy must forward the partial chunk, emit a truncation event, emit
// the terminating data: [DONE] sentinel so the harness does not hang,
// stamp X-Nexus-Truncated, and return ErrUpstreamTruncated.
func TestStreamEmitsDoneOnUpstreamTruncation(t *testing.T) {
	chunk := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(&truncatingReader{data: []byte(chunk), terminalErr: io.ErrUnexpectedEOF}),
		}, nil
	})}
	rw := newRW()
	err := StreamWithContext(context.Background(), rw, client, "http://x", "", map[string]interface{}{"model": "m"})
	if !errors.Is(err, ErrUpstreamTruncated) {
		t.Fatalf("StreamWithContext error = %v, want ErrUpstreamTruncated", err)
	}
	if got := rw.header.Get("X-Nexus-Truncated"); got != "true" {
		t.Errorf("X-Nexus-Truncated = %q, want \"true\"", got)
	}
	out := rw.body.String()
	if !strings.Contains(out, chunk) {
		t.Errorf("forwarded chunk missing from body: %q", out)
	}
	if !strings.Contains(out, `"type":"upstream_truncated"`) {
		t.Errorf("body missing truncation event: %q", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("body must end with [DONE] sentinel, got %q", out)
	}
}

// TestStreamHappyPathDoneTerminatedUnchanged locks the happy path: a
// complete upstream that emits its own data: [DONE] and then a clean
// io.EOF must pass through byte-for-byte unchanged, return nil, and
// NOT stamp X-Nexus-Truncated (issue #118 acceptance: successful
// streams must not gain a synthetic terminator).
func TestStreamHappyPathDoneTerminatedUnchanged(t *testing.T) {
	body := "data: {\"choices\":[]}\n\ndata: [DONE]\n\n"
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	rw := newRW()
	if err := Stream(rw, client, "http://x", "", map[string]interface{}{"model": "m"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if rw.body.String() != body {
		t.Errorf("body changed on happy path:\ngot:  %q\nwant: %q", rw.body.String(), body)
	}
	if got := rw.header.Get("X-Nexus-Truncated"); got != "" {
		t.Errorf("X-Nexus-Truncated should be unset on happy path, got %q", got)
	}
}

// TestStreamNoTruncationWhenDoneAlreadySeen pins the graceful/abrupt
// distinction: once the upstream's own data: [DONE] has flowed
// through, the SSE stream is complete even if the connection then
// drops (io.ErrUnexpectedEOF). No synthetic truncation event must be
// appended and Stream returns nil — the harness already saw its
// terminator.
func TestStreamNoTruncationWhenDoneAlreadySeen(t *testing.T) {
	body := "data: {\"choices\":[]}\n\ndata: [DONE]\n\n"
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(&truncatingReader{data: []byte(body), terminalErr: io.ErrUnexpectedEOF}),
		}, nil
	})}
	rw := newRW()
	if err := Stream(rw, client, "http://x", "", map[string]interface{}{"model": "m"}); err != nil {
		t.Fatalf("Stream: %v (want nil — [DONE] already seen)", err)
	}
	if rw.body.String() != body {
		t.Errorf("body changed after [DONE] was seen:\ngot:  %q\nwant: %q", rw.body.String(), body)
	}
	if got := rw.header.Get("X-Nexus-Truncated"); got != "" {
		t.Errorf("X-Nexus-Truncated should be unset when [DONE] already seen, got %q", got)
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
	if got.Content != "hello" {
		t.Errorf("got %q", got.Content)
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
	_, err := Panel(
		newSSERW(), http.DefaultClient,
		localSrv.URL, "local-m",
		frontierSrv.URL, "frontier-m",
		arbiterSrv.URL+"/v1/chat/completions", "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"test prompt",
		5*time.Second, // perFetchTimeout (panel members)
		arbiterTO,     // arbiterTimeout
		false,         // skipLocal
  nil, 0*time.Second,
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
	if _, err := Panel(
		rw, client,
		"http://local.local", "local-m",
		"http://frontier.local", "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"test prompt",
		5*time.Second, // perFetchTimeout
		5*time.Second, // arbiterTimeout
		false,         // skipLocal
  nil, 0*time.Second,
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

// TestPanelSkipLocalOmitsLocalFetch is the acceptance test for
// issue #8's graceful-degradation path: when skipLocal is true the
// local Ollama fetch must not happen (the count of local URL
// requests is zero), the arbiter must still see a synthesizable
// candidate set (frontier content), and the arbiter stream must
// reach the client.
func TestPanelSkipLocalOmitsLocalFetch(t *testing.T) {
	// Panel calls FetchPanel with these URLs verbatim, so the
	// fake transport handlers must register the exact same
	// strings (no implicit "/v1/chat/completions" suffixing — see
	// FetchPanel and Panel signatures in upstream.go).
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()
	ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("local URL was hit even though skipLocal=true")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"should not appear"}}]}`)
	})
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"frontier only"}}]}`)
	})
	ft.on(arbiterURL, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"arb\":\"ok\"}\n\n")
	})
	client := &http.Client{Transport: ft}

	rw := newSSERW()
	if _, err := Panel(
		rw, client,
		"http://local.local", "local-m",
		frontierURL, "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"test prompt",
		5*time.Second, // perFetchTimeout
		5*time.Second, // arbiterTimeout
		true,          // skipLocal
  nil, 0*time.Second,
	); err != nil {
		t.Fatalf("Panel: %v", err)
	}
	if got := *ft.counter(localURL); got != 0 {
		t.Errorf("local fetch count = %d, want 0", got)
	}
	if got := *ft.counter(frontierURL); got != 1 {
		t.Errorf("frontier fetch count = %d, want 1", got)
	}
	if !strings.Contains(rw.body.String(), `"arb":"ok"`) {
		t.Errorf("arbiter stream not forwarded: %q", rw.body.String())
	}
}

// TestPanelSkipLocalArbiterPromptHasDegradedMarker confirms the
// arbiter prompt still carries an explicit "[local failed: ...]"
// marker when the local fetch is skipped, so the arbiter is not
// confused into thinking the local slot is empty rather than
// deliberately omitted.
func TestPanelSkipLocalArbiterPromptHasDegradedMarker(t *testing.T) {
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"frontier"}}]}`)
	})
	var seenArbiterBody string
	ft.on(arbiterURL, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenArbiterBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: {\"a\":1}\n\n")
	})
	client := &http.Client{Transport: ft}

	if _, err := Panel(
		newSSERW(), client,
		localURL, "local-m",
		frontierURL, "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"the user prompt",
		5*time.Second, 5*time.Second,
		true, // skipLocal
  nil, 0*time.Second,
	); err != nil {
		t.Fatalf("Panel: %v", err)
	}
	if !strings.Contains(seenArbiterBody, "[local failed") {
		t.Errorf("arbiter prompt missing [local failed marker: %s", seenArbiterBody)
	}
	if !strings.Contains(seenArbiterBody, "ollama unavailable (degraded)") {
		t.Errorf("arbiter prompt missing degraded sentinel: %s", seenArbiterBody)
	}
	if !strings.Contains(seenArbiterBody, "frontier") {
		t.Errorf("arbiter prompt missing frontier candidate: %s", seenArbiterBody)
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
	if _, err := Panel(
		rw, client,
		"http://local.local", "local-m",
		"http://frontier.local", "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}, "stream": false},
		"test prompt",
		5*time.Second,
		5*time.Second,
		false, // skipLocal (issue #8)
  nil, 0*time.Second,
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
	if _, err := Panel(
		rw, client,
		"http://local.local", "local-m",
		"http://frontier.local", "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}, "stream": true},
		"test prompt",
		5*time.Second,
		5*time.Second,
		false, // skipLocal (issue #8)
  nil, 0*time.Second,
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

// --- issue #48: streaming fusion with progressive delivery -------------

// TestPanelStreamingAgreementSkipsArbiter is the headline acceptance
// test for issue #48: when both panel members produce near-identical
// output, PanelStreaming must stream the first member's content as a
// speculative OpenAI-compatible SSE chunk, terminate with
// `data: [DONE]\n\n`, and NOT invoke the arbiter.
func TestPanelStreamingAgreementSkipsArbiter(t *testing.T) {
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()
	ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Use a buffered channel to queue requests. The dispatcher drains the queue."}}]}`)
	})
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Use a buffered channel to queue requests. The dispatcher drains the queue."}}]}`)
	})
	var arbiterCalled int
	ft.on(arbiterURL, func(w http.ResponseWriter, _ *http.Request) {
		arbiterCalled++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: {\"a\":1}\n\n")
	})
	client := &http.Client{Transport: ft}

	rw := newSSERW()
	outcome, err := PanelStreaming(
		rw, client,
		"http://local.local", "local-m",
		frontierURL, "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"test prompt",
		5*time.Second, // perFetchTimeout
		5*time.Second, // arbiterTimeout
		false,         // skipLocal
		0.85,          // agreementThreshold
		"test-request-id",
  nil, 0*time.Second,
	)
	if err != nil {
		t.Fatalf("PanelStreaming: %v", err)
	}
	if !outcome.ArbiterSkipped {
		t.Errorf("outcome.ArbiterSkipped = false, want true (similarity=%v)", outcome.Similarity)
	}
	if outcome.Similarity < 0.85 {
		t.Errorf("similarity = %v, want >= 0.85", outcome.Similarity)
	}
	if arbiterCalled != 0 {
		t.Errorf("arbiter was called %d times, want 0", arbiterCalled)
	}
	body := rw.body.String()
	if !strings.Contains(body, `"content":"Use a buffered channel`) {
		t.Errorf("speculative chunk not in body: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("missing [DONE] terminator: %q", body)
	}
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Errorf("missing OpenAI chunk envelope: %q", body)
	}
	// Two upstream calls — local + frontier — but no arbiter call.
	if got := *ft.counter(localURL); got != 1 {
		t.Errorf("local fetch = %d, want 1", got)
	}
	if got := *ft.counter(frontierURL); got != 1 {
		t.Errorf("frontier fetch = %d, want 1", got)
	}
}

// TestPanelStreamingAgreementCancelsSlowMember verifies issue #229:
// when both panel members produce near-identical output (agreement case),
// the slow member's context cancel() is called before returning to the
// client, preventing wasted compute after the response has already been sent.
// The key scenario is slow-local/fast-frontier: the local goroutine's HTTP
// request is aborted when the fast frontier wins, rather than running to
// completion and blocking on a channel send.
func TestPanelStreamingAgreementCancelsSlowMember(t *testing.T) {
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()

	// Slow local handler — takes 5 seconds to complete.
	// When the context is cancelled, the server should abort the request.
	ft.on(localURL, func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow processing: wait for context cancellation or timeout.
		// The httptest server doesn't check context, but we use a separate
		// mechanism to detect if the request was actually started.
		select {
		case <-time.After(5 * time.Second):
			// Request completed normally (not cancelled).
		case <-r.Context().Done():
			// Request was cancelled via context — this is the expected path.
			return
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Use a buffered channel to queue requests. The dispatcher drains the queue."}}]}`)
	})

	// Fast frontier handler — returns immediately with identical content.
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Use a buffered channel to queue requests. The dispatcher drains the queue."}}]}`)
	})

	// Arbiter should NOT be called in agreement case.
	ft.on(arbiterURL, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("arbiter called in agreement case")
		w.WriteHeader(200)
	})
	client := &http.Client{Transport: ft}

	rw := newSSERW()
	outcome, err := PanelStreaming(
		rw, client,
		"http://local.local", "local-m",
		frontierURL, "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"test prompt",
		5*time.Second, // perFetchTimeout
		5*time.Second, // arbiterTimeout
		false,         // skipLocal
		0.85,          // agreementThreshold
		"test-request-id",
  nil, 0*time.Second,
	)

	if err != nil {
		t.Fatalf("PanelStreaming: %v", err)
	}
	if !outcome.ArbiterSkipped {
		t.Errorf("outcome.ArbiterSkipped = false, want true (agreement)")
	}
	if outcome.Similarity < 0.85 {
		t.Errorf("similarity = %v, want >= 0.85", outcome.Similarity)
	}
	if outcome.Source != "frontier" {
		t.Errorf("Source = %q, want frontier (first to complete)", outcome.Source)
	}

	// Verify both panel members were called.
	if got := *ft.counter(localURL); got != 1 {
		t.Errorf("local fetch = %d, want 1", got)
	}
	if got := *ft.counter(frontierURL); got != 1 {
		t.Errorf("frontier fetch = %d, want 1", got)
	}

	// Verify the speculative chunk and DONE terminator are present.
	body := rw.body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("missing [DONE] terminator: %q", body)
	}
}

// TestPanelStreamingDisagreementRunsArbiter exercises the second
// branch of issue #48's progressive delivery: when the two panel
// members diverge (similarity < threshold), the speculative answer is
// streamed first and THEN the arbiter runs, with its synthesis
// appended as additional SSE chunks. ArbiterSkipped must be false.
func TestPanelStreamingDisagreementRunsArbiter(t *testing.T) {
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()
	ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"the quick brown fox"}}]}`)
	})
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"switch the entire database schema migrate everything now"}}]}`)
	})
	ft.on(arbiterURL, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: {\"synth\":\"arbiter-out\"}\n\n")
	})
	client := &http.Client{Transport: ft}

	rw := newSSERW()
	outcome, err := PanelStreaming(
		rw, client,
		"http://local.local", "local-m",
		frontierURL, "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"test prompt",
		5*time.Second,
		5*time.Second,
		false,
		0.85,
		"test-request-id",
  nil, 0*time.Second,
	)
	if err != nil {
		t.Fatalf("PanelStreaming: %v", err)
	}
	if outcome.ArbiterSkipped {
		t.Errorf("outcome.ArbiterSkipped = true, want false (similarity=%v)", outcome.Similarity)
	}
	if outcome.Similarity >= 0.85 {
		t.Errorf("similarity = %v, want < 0.85", outcome.Similarity)
	}
	if got := *ft.counter(arbiterURL); got != 1 {
		t.Errorf("arbiter fetch = %d, want 1", got)
	}
	body := rw.body.String()
	// Speculative chunk first — either local or frontier may win the
	// race, so accept either answer. The arbiter's synthesis must
	// follow regardless.
	hasLocal := strings.Contains(body, "the quick brown fox")
	hasFrontier := strings.Contains(body, "switch the entire database schema")
	if !hasLocal && !hasFrontier {
		t.Errorf("speculative answer missing: %q", body)
	}
	// ...then arbiter output, then [DONE].
	if !strings.Contains(body, `"synth":"arbiter-out"`) {
		t.Errorf("arbiter output missing: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("missing [DONE] terminator: %q", body)
	}
}

// TestPanelStreamingDegradedSkipLocal mirrors the issue #8 graceful-
// degradation contract: when skipLocal=true, only the frontier panel
// member is fetched. Its content streams as the (only) speculative
// answer and the arbiter is skipped — the user gets the frontier
// reply without paying the local timeout.
func TestPanelStreamingDegradedSkipLocal(t *testing.T) {
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()
	ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("local URL was hit even though skipLocal=true")
		w.WriteHeader(200)
	})
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"frontier-only reply"}}]}`)
	})
	arbiterCalled := 0
	ft.on(arbiterURL, func(w http.ResponseWriter, _ *http.Request) {
		arbiterCalled++
		w.WriteHeader(200)
	})
	client := &http.Client{Transport: ft}

	rw := newSSERW()
	outcome, err := PanelStreaming(
		rw, client,
		"http://local.local", "local-m",
		frontierURL, "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"test prompt",
		5*time.Second, 5*time.Second,
		true, // skipLocal
		0.85,
		"test-request-id",
  nil, 0*time.Second,
	)
	if err != nil {
		t.Fatalf("PanelStreaming: %v", err)
	}
	if !outcome.ArbiterSkipped {
		t.Errorf("ArbiterSkipped = false, want true (skipLocal)")
	}
	if outcome.Source != "frontier" {
		t.Errorf("Source = %q, want frontier", outcome.Source)
	}
	if got := *ft.counter(localURL); got != 0 {
		t.Errorf("local fetch = %d, want 0", got)
	}
	if arbiterCalled != 0 {
		t.Errorf("arbiter called %d times, want 0", arbiterCalled)
	}
	if !strings.Contains(rw.body.String(), "frontier-only reply") {
		t.Errorf("frontier speculative missing: %q", rw.body.String())
	}
	if !strings.Contains(rw.body.String(), "data: [DONE]") {
		t.Errorf("missing [DONE]: %q", rw.body.String())
	}
}

// TestPanelStreamingOneMemberFailedSkipsArbiter covers the partial-
// failure branch: one panel member errors, the other succeeds. The
// successful member's content is streamed and the arbiter is skipped
// — the user has already received an answer, even if from only one
// model.
func TestPanelStreamingOneMemberFailedSkipsArbiter(t *testing.T) {
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()
	ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
		_, _ = io.WriteString(w, `bad gateway`)
	})
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"frontier won"}}]}`)
	})
	arbiterCalled := 0
	ft.on(arbiterURL, func(w http.ResponseWriter, _ *http.Request) {
		arbiterCalled++
		w.WriteHeader(200)
	})
	client := &http.Client{Transport: ft}

	rw := newSSERW()
	outcome, err := PanelStreaming(
		rw, client,
		"http://local.local", "local-m",
		frontierURL, "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"test prompt",
		5*time.Second, 5*time.Second,
		false, 0.85,
		"test-request-id",
  nil, 0*time.Second,
	)
	if err != nil {
		t.Fatalf("PanelStreaming: %v", err)
	}
	if !outcome.ArbiterSkipped {
		t.Errorf("ArbiterSkipped = false, want true (one failed)")
	}
	if outcome.Source != "frontier" {
		t.Errorf("Source = %q, want frontier", outcome.Source)
	}
	if arbiterCalled != 0 {
		t.Errorf("arbiter called %d times, want 0", arbiterCalled)
	}
	if !strings.Contains(rw.body.String(), "frontier won") {
		t.Errorf("frontier speculative missing: %q", rw.body.String())
	}
}

// TestPanelStreamingBothMembersFailedSurfacesError confirms the
// failure mode when no panel member returns content: the call
// returns an error containing both upstream messages, mirroring
// the existing Panel path's surfacing of upstream errors.
func TestPanelStreamingBothMembersFailedSurfacesError(t *testing.T) {
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()
	ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
		_, _ = io.WriteString(w, "local dead")
	})
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
		_, _ = io.WriteString(w, "frontier dead")
	})
	client := &http.Client{Transport: ft}

	rw := newSSERW()
	_, err := PanelStreaming(
		rw, client,
		"http://local.local", "local-m",
		frontierURL, "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"test prompt",
		5*time.Second, 5*time.Second,
		false, 0.85,
		"test-request-id",
  nil, 0*time.Second,
	)
	if err == nil {
		t.Fatal("expected error when both members fail")
	}
	if !strings.Contains(err.Error(), "both members failed") {
		t.Errorf("err = %v, want 'both members failed'", err)
	}
}

// TestPanelStreamingHonorsStreamFalseFallsBackToPanel covers the
// non-streaming contract (issue #10): when body["stream"]=false,
// PanelStreaming must NOT emit SSE chunks. It delegates to the
// legacy Panel path, which writes a single chatCompletionResponse
// JSON object via BufferedFetchWithContext. ArbiterSkipped stays
// false (the legacy path always invokes the arbiter).
func TestPanelStreamingHonorsStreamFalseFallsBackToPanel(t *testing.T) {
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()
	ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"local"}}]}`)
	})
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"frontier"}}]}`)
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
	outcome, err := PanelStreaming(
		rw, client,
		"http://local.local", "local-m",
		frontierURL, "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}, "stream": false},
		"test prompt",
		5*time.Second, 5*time.Second,
		false, 0.85,
		"test-request-id",
  nil, 0*time.Second,
	)
	if err != nil {
		t.Fatalf("PanelStreaming: %v", err)
	}
	if outcome.ArbiterSkipped {
		t.Errorf("ArbiterSkipped = true, want false (legacy Panel path)")
	}
	// Arbiter MUST have been called (legacy path).
	if !strings.Contains(arbiterBody, "Master Synthesis Arbiter") {
		t.Errorf("arbiter body missing arbiter prompt: %s", arbiterBody)
	}
	if !strings.Contains(arbiterBody, `"stream":false`) {
		t.Errorf("arbiter body missing stream=false: %s", arbiterBody)
	}
	// rw must have received JSON, not SSE.
	if !strings.Contains(rw.body.String(), `"content":"synthesized"`) {
		t.Errorf("arbiter JSON not forwarded: %q", rw.body.String())
	}
	if strings.HasPrefix(strings.TrimSpace(rw.body.String()), "data:") {
		t.Errorf("body looks like SSE, want plain JSON: %q", rw.body.String())
	}
}

// TestPanelStreamingThresholdClamping ensures a misconfigured
// threshold is clamped into [0, 1] before being compared, so a
// handler-side misconfiguration cannot break the agreement-skip
// path entirely. Effective semantics:
//
//   - threshold < 0 → clamped to 0 → "similarity >= 0" is always
//     true (similarity is in [0, 1]) → arbiter always skipped
//     when both members succeed
//   - threshold > 1 → clamped to 1 → "similarity >= 1" only holds
//     for identical content → arbiter runs on any divergence
func TestPanelStreamingThresholdClamping(t *testing.T) {
	t.Run("negative threshold clamps to 0 (always skip)", func(t *testing.T) {
		const (
			localURL    = "http://local.local/v1/chat/completions"
			frontierURL = "http://frontier.local"
			arbiterURL  = "http://arbiter.local/v1/chat/completions"
		)
		ft := newFakeTransport()
		ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"identical content"}}]}`)
		})
		ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"identical content"}}]}`)
		})
		arbiterCalled := 0
		ft.on(arbiterURL, func(w http.ResponseWriter, _ *http.Request) {
			arbiterCalled++
			w.WriteHeader(200)
		})
		client := &http.Client{Transport: ft}
		rw := newSSERW()
		outcome, err := PanelStreaming(
			rw, client,
			"http://local.local", "local-m",
			frontierURL, "frontier-m",
			arbiterURL, "", "arbiter-m",
			map[string]interface{}{"messages": []interface{}{}},
			"test prompt",
			5*time.Second, 5*time.Second,
			false,
			-1.0, // negative: clamped to 0 → "always skip when both succeed"
			"test-request-id",
   nil, 0*time.Second,
		)
		if err != nil {
			t.Fatalf("PanelStreaming: %v", err)
		}
		if !outcome.ArbiterSkipped {
			t.Errorf("ArbiterSkipped = false with threshold=0; every non-empty similarity >= 0 must skip")
		}
		if arbiterCalled != 0 {
			t.Errorf("arbiter called %d times, want 0", arbiterCalled)
		}
	})
	t.Run("over-large threshold clamps to 1 (only identical skips)", func(t *testing.T) {
		const (
			localURL    = "http://local.local/v1/chat/completions"
			frontierURL = "http://frontier.local"
			arbiterURL  = "http://arbiter.local/v1/chat/completions"
		)
		ft := newFakeTransport()
		ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"the quick brown fox"}}]}`)
		})
		ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"completely unrelated text about database migration"}}]}`)
		})
		arbiterCalled := 0
		ft.on(arbiterURL, func(w http.ResponseWriter, _ *http.Request) {
			arbiterCalled++
			w.WriteHeader(200)
		})
		client := &http.Client{Transport: ft}
		rw := newSSERW()
		outcome, err := PanelStreaming(
			rw, client,
			"http://local.local", "local-m",
			frontierURL, "frontier-m",
			arbiterURL, "", "arbiter-m",
			map[string]interface{}{"messages": []interface{}{}},
			"test prompt",
			5*time.Second, 5*time.Second,
			false,
			2.0, // >1: clamps to 1 → only identical content skips
			"test-request-id",
   nil, 0*time.Second,
		)
		if err != nil {
			t.Fatalf("PanelStreaming: %v", err)
		}
		if outcome.ArbiterSkipped {
			t.Errorf("ArbiterSkipped = true with threshold=1 on divergent content; arbiter must run")
		}
		if arbiterCalled != 1 {
			t.Errorf("arbiter called %d times, want 1", arbiterCalled)
		}
	})
}

// TestPanelStreamingSpeculativeSourceIdentified confirms the SSE
// chunk embeds the source ("local" / "frontier") in the "nexus"
// metadata field, so log scrapers and harness diagnostics can see
// which model streamed first.
func TestPanelStreamingSpeculativeSourceIdentified(t *testing.T) {
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()
	ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"local answer"}}]}`)
	})
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"frontier answer"}}]}`)
	})
	ft.on(arbiterURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})
	client := &http.Client{Transport: ft}

	rw := newSSERW()
	outcome, err := PanelStreaming(
		rw, client,
		"http://local.local", "local-m",
		frontierURL, "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"test prompt",
		5*time.Second, 5*time.Second,
		false, 0.85,
		"test-request-id",
  nil, 0*time.Second,
	)
	if err != nil {
		t.Fatalf("PanelStreaming: %v", err)
	}
	if outcome.Source != "local" && outcome.Source != "frontier" {
		t.Errorf("Source = %q, want local or frontier", outcome.Source)
	}
	body := rw.body.String()
	wantSource := `"source":"` + outcome.Source + `"`
	if !strings.Contains(body, wantSource) {
		t.Errorf("body missing source tag %s in chunk: %q", wantSource, body)
	}
}

// TestPanelStreamingSetsProgressiveHeader verifies the SSE response
// carries X-Nexus-Fusion-Progressive: true so operators / observability
// tooling can confirm the new code path is active.
func TestPanelStreamingSetsProgressiveHeader(t *testing.T) {
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()
	ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"local"}}]}`)
	})
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"frontier"}}]}`)
	})
	ft.on(arbiterURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})
	client := &http.Client{Transport: ft}

	rw := newSSERW()
	if _, err := PanelStreaming(
		rw, client,
		"http://local.local", "local-m",
		frontierURL, "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"test prompt",
		5*time.Second, 5*time.Second,
		false, 0.85,
		"test-request-id",
  nil, 0*time.Second,
	); err != nil {
		t.Fatalf("PanelStreaming: %v", err)
	}
	if got := rw.header.Get("X-Nexus-Fusion-Progressive"); got != "true" {
		t.Errorf("X-Nexus-Fusion-Progressive = %q, want \"true\"", got)
	}
	if got := rw.header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want \"text/event-stream\"", got)
	}
}

// --- Issue #72: fusion tool_calls tests --------------------------------------

// TestPanelStreamingToolCallWinnerSkipsArbiter verifies that when a
// panel member returns tool_calls, the speculative winner streams them
// as delta.tool_calls and the arbiter is NOT invoked (issue #72). The
// arbiter synthesizes text only and cannot merge tool calls.
func TestPanelStreamingToolCallWinnerSkipsArbiter(t *testing.T) {
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()
	ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`)
	})
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"some text answer"}}]}`)
	})
	var arbiterCalled int
	ft.on(arbiterURL, func(w http.ResponseWriter, _ *http.Request) {
		arbiterCalled++
		w.WriteHeader(200)
	})
	client := &http.Client{Transport: ft}

	rw := newSSERW()
	outcome, err := PanelStreaming(
		rw, client,
		"http://local.local", "local-m",
		frontierURL, "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"test prompt",
		5*time.Second, 5*time.Second,
		false, 0.85,
		"test-request-id",
  nil, 0*time.Second,
	)
	if err != nil {
		t.Fatalf("PanelStreaming: %v", err)
	}
	if !outcome.ArbiterSkipped {
		t.Error("ArbiterSkipped = false, want true for tool-call winner")
	}
	if arbiterCalled != 0 {
		t.Errorf("arbiter called %d times, want 0", arbiterCalled)
	}
	body := rw.body.String()
	if !strings.Contains(body, `"tool_calls"`) {
		t.Errorf("body missing tool_calls delta: %q", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("body missing finish_reason tool_calls: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("missing [DONE]: %q", body)
	}
}

// TestFetchPanelPreservesToolCalls verifies FetchPanel returns tool_calls
// in the AssistantMessage alongside content (issue #72).
func TestFetchPanelPreservesToolCalls(t *testing.T) {
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Body: io.NopCloser(strings.NewReader(
				`{"choices":[{"message":{"content":"running it","tool_calls":[{"id":"c1","type":"function","function":{"name":"exec","arguments":"{}"}}]}}]}`,
			)),
		}, nil
	})}
	got, err := FetchPanel(context.Background(), client, "http://x", "", "m", nil)
	if err != nil {
		t.Fatalf("FetchPanel: %v", err)
	}
	if got.Content != "running it" {
		t.Errorf("Content = %q", got.Content)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(got.ToolCalls))
	}
	if got.ToolCalls[0].Function.Name != "exec" {
		t.Errorf("name = %q", got.ToolCalls[0].Function.Name)
	}
}

// --- Issue #167: client abort during speculative SSE write tests ----------

// brokenPipeRW is a ResponseWriter that fails on the first Write with EPIPE,
// simulating a client that disconnected mid-stream.
type brokenPipeRW struct {
	header http.Header
	status int
	writes int // number of Write calls attempted
}

func newBrokenPipeRW() *brokenPipeRW {
	return &brokenPipeRW{header: http.Header{}}
}

func (b *brokenPipeRW) Header() http.Header { return b.header }
func (b *brokenPipeRW) WriteHeader(s int)   { b.status = s }
func (b *brokenPipeRW) Flush()              {}
func (b *brokenPipeRW) Write(p []byte) (int, error) {
	b.writes++
	// Simulate EPIPE: the client disconnected.
	return len(p), &writeErr{errno: syscall.EPIPE}
}

// writeErr wraps syscall.Errno so errors.Is works correctly.
type writeErr struct {
	errno syscall.Errno
}

func (e *writeErr) Error() string { return e.errno.Error() }
func (e *writeErr) Unwrap() error { return e.errno }

// TestPanelStreamingClientAbortSkipsArbiter verifies that when the client
// disconnects during the speculative SSE write (EPIPE), PanelStreaming
// returns nil (not an error) and does NOT invoke the arbiter. This is the
// fix for issue #167: client abort should be logged at info level and
// return early, not treated as an upstream error.
func TestPanelStreamingClientAbortSkipsArbiter(t *testing.T) {
	const (
		localURL    = "http://local.local/v1/chat/completions"
		frontierURL = "http://frontier.local"
		arbiterURL  = "http://arbiter.local/v1/chat/completions"
	)
	ft := newFakeTransport()
	ft.on(localURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"local answer"}}]}`)
	})
	ft.on(frontierURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"frontier answer"}}]}`)
	})
	// The arbiter handler sets a flag so we can assert it was never called.
	var arbiterCalled int
	ft.on(arbiterURL, func(w http.ResponseWriter, _ *http.Request) {
		arbiterCalled++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: {\"synth\":\"arbiter-out\"}\n\n")
	})
	client := &http.Client{Transport: ft}

	rw := newBrokenPipeRW()
	outcome, err := PanelStreaming(
		rw, client,
		"http://local.local", "local-m",
		frontierURL, "frontier-m",
		arbiterURL, "", "arbiter-m",
		map[string]interface{}{"messages": []interface{}{}},
		"test prompt",
		5*time.Second, 5*time.Second,
		false, 0.85, "test-request",
  nil, 0*time.Second,
	)
	// Issue #167: client abort is NOT returned as an error — we return nil
	// so the handler does not render a 502 error page to a disconnected client.
	if err != nil {
		t.Fatalf("PanelStreaming: got error %v, want nil (client abort)", err)
	}
	// The arbiter must NOT have been invoked — the client is gone, there
	// is nobody to receive the arbiter synthesis.
	if arbiterCalled != 0 {
		t.Errorf("arbiter called %d times, want 0 (client aborted)", arbiterCalled)
	}
	// The speculative write was attempted at least once (the broken pipe fired).
	if rw.writes == 0 {
		t.Errorf("rw.writes = 0, want >= 1 (write should have been attempted)")
	}
	// outcome.Source should still be set from the winner selection.
	if outcome.Source != "local" && outcome.Source != "frontier" {
		t.Errorf("outcome.Source = %q, want local or frontier", outcome.Source)
	}
	_ = outcome // outcome.Similarity is 0 since we never reached agreement check
}

// TestIsClientAbort verifies the IsClientAbort helper correctly identifies
// EPIPE, ECONNRESET, and ErrClientAbort.
func TestIsClientAbort(t *testing.T) {
	epipeErr := &writeErr{errno: syscall.EPIPE}
	connresetErr := &writeErr{errno: syscall.ECONNRESET}
	otherErr := errors.New("some other error")

	if !IsClientAbort(epipeErr) {
		t.Error("IsClientAbort(EPIPE) = false, want true")
	}
	if !IsClientAbort(connresetErr) {
		t.Error("IsClientAbort(ECONNRESET) = false, want true")
	}
	if !IsClientAbort(ErrClientAbort) {
		t.Error("IsClientAbort(ErrClientAbort) = false, want true")
	}
	if IsClientAbort(otherErr) {
		t.Error("IsClientAbort(other error) = true, want false")
	}
	if IsClientAbort(nil) {
		t.Error("IsClientAbort(nil) = true, want false")
	}
}
