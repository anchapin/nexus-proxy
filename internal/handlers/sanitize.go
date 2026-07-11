package handlers

import (
	"strings"
)

// MaxHeaderValue is the hard cap on any X-Nexus-Route-* response
// header value. Header values are echoed from the routing Decision
// and could carry attacker-influenced text (e.g. an SLM error
// message echoing a malformed prompt). Truncating to a small bound
// keeps the response header block bounded and prevents log-injection
// via CR/LF smuggling.
const MaxHeaderValue = 128

// SanitizeHeaderValue prepares a string for use as an X-Nexus-Route-*
// response header value (issue #74). It:
//   - strips CR and LF (header injection prevention);
//   - collapses other control characters to spaces;
//   - trims leading/trailing whitespace;
//   - truncates to MaxHeaderValue runes.
//
// The function never returns an empty string when the input is
// non-empty after cleaning — callers that want a placeholder for an
// empty result should supply one before calling.
func SanitizeHeaderValue(s string) string {
	if s == "" {
		return ""
	}
	if isCleanHeaderValue(s) && len([]rune(s)) <= MaxHeaderValue {
		return strings.TrimSpace(s)
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n', r == '\r':
			continue
		case r < 0x20:
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if r := []rune(out); len(r) > MaxHeaderValue {
		out = string(r[:MaxHeaderValue])
	}
	return out
}

// isCleanHeaderValue reports whether s contains no control characters
// and no leading/trailing whitespace, so the fast path can skip the
// rune scan entirely.
func isCleanHeaderValue(s string) bool {
	if s == "" {
		return true
	}
	if s[0] <= 0x20 || s[len(s)-1] <= 0x20 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 {
			return false
		}
	}
	return true
}
