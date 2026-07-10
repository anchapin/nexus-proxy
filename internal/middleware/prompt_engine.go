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
func InjectRAG(messages []interface{}, contextBlock string) []interface{} {
	if contextBlock == "" {
		return messages
	}
	for i := len(messages) - 1; i >= 0; i-- {
		raw, ok := messages[i].(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := raw["role"].(string); role == "user" {
			content, _ := raw["content"].(string)
			raw["content"] = content + contextBlock
			return messages
		}
	}
	return messages
}