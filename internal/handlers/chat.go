// Package handlers contains the HTTP entry points for the proxy. The chat
// handler is the only public endpoint; it owns the request lifecycle:
// middleware chain -> routing decision -> upstream execution.
package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/middleware"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/upstream"
)

// LocalCompletion is the event surface the chat handler exposes for
// out-of-band observers (notably the async LLM-as-a-judge evaluator,
// see internal/judge). It is intentionally tiny: only the fields a
// judge needs to score a response. The handler never imports judge —
// the observer plugs in from cmd/nexus/main.go.
type LocalCompletion struct {
	RequestID   string
	Instruction string
	Output      string
	LocalModel  string
}

// JudgeObserver is the hook the chat handler invokes when a
// RouteLocal request completes successfully. Implementations must be
// safe to call concurrently; the handler invokes them on the same
// goroutine that serves the request so the observer should not block
// long. The handler also enforces a body-size cap before invoking
// the hook so a runaway model cannot blow the observer's memory.
type JudgeObserver interface {
	Submit(LocalCompletion)
}

// JudgeObserverFunc adapts a plain function to the JudgeObserver
// interface so wiring from main.go stays a one-liner.
type JudgeObserverFunc func(LocalCompletion)

// Submit implements JudgeObserver.
func (f JudgeObserverFunc) Submit(c LocalCompletion) { f(c) }

// Deps bundles the collaborators the chat handler needs. Wiring them
// explicitly makes the handler trivial to unit-test with stubs.
type Deps struct {
	Config          config.Config
	Client          upstream.Client // http.Client satisfies this interface
	RAG             *rag.Store
	SLM             *router.SLMClient
	FormattingRegex *regexp.Regexp

	// JudgeObserver is optional. When nil, the handler does not
	// buffer the streamed response for judge sampling — every
	// request takes the fast path with zero added overhead.
	//
	// The handler never imports the judge package; main.go wires
	// a closure that adapts LocalCompletion to the judge's
	// Sample/Enqueue entry points.
	JudgeObserver JudgeObserver

	// maxObservedBytes caps the body the observer sees. The full
	// response is still streamed to the client — only the buffered
	// copy used for sampling is bounded. Zero uses DefaultObservedCap.
	maxObservedBytes int
}

// DefaultObservedCap is the upper bound on the bytes the judge hook
// buffers per request. Typical LLM completions are well under this;
// the cap exists so a runaway local model cannot OOM the proxy.
const DefaultObservedCap = 256 * 1024 // 256 KiB

// Chat returns an http.Handler that runs the middleware chain, picks a
// route, and streams the chosen upstream's response back to the harness.
//
// Middleware order (do not reorder casually):
//  1. applyPromptEngineering    — inject role/CoT/constraints into system
//  2. applyRetrievalAugmentation — embed latest prompt, inject best match
//  3. optimizePromptContext     — TOON-compress JSON arrays in user msgs
//  4. evaluateDSL -> getSLMRoutingDecision
//  5. Stream (local/frontier) or Panel (fusion)
//
// After a successful RouteLocal stream the handler invokes
// JudgeObserver (when configured) with a LocalCompletion event. The
// observer may decide to enqueue the request for judge scoring; the
// handler does not wait on the observer.
func Chat(d Deps) http.Handler {
	if d.maxObservedBytes <= 0 {
		d.maxObservedBytes = DefaultObservedCap
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read request", http.StatusBadRequest)
			return
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
			return
		}
		rawMessages, ok := body["messages"].([]interface{})
		if !ok {
			http.Error(w, "Invalid or missing messages array", http.StatusBadRequest)
			return
		}

		messages := middleware.ApplyPromptEngineering(rawMessages, d.Config.MetaPrompt)
		latestPrompt := middleware.ExtractLatestUserPrompt(messages)
		if ex, score, err := d.RAG.Retrieve(r.Context(), latestPrompt); err == nil && ex != nil {
			messages = middleware.InjectRAG(messages, rag.FormatInjection(ex))
			log.Printf("[RAG HIT]: Injected %s (Score: %.2f)", ex.Filename, score)
		}
		if middleware.CompressJSONBlocks(messages) {
			messages = middleware.AppendSystemNote(messages, d.Config.TOONNotice)
			log.Println("[TOON COMPRESSOR]: Successfully compressed JSON data arrays.")
		}
		body["messages"] = messages
		latestPrompt = middleware.ExtractLatestUserPrompt(messages)

		var route router.Route
		if g, hit := router.Guardrail(latestPrompt, d.Config.TokenGuardrail); hit {
			log.Printf("[ROUTER]: DSL Match (Context too large: ~%d tokens) -> Force routing to FRONTIER", len(latestPrompt)/4)
			route = g
		} else if r2, hit := router.DSL(latestPrompt, d.FormattingRegex); hit {
			log.Printf("[ROUTER]: DSL Match -> %s", r2)
			route = r2
		} else {
			log.Println("[ROUTER]: DSL bypassed, asking SLM for analysis...")
			dec, err := d.SLM.Decide(r.Context(), latestPrompt)
			if err != nil {
				log.Printf("[SLM ERROR]: %v. Defaulting to Frontier.", err)
				dec = router.RouteFrontier
			}
			log.Printf("[ROUTER]: SLM Decision -> %s", dec)
			route = dec
		}

		switch route {
		case router.RouteFusion:
			log.Println("[FUSION]: Spinning up model panel...")
			err := upstream.Panel(
				w, d.Client,
				d.Config.OllamaURL, d.Config.LocalModel,
				d.Config.FrontierURL, d.Config.FrontierModel,
				d.Config.FrontierURL, d.Config.FrontierKey, d.Config.FrontierModel,
				body, latestPrompt, d.Config.FusionTimeout,
			)
			if err != nil {
				log.Printf("[FUSION ERROR]: %v", err)
				http.Error(w, "Upstream error", http.StatusBadGateway)
			}

		case router.RouteLocal:
			body["model"] = d.Config.LocalModel

			// Cascade (issue #14): try local Ollama first, fall back to
			// configured frontier endpoints (frontier, then z.ai) on
			// retryable failures. Cascade is rebuilt per request so
			// config changes take effect without restarting the
			// process.
			cas := upstream.BuildLocalCascade(upstream.CascadeConfig{
				LocalURL:      d.Config.OllamaURL,
				LocalModel:    d.Config.LocalModel,
				FrontierURL:   d.Config.FrontierURL,
				FrontierModel: d.Config.FrontierModel,
				FrontierKey:   d.Config.FrontierKey,
				ZAIURL:        d.Config.ZAIURL,
				ZAIModel:      d.Config.ZAIModel,
				ZAIKey:        d.Config.ZAIKey,
				Timeout:       d.Config.CascadeTimeout,
			})

			// Tee the response into a bounded buffer so the judge
			// observer (issue #15) can score it after the client
			// stream is fully flushed. The buffer cap matches the
			// dep's configured ceiling — overflow drops silently
			// (the observer sees a truncated body, which is better
			// than OOMing the proxy). The full body is still streamed
			// to the client via captureWriter.
			rw := http.ResponseWriter(w)
			var cap *captureWriter
			if d.JudgeObserver != nil {
				cap = newCaptureWriter(w, d.maxObservedBytes)
				rw = cap
			}
			res, err := cas.Run(rw, d.Client, body)
			logCascadeTelemetry(res, err)
			if err != nil {
				log.Printf("[UPSTREAM ERROR]: %v", err)
				http.Error(w, "Upstream error", http.StatusBadGateway)
				return
			}
			if d.JudgeObserver != nil && res.Succeeded {
				d.JudgeObserver.Submit(LocalCompletion{
					RequestID:   requestID(r),
					Instruction: latestPrompt,
					Output:      cap.Buffer(),
					LocalModel:  d.Config.LocalModel,
				})
			}

		default:
			if err := upstream.Stream(w, d.Client,
				d.Config.FrontierURL, d.Config.FrontierKey, body); err != nil {
				log.Printf("[UPSTREAM ERROR]: %v", err)
				http.Error(w, "Upstream error", http.StatusBadGateway)
			}
		}
	})
}

