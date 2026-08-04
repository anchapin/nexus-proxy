// Response-content redaction (issue #1172).
//
// This file implements an optional http.ResponseWriter wrapper that
// scans every Write against a configurable set of regex patterns and
// replaces matches with [REDACTED] before the bytes reach the client.
// The redactor lives in internal/handlers (NOT internal/middleware)
// because it must intercept the *response* stream and therefore needs
// net/http — internal/middleware is intentionally net/http-free.
//
// Profiles:
//   - secrets: bearer tokens (sk-…, AKIA…, ASI…, ghp_…, xoxb-…, AIza…),
//     PEM private-key blocks.
//   - pii: credit-card numbers (Luhn-validated), US SSNs, email.
//   - custom: operator-supplied regexes from NEXUS_REDACT_PATTERNS.
//
// Streaming safety: multi-line patterns (PEM private keys) that are
// split across two SSE chunks are handled by a rolling buffer capped at
// maxBuffer bytes. When the buffer contains the start of a PEM block
// that has not completed yet, Flush calls are suppressed until the END
// marker arrives or the buffer overflows — preventing a partial key
// from leaking on the wire.

package handlers

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
)

// Redaction profile constants (issue #1172). These are the valid
// values for NEXUS_REDACT_PROFILE.
const (
	RedactProfileOff     = "off"
	RedactProfileSecrets = "secrets"
	RedactProfilePII     = "pii"
	RedactProfileCustom  = "custom"
)

// DefaultRedactBufferBytes is the default rolling buffer cap for
// cross-chunk multi-line pattern matching (issue #1172). 4 KiB is
// large enough to hold a typical PEM-encoded RSA private key while
// keeping the streaming latency overhead negligible.
const DefaultRedactBufferBytes = 4 * 1024

// RedactedPlaceholder replaces every matched span in the response body.
// No matched content is ever logged — the counter records only the
// number of substitutions, never the text.
const RedactedPlaceholder = "[REDACTED]"

// pemBeginMarker is the sentinel indicating a PEM-encoded block has
// started but may not have finished. When the buffer contains this
// marker without pemEndMarker we suppress Flush to avoid leaking the
// partial block across an SSE chunk boundary.
const (
	pemBeginMarker = "-----BEGIN"
	pemEndMarker   = "-----END"
)

// compiledPattern pairs a compiled regex with a flag indicating
// whether matches must pass Luhn validation before replacement.
// Only credit-card patterns set luhn=true.
type compiledPattern struct {
	re   *regexp.Regexp
	luhn bool
}

