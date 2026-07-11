package upstream

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// genCompletionBody builds an OpenAI-compatible chat completion JSON
// body with a content string of approximately contentKB kilobytes.
// This represents the cascade's primary workload: validating and
// re-emitting local model responses.
func genCompletionBody(contentKB int) []byte {
	content := strings.Repeat("This is a representative code completion response. ", contentKB*13)
	return []byte(fmt.Sprintf(
		`{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}]}`,
		jsonString(content),
	))
}

// jsonString wraps a string in a JSON string literal.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// BenchmarkExtractAssistantContent measures the JSON-decode +
// validation pass the cascade runs on every local response before
// streaming to the client. The 1KB case is typical for a short
// function; the 16KB case represents a multi-function edit.
func BenchmarkExtractAssistantContent(b *testing.B) {
	cases := []struct {
		name string
		kb   int
	}{
		{"1KB", 1},
		{"4KB", 4},
		{"16KB", 16},
	}
	for _, tc := range cases {
		body := genCompletionBody(tc.kb)
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := extractAssistantContent(body); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkWriteSSEResponse measures the JSON-marshal + SSE-emit path
// the cascade uses to deliver the validated content to the harness.
// Every RouteLocal request pays this cost exactly once.
func BenchmarkWriteSSEResponse(b *testing.B) {
	cases := []struct {
		name string
		kb   int
	}{
		{"1KB", 1},
		{"4KB", 4},
		{"16KB", 16},
	}
	for _, tc := range cases {
		content := strings.Repeat("This is a representative code completion response. ", tc.kb*13)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rw := httptest.NewRecorder()
				if err := writeSSEResponse(rw, "local", "qwen3-coder:8b", content); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkShouldRetry measures the retry-classification logic. This
// runs on every cascade step failure so it must stay O(1).
func BenchmarkShouldRetry(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ShouldRetry(503, nil)
	}
}
