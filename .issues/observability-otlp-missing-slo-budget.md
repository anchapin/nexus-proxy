---
about: The Collector implements nexus_slo_error_budget_remaining as a GaugeSample
  consumed by the Prometheus renderer, but CollectMetricSnapshot() never exports it
  to OTLP/JSON — OTLP consumers have no visibility into SLO error budgets.
labels: area=observability,area=metrics,phase=2-observability
severity: medium
---

## Problem

The `Collector` tracks SLO error budget remaining for three SLOs (`availability`, `local_latency_p99`, `ttft_p95`) via `SetSLOErrorBudget()` / `SLOErrorBudgetGauges()` (issue #1239). These are rendered as Prometheus `Gauge` samples:

```
# HELP nexus_slo_error_budget_remaining SLO error budget remaining (0..1, 1=full budget).
# TYPE nexus_slo_error_budget_remaining gauge
nexus_slo_error_budget_remaining{slo="availability"} 0.873
nexus_slo_error_budget_remaining{slo="local_latency_p99"} 0.950
nexus_slo_error_budget_remaining{slo="ttft_p95"} 0.720
```

However, `CollectMetricSnapshot()` in `otel_metrics.go` never includes these gauges. OTLP consumers (Grafana Cloud, Honeycomb, New Relic) that use the OTLP/JSON endpoint instead of Prometheus scraping have **no way to monitor SLO error budget health**.

## Fix

In `CollectMetricSnapshot()`, add a section after the existing metric snapshots that calls `c.SLOErrorBudgetGauges()` and emits each `GaugeSample` as an OTLP gauge metric with `name: "nexus_slo_error_budget_remaining"`, `type: MetricTypeGauge`, and `labels: {slo: ...}`.

## Files

- `internal/observability/otel_metrics.go` — add SLO error budget gauges to `CollectMetricSnapshot()`
