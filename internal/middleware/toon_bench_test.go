package middleware

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// genJSONArray builds a JSON array of n objects, each with keys
// "id","name","language","category","value". Values are short strings
// so the payload resembles a real code-metadata table the proxy
// would compress on the hot path.
func genJSONArray(n int) []byte {
	items := make([]map[string]interface{}, n)
	for i := 0; i < n; i++ {
		items[i] = map[string]interface{}{
			"id":       i,
			"name":     fmt.Sprintf("func_%d", i),
			"language": "go",
			"category": "handler",
			"value":    fmt.Sprintf("result-%d", i),
		}
	}
	b, _ := json.Marshal(items)
	return b
}

// genMessages wraps a JSON-array code block inside a user message,
// matching the shape the chat handler passes to CompressJSONBlocks.
func genMessages(n int) []interface{} {
	arr := genJSONArray(n)
	content := "Here is the data:\n```json\n" + string(arr) + "\n```\nPlease analyze."
	return []interface{}{
		map[string]interface{}{
			"role":    "system",
			"content": "You are a helpful coding assistant.",
		},
		map[string]interface{}{
			"role":    "user",
			"content": content,
		},
	}
}

// genNoJSONMessages returns a user message with no JSON blocks so
// CompressJSONBlocks exercises its fast no-op path.
func genNoJSONMessages() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"role":    "system",
			"content": "You are a helpful coding assistant.",
		},
		map[string]interface{}{
			"role":    "user",
			"content": strings.Repeat("This is a plain text prompt with no JSON blocks. ", 20),
		},
	}
}

// BenchmarkSerializeToTOON measures the core JSON-array → TOON
// conversion at three payload sizes. The 50-row case is the upper
// bound of what a real agent context window typically carries; the
// 3-row case is the common minimum.
func BenchmarkSerializeToTOON(b *testing.B) {
	cases := []struct {
		name string
		rows int
	}{
		{"3rows", 3},
		{"10rows", 10},
		{"50rows", 50},
	}
	for _, tc := range cases {
		jsonBytes := genJSONArray(tc.rows)
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(jsonBytes)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := SerializeToTOON(jsonBytes); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCompressJSONBlocks measures the full regex-scan + TOON
// rewrite pass the chat handler runs on every request. The "noJSON"
// sub-benchmark establishes the baseline cost when no blocks match
// (the common case — most prompts don't carry fenced JSON arrays).
func BenchmarkCompressJSONBlocks(b *testing.B) {
	cases := []struct {
		name string
		msgs []interface{}
	}{
		{"noJSON", genNoJSONMessages()},
		{"3rows", genMessages(3)},
		{"50rows", genMessages(50)},
	}
	for _, tc := range cases {
		// Snapshot so each iteration starts from a fresh copy.
		orig := tc.msgs
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				CompressJSONBlocks(orig)
			}
		})
	}
}

// BenchmarkAppendSystemNote measures the system-prompt append path.
// This runs on every request where TOON compression fires, so it
// must stay cheap.
func BenchmarkAppendSystemNote(b *testing.B) {
	msgs := []interface{}{
		map[string]interface{}{"role": "system", "content": "base"},
		map[string]interface{}{"role": "user", "content": "hi"},
	}
	notice := "[PROXY SYSTEM NOTE]: TOON compression applied to JSON arrays."
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		AppendSystemNote(msgs, notice)
	}
}
