package tracing

import (
	"context"
	"log/slog"
)

// LogHandler is a slog.Handler wrapper that injects trace_id and
// span_id attributes (OTel semantic-convention key names) from the
// active trace context carried in the context.Context passed to each
// Handle call.
//
// When the context has no trace context (background goroutines,
// non-traced paths) the record is delegated unchanged — byte-identical
// to the inner handler's output. This provides log-to-trace
// correlation without modifying any call site: the value flows
// through the existing context.Context threaded through the request
// lifecycle (issue #1169).
type LogHandler struct {
	Inner slog.Handler
}

// NewLogHandler wraps inner with a handler that injects trace_id /
// span_id. Returns inner unchanged when it is nil so callers can
// wrap unconditionally.
func NewLogHandler(inner slog.Handler) slog.Handler {
	if inner == nil {
		return nil
	}
	return LogHandler{Inner: inner}
}

// Enabled delegates to the inner handler.
func (h LogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Inner.Enabled(ctx, level)
}

// Handle reads the active trace context from ctx and, when present,
// adds trace_id and span_id attributes to the record before
// delegating to the inner handler. When ctx is nil or carries no
// trace context the record passes through unchanged.
func (h LogHandler) Handle(ctx context.Context, r slog.Record) error {
	if tc, ok := SpanContextFromContext(ctx); ok && tc.TraceID != "" {
		r.AddAttrs(slog.String("trace_id", tc.TraceID))
		if tc.SpanID != "" {
			r.AddAttrs(slog.String("span_id", tc.SpanID))
		}
	}
	return h.Inner.Handle(ctx, r)
}

// WithAttrs returns a new LogHandler wrapping the inner handler's
// WithAttrs result so trace injection is preserved through derived
// loggers.
func (h LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return LogHandler{Inner: h.Inner.WithAttrs(attrs)}
}

// WithGroup returns a new LogHandler wrapping the inner handler's
// WithGroup result so trace injection is preserved through derived
// loggers.
func (h LogHandler) WithGroup(name string) slog.Handler {
	return LogHandler{Inner: h.Inner.WithGroup(name)}
}
