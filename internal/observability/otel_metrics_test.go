package observability

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
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
