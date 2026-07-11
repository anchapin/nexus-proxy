package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSanitizeRequestID covers the X-Request-Id sanitization rules
// (issue #39): strip chars outside [a-zA-Z0-9._:-], cap at 128 bytes,
// return "" when nothing survives so the caller mints a hex id.
func TestSanitizeRequestID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"clean alphanumeric", "abc123", "abc123"},
		{"dots dashes underscores colons", "req.1_2-3:4", "req.1_2-3:4"},
		{"strip spaces", "ab cd", "abcd"},
		{"strip newlines (log injection)", "ab\ncd\nef", "abcdef"},
		{"strip control bytes", "a\x00b\x1fcd", "abcd"},
		{"strip quotes and angle brackets", `a"b<c>`, "abc"},
		{"strip unicode", "café", "caf"},
		{"keep colon separator", "trace:abc:123", "trace:abc:123"},
		{"only disallowed chars", "  \n\t  ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeRequestID(tc.in); got != tc.want {
				t.Errorf("sanitizeRequestID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeRequestIDTruncates asserts values longer than 128 bytes are
// truncated to the cap (issue #39).
func TestSanitizeRequestIDTruncates(t *testing.T) {
	long := strings.Repeat("a", 300)
	got := sanitizeRequestID(long)
	if len(got) != requestIDMaxLen {
		t.Errorf("len(sanitizeRequestID(300×a)) = %d, want %d", len(got), requestIDMaxLen)
	}
}

// TestRequestIDSanitizesInboundHeader asserts the requestID() helper
// sanitizes an inbound X-Request-Id before returning it (issue #39),
// and falls through to a generated hex id when sanitization empties it.
func TestRequestIDSanitizesInboundHeader(t *testing.T) {
	// Dirty inbound id → sanitized.
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Request-Id", "abc\r\n<script>123")
	got := requestID(r)
	if want := "abcscript123"; got != want {
		t.Errorf("requestID with dirty header = %q, want %q", got, want)
	}

	// Inbound id that is only junk → fall through to generated hex.
	r2 := httptest.NewRequest(http.MethodPost, "/", nil)
	r2.Header.Set("X-Request-Id", "\n\n  ")
	got2 := requestID(r2)
	if !strings.HasPrefix(got2, "req-") {
		t.Errorf("requestID with junk-only header = %q, want a req-<hex> fallback", got2)
	}

	// No inbound id → generated hex.
	r3 := httptest.NewRequest(http.MethodPost, "/", nil)
	got3 := requestID(r3)
	if !strings.HasPrefix(got3, "req-") {
		t.Errorf("requestID with no header = %q, want a req-<hex> fallback", got3)
	}
}
