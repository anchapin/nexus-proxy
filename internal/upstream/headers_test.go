package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestHeaderAllowed verifies the allowlist logic directly (issue #39):
// Content-Type and Cache-Control pass; X-Nexus-* passes by prefix;
// Server, Set-Cookie, Via, and X-RateLimit-* are dropped.
func TestHeaderAllowed(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"Content-Type", true},
		{"Cache-Control", true},
		{"X-Nexus-Degraded", true},
		{"X-Nexus-Overflow", true},
		{"X-Nexus-Ratelimit-Remaining", true},
		{"Server", false},
		{"Set-Cookie", false},
		{"Via", false},
		{"X-RateLimit-Remaining", false},
		{"X-Powered-By", false},
		{"Authorization", false},
	}
	for _, tc := range cases {
		if got := headerAllowed(tc.name); got != tc.want {
			t.Errorf("headerAllowed(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCopyAllowedHeadersDropsLeaks asserts the helper copies only
// allowlisted headers and leaves the rest behind (issue #39).
func TestCopyAllowedHeadersDropsLeaks(t *testing.T) {
	src := http.Header{
		"Content-Type":     []string{"text/event-stream"},
		"Cache-Control":    []string{"no-cache"},
		"X-Nexus-Degraded": []string{"true"},
		"Server":           []string{"cloudfront"},
		"Set-Cookie":       []string{"session=abc; HttpOnly"},
		"Via":              []string{"1.1 proxy"},
		"X-Ratelimit-Remaining": []string{"0"},
	}
	dst := http.Header{}
	copyAllowedHeaders(dst, src)

	if got := dst.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := dst.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := dst.Get("X-Nexus-Degraded"); got != "true" {
		t.Errorf("X-Nexus-Degraded = %q, want true", got)
	}
	for _, leaked := range []string{"Server", "Set-Cookie", "Via", "X-Ratelimit-Remaining"} {
		if v := dst.Get(leaked); v != "" {
			t.Errorf("leaked header %s = %q, want dropped", leaked, v)
		}
	}
}

// TestStreamDropsNonAllowlistedHeaders asserts StreamWithContext forwards
// only allowlisted upstream headers to the client writer (issue #39).
func TestStreamDropsNonAllowlistedHeaders(t *testing.T) {
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header: http.Header{
				"Content-Type":     []string{"text/event-stream"},
				"Server":           []string{"cloudfront"},
				"Set-Cookie":       []string{"s=1"},
				"Via":              []string{"1.1 gw"},
				"X-Ratelimit-Remaining": []string{"0"},
				"X-Nexus-Custom":   []string{"ok"},
			},
			Body: io.NopCloser(strings.NewReader("data: {\"a\":1}\n\n")),
		}, nil
	})}
	rw := newRW()
	if err := StreamWithContext(context.Background(), rw, client, "http://x", "",
		map[string]interface{}{"model": "m"}); err != nil {
		t.Fatalf("StreamWithContext: %v", err)
	}
	if got := rw.header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rw.header.Get("X-Nexus-Custom"); got != "ok" {
		t.Errorf("X-Nexus-Custom = %q, want ok", got)
	}
	for _, leaked := range []string{"Server", "Set-Cookie", "Via", "X-Ratelimit-Remaining"} {
		if v := rw.header.Get(leaked); v != "" {
			t.Errorf("Stream leaked upstream header %s = %q", leaked, v)
		}
	}
}

// TestBufferedFetchDropsNonAllowlistedHeaders asserts BufferedFetchWithContext
// forwards only allowlisted headers and re-asserts Content-Type as JSON
// (issue #39).
func TestBufferedFetchDropsNonAllowlistedHeaders(t *testing.T) {
	client := &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header: http.Header{
				"Content-Type":     []string{"application/xml"},
				"Server":           []string{"nginx"},
				"Set-Cookie":       []string{"s=2"},
				"X-Powered-By":     []string{"Express"},
				"Cache-Control":    []string{"no-store"},
			},
			Body: io.NopCloser(strings.NewReader(`{"id":"x","object":"chat.completion","choices":[{"message":{"content":"hi"}}]}`)),
		}, nil
	})}
	rw := newJSONRW()
	if err := BufferedFetchWithContext(context.Background(), rw, client, "http://x", "",
		map[string]interface{}{"model": "m"}); err != nil {
		t.Fatalf("BufferedFetchWithContext: %v", err)
	}
	// Content-Type is always re-asserted to application/json.
	if got := rw.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	// Cache-Control is on the allowlist.
	if got := rw.header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	for _, leaked := range []string{"Server", "Set-Cookie", "X-Powered-By"} {
		if v := rw.header.Get(leaked); v != "" {
			t.Errorf("BufferedFetch leaked upstream header %s = %q", leaked, v)
		}
	}
}
