package tracingtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/tracing"
)

// capturedAttr mirrors the OTLP/JSON attribute shape emitted by the
// exporter. Tests across all packages consume this; the shape is
// intentionally loose so a future exporter refactor that reorders
// fields does not ripple through every middleware test.
type CapturedAttr struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

// CapturedSpan is one span as posted to a throwaway collector.
// Tests assert against the Name / Attributes / Status fields.
type CapturedSpan struct {
	Name       string         `json:"name"`
	Attributes []CapturedAttr `json:"attributes"`
	Status     struct {
		Code string `json:"code"`
	} `json:"status"`
}

// CapturedSpans collects every body the exporter POSTs and lets the
// caller walk the embedded spans. The httptest.Server is closed via
// t.Cleanup so the test does not have to track it.
type CapturedSpans struct {
	mu     sync.Mutex
	bodies [][]byte
	server *httptest.Server
}

// NewCapturedSpans starts a throwaway collector bound to t.Cleanup.
// Returns the collector; the underlying server closes at test end.
func NewCapturedSpans(t *testing.T) *CapturedSpans {
	t.Helper()
	c := &CapturedSpans{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.server.Close)
	return c
}

// ServerURL returns the collector URL — feed it into ExporterConfig.
func (c *CapturedSpans) ServerURL() string { return c.server.URL }

// Spans parses every captured body and returns the spans inside.
// The exporter batches posts so multiple bodies is the norm.
func (c *CapturedSpans) Spans(t *testing.T) []CapturedSpan {
	t.Helper()
	var out []CapturedSpan
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, raw := range c.bodies {
		var payload struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []CapturedSpan `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode OTLP body: %v", err)
		}
		for _, rs := range payload.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				out = append(out, ss.Spans...)
			}
		}
	}
	return out
}

// FindSpan returns the first span with the given name. Returns nil
// (no fatal) so callers can write "if s == nil { t.Fatal(...) }" or
// "if s != nil { t.Error(...) }" assertions naturally.
func (c *CapturedSpans) FindSpan(t *testing.T, name string) *CapturedSpan {
	t.Helper()
	for _, s := range c.Spans(t) {
		if s.Name == name {
			s := s
			return &s
		}
	}
	return nil
}

// AttrString extracts a string-valued attribute. Returns "" when
// the key is absent or the value is non-string.
func AttrString(s *CapturedSpan, key string) string {
	for _, a := range s.Attributes {
		if a.Key != key {
			continue
		}
		if v, ok := a.Value["stringValue"].(string); ok {
			return v
		}
	}
	return ""
}

// AttrBool extracts a bool-valued attribute. Returns false when
// the key is absent or non-bool.
func AttrBool(s *CapturedSpan, key string) bool {
	for _, a := range s.Attributes {
		if a.Key != key {
			continue
		}
		if v, ok := a.Value["boolValue"].(bool); ok {
			return v
		}
	}
	return false
}

// AttrInt extracts an int64-valued attribute (numbers decode as
// float64 in generic JSON; the int branch is transparently
// converted). Returns 0 when absent.
func AttrInt(s *CapturedSpan, key string) int64 {
	for _, a := range s.Attributes {
		if a.Key != key {
			continue
		}
		if v, ok := a.Value["intValue"].(float64); ok {
			return int64(v)
		}
	}
	return 0
}

// StartTestExporter spins up an exporter pointed at coll, registers
// it as the process-wide exporter (so middleware can guard with
// tracing.Enabled), and returns the *Exporter so the test can Close
// it and drain the queue. The deferred cleanup clears the global
// pointer via RegisterExporter(nil) so subsequent tests start fresh.
func StartTestExporter(t *testing.T, coll *CapturedSpans) *tracing.Exporter {
	t.Helper()
	exp := tracing.NewExporter(tracing.ExporterConfig{
		Endpoint:  coll.ServerURL(),
		QueueSize: 8,
		Timeout:   2 * time.Second,
	})
	if exp == nil {
		t.Fatal("NewExporter returned nil; Endpoint empty?")
	}
	tracing.RegisterExporter(exp)
	t.Cleanup(func() {
		tracing.RegisterExporter(nil)
	})
	return exp
}
