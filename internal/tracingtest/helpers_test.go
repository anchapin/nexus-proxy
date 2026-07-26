package tracingtest

import (
	"testing"

	"github.com/anchapin/nexus-proxy/internal/tracing"
)

func TestNewCapturedSpans(t *testing.T) {
	t.Parallel()
	coll := NewCapturedSpans(t)
	if coll == nil {
		t.Fatal("NewCapturedSpans returned nil")
	}
	if coll.ServerURL() == "" {
		t.Error("ServerURL returned empty string")
	}
}

func TestCapturedSpansSpansEmpty(t *testing.T) {
	t.Parallel()
	coll := NewCapturedSpans(t)
	spans := coll.Spans(t)
	if len(spans) != 0 {
		t.Errorf("Spans on empty collector = %d, want 0", len(spans))
	}
}

func TestCapturedSpansFindSpanNone(t *testing.T) {
	t.Parallel()
	coll := NewCapturedSpans(t)
	found := coll.FindSpan(t, "does-not-exist")
	if found != nil {
		t.Error("FindSpan returned non-nil for missing span")
	}
}

func TestStartTestExporter(t *testing.T) {
	t.Parallel()
	coll := NewCapturedSpans(t)
	exp := StartTestExporter(t, coll)
	if exp == nil {
		t.Fatal("StartTestExporter returned nil")
	}
	if coll.ServerURL() == "" {
		t.Error("ServerURL is empty after StartTestExporter")
	}
}

func TestSpanRoundTrip(t *testing.T) {
	t.Parallel()
	coll := NewCapturedSpans(t)
	exp := StartTestExporter(t, coll)

	ctx, span := exp.StartSpan(tracing.Context{}, "test-span-round-trip")
	span.SetAttr("string.attr", "hello-world")
	span.SetAttr("bool.attr", true)
	span.SetAttr("int.attr", int64(42))
	span.SetAttr("float.attr", 3.14)
	span.SetStatus(tracing.StatusError, "something went wrong")
	span.End()
	_ = ctx

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	spans := coll.Spans(t)
	if len(spans) == 0 {
		t.Fatal("Spans returned empty after submit")
	}

	s := coll.FindSpan(t, "test-span-round-trip")
	if s == nil {
		t.Fatal("FindSpan returned nil for submitted span")
	}

	if got := AttrString(s, "string.attr"); got != "hello-world" {
		t.Errorf("string.attr = %q, want %q", got, "hello-world")
	}
	if got := AttrBool(s, "bool.attr"); !got {
		t.Error("bool.attr = false, want true")
	}
	if got := AttrInt(s, "int.attr"); got != 42 {
		t.Errorf("int.attr = %d, want 42", got)
	}

	if s.Status.Code != "STATUS_CODE_ERROR" {
		t.Errorf("Status.Code = %q, want %q", s.Status.Code, "STATUS_CODE_ERROR")
	}
}

func TestContextPropagation(t *testing.T) {
	t.Parallel()
	coll := NewCapturedSpans(t)
	exp := StartTestExporter(t, coll)

	parentCtx, parentSpan := exp.StartSpan(tracing.Context{}, "parent-span")
	parentSpan.SetAttr("phase", "parent")
	parentSpan.End()

	childCtx, childSpan := exp.StartSpan(parentCtx, "child-span")
	childSpan.SetAttr("phase", "child")
	childSpan.End()
	_ = childCtx

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	spans := coll.Spans(t)
	if len(spans) < 2 {
		t.Fatalf("Spans count = %d, want >= 2", len(spans))
	}

	parent := coll.FindSpan(t, "parent-span")
	if parent == nil {
		t.Fatal("parent span not found")
	}
	child := coll.FindSpan(t, "child-span")
	if child == nil {
		t.Fatal("child span not found")
	}

	if len(parent.Attributes) == 0 {
		t.Error("parent span has no attributes")
	}
	if len(child.Attributes) == 0 {
		t.Error("child span has no attributes")
	}

	if phase := AttrString(parent, "phase"); phase != "parent" {
		t.Errorf("parent phase = %q, want %q", phase, "parent")
	}
	if phase := AttrString(child, "phase"); phase != "child" {
		t.Errorf("child phase = %q, want %q", phase, "child")
	}
}

func TestSpanStatusOK(t *testing.T) {
	t.Parallel()
	coll := NewCapturedSpans(t)
	exp := StartTestExporter(t, coll)

	_, span := exp.StartSpan(tracing.Context{}, "ok-span")
	span.SetStatus(tracing.StatusOK, "")
	span.End()

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := coll.FindSpan(t, "ok-span")
	if s == nil {
		t.Fatal("ok-span not found")
	}
	if s.Status.Code != "STATUS_CODE_OK" {
		t.Errorf("Status.Code = %q, want %q", s.Status.Code, "STATUS_CODE_OK")
	}
}

