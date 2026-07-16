package tracing

import "context"

// Propagation helpers for the W3C Trace Context `traceparent`
// header. The header is the only propagation format this package
// understands; future support for `tracestate` (vendor-specific
// flags) lives here too.

// InjectTraceparent renders the W3C `traceparent` header value for
// ctx. Returns "" when ctx has no trace id, in which case the caller
// must not set the header (a malformed header would cause
// downstream services to ignore it AND confuse collectors that
// surface partial headers).
//
// The sampled flag is always set so downstream collectors record
// the propagated span. This matches the issue requirement that
// "outbound upstream calls carry traceparent for distributed
// correlation".
func InjectTraceparent(ctx Context) string {
	return FormatTraceparent(ctx.TraceID, ctx.SpanID)
}

// TraceparentFromContext extracts the active tracing Context from ctx
// (via SpanContextFromContext) and returns the W3C traceparent header
// value formatted via FormatTraceparent. Returns "" when ctx carries
// no trace id so callers can skip setting the header entirely.
//
// This is the outbound-context helper the upstream package uses to
// propagate trace context to Ollama and frontier endpoints (issue #299).
func TraceparentFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	tc, ok := SpanContextFromContext(ctx)
	if !ok {
		return ""
	}
	return InjectTraceparent(tc)
}
