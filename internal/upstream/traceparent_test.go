package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestStreamSendsTraceparentWhenSet verifies the W3C traceparent
// header propagates through Stream when WithTraceparent is passed.
func TestStreamSendsTraceparentWhenSet(t *testing.T) {
	const wantTP = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	var seenTP string
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		seenTP = r.Header.Get("traceparent")
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader("ok")),
		}, nil
	})}
	if err := Stream(newRW(), client, "http://x", "", map[string]interface{}{"model": "m"}, WithTraceparent(wantTP)); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if seenTP != wantTP {
		t.Errorf("traceparent = %q, want %q", seenTP, wantTP)
	}
}

// TestStreamOmitsTraceparentByDefault verifies no header is set when
// no option is passed — preserves pre-issue-#41 behaviour exactly.
func TestStreamOmitsTraceparentByDefault(t *testing.T) {
	var seenTP string
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		seenTP = r.Header.Get("traceparent")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	if err := Stream(newRW(), client, "http://x", "", map[string]interface{}{"model": "m"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if seenTP != "" {
		t.Errorf("expected no traceparent header, got %q", seenTP)
	}
}

// TestStreamIgnoresEmptyTraceparent confirms an empty-string opt is a
// no-op, so the chat handler can thread the conditional header
// without nil-checking.
func TestStreamIgnoresEmptyTraceparent(t *testing.T) {
	var seenTP string
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		seenTP = r.Header.Get("traceparent")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	if err := Stream(newRW(), client, "http://x", "", map[string]interface{}{"model": "m"}, WithTraceparent("")); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if seenTP != "" {
		t.Errorf("expected empty traceparent to be ignored, got %q", seenTP)
	}
}

// TestBufferedFetchSendsTraceparent mirrors the Stream test for the
// non-streaming path. Both functions share the same option plumbing.
func TestBufferedFetchSendsTraceparent(t *testing.T) {
	const wantTP = "00-deadbeefdeadbeefdeadbeefdeadbeef-cafebabecafebabe-01"
	var seenTP string
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		seenTP = r.Header.Get("traceparent")
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"choices":[]}`)),
		}, nil
	})}
	if err := BufferedFetch(newRW(), client, "http://x", "", map[string]interface{}{"model": "m"}, WithTraceparent(wantTP)); err != nil {
		t.Fatalf("BufferedFetch: %v", err)
	}
	if seenTP != wantTP {
		t.Errorf("traceparent = %q, want %q", seenTP, wantTP)
	}
}

// TestFetchPanelSendsTraceparent verifies the fusion panel's per-member
// fetch also carries the header so distributed traces stay correlated
// across the local + frontier + arbiter split.
func TestFetchPanelSendsTraceparent(t *testing.T) {
	const wantTP = "00-11112222333344445555666677778888-9999aaaabbbbcccc-01"
	var seenTP string
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		seenTP = r.Header.Get("traceparent")
		return &http.Response{
			StatusCode: 200,
			Body: io.NopCloser(strings.NewReader(
				`{"choices":[{"message":{"content":"hi"}}]}`)),
		}, nil
	})}
	body := map[string]interface{}{"messages": []interface{}{}}
	if _, err := FetchPanel(context.Background(), client, "http://x", "", "m", body, WithTraceparent(wantTP)); err != nil {
		t.Fatalf("FetchPanel: %v", err)
	}
	if seenTP != wantTP {
		t.Errorf("traceparent = %q, want %q", seenTP, wantTP)
	}
}

// TestWithTraceparentNilSafe ensures a nil option does not crash the
// apply chain. Defensive against future wiring mistakes.
func TestWithTraceparentNilSafe(t *testing.T) {
	o := applyCallOpts([]CallOption{nil, WithTraceparent(""), WithTraceparent("ab")})
	if o.traceparent != "ab" {
		t.Errorf("got %q, want ab", o.traceparent)
	}
}

// Compile-time guard that applyCallOpts is nil-safe under empty input.
var _ = applyCallOpts(nil)
