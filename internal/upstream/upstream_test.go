package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func (f rtFunc) Do(r *http.Request) (*http.Response, error)       { return f(r) }

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

func (r *recordingRW) Header() http.Header        { return r.header }
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

func (r *nonFlushRW) Header() http.Header        { return r.header }
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

// avoid unused-import warning when only some tests run.
var _ = time.Second