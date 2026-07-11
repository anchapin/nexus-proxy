package tracing

import (
	"net/http"
)

// RequestMiddleware wraps next so every request opens a root
// tracing span named "nexus.chat_completions" before any other
// middleware runs. The span is propagated to downstream handlers
// via the request's context (WithSpanContext) so per-phase
// middlewares — auth, rate-limit, security, body-size — can
// attach their child spans to the same trace tree.
//
// Inbound W3C `traceparent` headers are honoured so an upstream
// caller can extend its own trace tree across the proxy. Missing
// or malformed headers fall through to a fresh trace id.
//
// When Enabled() returns false the middleware is a pure
// pass-through — no allocation beyond a single boolean check, so
// the zero-overhead contract documented in StartSpan holds for
// the entire chain.
//
// The middleware MUST be installed before auth.Middleware /
// ratelimit.Middleware / SecurityHeaders so the per-phase spans
// parent under the root.
func RequestMiddleware(next http.Handler) http.Handler {
	if !Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var parent Context
		if tp := r.Header.Get("traceparent"); tp != "" {
			if traceID, spanID, ok := ParseTraceparent(tp); ok {
				parent = Context{TraceID: traceID, SpanID: spanID}
			}
		}
		rootCtx, rootSpan := StartSpan(parent, "nexus.chat_completions")
		defer rootSpan.End()

		ctx := WithSpanContext(r.Context(), rootCtx)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