var (
	// secretsPatterns: common bearer tokens and PEM private-key blocks.
	secretsPatterns = []compiledPattern{
		{re: regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`)},
		{re: regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
		{re: regexp.MustCompile(`ASI[A-Za-z0-9]{16,}`)},
		{re: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`)},
		{re: regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`)},
		{re: regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
		{re: regexp.MustCompile(`(?s)-----BEGIN (?:[A-Z ]+ )?PRIVATE KEY-----.*?-----END (?:[A-Z ]+ )?PRIVATE KEY-----`)},
	}

	// piiPatterns: credit-card numbers (Luhn-validated), US SSNs, emails.
	piiPatterns = []compiledPattern{
		{re: regexp.MustCompile(`\b(?:\d[ -]*?){13,19}\b`), luhn: true},
		{re: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
		{re: regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)},
	}
)

// PatternsForProfile returns the compiled pattern set for the given
// profile, merging in custom patterns when profile is "custom". Returns
// nil for "off" or unrecognised profiles (no redaction).
func PatternsForProfile(profile string, custom []*regexp.Regexp) []compiledPattern {
	switch profile {
	case RedactProfileSecrets:
		return secretsPatterns
	case RedactProfilePII:
		return piiPatterns
	case RedactProfileCustom:
		out := make([]compiledPattern, len(custom))
		for i, re := range custom {
			out[i] = compiledPattern{re: re}
		}
		return out
	default:
		return nil
	}
}

// ResponseRedactor wraps an http.ResponseWriter, scanning each write
// against a compiled set of regex patterns and replacing matches with
// [REDACTED] before forwarding to the inner writer. Multi-line
// patterns (e.g. PEM private-key blocks) are handled by a rolling
// buffer capped at maxBuffer bytes.
type ResponseRedactor struct {
	w             http.ResponseWriter
	patterns      []compiledPattern
	buf           []byte
	maxBuffer     int
	substitutions int64
}

// NewResponseRedactor wraps w with a redactor using the given
// compiled patterns. bufferBytes controls the rolling buffer cap for
// cross-chunk multi-line matching; values <= 0 fall back to
// DefaultRedactBufferBytes. A nil or empty patterns slice produces a
// transparent passthrough (no substitutions).
func NewResponseRedactor(w http.ResponseWriter, patterns []compiledPattern, bufferBytes int) *ResponseRedactor {
	if bufferBytes <= 0 {
		bufferBytes = DefaultRedactBufferBytes
	}
	return &ResponseRedactor{
		w:         w,
		patterns:  patterns,
		maxBuffer: bufferBytes,
	}
}

// Header implements http.ResponseWriter.
func (r *ResponseRedactor) Header() http.Header { return r.w.Header() }

// Write appends to the internal buffer and drains when the buffer
// reaches the cap. The returned length always equals len(b) so the
// upstream writer records the correct byte count.
func (r *ResponseRedactor) Write(b []byte) (int, error) {
	r.buf = append(r.buf, b...)
	if len(r.buf) >= r.maxBuffer {
		r.drain()
	}
	return len(b), nil
}

// WriteHeader implements http.ResponseWriter.
func (r *ResponseRedactor) WriteHeader(s int) { r.w.WriteHeader(s) }

// Flush drains the buffer and forwards the flush to the inner writer —
// unless the buffer contains the start of an incomplete PEM block and
// is still under the cap, in which case the flush is suppressed to
// wait for the block to complete.
func (r *ResponseRedactor) Flush() {
	if r.holdingForMultiline() {
		return
	}
	r.drain()
}

// holdingForMultiline returns true when the buffer contains the start
// of a PEM block that hasn't completed yet. This prevents a partial
// private key from leaking across an SSE chunk boundary.
func (r *ResponseRedactor) holdingForMultiline() bool {
	if len(r.buf) == 0 || len(r.buf) >= r.maxBuffer {
		return false
	}
	s := string(r.buf)
	return strings.Contains(s, pemBeginMarker) &&
		!strings.Contains(s, pemEndMarker)
}

// drain applies all patterns to the buffer, writes the redacted result
// to the inner writer, clears the buffer, and forwards the flush.
func (r *ResponseRedactor) drain() {
	if len(r.buf) == 0 {
		if f, ok := r.w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	result := string(r.buf)
	r.buf = r.buf[:0]
	for _, cp := range r.patterns {
		if cp.luhn {
			result = cp.re.ReplaceAllStringFunc(result, func(m string) string {
				if luhnValid(m) {
					atomic.AddInt64(&r.substitutions, 1)
					return RedactedPlaceholder
				}
				return m
			})
		} else {
			matches := cp.re.FindAllString(result, -1)
			if len(matches) > 0 {
				result = cp.re.ReplaceAllString(result, RedactedPlaceholder)
				atomic.AddInt64(&r.substitutions, int64(len(matches)))
			}
		}
	}
	_, _ = r.w.Write([]byte(result))
	if f, ok := r.w.(http.Flusher); ok {
		f.Flush()
	}
}

// Substitutions returns the total number of pattern replacements made
// across all writes. Safe to call after the upstream Stream returns.
func (r *ResponseRedactor) Substitutions() int64 {
	return atomic.LoadInt64(&r.substitutions)
}

// luhnValid reports whether the digit sequence in s satisfies the
// Luhn checksum. Non-digit characters (spaces, hyphens) are stripped
// before validation. Returns false for sequences shorter than 13 or
// longer than 19 digits (valid credit-card range).
func luhnValid(s string) bool {
	var digits []int
	for _, c := range s {
		if c >= '0' && c <= '9' {
			digits = append(digits, int(c-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

// RedactionEnabled reports whether redaction should be applied based on
// the config. Redaction is active when NEXUS_REDACT_ENABLED is true AND
// NEXUS_REDACT_PROFILE is one of {secrets, pii, custom}.
func RedactionEnabled(enabled bool, profile string) bool {
	if !enabled {
		return false
	}
	switch profile {
	case RedactProfileSecrets, RedactProfilePII, RedactProfileCustom:
		return true
	default:
		return false
	}
}

// compileCustomPatterns parses a comma-separated list of regex
// patterns and compiles each one. Whitespace around each pattern is
// trimmed. Empty entries are skipped. An invalid pattern causes a
// boot-time error.
func compileCustomPatterns(raw string) ([]*regexp.Regexp, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]*regexp.Regexp, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("config: NEXUS_REDACT_PATTERNS entry %q is not a valid regex: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// compileCustomPatternsOrNil is a convenience wrapper that panics on
// error. It is intended for the chat-handler hot path where the
// patterns have already been validated by config.Validate().
func compileCustomPatternsOrNil(raw string) []*regexp.Regexp {
	re, err := compileCustomPatterns(raw)
	if err != nil {
		// Should not happen — Validate() catches bad patterns at boot.
		panic(fmt.Sprintf("redaction: invalid custom patterns after Validate: %v", err))
	}
	return re
}

// Compile-time assertions: ResponseRedactor satisfies the interfaces
// upstream.Stream / upstream.BufferedFetch require.
var (
	_ http.ResponseWriter = (*ResponseRedactor)(nil)
	_ http.Flusher        = (*ResponseRedactor)(nil)
)
