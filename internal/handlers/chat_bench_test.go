package handlers

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/telemetry"
)

// silenceLogs swaps the default slog handler for one that discards all
// output so the benchmark stdout stays readable. The previous default
// is restored on Cleanup.
func silenceLogs(b *testing.B) {
	b.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	b.Cleanup(func() { slog.SetDefault(prev) })
}

// benchEmbedder is a zero-cost embedder used in benchmarks so the RAG
// path (if exercised) does not distort the handler measurement. It
// returns a fixed unit vector.
type benchEmbedder struct{}

func (benchEmbedder) Embed(_ context.Context, _ string) ([]float64, error) {
	return []float64{1.0}, nil
}

// jsonStringLit wraps a string in a JSON string literal via fmt %q.
func jsonStringLit(s string) string {
	return fmt.Sprintf("%q", s)
}

// benchDeps builds a Deps wired to mock httptest.Servers. The caller
// must close the returned servers via b.Cleanup.
func benchDeps(b *testing.B, localSrv, frontierSrv *httptest.Server) Deps {
	b.Helper()
	cfg := config.Config{
		Addr:           ":0",
		OllamaURL:      localSrv.URL,
		RouterModel:    "qwen3-coder:4b",
		LocalModel:     "qwen3-coder:8b",
		EmbeddingModel: "nomic-embed-text",
		FrontierURL:    frontierSrv.URL,
		FrontierModel:  "gpt-4o",
		FrontierKey:    "sk-bench",
		RAGThreshold:   0.55,
		TokenGuardrail: 6000,
		MetaPrompt:     " BOOST",
		TOONNotice:     "[PROXY SYSTEM NOTE]: TOON compression applied",
	}
	client := localSrv.Client()
	store := rag.NewStore(benchEmbedder{}, 0.55)
	return Deps{
		Config:          cfg,
		Client:          client,
		RAG:             store,
		SLM:             router.NewSLMClient(cfg.OllamaURL, cfg.RouterModel, 1, client),
		FormattingRegex: regexp.MustCompile(`(?i)\b(css|format|docstring|lint|typo|boilerplate|debug|fix bug|git commit|sql query|parse json|validate input|regex|api endpoint|test|optimize|readme)\b`),
		Recorder:        telemetry.Noop{},
	}
}

// mockFrontierHandler returns a minimal SSE stream the proxy's
// upstream.Stream will forward to the harness line-by-line.
func mockFrontierHandler(content string) http.HandlerFunc {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":" + jsonStringLit(content) + "}}]}\n\ndata: [DONE]\n\n"
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, sse)
	}
}

// mockLocalHandler returns a valid OpenAI-compatible JSON completion
// that the cascade validates and re-emits as a single SSE chunk.
func mockLocalHandler(content string) http.HandlerFunc {
	body := `{"model":"qwen3-coder:8b","choices":[{"index":0,"message":{"role":"assistant","content":` +
		jsonStringLit(content) + `},"finish_reason":"stop"}]}`
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

// BenchmarkChatRouteFrontierStream measures the full request
// lifecycle for a frontier streaming response: parse → middleware →
// guardrail routing → upstream.Stream → SSE relay. The 30 000-char
// prompt trips the token guardrail so the route decision is
// deterministic (no SLM round-trip).
func BenchmarkChatRouteFrontierStream(b *testing.B) {
	silenceLogs(b)
	frontierSrv := httptest.NewServer(mockFrontierHandler("Here is the frontier response."))
	b.Cleanup(frontierSrv.Close)
	localSrv := httptest.NewServer(mockLocalHandler("unused"))
	b.Cleanup(localSrv.Close)

	deps := benchDeps(b, localSrv, frontierSrv)
	handler := Chat(deps)
	largePrompt := strings.Repeat("a", 49000) // ≈ 6125 tokens > 6000 guardrail
	body := `{"messages":[{"role":"user","content":"` + largePrompt + `"}]}`

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rw := httptest.NewRecorder()
		handler.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			b.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
		}
	}
}

// BenchmarkChatRouteLocalCascade measures the full request lifecycle
// for a local cascade response: parse → middleware → DSL routing →
// cascade validation → writeSSEResponse. The "css" keyword triggers
// the DSL fast-pass to RouteLocal so the route decision is
// deterministic (no SLM round-trip).
func BenchmarkChatRouteLocalCascade(b *testing.B) {
	silenceLogs(b)
	localSrv := httptest.NewServer(mockLocalHandler("func fixCSS() { /* local answer */ }"))
	b.Cleanup(localSrv.Close)
	frontierSrv := httptest.NewServer(mockFrontierHandler("unused"))
	b.Cleanup(frontierSrv.Close)

	deps := benchDeps(b, localSrv, frontierSrv)
	handler := Chat(deps)
	body := `{"messages":[{"role":"user","content":"please fix the css formatting"}]}`

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rw := httptest.NewRecorder()
		handler.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			b.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
		}
	}
}

