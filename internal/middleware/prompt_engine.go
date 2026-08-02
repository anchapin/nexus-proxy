package middleware

import "strings"

// ApplyPromptEngineering injects enhancement onto the system prompt.
//
// If a system message already exists, the enhancement is appended to its
// content. Otherwise a new system message is prepended. Returns the
// (possibly modified) slice.
func ApplyPromptEngineering(messages []interface{}, enhancement string) []interface{} {
	if enhancement == "" {
		return messages
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "system" {
			content, _ := msg["content"].(string)
			if content != "" {
				msg["content"] = content + "\n" + enhancement
			} else {
				msg["content"] = enhancement
			}
			return messages
		}
	}
	newSys := map[string]interface{}{
		"role":    "system",
		"content": enhancement,
	}
	return append([]interface{}{newSys}, messages...)
}

// ExtractLatestUserPrompt returns the content of the most recent user-role
// message in msgs, or "" if none. Useful for feeding the DSL and SLM routers.
func ExtractLatestUserPrompt(msgs []interface{}) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		raw, ok := msgs[i].(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := raw["role"].(string); role == "user" {
			if content, ok := raw["content"].(string); ok {
				return content
			}
		}
	}
	return ""
}

// InjectRAG appends the retrieval context block onto the most recent user
// message's content. Returns the modified slice (in place when possible).
// If no user message is found the slice is returned unchanged.
//
// It is a backward-compatible wrapper around InjectRAGWithLimit with the
// size guard disabled (maxBytes == 0).
func InjectRAG(messages []interface{}, contextBlock string) []interface{} {
	msgs, _ := InjectRAGWithLimit(messages, contextBlock, 0)
	return msgs
}

// InjectRAGWithLimit appends the retrieval context block onto the most
// recent user message, but only when the resulting content would not exceed
// maxBytes (issue #594). A large few-shot example that would silently
// overflow the model's context window is skipped instead.
//
// When maxBytes <= 0 the guard is disabled (the pre-#594 behaviour). The
// returned bool reports whether injection occurred so callers can emit a
// warning and bump a skip counter when it did not.
func InjectRAGWithLimit(messages []interface{}, contextBlock string, maxBytes int) ([]interface{}, bool) {
	if contextBlock == "" {
		return messages, false
	}
	for i := len(messages) - 1; i >= 0; i-- {
		raw, ok := messages[i].(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := raw["role"].(string); role == "user" {
			content, _ := raw["content"].(string)
			if maxBytes > 0 && len(content)+len(contextBlock) > maxBytes {
				return messages, false
			}
			raw["content"] = content + contextBlock
			return messages, true
		}
	}
	return messages, false
}

// BuildConversationContext assembles a bounded summary of prior
// conversation turns for routing context (issue #1147). It collects the
// `turns` messages immediately preceding the most recent user message,
// prefixes each with its role ("user: ...", "assistant: ..."), and caps
// the total length at maxChars.
//
// The latest user message is intentionally excluded — it is already
// extracted into PlanRequest.Prompt by the handler. When turns <= 0 or
// maxChars <= 0 the result is "" and context injection is disabled
// (byte-for-byte identical to the pre-#1147 routing behaviour). turns is
// capped at 10 to bound the window even if the operator sets a large
// value.
func BuildConversationContext(messages []interface{}, turns, maxChars int) string {
	if turns <= 0 || maxChars <= 0 {
		return ""
	}
	if turns > 10 {
		turns = 10
	}
	// Locate the most recent user message — the context window is the
	// `turns` messages immediately before it. When there is no user
	// message, the window ends at the tail of the slice.
	endIdx := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if msg, ok := messages[i].(map[string]interface{}); ok {
			if role, _ := msg["role"].(string); role == "user" {
				endIdx = i
				break
			}
		}
	}
	startIdx := endIdx - turns
	if startIdx < 0 {
		startIdx = 0
	}

	var sb strings.Builder
	for _, raw := range messages[startIdx:endIdx] {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "" || role == "system" {
			// System messages are proxy-injected instructions, not
			// conversational turns — skip them so only the actual
			// user/assistant dialogue feeds the routing context.
			continue
		}
		content, _ := msg["content"].(string)
		line := role + ": " + content
		if sb.Len() > 0 {
			line = "\n" + line
		}
		if sb.Len()+len(line) > maxChars {
			remaining := maxChars - sb.Len()
			if remaining > 0 {
				sb.WriteString(line[:remaining])
			}
			break
		}
		sb.WriteString(line)
	}
	return sb.String()
}
