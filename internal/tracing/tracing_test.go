package tracing

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseTraceparentValid(t *testing.T) {
	// Real example taken from the W3C Trace Context spec.
	const want = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	traceID, spanID, ok := ParseTraceparent(want)
	if !ok {
		t.Fatalf("ParseTraceparent(%q) rejected valid header", want)
	}
	if traceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("traceID = %q, want 0af7651916cd43dd8448eb211c80319c", traceID)
	}
	if spanID != "b7ad6b7169203331" {
		t.Errorf("spanID = %q, want b7ad6b7169203331", spanID)
	}
}

func TestParseTraceparentNotSampledFlag(t *testing.T) {
	// 00 in the flags byte means "not sampled"; we still accept it
	// (the exporter always emits sampled spans).
	traceID, spanID, ok := ParseTraceparent(
		"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-00")
	if !ok {
		t.Fatalf("rejected unsampled flag")
	}
	if traceID == "" || spanID == "" {
		t.Errorf("ids empty: %q %q", traceID, spanID)
	}
}

func TestParseTraceparentRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"too short", "00-aa-bb-cc"},
		{"too long", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01-extra"},
		{"wrong version", "01-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
		{"uppercase hex", "00-0AF7651916CD43DD8448EB211C80319C-B7AD6B7169203331-01"},
		{"non-hex char", "00-0af7651916cd43dd8448eb211c80319z-b7ad6b7169203331-01"},
		{"all-zero trace", "00-00000000000000000000000000000000-b7ad6b7169203331-01"},
		{"all-zero span", "00-0af7651916cd43dd8448eb211c80319c-0000000000000000-01"},
		{"bad dashes", "000af7651916cd43dd8448eb211c80319cb7ad6b716920333101"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := ParseTraceparent(tc.in); ok {
				t.Errorf("ParseTraceparent(%q) accepted invalid input", tc.in)
			}
		})
	}
}

func TestFormatTraceparentRoundTrip(t *testing.T) {
	traceID := "0af7651916cd43dd8448eb211c80319c"
	spanID := "b7ad6b7169203331"
	h := FormatTraceparent(traceID, spanID)
	if h == "" {
		t.Fatal("FormatTraceparent returned empty")
	}
	gotTrace, gotSpan, ok := ParseTraceparent(h)
	if !ok {
		t.Fatalf("ParseTraceparent rejected FormatTraceparent output: %q", h)
	}
	if gotTrace != traceID {
		t.Errorf("traceID = %q, want %q", gotTrace, traceID)
	}
	if gotSpan != spanID {
		t.Errorf("spanID = %q, want %q", gotSpan, spanID)
	}
	// Flags byte must indicate "sampled" so downstream services
	// do not drop the propagated span.
	if !strings.HasSuffix(h, "-01") {
		t.Errorf("expected sampled flag (-01), got %q", h)
	}
}

func TestFormatTraceparentEmpty(t *testing.T) {
	if got := FormatTraceparent("", "abc"); got != "" {
		t.Errorf("expected empty for empty traceID, got %q", got)
	}
	if got := FormatTraceparent("abc", ""); got != "" {
		t.Errorf("expected empty for empty spanID, got %q", got)
	}
}

func TestInjectTraceparent(t *testing.T) {
	ctx := Context{TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "b7ad6b7169203331"}
	got := InjectTraceparent(ctx)
	want := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	if got != want {
		t.Errorf("InjectTraceparent = %q, want %q", got, want)
	}
	if got := InjectTraceparent(Context{}); got != "" {
		t.Errorf("empty context should produce empty header, got %q", got)
	}
}

func TestNewTraceIDAndSpanID(t *testing.T) {
	trace := NewTraceID()
	if len(trace) != 32 {
		t.Errorf("trace id length = %d, want 32", len(trace))
	}
	if !isLowerHex(trace) {
		t.Errorf("trace id %q not lowercase hex", trace)
	}
	span := NewSpanID()
	if len(span) != 16 {
		t.Errorf("span id length = %d, want 16", len(span))
	}
	if !isLowerHex(span) {
		t.Errorf("span id %q not lowercase hex", span)
	}
	// Two consecutive calls must return distinct ids (overwhelming
	// probability with crypto/rand).
	if a, b := NewTraceID(), NewTraceID(); a == b {
		t.Errorf("two NewTraceID calls returned equal values %q", a)
	}
	if a, b := NewSpanID(), NewSpanID(); a == b {
		t.Errorf("two NewSpanID calls returned equal values %q", a)
	}
}

