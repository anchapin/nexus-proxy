package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/anchapin/nexus-proxy/internal/observability"
)

// HandlerPanicObserver is the hook handlers.Recover invokes when it
// catches a panic in the request hot path (issue #480). The path
// argument is the mux route template (not the raw URL) so label
// cardinality stays bounded. Implementations must be safe to call
// concurrently and must not block; the Recover middleware invokes the
// observer synchronously after logging, before writing the response.
// A nil observer is a no-op so the middleware works unchanged when no
// metrics collection is wired.
type HandlerPanicObserver func(path string)

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
//
// obs is an optional HandlerPanicObserver invoked after the slog.Error
// call (issue #480). Pass nil when no metrics collection is needed; the
// middleware is nil-safe.
func Recover(obs HandlerPanicObserver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var flusher http.Flusher
			if f, ok := w.(http.Flusher); ok {
				flusher = f
			}
			rw := &panicRecorder{ResponseWriter: w, flusher: flusher}
			defer func() {
				rv := recover()
				if rv == nil {
					return
				}
				reqID := requestID(r)
				route := routePattern(r)
				redacted, wasRedacted := redactPanicValue(rv)
				if wasRedacted {
					slog.Warn("panic contained secrets — redacted before logging",
						slog.String("component", "recovery"),
						slog.String("panic", redacted),
						slog.String("request_id", reqID),
						slog.String("method", r.Method),
						slog.String("path", r.URL.Path),
						slog.String("stack", string(debug.Stack())),
					)
				} else {
					slog.Error("panic recovered",
						slog.String("component", "recovery"),
						slog.String("panic", redacted),
						slog.String("request_id", reqID),
						slog.String("method", r.Method),
						slog.String("path", r.URL.Path),
						slog.String("stack", string(debug.Stack())),
					)
				}
				if obs != nil {
					obs(route)
				}
				if rw.headerWritten {
					// The response already started (e.g. a partial SSE
					// flush after WriteHeader(200)). We can no longer
					// change the status code, so emit a trailing SSE
					// error frame and terminate the stream so the client
					// gets a parseable ending instead of a TCP reset.
					// If the underlying writer does not implement
					// http.Flusher, fall back to the 500 envelope path
					// to avoid silently hanging the client (issue #686).
					if rw.flusher != nil {
						payload, _ := json.Marshal(map[string]string{
							"message": "internal server error",
							"type":    "internal_error",
						})
						if _, err := fmt.Fprintf(rw, "data: {\"error\":%s}\n\n", payload); err != nil {
							slog.Error("panic SSE error frame write failed",
								slog.String("component", "recovery"),
								slog.String("request_id", reqID),
								slog.String("err", err.Error()),
							)
							observability.IncrementPanicSSEWriteFailuresCounter()
						}
						if _, err := fmt.Fprint(rw, "data: [DONE]\n\n"); err != nil {
							slog.Error("panic SSE done sentinel write failed",
								slog.String("component", "recovery"),
								slog.String("request_id", reqID),
								slog.String("err", err.Error()),
							)
							observability.IncrementPanicSSEWriteFailuresCounter()
						}
						rw.Flush()
						return
					}
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

var (
	bearerTokenRe   = regexp.MustCompile(`(?i)(Bearer\s+)[a-zA-Z0-9\-_.~+/]+`)
	apiKeyRe        = regexp.MustCompile(`(?i)(api[_-]?key|apikey|api[_-]?secret|api[_-]?token|secret[_-]?key|auth[_-]?token|access[_-]?token)\s*[:=]\s*["']?[a-zA-Z0-9\-_.~+/]+["']?`)
	awsKeyRe        = regexp.MustCompile(`(?i)(aws[_-]?access[_-]?key[_-]?id|aws[_-]?secret[_-]?access[_-]?key)\s*[:=]\s*["']?[A-Z0-9]{20}["']?`)
	awsSecretRe     = regexp.MustCompile(`(?i)(aws[_-]?secret)\s*[:=]\s*["']?[a-zA-Z0-9/+=]{40}["']?`)
	passwordURLRe   = regexp.MustCompile(`(?i)://[^:]+:[^@]+@`)
	genericSecretRe = regexp.MustCompile(`(?i)\b(auth_token|token|password|passwd|pwd|credential|private[_-]?key)\s*[:=]\s*["']?[a-zA-Z0-9\-_.~+/ ]+(?:[ "'/]|$)`)
)

func redactPanicValue(rv any) (string, bool) {
	var raw string
	switch v := rv.(type) {
	case string:
		raw = v
	case error:
		raw = v.Error()
	default:
		raw = fmt.Sprintf("%v", v)
	}

	redacted := raw
	redacted = bearerTokenRe.ReplaceAllString(redacted, "${1}****")
	redacted = genericSecretRe.ReplaceAllStringFunc(redacted, func(s string) string {
		i := strings.Index(s, ":")
		if i < 0 {
			i = strings.Index(s, "=")
		}
		if i < 0 {
			return "****"
		}
		return s[:i+1] + "****"
	})
	redacted = apiKeyRe.ReplaceAllStringFunc(redacted, func(s string) string {
		i := strings.Index(s, ":")
		if i < 0 {
			i = strings.Index(s, "=")
		}
		if i < 0 {
			return "****"
		}
		return s[:i+1] + "****"
	})
	redacted = awsKeyRe.ReplaceAllStringFunc(redacted, func(s string) string {
		i := strings.Index(s, ":")
		if i < 0 {
			i = strings.Index(s, "=")
		}
		if i < 0 {
			return "****"
		}
		return s[:i+1] + "****"
	})
	redacted = awsSecretRe.ReplaceAllStringFunc(redacted, func(s string) string {
		i := strings.Index(s, ":")
		if i < 0 {
			i = strings.Index(s, "=")
		}
		if i < 0 {
			return "****"
		}
		return s[:i+1] + "****"
	})
	redacted = passwordURLRe.ReplaceAllString(redacted, "://****:****@")
	redacted = genericSecretRe.ReplaceAllStringFunc(redacted, func(s string) string {
		i := strings.Index(s, ":")
		if i < 0 {
			i = strings.Index(s, "=")
		}
		if i < 0 {
			return "****"
		}
		return s[:i+1] + "****"
	})

	return redacted, redacted != raw
}

// panicRecorder wraps the underlying http.ResponseWriter to track whether
// any response bytes or status code have been committed to the client.
// The recover middleware uses this to decide whether a panic can still be
// turned into a fresh 500 envelope (headers unwritten) or must be
// surfaced as a trailing SSE error frame (stream already in flight).
type panicRecorder struct {
	http.ResponseWriter
	headerWritten bool
	flusher       http.Flusher // nil if underlying writer does not implement Flusher
}

// WriteHeader marks the response as started and delegates to the inner
// writer. The flag is set unconditionally; WriteHeader is idempotent per
// the http.ResponseWriter contract.
func (p *panicRecorder) WriteHeader(code int) {
	if !p.headerWritten {
		p.headerWritten = true
	} else {
		slog.Warn("panicRecorder.WriteHeader: DOUBLE CALL DETECTED", slog.Int("code", code), slog.String("stack", string(debug.Stack())))
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

// Written returns true if headers (or body) have already been committed
// to the client. Used by streamCachedArbiterSynthesis to detect whether
// PanelStreaming has already committed SSE headers before attempting to set
// them again (issue #1416).
func (p *panicRecorder) Written() bool { return p.headerWritten }

// Flush delegates to the stored flusher. If the underlying writer does not
// implement http.Flusher, this is a no-op and the SSE panic path will use
// the 500 envelope instead (issue #686).
func (p *panicRecorder) Flush() {
	if p.flusher != nil {
		p.flusher.Flush()
	}
}

// routePattern returns the mux route template for r, falling back to the
// URL path when the request did not traverse a ServeMux (or the mux has
// not yet set the pattern). Using the template rather than the raw URL
// keeps the panic counter's label cardinality bounded (issue #480).
func routePattern(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return r.URL.Path
}
