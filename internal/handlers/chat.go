// Package handlers contains the HTTP entry points for the proxy. The chat
// handler is the only public endpoint; it owns the request lifecycle:
// middleware chain -> routing decision -> upstream execution.
package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/middleware"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/telemetry"
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

// QualityEvent is emitted to the QualityObserver hook each time the
// handler detects a tool call in an upstream response that looks like
// a file edit (write_file, edit_file, apply_patch, ...). It mirrors
// the Event type in internal/quality — the handler does not import
// that package so the dependency direction stays one-way (cmd/nexus
// adapts between the two shapes).
type QualityEvent struct {
	RequestID string // correlates to the chat handler's request id
	Path      string // path of the edited file
	ToolName  string // write_file, edit_file, apply_patch, ...
}

// QualityObserver is the hook the chat handler invokes with detected
// edits. Like the JudgeObserver, it must be safe to call from many
// goroutines and must not block; the verifier does the heavy work
// asynchronously and only the Submit call happens on the request
// goroutine.
type QualityObserver interface {
	Submit(QualityEvent)
}

// QualityObserverFunc adapts a plain function to the QualityObserver
// interface so wiring from main.go stays a one-liner.
type QualityObserverFunc func(QualityEvent)

// Submit implements QualityObserver.
func (f QualityObserverFunc) Submit(e QualityEvent) { f(e) }

// MetricsEvent carries the per-request data needed by the savings
// dashboard (issue #4). Fields track the full Round-trip metrics:
// route/model/input-tokens are routing dimensions; TOON/RAG/cost are
// the savings dimensions the dashboard renders. The handler builds
// one of these after every proxied request (success, failure, or
// short-circuit) and dispatches it to the configured MetricsObserver.
//
// Mirrors internal/metrics.Request so a tiny adapter in main.go can
// forward directly without translation — kept here as a separate
// type so the handlers package stays free of the metrics import.
type MetricsEvent struct {
	Timestamp         time.Time
	RequestID         string
	Route             string
	Model             string
	InputTokens       int
	TOONSavingsTokens int
	RAGInjected       bool
	RAGFilename       string
	EstimatedCostUSD  float64

	OutputTokens   int
	TTFTMs         int64
	TotalLatencyMs int64
	TPS            float64
	Streaming      bool
	Error          string
}

// MetricsObserver is the hook the chat handler invokes once per
// proxied request with a MetricsEvent payload. Same invariants as
// JudgeObserver / QualityObserver:
//   - safe to call from many goroutines;
//   - must not block (the SQLite backend uses a buffered channel);
//   - may be nil (treated as "no observer"; hot path is unaffected).
type MetricsObserver interface {
	Submit(MetricsEvent)
}

// MetricsObserverFunc adapts a plain function to the MetricsObserver
// interface so wiring from main.go stays a one-liner.
type MetricsObserverFunc func(MetricsEvent)