// logCascadeTelemetry emits the route_attempted / attempts line for the
// cascade. Once issue #16 wires a real metrics store this becomes the
// place to publish a structured event; for now it's plain log.Println.
func logCascadeTelemetry(res upstream.CascadeResult, err error) {
	log.Printf("[CASCADE TELEMETRY]: route_attempted=%s attempts=%d served_by=%s success=%v err=%v",
		res.RouteAttempted, res.Attempts, res.ServedBy, res.Succeeded, err)
}

// requestID extracts (or generates) a correlation id for the judge
// hook. The handler honours an inbound X-Request-Id so a caller can
// thread its own id through; otherwise we mint a short hex token.
func requestID(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" {
		return v
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail on linux; fall back to a
		// static marker so downstream logging still has *some*
		// correlation id.
		return "req-unknown"
	}
	return "req-" + hex.EncodeToString(b[:])
}

// captureWriter is an http.ResponseWriter that tees every Write into
// a bounded in-memory buffer while passing through to the underlying
// writer. It implements http.Flusher so upstream.Stream's flush
// behaviour is preserved. The buffer cap is a hard ceiling: writes
// past the cap are still forwarded to the client but the buffer
// silently stops growing.
type captureWriter struct {
	w        http.ResponseWriter
	cap      int
	buf      strings.Builder
	overflow bool
}

// newCaptureWriter wires a captureWriter around w with the given
// hard byte cap (use 0 to default).
func newCaptureWriter(w http.ResponseWriter, capBytes int) *captureWriter {
	if capBytes <= 0 {
		capBytes = DefaultObservedCap
	}
	return &captureWriter{w: w, cap: capBytes}
}

// Header implements http.ResponseWriter.
func (c *captureWriter) Header() http.Header { return c.w.Header() }

// Write implements http.ResponseWriter. The bytes are forwarded to
// the client unconditionally; they are also buffered up to the cap.
func (c *captureWriter) Write(b []byte) (int, error) {
	if !c.overflow && c.buf.Len()+len(b) <= c.cap {
		c.buf.Write(b)
	} else if !c.overflow {
		// Approaching the cap — stop buffering but keep passing
		// through. The observer will see a truncated body; that's
		// better than OOMing the proxy.
		c.overflow = true
	}
	return c.w.Write(b)
}

// WriteHeader implements http.ResponseWriter.
func (c *captureWriter) WriteHeader(s int) { c.w.WriteHeader(s) }

// Flush forwards to the underlying writer if it supports flushing.
// The chat handler relies on flushes for SSE; the judge capture must
// not regress that behaviour.
func (c *captureWriter) Flush() {
	if f, ok := c.w.(http.Flusher); ok {
		f.Flush()
	}
}

// Buffer returns the bytes captured so far. Safe to call after the
// upstream Stream has returned.
func (c *captureWriter) Buffer() string { return c.buf.String() }

// Compile-time assertion: captureWriter satisfies http.ResponseWriter
// and the optional Flusher interface upstream.Stream requires.
var (
	_ http.ResponseWriter = (*captureWriter)(nil)
	_ http.Flusher        = (*captureWriter)(nil)
)
