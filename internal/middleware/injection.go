// Package middleware — prompt-injection hardening (issue #76).
//
// This file adds a configurable mode that isolates proxy-controlled
// prompt text from user-supplied system content and optionally
// detects/rejects suspicious prompt-injection patterns.
//
// Modes:
//   - off (default): legacy append behaviour, byte-for-byte unchanged.
//   - warn: proxy text is wrapped in delimiters and placed in a
//     dedicated leading system message; suspicious user system
//     messages are logged but not rejected.
//   - strict: same as warn, plus suspicious patterns are rejected
//     by the chat handler with a 400 OpenAI-style error.
//
// The detection patterns are intentionally conservative to avoid
// false positives on legitimate system prompts.

package middleware

import (
	"log/slog"
	"regexp"
	"strings"
)

// InjectionMode controls how the proxy isolates its prompt text and
// guards against prompt-injection attempts in user-supplied system
// messages (issue #76).
type InjectionMode int

const (
	// InjectionModeOff is the default: legacy append behaviour with
	// no delimiters and no detection. Byte-for-byte identical to
	// the pre-issue-76 path.
	InjectionModeOff InjectionMode = iota
	// InjectionModeWarn wraps proxy-controlled text in delimiters
	// (dedicated leading system message) and logs suspicious
	// patterns found in user system messages without rejecting.
	InjectionModeWarn
	// InjectionModeStrict behaves like warn but additionally causes
	// the chat handler to reject requests whose user system messages
	// match suspicious injection patterns (400 OpenAI-style error).
	InjectionModeStrict
)

// ProxyPolicyBegin and ProxyPolicyEnd delimit the proxy-controlled
// policy block within a system message. They make it easy for humans
// (and for the detection logic) to distinguish trusted proxy text
// from user-supplied system content.
const (
	ProxyPolicyBegin = "[NEXUS PROXY POLICY BEGIN]"
	ProxyPolicyEnd   = "[NEXUS PROXY POLICY END]"
)

// ParseInjectionMode maps the NEXUS_PROMPT_INJECTION_MODE env value
// to an InjectionMode. Unknown / empty values fall back to
// InjectionModeOff so a stock deployment boots with the legacy
// behaviour (backward compatible).
func ParseInjectionMode(raw string) InjectionMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "warn":
		return InjectionModeWarn
	case "strict":
		return InjectionModeStrict
	default:
		return InjectionModeOff
	}
}

// suspiciousInjectionPatterns are conservative regexes that match
// common prompt-injection override attempts in user-supplied system
// messages. They are intentionally narrow to avoid false positives on
// legitimate system prompts such as "You are a helpful assistant."
//
// Each pattern targets explicit override language ("ignore previous
// instructions", "disregard the above", etc.) rather than generic
// instructional text.
var suspiciousInjectionPatterns = []*regexp.Regexp{
	// "ignore previous/prior/above instructions"
	regexp.MustCompile(`(?i)ignore\s+(?:all\s+)?(?:previous|prior|above|earlier|earlier\s+defined)\s+(?:instructions?|prompts?|rules?|directives?|system\s+messages?)`),
	// "disregard the above/previous..."
	regexp.MustCompile(`(?i)disregard\s+(?:all\s+)?(?:the\s+)?(?:above|previous|prior|foregoing|earlier)`),
	// "forget everything/all previous instructions"
	regexp.MustCompile(`(?i)forget\s+(?:everything|all\s+(?:previous|prior)\s+(?:instructions?|rules?|prompts?))`),
	// "new instructions:" / "updated rules:" override framing
	regexp.MustCompile(`(?i)\b(?:new|updated?|revised?)\s+(?:instructions?|rules?|directives?|system\s+prompt)\s*:`),
	// "you are now free/unrestricted/DAN/jailbroken"
	regexp.MustCompile(`(?i)\b(?:you\s+are\s+now|from\s+now\s+on|act\s+as\s+if)\b.{0,80}\b(?:free|unrestricted|jailbroken|dan|developer\s+mode|admin)\b`),
	// "do not follow your previous/system instructions"
	regexp.MustCompile(`(?i)do\s+not\s+follow\s+(?:your\s+)?(?:previous|prior|original)\s+(?:system\s+)?(?:instructions?|rules?|directives?|guidelines?)`),
}

// DetectSuspiciousSystem scans user-supplied system messages for
// patterns that look like prompt-injection override attempts. Returns
// a slice of human-readable pattern descriptions (empty if clean).
//
// Proxy-controlled policy blocks (delimited by ProxyPolicyBegin /
// ProxyPolicyEnd) are skipped so the proxy's own injected text is
// never flagged — only the user's original system content is scanned.
func DetectSuspiciousSystem(messages []interface{}) []string {
	var hits []string
	for _, raw := range messages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "system" {
			continue
		}
		content, _ := msg["content"].(string)
		if content == "" {
			continue
		}
		// Skip proxy-controlled policy blocks — those are trusted.
		if strings.Contains(content, ProxyPolicyBegin) {
			continue
		}
		for _, re := range suspiciousInjectionPatterns {
			if re.MatchString(content) {
				hits = append(hits, re.String())
				break // one hit per message is sufficient
			}
		}
	}
	return hits
}

// ApplyPromptEngineeringIsolated inserts a new leading system message
// containing the enhancement wrapped in proxy-policy delimiters (issue
// #76). User-supplied system messages are preserved unchanged AFTER
// the proxy block, so proxy-controlled policy always precedes user
// content in the instruction hierarchy.
//
// This is the warn/strict-mode counterpart to ApplyPromptEngineering
// (which is the off-mode legacy append path).
func ApplyPromptEngineeringIsolated(messages []interface{}, enhancement string) []interface{} {
	if enhancement == "" {
		return messages
	}
	policy := ProxyPolicyBegin + "\n" + strings.TrimSpace(enhancement) + "\n" + ProxyPolicyEnd
	newSys := map[string]interface{}{
		"role":    "system",
		"content": policy,
	}
	return append([]interface{}{newSys}, messages...)
}

// AppendSystemNoteIsolated appends the notice to the existing
// proxy-policy block (the system message containing ProxyPolicyBegin)
// by inserting the text just before the END marker. If no proxy block
// exists it creates a new delimited one. This ensures the TOON notice
// stays inside the trusted proxy boundary in warn/strict modes.
func AppendSystemNoteIsolated(messages []interface{}, notice string) []interface{} {
	if notice == "" {
		return messages
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "system" {
			continue
		}
		content, _ := msg["content"].(string)
		if strings.Contains(content, ProxyPolicyBegin) {
			msg["content"] = strings.Replace(
				content, ProxyPolicyEnd,
				notice+"\n"+ProxyPolicyEnd, 1,
			)
			return messages
		}
	}
	// No proxy block exists yet — create one with the notice.
	policy := ProxyPolicyBegin + "\n" + strings.TrimSpace(notice) + "\n" + ProxyPolicyEnd
	newSys := map[string]interface{}{
		"role":    "system",
		"content": policy,
	}
	return append([]interface{}{newSys}, messages...)
}

// LogSuspicious emits a structured slog warning listing the detected
// patterns. Called by the chat handler in warn mode. Keeping the log
// formatting here ensures consistency between the middleware-level
// tests and the handler.
func LogSuspicious(hits []string, requestID string) {
	slog.Warn("suspicious prompt-injection patterns in system message",
		slog.Int("patterns", len(hits)),
		slog.String("request_id", requestID),
	)
}
