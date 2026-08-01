package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/ioutils"
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
				if _, _, err := extractAssistantMessage(body); err != nil {
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
				if err := writeSSEResponse(rw, "local", "qwen3-coder:8b", AssistantMessage{Content: content}); err != nil {
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

// BenchmarkFetchCascadeStepPooled measures the full cascade response-read +
// JSON-decode path with pooled response-body buffers (issue #1177). This is
// the hot path for every RouteLocal request: read body → validate → return
// message. The pool recycles the intermediate *bytes.Buffer across calls so
// allocs/op should be lower than the un-pooled baseline at 4/16 KiB.
func BenchmarkFetchCascadeStepPooled(b *testing.B) {
	cases := []struct {
		name string
		kb   int
	}{
		{"4KB", 4},
		{"16KB", 16},
	}
	for _, tc := range cases {
		body := genCompletionBody(tc.kb)
		// Each iteration hits a fresh httptest server cycle: the server
		// writes the completion body, the cascade reads it into a pooled
		// buffer, validates the JSON, and returns. The server is created
		// outside the b.N loop so only the fetch+validate path is measured.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}))
		b.Cleanup(srv.Close)

		cas := &Cascade{
			Steps:            []CascadeStep{{Name: "local", URL: srv.URL, Model: "test"}},
			MaxResponseBytes: 64 << 20,
		}
		// Warm up the pool.
		warm := ioutils.GetBuffer()
		ioutils.PutBuffer(warm)

		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				msg, _, err := cas.fetchCascadeStep(context.Background(), http.DefaultClient, cas.Steps[0], map[string]interface{}{})
				if err != nil {
					b.Fatal(err)
				}
				if msg.Content == "" {
					b.Fatal("expected non-empty content")
				}
			}
		})
	}
}
