package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// newJSONHandler builds a JSON slog.Handler writing to buf for test
// inspection.
func newJSONHandler(buf *bytes.Buffer) slog.Handler {
	return slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
}

// parseJSON reads the first JSON line from buf into a map.
func parseJSON(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected at least one log line, got empty buffer")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("failed to parse log line as JSON: %v\nline: %s", err, line)
	}
	return m
}

func TestLogHandler_InjectsTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	h := NewLogHandler(newJSONHandler(&buf))
	logger := slog.New(h)

	traceID := "0af7651916cd43dd8448eb211c80319c"
	spanID := "b7ad6b7169203331"
	ctx := WithSpanContext(context.Background(), Context{TraceID: traceID, SpanID: spanID})

	logger.InfoContext(ctx, "traced request", slog.String("component", "chat"))

	m := parseJSON(t, &buf)
	if got := m["trace_id"]; got != traceID {
		t.Errorf("trace_id = %v, want %s", got, traceID)
	}
	if got := m["span_id"]; got != spanID {
		t.Errorf("span_id = %v, want %s", got, spanID)
	}
	if got := m["msg"]; got != "traced request" {
		t.Errorf("msg = %v, want %q", got, "traced request")
	}
}

func TestLogHandler_NilContextSafe(t *testing.T) {
	var buf bytes.Buffer
	h := NewLogHandler(newJSONHandler(&buf))
	logger := slog.New(h)

	// context.Background() carries no trace context — must not panic
	// and must not inject trace_id / span_id.
	logger.InfoContext(context.Background(), "background log")

	m := parseJSON(t, &buf)
	if _, ok := m["trace_id"]; ok {
		t.Error("trace_id should not be present when context has no trace")
	}
	if _, ok := m["span_id"]; ok {
		t.Error("span_id should not be present when context has no trace")
	}
}

func TestLogHandler_NoTraceContextNoExtraKeys(t *testing.T) {
	// A record without trace context must not introduce any extra
	// keys — the key set from the wrapped handler must match the
	// base handler exactly. (Exact byte comparison is impossible
	// because the timestamp differs per call.)
	msg := "hello"

	assertSameKeys := func(t *testing.T, baseOut, wrappedOut string) {
		t.Helper()
		baseKeys := jsonKeys(t, baseOut)
		wrappedKeys := jsonKeys(t, wrappedOut)
		if len(baseKeys) != len(wrappedKeys) {
			t.Errorf("key count differs: base=%v wrapped=%v", baseKeys, wrappedKeys)
			return
		}
		for k := range baseKeys {
			if !wrappedKeys[k] {
				t.Errorf("key %q present in base but the wrapped handler output differs:\nbase:   %s\nwrapped: %s",
					k, baseOut, wrappedOut)
			}
		}
		// Specifically assert trace_id and span_id are NOT present.
		if _, ok := wrappedKeys["trace_id"]; ok {
			t.Error("trace_id should not be present when context has no trace")
		}
		if _, ok := wrappedKeys["span_id"]; ok {
			t.Error("span_id should not be present when context has no trace")
		}
	}

	t.Run("background context", func(t *testing.T) {
		var baseBuf, wrappedBuf bytes.Buffer
		baseLogger := slog.New(newJSONHandler(&baseBuf))
		wrappedLogger := slog.New(NewLogHandler(newJSONHandler(&wrappedBuf)))

		ctx := context.Background()
		baseLogger.InfoContext(ctx, msg, slog.String("key", "val"))
		wrappedLogger.InfoContext(ctx, msg, slog.String("key", "val"))

		assertSameKeys(t, baseBuf.String(), wrappedBuf.String())
	})

	t.Run("non-Context slog call (context.Background internally)", func(t *testing.T) {
		var baseBuf, wrappedBuf bytes.Buffer
		baseLogger := slog.New(newJSONHandler(&baseBuf))
		wrappedLogger := slog.New(NewLogHandler(newJSONHandler(&wrappedBuf)))

		baseLogger.Info(msg)
		wrappedLogger.Info(msg)

		assertSameKeys(t, baseBuf.String(), wrappedBuf.String())
	})
}

// jsonKeys extracts the top-level JSON keys from a single JSON object
// line.
func jsonKeys(t *testing.T, s string) map[string]bool {
	t.Helper()
	line := strings.TrimSpace(s)
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("failed to parse JSON: %v\nline: %s", err, line)
	}
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}

func TestLogHandler_WithAttrsPreservesInjection(t *testing.T) {
	var buf bytes.Buffer
	base := newJSONHandler(&buf)
	wrapped := NewLogHandler(base)

	// Derived logger via WithAttrs must still inject trace IDs.
	derived := wrapped.WithAttrs([]slog.Attr{slog.String("preset", "abc")})
	logger := slog.New(derived)

	traceID := "abcdef0123456789abcdef0123456789"
	ctx := WithSpanContext(context.Background(), Context{TraceID: traceID, SpanID: "1111111111111111"})
	logger.WarnContext(ctx, "derived warn")

	m := parseJSON(t, &buf)
	if got := m["trace_id"]; got != traceID {
		t.Errorf("trace_id = %v, want %s (WithAttrs broke injection)", got, traceID)
	}
	if got := m["preset"]; got != "abc" {
		t.Errorf("preset = %v, want abc", got)
	}
}

func TestLogHandler_WithGroupPreservesInjection(t *testing.T) {
	var buf bytes.Buffer
	base := newJSONHandler(&buf)
	wrapped := NewLogHandler(base)

	// Derived logger via WithGroup must still inject trace IDs.
	derived := wrapped.WithGroup("req")
	logger := slog.New(derived)

	traceID := "fedcba9876543210fedcba9876543210"
	ctx := WithSpanContext(context.Background(), Context{TraceID: traceID, SpanID: "2222222222222222"})
	logger.ErrorContext(ctx, "grouped error", slog.String("detail", "boom"))

	// WithGroup wraps subsequent attrs under "req", but trace_id /
	// span_id are added before the inner handler so they appear in
	// the JSON output regardless of the group nesting.
	raw := buf.String()
	if !strings.Contains(raw, traceID) {
		t.Errorf("trace_id %s not found in output:\n%s", traceID, raw)
	}
}

func TestLogHandler_NewLogHandlerNilInner(t *testing.T) {
	if h := NewLogHandler(nil); h != nil {
		t.Errorf("NewLogHandler(nil) = %v, want nil", h)
	}
}

func TestLogHandler_TraceIDOnlyNoSpanID(t *testing.T) {
	// When the context has a TraceID but no SpanID, only trace_id
	// should be injected (span_id omitted).
	var buf bytes.Buffer
	h := NewLogHandler(newJSONHandler(&buf))
	logger := slog.New(h)

	traceID := "1234567890abcdef1234567890abcdef"
	ctx := WithSpanContext(context.Background(), Context{TraceID: traceID})
	logger.InfoContext(ctx, "trace only")

	m := parseJSON(t, &buf)
	if got := m["trace_id"]; got != traceID {
		t.Errorf("trace_id = %v, want %s", got, traceID)
	}
	if _, ok := m["span_id"]; ok {
		t.Error("span_id should be omitted when SpanID is empty")
	}
}
