---
about: OTLP/JSON metrics export (issue #1238) currently ships only Collector metrics.
  Route-decision, SLM, fusion, cascade, RAG cache, and DSL metrics are absent
  from the OTLP export even though they are fully implemented for the Prometheus
  text endpoint.
labels: area=observability,area=metrics,phase=2-observability
severity: medium
---

## Problem

`CollectMetricSnapshot()` in `internal/observability/otel_metrics.go` replicates the Prometheus renderer's metric snapshot logic for OTLP/JSON export. It exports all `Collector` (requests/errors/tokens/cost/auth/rate-limit/budget/latency/TTFT/pipeline-stage/confidence/similarity/judge/embedder/RAG-circuit/frontier-probe) metrics but entirely omits the `RouteCounters` family that powers the Prometheus `/metrics` endpoint.

**OTLP consumers cannot see any of the following metrics that are available via Prometheus:**

| Metric | Type | What it tracks |
|--------|------|----------------|
| `nexus_route_decisions_total` | counter | Routing decisions by route + source |
| `nexus_slm_decisions_total` | counter | SLM routing decisions by route + confidence bucket + task type |
| `nexus_slm_low_confidence_escalations_total` | counter | Escalations due to low SLM confidence |
| `nexus_slm_escalations_total` | counter | Hard escalations to frontier |
| `nexus_slm_cache_embedding_errors_total` | counter | Embedder errors inside SLM cache (issue #741) |
| `nexus_slm_cache_evictions_total` | counter | SLM cache evictions by reason (issue #449) |
| `nexus_local_cooldown_triggers_total` | counter | Local-route cooldown arm events (issue #530) |
| `nexus_route_budget_downtier_total` | counter | Requests down-tiered due to exhausted budget (issue #1163) |
| `nexus_requests_rejected_total` | counter | Proxy rejections by reason |
| `nexus_fusion_arbiter_total` | counter | Fusion arbiter outcomes by reason |
| `nexus_rag_retrieval_total` | counter | RAG retrieval hits/misses by path and miss reason |
| `nexus_rag_cache_hits_total` | counter | RAG prompt embedding cache hits |
| `nexus_rag_cache_misses_total` | counter | RAG prompt embedding cache misses |
| `nexus_cascade_fallback_total` | counter | Cascade fallback by reason |
| `nexus_fusion_arbiter_cache_total` | counter | Fusion arbiter cache hits/misses |
| `nexus_judge_queue_overflow_total` | counter | Judge queue saturation events |
| `nexus_quality_queue_overflow_total` | counter | Quality queue saturation events |
| `nexus_panel_panics_total` | counter | Panel panic recoveries |
| `nexus_prompt_injection_hits_total` | counter | Prompt injection detections by mode (off/warn/strict) |
| `nexus_redacted_total` | counter | Response-content redactions by profile (issue #1172) |
| `nexus_slm_cache_hits_total` | counter | SLM decision cache hits by kind |
| `nexus_slm_cache_misses_total` | counter | SLM decision cache misses |
| `nexus_router_dsl_hits_total` | counter | DSL fast-pass hits by reason (issue #875) |
| `nexus_router_dsl_misses_total` | counter | DSL fast-pass misses (issue #875) |

## Fix

`CollectMetricSnapshot()` should also call `RouteCounters().WriteTo(io.Discard)` semantics equivalent — snapshot the `RouteCounters` atomic counters and emit them as OTLP metric snapshots with appropriate labels. A helper analogous to `writeSeries`/`writeLabelledSeries` but building `[]MetricSnapshot` instead of Prometheus text would avoid duplicating the label/key logic.

The OTLP metric names should match the Prometheus names (e.g. `nexus_route_decisions_total`) with `counter` / `histogram` / `gauge` `MetricType` set per family.

## Files

- `internal/observability/otel_metrics.go` — add route-counter snapshot section to `CollectMetricSnapshot()`
