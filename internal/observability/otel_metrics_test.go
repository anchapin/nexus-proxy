package observability

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/ioutils"
	"github.com/anchapin/nexus-proxy/internal/upstream"
)

func TestOtelMetricsExporterExportFailureCounter(t *testing.T) {
	var requestCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	collector := NewCollector()
	RegisterCollector(collector)

	exporter := NewOtelMetricsExporter(OtelMetricsConfig{
		Endpoint:       server.URL,
		Interval:       500 * time.Millisecond,
		Timeout:        2 * time.Second,
		MaxRetries:     1,
		RetryBaseDelay: 10 * time.Millisecond,
		MaxRetryDelay:  10 * time.Millisecond,
	})
	if exporter == nil {
		t.Fatal("expected non-nil exporter")
	}
	defer exporter.Close()

	initialFailures := exporter.ExportFailures()
	if initialFailures != 0 {
		t.Fatalf("expected 0 initial failures, got %d", initialFailures)
	}

	time.Sleep(1200 * time.Millisecond)

	afterFailures := exporter.ExportFailures()
	if afterFailures <= 0 {
		t.Errorf("expected export failures to be > 0 after failed export, got %d", afterFailures)
	}
	if requestCount.Load() == 0 {
		t.Error("expected at least one request to the test server")
	}
}

func TestOtelMetricsExporterExportFailureCounterOnClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	collector := NewCollector()
	RegisterCollector(collector)

	exporter := NewOtelMetricsExporter(OtelMetricsConfig{
		Endpoint:       server.URL,
		Interval:       60 * time.Second,
		Timeout:        2 * time.Second,
		MaxRetries:     1,
		RetryBaseDelay: 10 * time.Millisecond,
		MaxRetryDelay:  10 * time.Millisecond,
	})
	if exporter == nil {
		t.Fatal("expected non-nil exporter")
	}

	initialFailures := exporter.ExportFailures()
	if initialFailures != 0 {
		t.Fatalf("expected 0 initial failures, got %d", initialFailures)
	}

	exporter.Close()

	afterCloseFailures := exporter.ExportFailures()
	if afterCloseFailures == 0 {
		t.Error("expected export failures > 0 after Close triggers export to failed server")
	}
}

func TestOtelMetricsExporterExportSuccessNoCounterIncrement(t *testing.T) {
	var requestCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	collector := NewCollector()
	RegisterCollector(collector)

	exporter := NewOtelMetricsExporter(OtelMetricsConfig{
		Endpoint:       server.URL,
		Interval:       500 * time.Millisecond,
		Timeout:        2 * time.Second,
		MaxRetries:     1,
		RetryBaseDelay: 10 * time.Millisecond,
		MaxRetryDelay:  10 * time.Millisecond,
	})
	if exporter == nil {
		t.Fatal("expected non-nil exporter")
	}
	defer exporter.Close()

	time.Sleep(1200 * time.Millisecond)

	afterSuccess := exporter.ExportFailures()
	if afterSuccess != 0 {
		t.Errorf("expected 0 failures after successful export, got %d", afterSuccess)
	}
	if requestCount.Load() == 0 {
		t.Error("expected at least one request to the test server")
	}
}

func TestOtelMetricsExporterExportFailureIncrementsByOne(t *testing.T) {
	var failCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	collector := NewCollector()
	RegisterCollector(collector)

	exporter := NewOtelMetricsExporter(OtelMetricsConfig{
		Endpoint:       server.URL,
		Interval:       200 * time.Millisecond,
		Timeout:        2 * time.Second,
		MaxRetries:     0,
		RetryBaseDelay: 10 * time.Millisecond,
		MaxRetryDelay:  10 * time.Millisecond,
	})
	if exporter == nil {
		t.Fatal("expected non-nil exporter")
	}
	defer exporter.Close()

	time.Sleep(900 * time.Millisecond)

	failures := exporter.ExportFailures()
	if failures == 0 {
		t.Fatal("expected export failures to be incremented")
	}
	if failCount.Load() == 0 {
		t.Fatal("expected server to have received requests")
	}
}

func TestOtelMetricsExporterTransportError(t *testing.T) {
	collector := NewCollector()
	RegisterCollector(collector)

	exporter := NewOtelMetricsExporter(OtelMetricsConfig{
		Endpoint:       "http://127.0.0.1:1/unreachable",
		Interval:       200 * time.Millisecond,
		Timeout:        500 * time.Millisecond,
		MaxRetries:     0,
		RetryBaseDelay: 10 * time.Millisecond,
		MaxRetryDelay:  10 * time.Millisecond,
	})
	if exporter == nil {
		t.Fatal("expected non-nil exporter")
	}
	defer exporter.Close()

	time.Sleep(500 * time.Millisecond)

	failures := exporter.ExportFailures()
	if failures == 0 {
		t.Error("expected export failures > 0 after transport error to unreachable endpoint")
	}
}

