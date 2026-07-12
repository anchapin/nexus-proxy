package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover returns HTTP middleware that catches panics arising anywhere in
// the downstream handler chain (issue #110). Without it a panic — a nil
// dereference, a malformed input that surprises a regex, a JSON parse
// after MaxBytesReader — kills the request goroutine; net/http's built-in
// connection-level recover closes the socket but emits no response, so
// clients see a TCP reset or an empty body. This middleware converts
// such panics into a structured slog.Error plus a 500 JSON envelope (or,
// when the response has already started streaming, a trailing SSE error
// frame + [DONE]) so the client gets a parseable ending instead.
//
// The middleware is intended to be the OUTERMOST wrapper on the mux so
// panics in every downstream middleware and handler are caught. It has
// zero overhead on the happy path: the deferred recover is cheap and the
// requestID lookup runs only when a panic actually fires.
func Recover() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := &panicRecorder{ResponseWriter: w}
			defer func() {
				rv := recover()
				if rv == nil {
					return
				}
				reqID := requestID(r)
				slog.Error("panic recovered",
					slog.String("component", "recovery"),
					slog.Any("panic", rv),
					slog.String("request_id", reqID),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())),
				)
				if rw.headerWritten {
					// The response already started (e.g. a partial SSE
					// flush after WriteHeader(200)). We can no longer
					// change the status code, so emit a trailing SSE
					// error frame and terminate the stream so the client
					// gets a parseable ending instead of a TCP reset.
					payload, _ := json.Marshal(map[string]string{
						"message": "internal server error",
						"type":    "internal_error",
					})
					_, _ = fmt.Fprintf(rw, "data: {\"error\":%s}\n\n", payload)
					_, _ = fmt.Fprint(rw, "data: [DONE]\n\n")
					if f, ok := any(rw).(http.Flusher); ok {
						f.Flush()
					}
					return
				}
				// Headers not yet written — return a clean 500 envelope
				// in the OpenAI-compatible error shape so existing
				// clients surface the message without changes.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]string{
						"message": "internal server error",
						"type":    "internal_error",
					},
				})
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

// panicRecorder wraps the underlying http.ResponseWriter to track whether
// any response bytes or status code have been committed to the client.
// The recover middleware uses this to decide whether a panic can still be
// turned into a fresh 500 envelope (headers unwritten) or must be
// surfaced as a trailing SSE error frame (stream already in flight).
type panicRecorder struct {
	http.ResponseWriter
	headerWritten bool
}

// WriteHeader marks the response as started and delegates to the inner
// writer. The flag is set unconditionally; WriteHeader is idempotent per
// the http.ResponseWriter contract.
func (p *panicRecorder) WriteHeader(code int) {
	if !p.headerWritten {
		p.headerWritten = true
	}
	p.ResponseWriter.WriteHeader(code)
}

// Write marks the response as started on the first byte and delegates to
// the inner writer. Per the http.ResponseWriter contract the first Write
// implicitly triggers WriteHeader(http.StatusOK), so this must mirror
// that side effect or the recover path would wrongly believe it can
// still write a 500.
func (p *panicRecorder) Write(b []byte) (int, error) {
	if !p.headerWritten {
		p.headerWritten = true
	}
	return p.ResponseWriter.Write(b)
}
