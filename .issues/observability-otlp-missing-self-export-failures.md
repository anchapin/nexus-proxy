---
about: nexus_otel_metrics_export_failures_total is tracked as an atomic.Uint64 in
  OtelMetricsExporter and wired to the Prometheus endpoint via GaugeProvider, but
  it is not included in the OTLP/JSON metrics export itself — the self-monitoring
  path is incomplete.
labels: area=observability,area=metrics,phase=2-observability
severity: low
---

## Problem

`OtelMetricsExporter.exportFailures` (`atomic.Uint64`) tracks every failed OTLP/JSON POST attempt. It is wired to the Prometheus endpoint via a `GaugeProvider` in `server.go`:

```go
 GaugeProviderFunc(func() []observability.GaugeSample {
     exp := observability.GlobalOtelMetricsExporter()
     if exp == nil { return nil }
     return []observability.GaugeSample{{
         Name: "nexus_otel_metrics_export_failures_total",
         Value: float64(exp.ExportFailures()),
     }}
 }),
```

This means the Prometheus scrape path correctly exposes `nexus_otel_metrics_export_failures_total`.

However, `CollectMetricSnapshot()` does **not** include this counter. Consequently, an OTLP consumer cannot monitor OTLP export health through the OTLP path itself. If the OTLP exporter is struggling, OTLP consumers cannot see it through OTLP — they must fall back to Prometheus scraping for this specific metric.

## Fix

In `CollectMetricSnapshot()`, add a snapshot for `nexus_otel_metrics_export_failures_total` as a counter:

```go
out = append(out, MetricSnapshot{
    Name: "nexus_otel_metrics_export_failures_total",
    Type: MetricTypeCounter,
    Sum:  float64(exportFailures.Load()),
})
```

Also expose `ExportFailures() uint64` on `OtelMetricsExporter` if not already public (check `internal/observability/otel_metrics.go`).

## Files

- `internal/observability/otel_metrics.go` — add self-referential export-failures snapshot to `CollectMetricSnapshot()`