// Submit implements MetricsObserver.
func (f MetricsObserverFunc) Submit(e MetricsEvent) { f(e) }

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

	// QualityObserver is optional. When non-nil, the handler scans
	// the captured upstream body for tool-call patterns that look
	// like file edits (write_file / edit_file / apply_patch / ...)
	// and dispatches one QualityEvent per detected file path.
	// The scan runs on the request goroutine AFTER the response
	// has been fully streamed; it adds negligible overhead and
	// never blocks the response path. The handler does not import
	// the quality package; main.go wires a closure that forwards
	// to the verifier's Submit.
	QualityObserver QualityObserver

	// MetricsObserver is optional. When non-nil, the handler
	// dispatches one MetricsEvent per proxied request after the
	// upstream response flushes, with the route, model, input
	// tokens, TOON savings, RAG injection, and rough frontier
	// cost. The observer is invoked synchronously from the request
	// goroutine but does not block the response path because the
	// SQLite-backed implementation (internal/metrics) enqueues
	// onto a buffered channel and writes asynchronously. The
	// handler does not import the metrics package; main.go wires
	// a closure that forwards to metricsStore.Submit.
	MetricsObserver MetricsObserver

	// Recorder receives one record per proxied request. Never nil at
	// runtime: Chat() installs telemetry.Noop{} when the caller
	// passes a zero value, so downstream code can invoke Record
	// unconditionally without a nil-check.
	Recorder telemetry.Recorder

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
//
// Likewise, when QualityObserver is configured, the handler scans
// the captured response body for tool-call envelopes that look like
// file edits (write_file / edit_file / apply_patch / ...) and
// forwards a QualityEvent per detected path. The verifier (cmd/nexus)
// runs `cargo check` / `npx tsc` asynchronously and dispatches the
// verdict back via its own observer hook.
//
// Every proxied request — success, failure, or short-circuit — emits
// exactly one telemetry.Record via Recorder so the issue #16 dashboard
// can answer "what fraction of traffic failed in the last hour?".
func Chat(d Deps) http.Handler {
	if d.maxObservedBytes <= 0 {
		d.maxObservedBytes = DefaultObservedCap
	}
	if d.Recorder == nil {
		d.Recorder = telemetry.Noop{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := requestID(r)
		started := time.Now()

		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		// Hard cap on request body (issue #11). Wrap with
		// http.MaxBytesReader BEFORE any allocation so an oversized
		// POST cannot exhaust proxy memory; MaxBytesReader caps
		// reads at the limit and surfaces *http.MaxBytesError on
		// overflow, which we translate to 413 below.
		maxBytes := d.Config.EffectiveMaxBodyBytes()
		r.Body = http.MaxBytesReader(w, r.Body, int64(maxBytes))

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeJSONError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
					"Request body exceeds NEXUS_MAX_BODY_BYTES (%d bytes)", maxBytes))
				log.Printf("[HARDENING]: rejected oversized request from %s (limit=%d)",
					r.RemoteAddr, maxBytes)
				return
			}
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
		var ragInjected bool
		var ragFilename string
		if ex, score, err := d.RAG.Retrieve(r.Context(), latestPrompt); err == nil && ex != nil {
			messages = middleware.InjectRAG(messages, rag.FormatInjection(ex))
			log.Printf("[RAG HIT]: Injected %s (Score: %.2f)", ex.Filename, score)
			ragInjected = true
			ragFilename = ex.Filename
			_ = score
		}
		// Snapshot the JSON size BEFORE TOON compression so the
		// metrics observer can attribute tokens saved by the
		// round-trip pass. Uses the cheap "4 chars per token"
		// heuristic the rest of the project uses for telemetry
		// (see internal/telemetry.EstimateTokens).
		preCompressionChars := totalMessageChars(messages)
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

		// Decide streaming up-front so the telemetry row reflects what the
		// client actually asked for. The current Stream implementation
		// streams either way; honoring the flag here keeps TTFT semantics
		// honest and prepares us for issue #10 (which will switch on this).
		streaming := true
		if s, ok := body["stream"].(bool); ok && !s {
			streaming = false
		}

		// Wrap the response writer so we can capture TTFT and byte counts
		// without affecting upstream.Stream's flusher contract.
		var firstWriteAt atomic.Int64 // unix nano; 0 means "no write yet"
		obs := telemetry.NewObservingWriter(w, func(t time.Time) {
			firstWriteAt.CompareAndSwap(0, t.UnixNano())
		})

		var model string
		var upErr error
		switch route {
		case router.RouteFusion:
			log.Println("[FUSION]: Spinning up model panel...")
			upErr = upstream.Panel(
				obs, d.Client,
				d.Config.OllamaURL, d.Config.LocalModel,
				d.Config.FrontierURL, d.Config.FrontierModel,
				d.Config.FrontierURL, d.Config.FrontierKey, d.Config.FrontierModel,
				body, latestPrompt, d.Config.FusionTimeout,
				d.Config.ArbiterTimeout,
			)
			if upErr != nil {
				log.Printf("[FUSION ERROR]: %v", upErr)
				http.Error(w, "Upstream error", http.StatusBadGateway)
			}
			model = d.Config.FrontierModel

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

			// Writer chain (outermost first):
			//   cascade.Run -> captureWriter (judge + quality tee) ->
			//   ObservingWriter (telemetry byte count + TTFT) ->
			//   underlying ResponseWriter.
			// captureWriter is only installed when at least one
			// observer is set; otherwise the cascade writes
			// directly through obs with zero overhead.
			rw := http.ResponseWriter(obs)
			var cap *captureWriter
			if d.JudgeObserver != nil || d.QualityObserver != nil {
				cap = newCaptureWriter(obs, d.maxObservedBytes)
				rw = cap
			}
			res, err := cas.Run(rw, d.Client, body)
			logCascadeTelemetry(res, err)
			if err != nil {
				log.Printf("[UPSTREAM ERROR]: %v", err)
				upErr = err
				http.Error(w, "Upstream error", http.StatusBadGateway)
				// fall through: telemetry Record still fires below so
				// the failed request shows up in the dashboard.
			} else {
				model = d.Config.LocalModel
				if res.Succeeded && cap != nil {
					if d.JudgeObserver != nil {
						d.JudgeObserver.Submit(LocalCompletion{
							RequestID:   reqID,
							Instruction: latestPrompt,
							Output:      cap.Buffer(),
							LocalModel:  d.Config.LocalModel,
						})
					}
					if d.QualityObserver != nil {
						emitDetectedEdits(cap.Buffer(), reqID, d.QualityObserver)
					}
				}
			}

		default:
			model = d.Config.FrontierModel
			if upErr = upstream.Stream(obs, d.Client,
				d.Config.FrontierURL, d.Config.FrontierKey, body); upErr != nil {
				log.Printf("[UPSTREAM ERROR]: %v", upErr)
				http.Error(w, "Upstream error", http.StatusBadGateway)
			}
		}

		// Per-request recording. The metrics observer (issue #4)
		// is preferred when configured: it carries the savings
		// dimensions (TOON delta, RAG injection, cost) that
		// telemetry.Record cannot express. The legacy Recorder
		// remains for callers that only want the TTFT / latency
		// fields, or when no metrics store is wired (operator
		// opted out by leaving NEXUS_METRICS_DB empty).
		totalMs := time.Since(started).Milliseconds()
		var ttftMs int64
		if streaming && firstWriteAt.Load() > 0 {
			ttftMs = time.Unix(0, firstWriteAt.Load()).Sub(started).Milliseconds()
			if ttftMs < 0 {
				ttftMs = 0
			}
		}
		outputTokens := int(obs.BytesOut() / 4)
		rec := buildRecord(reqID, started, firstWriteAt.Load(), obs.BytesOut(), streaming, route, model, latestPrompt, upErr)
		if d.MetricsObserver != nil {
			postCompressionChars := totalMessageChars(messages)
			savings := totalTokenSavings(preCompressionChars, postCompressionChars)
			cost := frontierCostEstimate(string(route), model, telemetry.EstimateTokens(latestPrompt), d.Config.JudgeCostPer1KUSD)
			tps := telemetry.ComputeTPS(outputTokens, ttftMs, totalMs)
			d.MetricsObserver.Submit(MetricsEvent{
				Timestamp:         rec.Timestamp,
				RequestID:         reqID,
				Route:             string(route),
				Model:             model,
				InputTokens:       rec.InputTokens,
				TOONSavingsTokens: savings,
				RAGInjected:       ragInjected,
				RAGFilename:       ragFilename,
				EstimatedCostUSD:  cost,
				OutputTokens:      outputTokens,
				TTFTMs:            ttftMs,
				TotalLatencyMs:    totalMs,
				TPS:               tps,
				Streaming:         streaming,
				Error:             rec.Error,
			})
		} else {
			d.Recorder.Record(rec)
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

// writeJSONError writes a structured JSON error response. Used for the
// 413 body-cap overflow (issue #11) so clients get a parseable error
// rather than a plain-text body. The shape matches the OpenAI error
// envelope (`{"error":{"message":...,"type":...}}`) so existing
// OpenAI-compatible clients surface the message without changes.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"message": message,
			"type":    http.StatusText(status),
		},
	})
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