func TestStartSpanAttachesToParent(t *testing.T) {
	parent := Context{TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "b7ad6b7169203331"}
	ctx, s := StartSpan(parent, "child")
	if ctx.TraceID != parent.TraceID {
		t.Errorf("child trace = %q, want parent %q", ctx.TraceID, parent.TraceID)
	}
	if s.TraceID != parent.TraceID {
		t.Errorf("span trace = %q, want parent %q", s.TraceID, parent.TraceID)
	}
	if s.ParentSpanID != parent.SpanID {
		t.Errorf("parent span id = %q, want %q", s.ParentSpanID, parent.SpanID)
	}
	if s.SpanID == parent.SpanID || s.SpanID == "" {
		t.Errorf("span id not fresh: %q", s.SpanID)
	}
	if s.Name != "child" {
		t.Errorf("name = %q, want child", s.Name)
	}
}

func TestStartSpanZeroParentBecomesRoot(t *testing.T) {
	ctx, s := StartSpan(Context{}, "root")
	if ctx.TraceID == "" {
		t.Error("root span should get a fresh trace id")
	}
	if s.ParentSpanID != "" {
		t.Errorf("root span parent id = %q, want empty", s.ParentSpanID)
	}
	if s.TraceID != ctx.TraceID {
		t.Errorf("trace mismatch: span %q vs ctx %q", s.TraceID, ctx.TraceID)
	}
}

func TestSpanSetAttrAndEnd(t *testing.T) {
	_, s := StartSpan(Context{}, "op")
	s.SetAttr("key", "value")
	s.SetAttr("count", 42)
	s.SetAttr("flag", true)
	s.End()
	if len(s.Attributes) != 3 {
		t.Errorf("attribute count = %d, want 3", len(s.Attributes))
	}
	if s.Status != StatusOK {
		t.Errorf("default end status = %v, want OK", s.Status)
	}
	if s.EndTime.IsZero() {
		t.Error("EndTime not set after End")
	}
}

func TestSpanRecordErrorSetsStatus(t *testing.T) {
	_, s := StartSpan(Context{}, "op")
	// RecordError before End:
	s.RecordError(errFake("boom"))
	s.End()
	if s.Status != StatusError {
		t.Errorf("status = %v, want Error", s.Status)
	}
	if s.StatusMessage != "boom" {
		t.Errorf("status message = %q, want boom", s.StatusMessage)
	}
}

func TestSpanEndTwiceIdempotent(t *testing.T) {
	_, s := StartSpan(Context{}, "op")
	first := time.Now()
	s.End()
	firstEnd := s.EndTime
	// Sleep a hair so the second End *would* change EndTime if it
	// wasn't ignored.
	time.Sleep(time.Millisecond)
	s.End()
	if !s.EndTime.Equal(firstEnd) {
		t.Errorf("second End changed EndTime: %v vs %v", s.EndTime, firstEnd)
	}
	if s.EndTime.Before(first) {
		t.Errorf("EndTime before End was called")
	}
}

func TestSpanNilSafe(t *testing.T) {
	// All Span methods must be nil-safe so the chat handler can
	// `defer span.End()` without a guard.
	var s *Span
	s.SetAttr("k", "v") // must not panic
	s.SetStatus(StatusOK, "")
	s.RecordError(errFake("x"))
	s.End()
}

// TestSpanConcurrentAttributes hammers SetAttr from many goroutines
// to verify the mutex. Uses distinct keys so each goroutine writes
// to a separate map slot (otherwise they would overwrite each
// other). Run with -race to catch regressions.
func TestSpanConcurrentAttributes(t *testing.T) {
	_, s := StartSpan(Context{}, "op")
	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			s.SetAttr("key-"+string(rune('A'+i%26))+string(rune('0'+i%10)), i)
		}()
	}
	wg.Wait()
	if len(s.Attributes) != n {
		t.Errorf("attribute count = %d, want %d", len(s.Attributes), n)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