// BenchmarkChatRouteFrontierStreamParallel measures throughput under
// concurrent load using Go's parallel benchmark runner. GOMAXPROCS
// goroutines issue requests simultaneously; this reveals lock
// contention, allocation pressure, and connection-reuse
// characteristics the sequential benchmark cannot surface.
func BenchmarkChatRouteFrontierStreamParallel(b *testing.B) {
	silenceLogs(b)
	frontierSrv := httptest.NewServer(mockFrontierHandler("frontier parallel response."))
	b.Cleanup(frontierSrv.Close)
	localSrv := httptest.NewServer(mockLocalHandler("unused"))
	b.Cleanup(localSrv.Close)

	deps := benchDeps(b, localSrv, frontierSrv)
	handler := Chat(deps)
	largePrompt := strings.Repeat("a", 49000)
	body := `{"messages":[{"role":"user","content":"` + largePrompt + `"}]}`

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			rw := httptest.NewRecorder()
			handler.ServeHTTP(rw, req)
		}
	})
}

// BenchmarkChatRouteLocalCascadeParallel is the parallel sibling of
// BenchmarkChatRouteLocalCascade, exercising the cascade path under
// goroutine contention.
func BenchmarkChatRouteLocalCascadeParallel(b *testing.B) {
	silenceLogs(b)
	localSrv := httptest.NewServer(mockLocalHandler("local parallel answer."))
	b.Cleanup(localSrv.Close)
	frontierSrv := httptest.NewServer(mockFrontierHandler("unused"))
	b.Cleanup(frontierSrv.Close)

	deps := benchDeps(b, localSrv, frontierSrv)
	handler := Chat(deps)
	body := `{"messages":[{"role":"user","content":"please fix the css formatting"}]}`

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			rw := httptest.NewRecorder()
			handler.ServeHTTP(rw, req)
		}
	})
}

// BenchmarkChatEndToEndProxy measures the full HTTP stack including
// the Go net/http server layer: a real httptest.Server proxy receives
// requests from a real http.Client, which then dispatches to a mock
// upstream. This is the closest benchmark to real-world latency.
func BenchmarkChatEndToEndProxy(b *testing.B) {
	silenceLogs(b)
	frontierSrv := httptest.NewServer(mockFrontierHandler("frontier e2e response."))
	b.Cleanup(frontierSrv.Close)
	localSrv := httptest.NewServer(mockLocalHandler("unused"))
	b.Cleanup(localSrv.Close)

	deps := benchDeps(b, localSrv, frontierSrv)
	proxySrv := httptest.NewServer(Chat(deps))
	b.Cleanup(proxySrv.Close)

	client := proxySrv.Client()
	largePrompt := strings.Repeat("a", 49000)
	body := `{"messages":[{"role":"user","content":"` + largePrompt + `"}]}`

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, proxySrv.URL+"/v1/chat/completions",
			strings.NewReader(body))
		resp, err := client.Do(req)
		if err != nil {
			b.Fatalf("client.Do: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkChatEndToEndProxyConcurrent measures end-to-end throughput
// at concurrency levels 1, 4, and 8. Each sub-benchmark spawns that
// many worker goroutines that send requests through a real
// httptest.Server proxy. This surfaces p50/p99 characteristics and
// connection-pool behaviour under load.
func BenchmarkChatEndToEndProxyConcurrent(b *testing.B) {
	silenceLogs(b)
	frontierSrv := httptest.NewServer(mockFrontierHandler("frontier concurrent response."))
	b.Cleanup(frontierSrv.Close)
	localSrv := httptest.NewServer(mockLocalHandler("unused"))
	b.Cleanup(localSrv.Close)

	deps := benchDeps(b, localSrv, frontierSrv)
	proxySrv := httptest.NewServer(Chat(deps))
	b.Cleanup(proxySrv.Close)

	client := proxySrv.Client()
	largePrompt := strings.Repeat("a", 49000)
	body := `{"messages":[{"role":"user","content":"` + largePrompt + `"}]}`

	for _, c := range []int{1, 4, 8} {
		b.Run(fmt.Sprintf("workers%d", c), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			var wg sync.WaitGroup
			// Divide total iterations across workers.
			perWorker := b.N / c
			if perWorker < 1 {
				perWorker = 1
			}
			for w := 0; w < c; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for r := 0; r < perWorker; r++ {
						req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
							proxySrv.URL+"/v1/chat/completions",
							strings.NewReader(body))
						resp, err := client.Do(req)
						if err != nil {
							b.Errorf("client.Do: %v", err)
							return
						}
						_, _ = io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
					}
				}()
			}
			wg.Wait()
		})
	}
}
