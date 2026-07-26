package middleware

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