func TestCollectMetricSnapshotOmitsExportFailuresCounterWhenNoExporter(t *testing.T) {
	collector := NewCollector()
	RegisterCollector(collector)

	RegisterOtelMetricsExporter(nil)

	snapshot := CollectMetricSnapshot()
	for _, m := range snapshot {
		if m.Name == "nexus_otel_metrics_export_failures_total" {
			t.Error("nexus_otel_metrics_export_failures_total should not be in snapshot when no global exporter is set")
			break
		}
	}
}

func TestCollectMetricSnapshotIncludesExportFailuresCounterWithExporter(t *testing.T) {
	collector := NewCollector()
	RegisterCollector(collector)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exporter := NewOtelMetricsExporter(OtelMetricsConfig{
		Endpoint: server.URL,
		Interval: 60 * time.Second,
		Timeout:  2 * time.Second,
	})
	RegisterOtelMetricsExporter(exporter)
	defer exporter.Close()

	found := false
	for _, m := range CollectMetricSnapshot() {
		if m.Name == "nexus_otel_metrics_export_failures_total" {
			found = true
			if m.Type != MetricTypeCounter {
				t.Errorf("expected counter type, got %s", m.Type)
			}
			break
		}
	}
	if !found {
		t.Error("expected nexus_otel_metrics_export_failures_total in snapshot when global exporter is set")
	}
}

func TestOtelMetricsExporterExportFailureIncrementsCorrectly(t *testing.T) {
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failServer.Close()

	collector := NewCollector()
	RegisterCollector(collector)

	exporter := NewOtelMetricsExporter(OtelMetricsConfig{
		Endpoint:   failServer.URL,
		Interval:   60 * time.Second,
		Timeout:    2 * time.Second,
		MaxRetries: 0,
	})
	if exporter == nil {
		t.Fatal("expected non-nil exporter")
	}

	beforeFailures := exporter.ExportFailures()

	exporter.Close()

	afterFailures := exporter.ExportFailures()
	if afterFailures != beforeFailures+1 {
		t.Errorf("expected failures to increment from %d to %d, got %d", beforeFailures, beforeFailures+1, afterFailures)
	}
}

func TestCollectMetricSnapshotRateLimitMetrics(t *testing.T) {
	collector := NewCollector()
	RegisterCollector(collector)

	collector.IncRateLimit("global", true)
	collector.IncRateLimit("global", true)
	collector.IncRateLimit("global", false)
	collector.IncRateLimit("per_client", true)
	collector.IncRateLimit("per_client", false)
	collector.IncRateLimit("per_client", false)

	snapshot := CollectMetricSnapshot()

	var allowedGlobal, allowedPerClient, rejectedGlobal, rejectedPerClient float64
	var allowedGlobalScope, allowedPerClientScope, rejectedGlobalScope, rejectedPerClientScope string
	var allowedFound, rejectedFound bool

	for _, m := range snapshot {
		if m.Name == "nexus_rate_limit_allowed_total" {
			allowedFound = true
			if scope, ok := m.Labels["scope"]; ok {
				switch scope {
				case "global":
					allowedGlobal = m.Sum
					allowedGlobalScope = scope
				case "per_client":
					allowedPerClient = m.Sum
					allowedPerClientScope = scope
				}
			}
		}
		if m.Name == "nexus_rate_limit_rejected_total" {
			rejectedFound = true
			if scope, ok := m.Labels["scope"]; ok {
				switch scope {
				case "global":
					rejectedGlobal = m.Sum
					rejectedGlobalScope = scope
				case "per_client":
					rejectedPerClient = m.Sum
					rejectedPerClientScope = scope
				}
			}
		}
	}

	if !allowedFound {
		t.Error("expected nexus_rate_limit_allowed_total in snapshot")
	}
	if !rejectedFound {
		t.Error("expected nexus_rate_limit_rejected_total in snapshot")
	}

	if allowedGlobal != 2 {
		t.Errorf("expected nexus_rate_limit_allowed_total{scope=global} = 2, got %v", allowedGlobal)
	}
	if allowedPerClient != 1 {
		t.Errorf("expected nexus_rate_limit_allowed_total{scope=per_client} = 1, got %v", allowedPerClient)
	}
	if rejectedGlobal != 1 {
		t.Errorf("expected nexus_rate_limit_rejected_total{scope=global} = 1, got %v", rejectedGlobal)
	}
	if rejectedPerClient != 2 {
		t.Errorf("expected nexus_rate_limit_rejected_total{scope=per_client} = 2, got %v", rejectedPerClient)
	}

	if allowedGlobalScope != "global" {
		t.Errorf("expected allowed metric scope 'global', got %q", allowedGlobalScope)
	}
	if allowedPerClientScope != "per_client" {
		t.Errorf("expected allowed metric scope 'per_client', got %q", allowedPerClientScope)
	}
	if rejectedGlobalScope != "global" {
		t.Errorf("expected rejected metric scope 'global', got %q", rejectedGlobalScope)
	}
	if rejectedPerClientScope != "per_client" {
		t.Errorf("expected rejected metric scope 'per_client', got %q", rejectedPerClientScope)
	}
}

