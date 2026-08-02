package handlers

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// newTestRedactor creates a ResponseRedactor wrapping an httptest recorder
// with the given patterns and a 4 KiB buffer.
func newTestRedactor(patterns []compiledPattern, t *testing.T) (*ResponseRedactor, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	r := NewResponseRedactor(rec, patterns, 0)
	return r, rec
}

func TestRedactSecretsProfile(t *testing.T) {
	r, rec := newTestRedactor(secretsPatterns, t)
	input := `{"choices":[{"delta":{"content":"key is sk-abcdefghijklmnopqrstuvwxyz0123456789 and AKIAIOSFODNN7EXAMPLE"}}]}`
	r.Write([]byte(input))
	r.Flush()
	body := rec.Body.String()
	if strings.Contains(body, "sk-abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("sk- token not redacted: %s", body)
	}
	if strings.Contains(body, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("AKIA token not redacted: %s", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("expected [REDACTED] placeholder in output")
	}
	if r.Substitutions() < 2 {
		t.Errorf("expected >=2 substitutions, got %d", r.Substitutions())
	}
}

func TestRedactGitHubToken(t *testing.T) {
	r, rec := newTestRedactor(secretsPatterns, t)
	token := "ghp_1234567890abcdefghijklmnopqrstuvwxyzABCD"
	r.Write([]byte("token: " + token))
	r.Flush()
	if strings.Contains(rec.Body.String(), token) {
		t.Errorf("GitHub token not redacted")
	}
}

func TestRedactSlackToken(t *testing.T) {
	r, rec := newTestRedactor(secretsPatterns, t)
	// Use a clearly-fake token that still matches the regex but
	// won't trigger GitHub secret scanning push protection.
	token := "xoxb-TESTFAKETOKEN0-XXXXXXXXXXXXXXXXXXX"
	r.Write([]byte("slack: " + token))
	r.Flush()
	if strings.Contains(rec.Body.String(), token) {
		t.Errorf("Slack token not redacted")
	}
}

func TestRedactGoogleAPIKey(t *testing.T) {
	r, rec := newTestRedactor(secretsPatterns, t)
	key := "AIzaTESTFAKEKEY000000000000000000000000"
	r.Write([]byte("google: " + key))
	r.Flush()
	if strings.Contains(rec.Body.String(), key) {
		t.Errorf("Google API key not redacted")
	}
}

func TestRedactPrivateKeyFull(t *testing.T) {
	r, rec := newTestRedactor(secretsPatterns, t)
	key := `-----BEGIN PRIVATE KEY-----
MIIEvAIBADANBgkqhkiG9w0BAQEFAASCBKYwggSiAgEAAoIBAQDB1234567890
abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890ab
-----END PRIVATE KEY-----`
	r.Write([]byte("Here is the key:\n" + key + "\nDone."))
	r.Flush()
	body := rec.Body.String()
	if strings.Contains(body, "MIIEvAIB") {
		t.Errorf("private key body not redacted")
	}
	if strings.Contains(body, "BEGIN PRIVATE KEY") {
		t.Errorf("BEGIN marker not redacted")
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("expected [REDACTED] placeholder")
	}
	if !strings.Contains(body, "Done.") {
		t.Errorf("text after key block should be preserved")
	}
}

func TestRedactPrivateKeySplitAcrossChunks(t *testing.T) {
	r, rec := newTestRedactor(secretsPatterns, t)
	chunk1 := `data: {"choices":[{"delta":{"content":"key:\n-----BEGIN PRIVATE KEY-----
MIIEvAIBADANBgkqhkiG9w0BAQEFAASCBKYwggSiAgEAAoIBAQDB1234567890ab`
	chunk2 := `cdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890
-----END PRIVATE KEY-----
"}}
`

	// Write chunk1 — buffer contains BEGIN but no END → suppress flush.
	r.Write([]byte(chunk1))
	r.Flush()
	if rec.Body.Len() > 0 {
		t.Errorf("expected buffer hold (no output yet), got %d bytes: %s",
			rec.Body.Len(), rec.Body.String())
	}

	// Write chunk2 — now the full key is in the buffer.
	r.Write([]byte(chunk2))
	r.Flush()
	body := rec.Body.String()
	if strings.Contains(body, "MIIEvAIB") {
		t.Errorf("private key body leaked across chunk boundary")
	}
	if strings.Contains(body, "BEGIN PRIVATE KEY") {
		t.Errorf("BEGIN marker leaked")
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("expected [REDACTED] placeholder after cross-chunk redaction")
	}
}

func TestRedactPIICreditCardValid(t *testing.T) {
	r, rec := newTestRedactor(piiPatterns, t)
	// 4242 4242 4242 4242 is a valid Luhn number (Stripe test card).
	r.Write([]byte("card: 4242 4242 4242 4242"))
	r.Flush()
	body := rec.Body.String()
	if strings.Contains(body, "4242") {
		t.Errorf("valid credit card not redacted: %s", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("expected [REDACTED]")
	}
}

func TestRedactPIICreditCardInvalidLuhn(t *testing.T) {
	r, rec := newTestRedactor(piiPatterns, t)
	// 13 digits but fails Luhn — should NOT be redacted.
	r.Write([]byte("id: 1234567890123"))
	r.Flush()
	body := rec.Body.String()
	if strings.Contains(body, "[REDACTED]") {
		t.Errorf("invalid Luhn number should not be redacted: %s", body)
	}
	if !strings.Contains(body, "1234567890123") {
		t.Errorf("non-card number should be preserved")
	}
}

func TestRedactPIICreditCardContinuous(t *testing.T) {
	r, rec := newTestRedactor(piiPatterns, t)
	// 4242424242424242 without spaces — still valid Luhn.
	r.Write([]byte("4242424242424242"))
	r.Flush()
	if !strings.Contains(rec.Body.String(), "[REDACTED]") {
		t.Errorf("continuous credit card should be redacted")
	}
}

func TestRedactPIISSN(t *testing.T) {
	r, rec := newTestRedactor(piiPatterns, t)
	r.Write([]byte("ssn: 123-45-6789"))
	r.Flush()
	body := rec.Body.String()
	if strings.Contains(body, "123-45-6789") {
		t.Errorf("SSN not redacted: %s", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("expected [REDACTED]")
	}
}

func TestRedactPIIEmail(t *testing.T) {
	r, rec := newTestRedactor(piiPatterns, t)
	r.Write([]byte("contact: user@example.com end"))
	r.Flush()
	body := rec.Body.String()
	if strings.Contains(body, "user@example.com") {
		t.Errorf("email not redacted: %s", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("expected [REDACTED]")
	}
}

func TestRedactCustomProfile(t *testing.T) {
	custom := []*regexp.Regexp{regexp.MustCompile(`SECRET-\d+`)}
	patterns := PatternsForProfile(RedactProfileCustom, custom)
	r, rec := newTestRedactor(patterns, t)
	r.Write([]byte("value: SECRET-12345 done"))
	r.Flush()
	body := rec.Body.String()
	if strings.Contains(body, "SECRET-12345") {
		t.Errorf("custom pattern not redacted: %s", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("expected [REDACTED]")
	}
	if !strings.Contains(body, "done") {
		t.Errorf("surrounding text should be preserved")
	}
}

func TestRedactOffIsPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	// Nil patterns = passthrough.
	r := NewResponseRedactor(rec, nil, 0)
	input := "sk-test12345678901234567890 raw content"
	n, err := r.Write([]byte(input))
	if err != nil {
		t.Fatalf("write error: %v", err)
	}
	if n != len(input) {
		t.Errorf("write returned %d, want %d", n, len(input))
	}
	r.Flush()
	if rec.Body.String() != input {
		t.Errorf("passthrough mismatch: got %q, want %q", rec.Body.String(), input)
	}
	if r.Substitutions() != 0 {
		t.Errorf("expected 0 substitutions for nil patterns, got %d", r.Substitutions())
	}
}

func TestRedactBufferOverflowFlushesPartial(t *testing.T) {
	// When buffer exceeds maxBuffer, a forced flush must happen even
	// if an incomplete PEM block is present — better to redact what
	// we can than to grow unbounded.
	r, rec := newTestRedactor(secretsPatterns, t)
	r.maxBuffer = 64 // force small buffer
	// Build content larger than maxBuffer containing a partial key.
	large := strings.Repeat("x", 100) + "-----BEGIN PRIVATE KEY-----\nMIIB1234"
	r.Write([]byte(large))
	r.Flush()
	body := rec.Body.String()
	if len(body) == 0 {
		t.Errorf("expected forced flush on buffer overflow")
	}
}

func TestRedactMultipleWritesAccumulate(t *testing.T) {
	r, rec := newTestRedactor(secretsPatterns, t)
	r.Write([]byte("part1 sk-"))
	r.Write([]byte("abcdefghijklmnopqrstuvwxyz0123456789"))
	r.Flush()
	body := rec.Body.String()
	if strings.Contains(body, "sk-abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("token split across writes not redacted: %s", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("expected [REDACTED]")
	}
}

func TestRedactSubstitutionCount(t *testing.T) {
	r, _ := newTestRedactor(secretsPatterns, t)
	r.Write([]byte("AKIAIOSFODNN7EXAMPLE and AKIAVRASECGHIJKLMNOP"))
	r.Flush()
	if r.Substitutions() != 2 {
		t.Errorf("expected 2 substitutions, got %d", r.Substitutions())
	}
}

func TestRedactHeaderPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	r := NewResponseRedactor(rec, secretsPatterns, 0)
	r.Header().Set("X-Test", "value")
	r.WriteHeader(200)
	if rec.Header().Get("X-Test") != "value" {
		t.Errorf("Header() not passed through")
	}
	if rec.Code != 200 {
		t.Errorf("WriteHeader not passed through: got %d", rec.Code)
	}
}

func TestRedactionEnabled(t *testing.T) {
	tests := []struct {
		enabled bool
		profile string
		want    bool
	}{
		{false, "off", false},
		{false, "secrets", false},
		{true, "off", false},
		{true, "", false},
		{true, "secrets", true},
		{true, "pii", true},
		{true, "custom", true},
		{true, "bogus", false},
	}
	for _, tt := range tests {
		got := RedactionEnabled(tt.enabled, tt.profile)
		if got != tt.want {
			t.Errorf("RedactionEnabled(%v, %q) = %v, want %v",
				tt.enabled, tt.profile, got, tt.want)
		}
	}
}

func TestPatternsForProfileOff(t *testing.T) {
	if p := PatternsForProfile("off", nil); p != nil {
		t.Errorf("PatternsForProfile(off) = %v, want nil", p)
	}
	if p := PatternsForProfile("bogus", nil); p != nil {
		t.Errorf("PatternsForProfile(bogus) = %v, want nil", p)
	}
}

func TestCompileCustomPatterns(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		patterns, err := compileCustomPatterns(`SECRET-\d+,\s*token:\s*\w+`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(patterns) != 2 {
			t.Errorf("expected 2 patterns, got %d", len(patterns))
		}
	})
	t.Run("empty", func(t *testing.T) {
		patterns, err := compileCustomPatterns("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if patterns != nil {
			t.Errorf("expected nil for empty input, got %v", patterns)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		_, err := compileCustomPatterns(`[invalid`)
		if err == nil {
			t.Errorf("expected error for invalid regex")
		}
	})
	t.Run("whitespace trimmed", func(t *testing.T) {
		patterns, err := compileCustomPatterns(` \d+ , \w+ `)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(patterns) != 2 {
			t.Errorf("expected 2 patterns after trim, got %d", len(patterns))
		}
	})
}

func TestLuhnValid(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"4242 4242 4242 4242", true},        // Stripe test card
		{"4242424242424242", true},           // no spaces
		{"4242-4242-4242-4242", true},        // hyphens
		{"1234567890123", false},             // 13 digits, fails Luhn
		{"123456789012", false},              // too short (12 digits)
		{"5555555555554444", true},           // Mastercard test card
		{"378282246310005", true},            // Amex test card (15 digits)
		{"1234567890123456789012345", false}, // too long
	}
	for _, tt := range tests {
		got := luhnValid(tt.input)
		if got != tt.want {
			t.Errorf("luhnValid(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