func TestSpanStatusUnset(t *testing.T) {
	t.Parallel()
	coll := NewCapturedSpans(t)
	exp := StartTestExporter(t, coll)

	_, span := exp.StartSpan(tracing.Context{}, "unset-span")
	span.End()

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := coll.FindSpan(t, "unset-span")
	if s == nil {
		t.Fatal("unset-span not found")
	}
	if s.Status.Code != "STATUS_CODE_OK" {
		t.Errorf("Status.Code = %q, want %q (unset promoted to OK on End)", s.Status.Code, "STATUS_CODE_OK")
	}
}

func TestMultipleSpansSameBatch(t *testing.T) {
	t.Parallel()
	coll := NewCapturedSpans(t)
	exp := StartTestExporter(t, coll)

	const n = 5
	for i := 0; i < n; i++ {
		_, span := exp.StartSpan(tracing.Context{}, "batch-span")
		span.SetAttr("index", int64(i))
		span.End()
	}

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	spans := coll.Spans(t)
	if len(spans) != n {
		t.Errorf("Spans count = %d, want %d", len(spans), n)
	}

	for i := 0; i < n; i++ {
		s := coll.FindSpan(t, "batch-span")
		if s == nil {
			t.Fatalf("batch-span not found at index %d", i)
		}
	}
}

func TestSpanAttrIntVariousTypes(t *testing.T) {
	t.Parallel()
	coll := NewCapturedSpans(t)
	exp := StartTestExporter(t, coll)

	_, span := exp.StartSpan(tracing.Context{}, "int-types-span")
	span.SetAttr("int", int64(10))
	span.SetAttr("int8", int8(20))
	span.SetAttr("int16", int16(30))
	span.SetAttr("int32", int32(40))
	span.SetAttr("uint", uint64(50))
	span.SetAttr("uint8", uint8(60))
	span.SetAttr("uint16", uint16(70))
	span.SetAttr("uint32", uint32(80))
	span.End()

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := coll.FindSpan(t, "int-types-span")
	if s == nil {
		t.Fatal("int-types-span not found")
	}

	if got := AttrInt(s, "int"); got != 10 {
		t.Errorf("int = %d, want 10", got)
	}
	if got := AttrInt(s, "int8"); got != 20 {
		t.Errorf("int8 = %d, want 20", got)
	}
	if got := AttrInt(s, "int16"); got != 30 {
		t.Errorf("int16 = %d, want 30", got)
	}
	if got := AttrInt(s, "int32"); got != 40 {
		t.Errorf("int32 = %d, want 40", got)
	}
	if got := AttrInt(s, "uint"); got != 50 {
		t.Errorf("uint = %d, want 50", got)
	}
	if got := AttrInt(s, "uint8"); got != 60 {
		t.Errorf("uint8 = %d, want 60", got)
	}
	if got := AttrInt(s, "uint16"); got != 70 {
		t.Errorf("uint16 = %d, want 70", got)
	}
	if got := AttrInt(s, "uint32"); got != 80 {
		t.Errorf("uint32 = %d, want 80", got)
	}
}

func TestAttrStringNotFound(t *testing.T) {
	t.Parallel()
	coll := NewCapturedSpans(t)
	exp := StartTestExporter(t, coll)

	_, span := exp.StartSpan(tracing.Context{}, "attrs-span")
	span.SetAttr("present", "yes")
	span.End()

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := coll.FindSpan(t, "attrs-span")
	if s == nil {
		t.Fatal("attrs-span not found")
	}

	if got := AttrString(s, "missing"); got != "" {
		t.Errorf("AttrString(missing) = %q, want %q", got, "")
	}
	if AttrBool(s, "missing") != false {
		t.Error("AttrBool(missing) = true, want false")
	}
	if AttrInt(s, "missing") != 0 {
		t.Error("AttrInt(missing) = non-zero, want 0")
	}
}

func TestSpanNamePropagation(t *testing.T) {
	t.Parallel()
	coll := NewCapturedSpans(t)
	exp := StartTestExporter(t, coll)

	_, span := exp.StartSpan(tracing.Context{}, "unique-span-name-xyz")
	span.End()

	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := coll.FindSpan(t, "unique-span-name-xyz")
	if s == nil {
		t.Fatal("FindSpan returned nil for submitted span")
	}
	if s.Name != "unique-span-name-xyz" {
		t.Errorf("Name = %q, want %q", s.Name, "unique-span-name-xyz")
	}
}
