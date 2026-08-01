package middleware

import (
	"testing"
	"unicode/utf8"
)

// FuzzSerializeToTOON feeds arbitrary byte slices into SerializeToTOON. The
// function must never panic and, when it succeeds, the output must be valid
// UTF-8 (it is injected into a system prompt consumed by an LLM).
func FuzzSerializeToTOON(f *testing.F) {
	// Seed corpus — valid, edge-case, and adversarial JSON arrays.
	f.Add([]byte(`[{"name":"Alice","age":30}]`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`[{}]`))
	f.Add([]byte(`[{"a":"1"},{"a":"2"},{"a":"3"}]`))
	// Values with commas (must be escaped to U+FF0C on round-trip).
	f.Add([]byte(`[{"desc":"hello, world"}]`))
	// Values with newlines (must be flattened to spaces).
	f.Add([]byte("[{\"code\":\"line1\nline2\"}]"))
	// Nested objects and arrays.
	f.Add([]byte(`[{"meta":{"k":"v"},"tags":[1,2]}]`))
	// Truncated / malformed JSON — should return an error, not panic.
	f.Add([]byte(`[{`))
	f.Add([]byte(`not json at all`))
	f.Add([]byte(`[{"k":"\u0000"}]`)) // null byte in string
	// Large nested structure.
	f.Add([]byte(`[{"deep":{"a":{"b":{"c":"end"}}}}]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		out, err := SerializeToTOON(data)
		if err != nil {
			// On error the output should be empty — nothing further to check.
			return
		}
		// On success the output must be valid UTF-8. The TOON text is
		// concatenated into a system prompt and an invalid sequence would
		// corrupt the downstream HTTP request.
		if !utf8.ValidString(out) {
			t.Fatalf("SerializeToTOON produced invalid UTF-8 for input %q", data)
		}
	})
}

// FuzzCompressJSONBlocks feeds arbitrary content through the full compression
// pipeline (fenced, nested, and unfenced passes). The pipeline must never
// panic regardless of the message content, and when compression occurs the
// resulting content must be valid UTF-8.
func FuzzCompressJSONBlocks(f *testing.F) {
	// Seed with content that exercises each compression path.
	f.Add([]byte("```json\n[{\"a\":1},{\"a\":2}]\n```"), true)
	f.Add([]byte(`[{"x":"foo"},{"x":"bar"}]`), true)
	f.Add([]byte(`{"files":[{"path":"a.go"},{"path":"b.go"}]}`), false)
	f.Add([]byte(""), true)
	f.Add([]byte(`[{`), true)
	f.Add([]byte("css\nformat\nlint"), false)
	// Adversarial: bracket-like sequences in prose.
	f.Add([]byte("see [1] and [2] for details"), true)

	f.Fuzz(func(t *testing.T, content []byte, unfenced bool) {
		msgs := []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": string(content),
			},
		}
		_ = CompressJSONBlocks(msgs, unfenced)

		// If compression occurred the content was replaced — verify it is
		// still valid UTF-8 (it must not corrupt the downstream request).
		if msg, ok := msgs[0].(map[string]interface{}); ok {
			if c, ok := msg["content"].(string); ok {
				if !utf8.ValidString(c) {
					t.Fatalf("CompressJSONBlocks produced invalid UTF-8 content")
				}
			}
		}
	})
}