func TestCollectMetricSnapshotIncludesTruncatedCounter(t *testing.T) {
	collector := NewCollector()
	RegisterCollector(collector)

	before := ioutils.ReadAllTruncatedCounter()

	_, _ = ioutils.ReadAllLimited(bytes.NewReader(bytes.Repeat([]byte("x"), 1024)), 512)

	if got := ioutils.ReadAllTruncatedCounter(); got != before+1 {
		t.Fatalf("expected truncation counter to increment: before=%d, got=%d", before, got)
	}

	snapshot := CollectMetricSnapshot()
	found := false
	for _, m := range snapshot {
		if m.Name == "nexus_upstream_response_truncated_total" {
			found = true
			if m.Type != MetricTypeCounter {
				t.Errorf("expected counter type, got %s", m.Type)
			}
			if m.Sum != float64(before+1) {
				t.Errorf("expected sum=%f, got %f", float64(before+1), m.Sum)
			}
			break
		}
	}
	if !found {
		t.Error("expected nexus_upstream_response_truncated_total in snapshot")
	}
}

func TestCollectMetricSnapshotIncludesAllSevenMissingMetrics(t *testing.T) {
	collector := NewCollector()
	RegisterCollector(collector)

	collector.IncAuthBlocked("missing")
	collector.IncAuthBlocked("missing")
	collector.IncAuthBlocked("invalid")

	RegisterTelemetryRecorder(mockTelemetryRecorder{
		dropped:     10,
		rotations:   5,
		writeErrors: 2,
	})

	RegisterTracingExporter(mockTracingExporter{
		dropped:       100,
		flushFailures: 7,
		queueDepth:    42,
	})

	snapshot := CollectMetricSnapshot()

	found := make(map[string]bool)
	for _, m := range snapshot {
		found[m.Name] = true
	}

	// Auth limiter blocked (issue #831/#937)
	if !found["nexus_auth_limiter_blocked_total"] {
		t.Error("expected nexus_auth_limiter_blocked_total in snapshot")
	}

	// Telemetry counters (issue #1360)
	if !found["nexus_telemetry_dropped_total"] {
		t.Error("expected nexus_telemetry_dropped_total in snapshot")
	}
	if !found["nexus_telemetry_rotations_total"] {
		t.Error("expected nexus_telemetry_rotations_total in snapshot")
	}
	if !found["nexus_telemetry_write_errors_total"] {
		t.Error("expected nexus_telemetry_write_errors_total in snapshot")
	}

	// Tracing counters and gauge (issue #1360)
	if !found["nexus_tracing_dropped_total"] {
		t.Error("expected nexus_tracing_dropped_total in snapshot")
	}
	if !found["nexus_tracing_flush_failures_total"] {
		t.Error("expected nexus_tracing_flush_failures_total in snapshot")
	}
	if !found["nexus_tracing_queue_depth"] {
		t.Error("expected nexus_tracing_queue_depth in snapshot")
	}

	// Verify specific values
	for _, m := range snapshot {
		switch m.Name {
		case "nexus_auth_limiter_blocked_total":
			if m.Labels["reason"] == "missing" && m.Sum != 2 {
				t.Errorf("expected nexus_auth_limiter_blocked_total{reason=missing} = 2, got %v", m.Sum)
			}
			if m.Labels["reason"] == "invalid" && m.Sum != 1 {
				t.Errorf("expected nexus_auth_limiter_blocked_total{reason=invalid} = 1, got %v", m.Sum)
			}
		case "nexus_telemetry_dropped_total":
			if m.Sum != 10 {
				t.Errorf("expected nexus_telemetry_dropped_total = 10, got %v", m.Sum)
			}
		case "nexus_telemetry_rotations_total":
			if m.Sum != 5 {
				t.Errorf("expected nexus_telemetry_rotations_total = 5, got %v", m.Sum)
			}
		case "nexus_telemetry_write_errors_total":
			if m.Sum != 2 {
				t.Errorf("expected nexus_telemetry_write_errors_total = 2, got %v", m.Sum)
			}
		case "nexus_tracing_dropped_total":
			if m.Sum != 100 {
				t.Errorf("expected nexus_tracing_dropped_total = 100, got %v", m.Sum)
			}
		case "nexus_tracing_flush_failures_total":
			if m.Sum != 7 {
				t.Errorf("expected nexus_tracing_flush_failures_total = 7, got %v", m.Sum)
			}
		case "nexus_tracing_queue_depth":
			if m.Value != 42 {
				t.Errorf("expected nexus_tracing_queue_depth = 42, got %v", m.Value)
			}
			if m.Type != MetricTypeGauge {
				t.Errorf("expected nexus_tracing_queue_depth to be a gauge, got %v", m.Type)
			}
		}
	}
}

