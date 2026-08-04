# Observability Surface

This document inventories every Prometheus metric the proxy exposes,
the SQLite metrics-store schema, the telemetry JSONL fields, and the
distributed-tracing surface. It exists so operators and contributors
can see the full observability contract in one place.

> **Alerting rules:** the repo ships production-ready Prometheus
> alerting and recording rules in `deploy/prometheus/` (alerting
> runbook, load path, and per-alert remediation in
> [alerting.md](alerting.md)). Validate them with `make check-rules`.

## Prometheus metrics (`GET /metrics`)

The proxy implements a bespoke Prometheus text-format exposition
(no `client_golang` dependency). Four counter families are defined
in `internal/observability/routemetrics.go`. All metrics use the
`nexus_` prefix, `_total` suffix (counter convention), and
snake_case naming.

### Metric inventory

| Family | Type | Labels | Cardinality | Defined in |
|--------|------|--------|-------------|------------|
| `nexus_route_decisions_total` | counter | `route`, `source` | 3 × 8 = 24 | `routemetrics.go` |
| `nexus_route_budget_downtier_total` | counter | *(none)* | 1 | `routemetrics.go` (issue #1163) |
| `nexus_slm_decisions_total` | counter | `route`, `confidence_bucket`, `task_type` | 3 × 4 × 8 = 96 | `routemetrics.go` |
| `nexus_slm_low_confidence_escalations_total` | counter | `task_type` | 8 | `routemetrics.go` |
| `nexus_slm_cache_hits_total` | counter | `kind` | 2 (`exact`, `semantic`) | `routemetrics.go` |
| `nexus_slm_cache_misses_total` | counter | *(none)* | 1 | `routemetrics.go` |
| `nexus_slm_cache_evictions_total` | counter | `reason` | 2 (`ttl`, `lru`) | `routemetrics.go` |
| `nexus_slm_cache_entries` | gauge | *(none)* | 1 | `prometheus.go` (issue #531) |
| `nexus_slm_cache_max_entries` | gauge | *(none)* | 1 | `prometheus.go` (issue #531) |
| `nexus_slm_cache_stale_entries` | gauge | *(none)* | 1 | `prometheus.go` (issue #801) |
| `nexus_local_cooldown_active` | gauge | *(none)* | 1 | `prometheus.go` (issue #530) |
| `nexus_local_cooldown_triggers_total` | counter | *(none)* | 1 | `routemetrics.go` (issue #530) |
| `nexus_requests_rejected_total` | counter | `reason` | 4 | `routemetrics.go` |
| `nexus_cascade_fallback_total` | counter | `reason` | 6 (`timeout`, `transport_error`, `rate_limited`, `http_error`, `malformed_toolcall`, `malformed_response`) | `routemetrics.go` |
| `nexus_upstream_response_truncated_total` | counter | *(none)* | 1 | `routemetrics.go` (issue #365) |
| `nexus_rag_retrieval_total` | counter | `hit`, `reason` (miss only) | 1 + 3 = 4 | `routemetrics.go` |
| `nexus_judge_dropped_total` | counter | *(none)* | 1 | `routemetrics.go` |
| `nexus_rag_judge_score_sum` | counter | `injected` | 2 (`true`, `false`) | `prometheus.go` (issue #1167) |
| `nexus_rag_judge_score_count` | counter | `injected` | 2 (`true`, `false`) | `prometheus.go` (issue #1167) |
| `nexus_fusion_client_abort_total` | counter | *(none)* | 1 | `prometheus.go` (issue #1046) |
| `nexus_frontier_probe_total` | counter | `provider`, `result` | 2 × N providers | `collector.go` (issue #1158) |
| `nexus_frontier_circuit_open_total` | counter | `provider` | N providers | `collector.go` (issue #1158) |
| `nexus_egress_blocked_total` | counter | `reason` | 2 (`redirect`, `dial`) | `egressguard.go` (issue #1363) |
| `nexus_ip_allowlist_blocked_total` | counter | *(none)* | 1 | `prometheus.go` (issue #1361) |
| `nexus_rate_limit_bucket_utilization` | histogram | `bucket_id` | dynamic (≤ concurrent client IPs) | `prometheus.go` (issue #746) |
| `nexus_build_info` | gauge | `version`, `commit`, `go_version` | 1 | `prometheus.go` (issue #529) |
| `nexus_slo_error_budget_remaining` | gauge | `slo` | 3 (`availability`, `local_latency_p99`, `ttft_p95`) | `collector.go` (issue #1239) |

**Maximum theoretical series**: 24 + 96 + 8 + 2 + 1 + 2 + 1 + 1 + 4 + 6 + 4 + 1 + 2 + 2 + 1 + 1 + 1 + 2 + 1 = 164 series.

> **Note (issue #486):** `nexus_rag_retrieval_total` previously carried
> a `filename` label whose value was the raw RAG source filename, which
> produced one series per indexed document — unbounded cardinality. The
> label has been removed; hits are now collapsed into a single
> `{hit="true"}` sample line. The per-filename breakdown is preserved
> in the SQLite `rag_filename` column for offline analysis.

### Label value catalog

#### `route`

| Value | Constant | Defined in |
|-------|----------|------------|
| `local` | `router.RouteLocal` | `internal/router/dsl.go` |
| `frontier` | `router.RouteFrontier` | `internal/router/dsl.go` |
| `fusion` | `router.RouteFusion` | `internal/router/dsl.go` |

#### `source`

| Value | Constant | Meaning |
|-------|----------|---------|
| `guardrail` | `router.SourceGuardrail` | VRAM-aware token budget forced frontier |
| `dsl` | `router.SourceDSL` | Regex fast-pass matched |
| `dsl-promoted` | `router.SourceDSLPromoted` | N-gram auto-promoted to DSL fast-pass by PatternPromoter (issue #1165) |
| `slm` | `router.SourceSLM` | SLM returned a valid decision |
| `slm-error` | `router.SourceSLMError` | SLM call failed (timeout, bad JSON) |
| `escalation` | `router.SourceEscalation` | Defensive nil-SLM fallback to frontier |
| `slm-low-confidence` | `router.SourceSLMEscalation` | SLM confidence below threshold, escalated to frontier |
| `budget-down-tier` | `router.SourceBudgetDownTier` | Frontier budget exhausted, down-tiered to local |
| `dsl-promoted` | `router.SourceDSLPromoted` | DSL matched a fusion pattern, promoted to fusion |

#### `confidence_bucket`

Collapsed from the SLM's raw `float64` confidence via
`BucketConfidence()`:

| Bucket | Range | Notes |
|--------|-------|-------|
| `none` | `c <= 0` or non-SLM sources | Guardrail / DSL / slm-error |
| `low` | `0 < c < 0.4` | Below the low threshold |
| `medium` | `0.4 <= c < 0.7` | Neutral band (matches `NeutralConfidence`) |
| `high` | `c >= 0.7` | Above the medium threshold |

#### `task_type`

Generated by `router.Categorize()` in `internal/router/confidence.go`:

| Value | Constant |
|-------|----------|
| `css` | `CategoryCSS` |
| `refactoring` | `CategoryRefactoring` |
| `debugging` | `CategoryDebugging` |
| `architecture` | `CategoryArchitecture` |
| `boilerplate` | `CategoryBoilerplate` |
| `documentation` | `CategoryDocumentation` |
| `other` | `CategoryOther` |
| `""` (empty) | Non-SLM sources |

#### `reason`

Defined in `internal/handlers/chat.go`:

| Value | Constant | HTTP status |
|-------|----------|-------------|
| `method` | `RejectionMethod` | 405 |
| `body_too_large` | `RejectionBodyTooLarge` | 413 |
| `bad_request` | `RejectionBadRequest` | 400 |
| `rate_limit` | `RejectionRateLimit` | 429 |

For the `nexus_slm_cache_evictions_total` family, `reason` is a
separate bounded label set defined in `internal/router/slm_cache.go`
(issue #449):

| Value | Constant | Meaning |
|-------|----------|---------|
| `ttl` | `router.EvictionReasonTTL` | Entry removed because its TTL elapsed |
| `lru` | `router.EvictionReasonLRU` | Entry removed to make room at capacity |

`ttl` events indicate that the configured
`NEXUS_SLM_CACHE_TTL` (default 30s) is shorter than the natural
burst window of duplicate prompts — operators can raise the TTL to
absorb more duplicate traffic. `lru` events indicate the cache is
saturated (`NEXUS_SLM_CACHE_MAX_ENTRIES` is too small) — operators
can raise the cap to keep more prompts warm. Together the two
counters let operators diagnose cache effectiveness without changing
configuration: a high `ttl / (ttl + lru)` ratio points at TTL churn;
a high `lru / (ttl + lru)` ratio points at capacity pressure.

For the `nexus_cascade_fallback_total` family, `reason` is a separate
bounded label set defined in `internal/upstream/cascade.go` (issue #205,
extended in #497, #534):

| Value | Meaning |
|-------|---------|
| `timeout` | Per-attempt context deadline exceeded (`context.DeadlineExceeded`) |
| `transport_error` | Real transport error from `client.Do` (DNS failure, connection refused, TCP reset, TLS handshake) |
| `rate_limited` | Upstream returned HTTP 429 (Too Many Requests) — the upstream is rate-limiting, which is transient and likely to resolve quickly |
| `http_error` | Upstream returned a retryable HTTP status (408, 500, 502, 503, 504) — the upstream is present but overloaded or buggy |
| `malformed_toolcall` | Upstream returned a `tool_calls` entry with missing required fields or invalid JSON arguments |
| `malformed_response` | Upstream returned a 200 but the body could not be JSON-decoded, or the `choices` array was empty |

> **Dashboard impact (issue #534):** `http_error` was previously conflated
> with `transport_error`. Dashboards or alerts that aggregate
> `nexus_cascade_fallback_total{reason="transport_error"}` will see its
> value drop after this change (the 5xx/408/429 counts move to
> `http_error`). Splitting the two lets operators distinguish a broken
> network (`transport_error`) from an overloaded upstream (`http_error`)
> — these have completely different remediations. Update any PromQL panels
> that keyed on the old four-value closed set.

> **Prior dashboard impact (issue #497):** `malformed_response` was
> previously conflated with `transport_error`. Dashboards or alerts that
> aggregate `nexus_cascade_fallback_total{reason="transport_error"}` will
> see its value drop after this change (the decode-failure and empty-choices
> counts move to `malformed_response`). Splitting the two lets operators
> distinguish a broken network (`transport_error`) from an upstream that
> returns invalid responses (`malformed_response`) — these have
> completely different remediations. Update any PromQL/JSON-stat panels
> that keyed on the old three-value closed set.

#### `injected`

Used by the RAG-vs-judge quality correlation metrics (issue #1167):

| Value | Meaning |
|-------|---------|
| `true` | A RAG few-shot snippet was injected into the prompt for the sampled request |
| `false` | No RAG context was injected |

`nexus_rag_judge_score_sum{injected}` and
`nexus_rag_judge_score_count{injected}` let operators compute the
average judge quality score per label via
`sum / count by (injected)`. A higher average for `injected="true"`
indicates the RAG corpus is improving model output; a flat or lower
average suggests `NEXUS_RAG_THRESHOLD` should be tightened or the
corpus needs better examples. Only valid scores (1–5) are recorded;
parse failures are excluded.

### Naming convention audit

| Check | Result |
|-------|--------|
| All metrics use `nexus_` prefix | PASS |
| All counters use `_total` suffix | PASS |
| All names are snake_case | PASS |
| HELP and TYPE annotations present | PASS |
| No `nexus_requests_total` counter exists | DOCUMENTED — referenced in `chat.go` comments but never defined; the rejected-requests counter is `nexus_requests_rejected_total` |

### Cardinality audit

| Label | Bounded? | Max values | Notes |
|-------|----------|------------|-------|
| `route` | Yes | 3 | Fixed enum: local, frontier, fusion |
| `source` | Yes | 5 | Fixed enum: guardrail, dsl, slm, slm-error, escalation |
| `confidence_bucket` | Yes | 4 | Collapsed from float64 to 4 ordinal buckets |
| `task_type` | Yes | 8 | Fixed set from Categorize() + empty |
| `reason` (rejections) | Yes | 4 | Fixed set of rejection reasons |
| `reason` (SLM cache evictions) | Yes | 2 | `ttl`, `lru` — closed set defined in `internal/router/slm_cache.go` (issue #449) |
| `kind` (SLM cache hits) | Yes | 2 | `exact`, `semantic` |
| `hit` (RAG retrieval) | Yes | 2 | `true`, `false` (issue #186, #486) |
| `reason` (RAG retrieval miss) | Yes | 3 | `empty_store`, `threshold`, `embed_error` — closed set emitted only when `hit="false"` |
| SLM cache gauges (issue #531) | N/A | 2 | `nexus_slm_cache_entries` and `nexus_slm_cache_max_entries` are unlabelled gauges (cardinality 1 each); no label cardinality concerns. |
| Local-route cooldown (issue #530) | N/A | 2 | `nexus_local_cooldown_active` (gauge, cardinality 1) and `nexus_local_cooldown_triggers_total` (counter, cardinality 1) are both unlabelled; no label cardinality concerns. |
| `bucket_id` (rate-limit utilization, issue #746) | Yes | ≤ concurrent client IPs | Each distinct bucket ID (SHA256 of client IP, 8 hex chars) creates 6 series (4 quartile buckets + sum + count). Bounded by the number of distinct IPs seen within the bucket TTL window (10 minutes). Operators who need per-IP granularity can hash the `bucket_id` label downstream. |
| `slo` (error budget remaining, issue #1239) | Yes | 3 | `availability`, `local_latency_p99`, `ttft_p95` — fixed set of defined SLO names. |

**No unbounded cardinality labels exist.** All label values are
short, pre-defined strings with no user-controlled input. The
`filename` label that previously adorned
`nexus_rag_retrieval_total{hit="true"}` was removed in issue #486
because it derived from raw RAG source filenames and grew one series
per indexed document; the per-filename breakdown now lives only in
the SQLite `rag_filename` column.

## SQLite metrics store (`internal/metrics`)

When `NEXUS_METRICS_DB` is set, a structured row per request is
written to a SQLite database. Schema is in `internal/metrics/sqlite.go`.

### Schema columns

| Column | Type | Default | Prometheus equivalent |
|--------|------|---------|----------------------|
| `id` | INTEGER PK | autoincrement | — |
| `timestamp` | DATETIME | — | — |
| `request_id` | TEXT | — | — |
| `route` | TEXT | — | `nexus_route_decisions_total.route` |
| `model` | TEXT | — | — |
| `input_tokens` | INTEGER | 0 | — |
| `output_tokens` | INTEGER | 0 | — |
| `toon_savings_tokens` | INTEGER | 0 | — |
| `rag_injected` | INTEGER | 0 | — |
| `rag_filename` | TEXT | `''` | — |
| `estimated_cost_usd` | REAL | 0 | — |
| `baseline_cost_usd` | REAL | 0 | — |
| `savings_usd` | REAL | 0 | — |
| `ttft_ms` | INTEGER | 0 | — |
| `total_latency_ms` | REAL | 0 | — |
| `tps` | REAL | 0 | — |
| `streaming` | INTEGER | 1 | — |
| `fusion_arbiter_skipped` | INTEGER | 0 | — |
| `error` | TEXT | `''` | — |
| `route_source` | TEXT | `''` | `nexus_route_decisions_total.source` |
| `route_reason` | TEXT | `''` | — |
| `slm_confidence` | REAL | 0 | collapsed to `confidence_bucket` |
| `slm_task_type` | TEXT | `''` | `nexus_slm_decisions_total.task_type` |

### Schema alignment with Prometheus labels

| SQLite column | Prometheus label | Aligned? |
|---------------|------------------|----------|
| `route` | `route` | PASS |
| `route_source` | `source` | PASS |
| `slm_task_type` | `task_type` | PASS |
| `slm_confidence` | `confidence_bucket` | Aligned (raw vs bucketed) |
| `route_reason` | — | No Prometheus equivalent (informational) |

## Telemetry JSONL (`internal/telemetry`)

When `NEXUS_TELEMETRY_PATH` is set, a JSON object per request is
appended to the file. Fields mirror the SQLite schema. No Prometheus
labels are derived directly from the JSONL — the JSONL is a
tail-friendly log, not a metrics source.

### File rotation (issue #485)

`NEXUS_TELEMETRY_MAX_BYTES` (default `0` = disabled) enables size-based
rotation. When the next record would push the active file past the cap
it is atomically renamed to `path.<unix-nanoseconds>` and a fresh file
is opened. `NEXUS_TELEMETRY_MAX_FILES` (default `5`) bounds the number
of rotated files retained; the oldest is evicted at the cap. Both knobs
require a restart — the file handle is swapped atomically at boot, so
they are intentionally excluded from hot-reload.

| Metric | Source | Notes |
|--------|--------|-------|
| `nexus_telemetry_rotations_total` | `JSONLRecorder.Rotations()` | Counter; always 0 when rotation is disabled. Confirm operators can see this climbing to verify rotation is firing. |
| `nexus_telemetry_dropped_total` | `JSONLRecorder.Dropped()` | Counter; buffer-full drops (unchanged by #485). |
| `nexus_telemetry_write_errors_total` | `JSONLRecorder.WriteErrors()` | Counter; write/flush error events in the JSONL background loop (issue #795). Distinct from `nexus_telemetry_dropped_total`: operators can distinguish disk/filesystem trouble from buffer back-pressure. |

## Response headers (`X-Nexus-Route-*`)

Every `/v1/chat/completions` response carries four routing-decision
headers (set in `internal/handlers/chat.go`, issue #74) so clients and
intermediate proxies can reason about routing without scraping logs:

| Header | Source | Example |
|--------|--------|---------|
| `X-Nexus-Route` | `decision.Route` | `local`, `frontier`, `fusion` |
| `X-Nexus-Route-Source` | `decision.Source` | `guardrail`, `dsl`, `slm`, `slm-error`, `escalation` |
| `X-Nexus-Route-Reason` | `decision.Reason` | short reason string; may echo SLM error text |
| `X-Nexus-Route-Confidence` | `formatConfidence(decision.Confidence)` | `0.85` |

Each value passes through `SanitizeHeaderValue`
(`internal/handlers/sanitize.go`), which strips CR/LF (header
injection prevention), collapses other control characters to spaces,
trims whitespace, and caps the value at **`MaxHeaderValue` = 128
runes**. Values that exceed 128 runes after cleaning are truncated and
a trailing **`...(+N)`** marker is appended, where *N* is the count of
dropped runes (issue #494) — for example a 200-rune reason becomes the
first 128 runes followed by `...(+72)`. The marker is consistent with
the `TruncateForDebug` precedent in `debug.go`. Clean values and values
exactly 128 runes long are returned unchanged (no false positive at the
boundary). The marker adds at most a few bytes, keeping total header
value length well under HTTP sane bounds.

## Distributed tracing (`internal/tracing`)

The OTLP/JSON exporter (#41) buffers spans and POSTs them as a single
batch to the configured collector endpoint. Two counters expose
distinct modes of silent span loss so operators can tell buffer
pressure apart from collector trouble:

| Metric | Backing source | Meaning |
|--------|----------------|---------|
| `nexus_tracing_dropped_total` | `Exporter.Dropped()` | Per-span count of spans shed at `Submit` time because the in-memory buffer was full (back-pressure). |
| `nexus_tracing_flush_failures_total` | `Exporter.FlushFailures()` | Per-batch count of flushes that failed to POST (HTTP 4xx/5xx, timeout, or transport error). Each failure drops up to 64 spans (#484). |

**Distinguishing the two:** `nexus_tracing_dropped_total` rising with
`nexus_tracing_flush_failures_total` flat indicates the proxy is
producing spans faster than the background loop drains them (raise
`NEXUS_TRACING_QUEUE_SIZE`). `nexus_tracing_flush_failures_total`
rising on its own indicates the collector is unreachable or rejecting
batches (check `NEXUS_TRACING_ENDPOINT`, collector health, network);
the proxy will not retry — the dropped batch is gone. A flat
`nexus_tracing_dropped_total` while traces silently disappear is the
exact symptom #484 fixed: flush failures previously had no metric.

The span/metric attribute pairing is:

| Span attribute | Prometheus metric/label | Notes |
|----------------|------------------------|-------|
| `nexus.route` | `nexus_route_decisions_total{route}` | Route chosen |
| `nexus.source` | `nexus_route_decisions_total{source}` | Decision source |
| `nexus.confidence_bucket` | `nexus_slm_decisions_total{confidence_bucket}` | SLM confidence |
| `nexus.task_type` | `nexus_slm_decisions_total{task_type}` | Task category |
| `nexus.rejection_reason` | `nexus_requests_rejected_total{reason}` | Rejection reason |

Span attributes use the same values as the Prometheus labels so
cross-referencing is trivial.

## OTLP metrics export (`internal/observability`)

The OTLP/JSON metrics exporter (`internal/observability/otel_metrics.go`,
issue #1238) POSTs metric snapshots to the configured collector endpoint
on a fixed interval. When the collector is unreachable or returns an error,
the failure is recorded so operators can detect when their observability
pipeline itself is broken:

| Metric | Backing source | Meaning |
|--------|----------------|---------|
| `nexus_otel_metrics_export_failures_total` | `OtelMetricsExporter.ExportFailures()` | Cumulative count of export batches that failed to POST (HTTP 4xx/5xx, timeout, or transport error). Each failure means one periodic export cycle dropped its payload — the proxy continues operating without the observability data. |

**Distinguishing from tracing:** `nexus_tracing_flush_failures_total` counts
trace batch POST failures; `nexus_otel_metrics_export_failures_total` counts
metrics export failures. The two are independent pipelines and have independent
failure signals.

## Concurrency / VRAM

The VRAM-aware local-route concurrency limiter (`internal/concurrencylimit`,
issue #81) shrinks its effective slot count dynamically from the latest
probe snapshot. Two gauges (issue #487) let operators see whether
requests are saturating the local path and how low the ceiling dropped:

| Metric | Type | Backing source | Meaning |
|--------|------|----------------|---------|
| `nexus_local_concurrency_effective_slots` | gauge | `Limiter.Effective()` | Current slot count the limiter honours: `min(Ceiling, freeVRAM / BytesPerSlot)`. 0 when the limiter is disabled (`NEXUS_LOCAL_MAX_CONCURRENT<=0`). |
| `nexus_local_concurrency_in_flight` | gauge | `Limiter.InFlight()` | Number of currently held slots. Equals `effective_slots` under saturation. |

Both gauges are unlabelled (cardinality 1 each) and are wired as
`GaugeProvider` closures in `cmd/nexus/main.go`, reading from the
concrete `*concurrencylimit.Limiter` instance. When the limiter is
disabled the provider returns `nil` so neither series appears in a
fresh scrape.

## SLM cache fill ratio (issue #531)

The SLM decision cache (`internal/router/slm_cache.go`, issue #206)
holds prompt → route mappings for the configured TTL window to reduce
SLM call frequency for duplicate prompts. Without a live entry count,
operators cannot see cache pressure building until LRU evictions appear.
Two gauges (issue #531) let operators chart the fill ratio
`nexus_slm_cache_entries / nexus_slm_cache_max_entries` and raise
`NEXUS_SLM_CACHE_MAX_ENTRIES` before LRU churn degrades cache
effectiveness:

| Metric | Type | Backing source | Meaning |
|--------|------|----------------|---------|
| `nexus_slm_cache_entries` | gauge | `SLMCache.Len()` | Current number of entries in the cache (including expired entries not yet evicted). |
| `nexus_slm_cache_max_entries` | gauge | `SLMCache.MaxEntries()` | Configured maximum entry capacity. |
| `nexus_slm_cache_stale_entries` | gauge | `SLMCache.StaleEntries()` | Number of entries that have passed their TTL but have not yet been evicted (issue #801). |

Both gauges are unlabelled (cardinality 1 each) and are wired as
`GaugeProvider` closures in `cmd/nexus/main.go`, reading from the
concrete `*router.SLMCache` instance. When the cache is disabled
(`NEXUS_SLM_CACHE_TTL=0`) the provider returns `nil` so neither
series appears in a fresh scrape. Operators can compute the fill ratio
directly in PromQL: `nexus_slm_cache_entries / nexus_slm_cache_max_entries`.

## Local-route cooldown (issue #530)

The local-route cooldown circuit (`internal/circuit/cooldown.go`, issue #80)
arms a short window after the cascade detects an Ollama failure, skipping the
local arm on subsequent `route=local` and `route=fusion` requests until the
window expires. Without it, every subsequent request pays the full upstream
timeout before falling back — a window of repeated slow local attempts.

Two Prometheus signals (issue #530) let operators observe the cooldown
arm/fire cycle directly from `/metrics` without correlating logs:

| Metric | Type | Backing source | Meaning |
|--------|------|----------------|---------|
| `nexus_local_cooldown_active` | gauge | `circuit.Cooldown.Active()` | `1` when the cooldown window is active; `0` otherwise. Absent from `/metrics` when the cooldown is disabled (`NEXUS_LOCAL_COOLDOWN<=0`). |
| `nexus_local_cooldown_triggers_total` | counter | `routeCounters.IncLocalCooldownTriggers()` | Cumulative cooldown arm events — each increment corresponds to one cascade failure that armed the cooldown. |

The gauge is wired as a `GaugeProvider` closure in `cmd/nexus/main.go`,
reading `localCooldown.Active()` at scrape time. The counter is incremented
via a failure observer callback (`circuit.Cooldown.SetFailureObserver`)
that forwards to `routeCounters.IncLocalCooldownTriggers()` — the circuit
package does not import observability, preserving the existing dependency
direction.

The `/status` endpoint also surfaces a `local_cooldown` sub-object:

```json
{
  "local_cooldown": {
    "enabled": true,
    "active": true,
    "expires_at": "2024-01-01T00:00:10Z"
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `enabled` | bool | Whether the cooldown circuit is wired (`NEXUS_LOCAL_COOLDOWN > 0`) |
| `active` | bool | Whether the cooldown window is currently in effect |
| `expires_at` | time | Wall-clock time when the active window ends (zero if inactive or disabled) |

## Per-route latency percentiles (issue #774, #1051)

Three gauge families expose request-latency percentiles per route,
computed from a sliding-window ring buffer of recent samples (capacity
1000). The ring buffer gives exact percentile values from actual
observations — operators can set precise SLO alerts (e.g. `p95 < 2s`)
without client-side estimation.

Values are expressed in **seconds** (Prometheus convention for latency).

| Metric | Type | Labels | Source |
|--------|------|--------|--------|
| `nexus_upstream_request_latency_p50_seconds` | gauge | `route` | `latencyPercentileBuffer.Perc()` (issue #774) |
| `nexus_upstream_request_latency_p95_seconds` | gauge | `route` | `latencyPercentileBuffer.Perc()` (issue #774) |
| `nexus_upstream_request_latency_p99_seconds` | gauge | `route` | `latencyPercentileBuffer.Perc()` (issue #1051) |

`route` label values: `local`, `frontier`, `fusion`.

p99 is critical for SLA monitoring: p95 misses the tail outliers that
cause user-visible issues. With p99, operators can alert on the latency
that only 1% of requests exceed.

## SLO error budget tracking (issue #1239)

The proxy exposes an in-process gauge that tracks the remaining error
budget fraction for each defined SLO. The gauge is computed from the
in-process percentile ring buffers at scrape time and is labelled by
SLO name so operators can chart budget consumption in Grafana and wire
alerts on budget exhaustion.

| Metric | Type | Labels | Source |
|--------|------|--------|--------|
| `nexus_slo_error_budget_remaining` | gauge | `slo` | `Collector.SLOErrorBudgetGauges()` (issue #1239) |

`slo` label values: `availability`, `local_latency_p99`, `ttft_p95`.

Values are in **[0, 1]**: 1 = full budget, 0 = exhausted.

### SLO definitions

| SLO | Target | Threshold | Error Budget | Meaning |
|-----|--------|-----------|-------------|---------|
| `availability` | 99.9% | 0.1% error rate | 0.1% of requests over 30d | Fraction of successful requests (no cascade fallback, no 5xx) |
| `local_latency_p99` | 99% | < 2s | 1% of requests over 30d | p99 of local-route (Ollama) request latency |
| `ttft_p95` | 95% | < 500ms | 5% of requests over 30d | p95 of time-to-first-token across all routes |

### Multi-window burn-rate alerts

The repo ships multi-window multi-burn-rate alerting rules in
`deploy/prometheus/alerts.yaml` (group `nexus-slo-burn-rate`) and
recording rules in `deploy/prometheus/recording-rules.yaml`. See
`deploy/prometheus/slos.yaml` for the full SLO contract.

**Alert severity levels:**

| Alert | Severity | Condition |
|-------|----------|-----------|
| `NexusSLOAvailabilityBurnRatePage` | critical | Error rate > 14.4× budget burn rate (5m window) |
| `NexusSLOAvailabilityBurnRatePageLong` | critical | Error rate > 14.4× budget burn rate (30m window) |
| `NexusSLOAvailabilityBurnRateTicket` | warning | Error rate > 6× budget burn rate (1h window) |
| `NexusSLOAvailabilityBurnRateTicketLong` | warning | Error rate > 6× budget burn rate (6h window) |
| `NexusSLOLatencyP99BurnRatePage` | critical | Local p99 > 2s, burning at > 14.4× rate |
| `NexusSLOLatencyP99BurnRateTicket` | warning | Local p99 > 2s, burning at > 6× rate |
| `NexusSLOTTFTP95BurnRatePage` | critical | TTFT p95 > 500ms, burning at > 14.4× rate |
| `NexusSLOTTFTP95BurnRateTicket` | warning | TTFT p95 > 500ms, burning at > 6× rate |
| `NexusSLOErrorBudgetExhausted` | critical | `nexus_slo_error_budget_remaining < 0.10` for 5m |
| `NexusSLOErrorBudgetLow` | warning | `nexus_slo_error_budget_remaining < 0.50` for 15m |

**Recording rules:**

| Rule | Meaning |
|------|---------|
| `nexus:error_budget_remaining_ratio` | Remaining availability error budget fraction over 30d |
| `nexus:slo_compliance_30d` | Overall SLO compliance fraction over 30d |
| `nexus:local_p99_breach_fraction_30d` | Fraction of time local p99 was above 2s over 30d |

## Observer wiring

The chat handler never imports `internal/observability` directly
(AGENTS.md dependency rule). Instead, observer function-typed hooks
on `handlers.Deps` are plugged in `cmd/nexus/main.go`:

| Hook | Observer | Wired in |
|------|----------|----------|
| `RouteDecisionObserver` | `RouteDecisionObserverFunc` → `routeCounters.Observe()` | `main.go:492-494` |
| `RejectionObserver` | `RejectionObserverFunc` → `routeCounters.ObserveRejection()` | `main.go:501-503` |
| Rate-limit rejection | `rateLimiter.SetRejectionHook()` → `routeCounters.ObserveRejection()` | `main.go:537-539` |
| Judge drop callback | `eval.SetDropCallback()` → `routeCounters.ObserveJudgeDrop()` | `main.go:498-500` |

## Endpoint exempt from auth

`/metrics` is exempt from inbound auth and rate limiting alongside
`/healthz`, `/livez`, `/readyz`, and `/status`.

## `GET /status` — extended health with judge state

Returns the same JSON shape as `/healthz` (frontier, probe, Ollama
health) **plus** a `judge` sub-object (always present):

```json
{
  "version": "v1.2.3",
  "frontier": { ... },
  "judge": {
    "enabled": true,
    "queue_depth": 64,
    "dropped": 3,
    "concurrency": 2
  },
  "probe": { ... },
  "ollama": { ... }
}
```

Top-level fields:

| Field | Type | Description |
|-------|------|-------------|
| `version` | string | Build version string injected via `-ldflags "-X main.version=..."` at compile time (issue #529); `"dev"` when built without ldflags. |

## Runtime profiling (`/debug/pprof/*`, `/debug/vars`) (issue #1150)

When `NEXUS_DEBUG_PPROF_ENABLED=true`, the proxy registers the standard
`net/http/pprof` and `expvar` handlers under `/debug/`:

| Endpoint | Description |
|----------|-------------|
| `/debug/pprof/` | Index page listing available profiles |
| `/debug/pprof/heap` | Heap allocation profile |
| `/debug/pprof/goroutine` | Goroutine stack dump |
| `/debug/pprof/profile` | CPU profile (30s default) |
| `/debug/pprof/trace` | Execution trace |
| `/debug/pprof/{allocs,block,mutex,threadcreate}` | Other runtime profiles |
| `/debug/pprof/{cmdline,symbol}` | Build info + symbol resolution |
| `/debug/vars` | Published `expvar` variables (memstats, cmdline) |

### Access control

| Mode | Config | Behaviour |
|------|--------|-----------|
| Disabled (default) | `NEXUS_DEBUG_PPROF_ENABLED=false` | `/debug/*` returns 404 |
| Loopback-only | `PPROF_ENABLED=true`, key empty | Only `127.0.0.1`/`::1` served; others get 403 |
| API-key gated | `PPROF_ENABLED=true`, key set | Requires `Authorization: Bearer <key>`; others get 401 |

The `/debug/` subtree is exempt from the main inbound auth gate
(`NEXUS_PROXY_API_KEY`) because it carries its own independent gate.

`nexus check` reports the exposure mode in the `pprof_endpoint` line.