// qualityEditNames is the set of tool names the handler treats as
// "this might have written a file". Mirrors internal/quality so the
// two stay in sync; the handler intentionally does not import the
// quality package (matching the AGENTS.md dependency rule for judge).
//
// Detection is liberal on purpose: the verifier's project-detection
// step filters false positives (e.g. a write_file on a path inside a
// non-project directory scores 0 but does not fail).
var qualityEditNames = []string{
	"write_file",
	"edit_file",
	"apply_patch",
	"create_file",
	"patch_file",
	"update_file",
	"write",
	"edit",
	"patch",
}

// qualityEditNameRe anchors on a known edit tool name inside a JSON
// payload. Operates on the post-stripJSONEscapes window, so the
// regex itself is plain — escape tolerance isn't necessary.
var qualityEditNameRe = regexp.MustCompile(
	`"name"\s*:\s*"(?:` + joinBars(qualityEditNames) + `)"`,
)

// qualityPathRe matches the first path-shaped field in a window.
// Captures the bare value with JSON-quote termination.
var qualityPathRe = regexp.MustCompile(
	`"(?:path|filePath|file_path|filepath)"\s*:\s*"([^"]+)"`,
)

// qualityPathWindowBytes bounds how far past a tool-name match we
// look for the path field. 4 KiB is comfortably larger than any real
// OpenCode tool call.
const qualityPathWindowBytes = 4 * 1024