// mockTelemetryRecorder implements the extended recorder interface for testing.
type mockTelemetryRecorder struct {
	dropped     uint64
	rotations   uint64
	writeErrors uint64
}

func (m mockTelemetryRecorder) Dropped() uint64     { return m.dropped }
func (m mockTelemetryRecorder) Rotations() uint64   { return m.rotations }
func (m mockTelemetryRecorder) WriteErrors() uint64 { return m.writeErrors }

// mockTracingExporter implements the extended exporter interface for testing.
type mockTracingExporter struct {
	dropped       uint64
	flushFailures uint64
	queueDepth    int
}

func (m mockTracingExporter) Dropped() uint64       { return m.dropped }
func (m mockTracingExporter) FlushFailures() uint64 { return m.flushFailures }
func (m mockTracingExporter) QueueDepth() int       { return m.queueDepth }

// TestCollectMetricSnapshotIncludesCoalescingFusionDSLCounters verifies that
// the coalescing, fusion, and DSL-promoted counters added in issue #1414
// are present in the OTLP CollectMetricSnapshot output.
func TestCollectMetricSnapshotIncludesCoalescingFusionDSLCounters(t *testing.T) {
	collector := NewCollector()
	RegisterCollector(collector)

	rc := NewRouteCounters()
	RegisterRouteCounters(rc)

	upstream.ResetCoalesceCountersForTest()

	snapshot := CollectMetricSnapshot()

	found := make(map[string]bool)
	for _, m := range snapshot {
		found[m.Name] = true
	}

	// Coalescing counters (issue #1414)
	if !found["nexus_coalesce_hits_total"] {
		t.Error("expected nexus_coalesce_hits_total in snapshot")
	}
	if !found["nexus_coalesce_misses_total"] {
		t.Error("expected nexus_coalesce_misses_total in snapshot")
	}

	// Fusion counters (issue #1414)
	if !found["nexus_fusion_client_abort_total"] {
		t.Error("expected nexus_fusion_client_abort_total in snapshot")
	}
	if !found["nexus_fusion_jaccard_similarity_total"] {
		t.Error("expected nexus_fusion_jaccard_similarity_total in snapshot")
	}
	if !found["nexus_fusion_semantic_similarity_total"] {
		t.Error("expected nexus_fusion_semantic_similarity_total in snapshot")
	}
	if !found["nexus_panel_panics_total"] {
		t.Error("expected nexus_panel_panics_total in snapshot")
	}

	// DSL promoted counter (issue #1414)
	if !found["nexus_route_dsl_promoted_total"] {
		t.Error("expected nexus_route_dsl_promoted_total in snapshot")
	}

	// Verify counter types are correct (all should be MetricTypeCounter)
	for _, m := range snapshot {
		switch m.Name {
		case "nexus_coalesce_hits_total", "nexus_coalesce_misses_total",
			"nexus_fusion_client_abort_total", "nexus_fusion_jaccard_similarity_total",
			"nexus_fusion_semantic_similarity_total", "nexus_panel_panics_total",
			"nexus_route_dsl_promoted_total":
			if m.Type != MetricTypeCounter {
				t.Errorf("expected %s to be MetricTypeCounter, got %s", m.Name, m.Type)
			}
		}
	}
}
