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
		{strings.Repeat("x", 200), strings.Repeat("x", MaxHeaderValue)},
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
