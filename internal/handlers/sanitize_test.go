package handlers

import (
	"strings"
	"testing"
)

func TestSanitizeHeaderValue(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"frontier", "frontier"},
		{"  spaced  ", "spaced"},
		{"line1\r\nline2", "line1line2"},
		{"line1\n", "line1"},
		{"a\x00b\x01c", "a b c"},
		// Exactly at the boundary: no marker (issue #494).
		{strings.Repeat("x", MaxHeaderValue), strings.Repeat("x", MaxHeaderValue)},
		// Below the boundary: no marker.
		{strings.Repeat("x", 50), strings.Repeat("x", 50)},
		// Above the boundary: truncated prefix plus "...(+N)" marker,
		// where N is the count of dropped runes (issue #494).
		{strings.Repeat("x", 200), strings.Repeat("x", MaxHeaderValue) + "...(+72)"},
	}
	for _, c := range cases {
		got := SanitizeHeaderValue(c.in)
		if got != c.want {
			t.Errorf("SanitizeHeaderValue(%q) = %q (len %d), want %q (len %d)",
				c.in, got, len(got), c.want, len(c.want))
		}
	}
	for _, evil := range []string{"a\r\nb", "a\rb", "a\nb", "\r\n"} {
		out := SanitizeHeaderValue(evil)
		if strings.ContainsAny(out, "\r\n") {
			t.Errorf("CR/LF leaked into sanitized output: %q", out)
		}
	}
}