// emitDetectedEdits scans body for tool-call envelopes whose name
// matches qualityEditNames and forwards one QualityEvent per unique
// path via obs. Body is the captured upstream response (already
// bounded by maxObservedBytes); obs must be safe to call from many
// goroutines — the verifier does its own queueing.
//
// The function is O(len(body)) and best-effort: malformed JSON,
// missing fields, or windows that don't contain a path field are
// silently skipped. Deduplication is per-call, by path.
func emitDetectedEdits(body, reqID string, obs QualityObserver) {
	if obs == nil || body == "" {
		return
	}
	cleaned := stripJSONEscapes(body)
	seen := make(map[string]bool, 4)
	for _, idxs := range qualityEditNameRe.FindAllStringIndex(cleaned, -1) {
		start := idxs[0]
		end := start + qualityPathWindowBytes
		if end > len(cleaned) {
			end = len(cleaned)
		}
		window := cleaned[start:end]
		path := firstPath(window)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		obs.Submit(QualityEvent{
			RequestID: reqID,
			Path:      path,
			ToolName:  "edit", // best-effort label; verifier doesn't depend on the exact name
		})
	}
}

// stripJSONEscapes reverses the small subset of escape sequences a
// tool-call envelope might carry after JSON encoding. We don't pull
// in encoding/json because that would force the handler to
// round-trip the whole response body to find a handful of patterns.
// File paths essentially never contain the other escape targets (\n,
// \t, etc.) — the few edge cases that slip through are caught at
// project-detection time and score 0.
func stripJSONEscapes(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '\\' && i+1 < len(s) {
			next := s[i+1]
			if next == '"' || next == '\\' || next == '/' {
				b.WriteByte(next)
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// firstPath pulls the first plausible path-looking field out of
// window. Exported as a small package-level helper so chat_test.go
// can call it without re-implementing the regex.
func firstPath(window string) string {
	if m := qualityPathRe.FindStringSubmatch(window); len(m) >= 2 && m[1] != "" {
		return m[1]
	}
	return ""
}

// joinBars concatenates names with "|" for use inside a regex
// alternation. stdlib does not export the equivalent helper from
// regexp so we keep the trivial inline version rather than pull in
// strings.Join (which would still need a trim on the trailing bar).
func joinBars(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "|"
		}
		out += p
	}
	return out
}

func buildRecord(
	requestID string,
	started time.Time,
	firstWriteNano int64,
	bytesOut uint64,
	streaming bool,
	route router.Route,
	model, latestPrompt string,
	upErr error,
) telemetry.Record {
	totalMs := time.Since(started).Milliseconds()
	var ttftMs int64
	if streaming && firstWriteNano > 0 {
		ttftMs = time.Unix(0, firstWriteNano).Sub(started).Milliseconds()
		if ttftMs < 0 {
			ttftMs = 0
		}
	}
	outputTokens := int(bytesOut / 4)
	rec := telemetry.Record{
		Timestamp:      time.Now().UTC(),
		RequestID:      requestID,
		Model:          model,
		Route:          string(route),
		InputTokens:    telemetry.EstimateTokens(latestPrompt),
		OutputTokens:   outputTokens,
		TTFTMs:         ttftMs,
		TotalLatencyMs: totalMs,
		Streaming:      streaming,
	}
	rec.TPS = telemetry.ComputeTPS(outputTokens, ttftMs, totalMs)
	if upErr != nil {
		rec.Error = upErr.Error()
	}
	return rec
}

// --- metrics (issue #4) helpers ----------------------------------------

// totalMessageChars marshals messages and returns the JSON byte
// length. Used to compute the TOON savings token estimate. Mirrors
// json.Marshal's own overflow behaviour (returns roughly the same
// bytes the proxy would emit across the wire).
func totalMessageChars(messages []interface{}) int {
	b, err := json.Marshal(messages)
	if err != nil {
		return 0
	}
	return len(b)
}

// totalTokenSavings returns the tokens saved by the TOON
// compression pass (pre - post character count, /4), clamped to
// zero in case the rewrite expanded the message (which can happen
// when the schema header outweighs the value rows for tiny inputs).
func totalTokenSavings(preChars, postChars int) int {
	if preChars <= postChars {
		return 0
	}
	return (preChars - postChars) / 4
}

// frontierCostEstimate multiplies input tokens by the configured
// cost-per-1k. Returns zero for non-frontier routes so local +
// fusion-trail rows count as zero cost in the dashboard. The
// JudgeCostPer1KUSD knob is reused here for lack of a separate
// frontier-rate env; the PRD says one is enough at this stage.
func frontierCostEstimate(route, model string, inputTokens int, costPer1KUSD float64) float64 {
	// model is reserved for future per-model pricing tables —
	// the bundle stays cheap by sharing JudgeCostPer1KUSD for now.
	_ = model
	if route != string(router.RouteFrontier) {
		return 0
	}
	if costPer1KUSD <= 0 || inputTokens <= 0 {
		return 0
	}
	return float64(inputTokens) * costPer1KUSD / 1000.0
}
