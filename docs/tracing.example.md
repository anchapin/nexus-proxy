# Tracing Configuration Examples

This document provides operator guidance for configuring the OTLP/JSON trace
exporter (`NEXUS_TRACING_ENDPOINT`) with common backends: **Jaeger** and
**Grafana Tempo**. It covers endpoint values, collector/receiver setup, sampling
strategy, and Grafana query examples.

> **Prerequisite**: `NEXUS_TRACING_ENDPOINT` must be set. When empty (the
> default), the tracing subsystem is disabled — no goroutines, no allocations
> beyond a single boolean check per request.

---

## 1. Span Surface

Every inbound chat completions request produces a **root span** named
`nexus.chat_completions`. Child spans are opened for each phase:

| Span name | Phase |
|-----------|-------|
| `nexus.chat_completions` | Root — request lifetime |
| `auth` | Bearer-token validation |
| `rate_limit` | Per-client-IP rate check |
| `security_headers` | Request sanitisation |
| `prompt_engineering` | CoT / role / constraints injection |
| `rag_retrieve` | Vector store lookup |
| `toon_compression` | JSON array → CSV rewrite |
| `guardrail` | Token-budget VRAM check |
| `dsl` | Pattern regex match |
| `slm_decide` | qwen3-coder JSON decision |
| `upstream_frontier` | OpenAI-compatible POST to frontier API |
| `upstream_local` | Ollama POST |
| `fusion_panel` | Parallel local + frontier race |
| `fusion_arbiter` | Jaccard-similarity synthesis |

### Span attributes

These attributes appear on the root span and enable cross-referencing with
Prometheus metrics:

| Attribute | Prometheus label | Notes |
|-----------|-----------------|-------|
| `nexus.route` | `nexus_route_decisions_total{route}` | `local` / `frontier` / `fusion` |
| `nexus.source` | `nexus_route_decisions_total{source}` | `guardrail` / `dsl` / `slm` / `slm-error` / `escalation` |
| `nexus.confidence_bucket` | `nexus_slm_decisions_total{confidence_bucket}` | `none` / `low` / `medium` / `high` |
| `nexus.task_type` | `nexus_slm_decisions_total{task_type}` | `css` / `refactoring` / `debugging` / … |
| `nexus.rejection_reason` | `nexus_requests_rejected_total{reason}` | `method` / `body_too_large` / `bad_request` / `rate_limit` |
| `http.status_code` | — | HTTP response status |

### W3C Trace Context propagation

Outbound upstream calls (Ollama, frontier API) carry the `traceparent` header
so spans form a single distributed trace across the proxy + LLM provider.
Inbound `traceparent` headers are honoured — an upstream caller can extend
its own trace tree across the proxy.

---

## 2. Jaeger

### 2.1 `NEXUS_TRACING_ENDPOINT`

```
NEXUS_TRACING_ENDPOINT=http://jaeger:4318/v1/traces
```

> **Note**: Jaeger's OTLP HTTP receiver listens on port **4318** by default.
> The `/v1/traces` path is required — the exporter POSTs an OTLP/JSON body.

### 2.2 Jaeger Collector / Agent receiver config

If you run the Jaeger **all-in-one** container (agent + collector + query in
one pod), no additional config is needed — it binds the OTLP HTTP receiver
on port 4318 out of the box:

```yaml
# docker-compose excerpt (jaeger-all-in-one)
services:
  jaeger:
    image: jaegertracing/all-in-one:1.62
    environment:
      COLLECTOR_OTLP_ENABLED: true    # enables port 4318
    ports:
      - "4318:4318"   # OTLP HTTP receiver
      - "16686:16686" # Jaeger UI
```

If you run the **distributed** stack (Agent → Collector → Storage), add the
OTLP receiver to the Collector:

```yaml
# jaeger-collector-config.yaml
apiVersion: jaegertracing.io/v1
kind: JaegerCollector
metadata:
  name: nexus-collector
spec:
  replicas: 1
  ports:
    - port: 4318
      protocol: HTTP
      adapter: otlp   # tells the collector to expect OTLP/JSON
```

Or, if you use the **Jaeger Operator** on Kubernetes:

```yaml
apiVersion: jaegertracing.io/v1
kind: Jaeger
metadata:
  name: nexus
spec:
  collector:
    ports:
      - port: 4318
        protocol: http
        daemonset:
          - name: otlp
            containerPort: 4318
            protocol: http
```

### 2.3 Grafana dashboard query (Jaeger)

Navigate to **Explore → Jaeger** in Grafana and filter by service name:

```traceql
service.name = "nexus-proxy"
```

To find slow traces routed to the frontier:

```traceql
service.name = "nexus-proxy"
and resource.attributes["nexus.route"] = "frontier"
| rate(duration, 1s)
```

To find errors:

```traceql
service.name = "nexus-proxy"
and resource.attributes["nexus.source"] = "slm-error"
```

---

## 3. Grafana Tempo

### 3.1 `NEXUS_TRACING_ENDPOINT`

```
NEXUS_TRACING_ENDPOINT=http://tempo:4318/v1/traces
```

> **Note**: Grafana Tempo's OTLP HTTP receiver listens on port **4318** by
> default (the same port as Jaeger — the `/v1/traces` path distinguishes them).

### 3.2 Tempo receiver config

Tempo ships a **single binary** with built-in receivers. Enable the OTLP HTTP
receiver in the config file:

```yaml
# tempo.yaml
server:
  http_listen_port: 3200   # Tempo query UI

distributor:
  receivers:
    otlp:
      protocols:
        http:               # port 4318
          endpoint: 0.0.0.0:4318

storage:
  trace:
    backend: local
    local:
      path: /var/tempo/traces
```

Start Tempo:

```bash
docker run -d \
  --name tempo \
  -p 4318:4318   \
  -p 3200:3200   \
  -v $(pwd)/tempo.yaml:/etc/tempo.yaml \
  grafana/tempo:2.6 \
  -config.file=/etc/tempo.yaml
```

Point Grafana at Tempo: **Settings → Data Sources → Add → Tempo → URL**
`http://tempo:3200`.

### 3.3 Grafana dashboard query (Tempo)

Use the **TraceQL** tab in Grafana Explore:

Find all nexus-proxy spans:

```traceql
service.name = "nexus-proxy"
```

Group by route and sum latency:

```traceql
service.name = "nexus-proxy"
| count(duration) by (resource.attributes["nexus.route"])
```

Find the 95th-percentile latency per task type:

```traceql
service.name = "nexus-proxy"
| quantile_over_time(0.95, duration) by (resource.attributes["nexus.task_type"])
```

Find high-confidence SLM decisions that escalated to frontier:

```traceql
service.name = "nexus-proxy"
and resource.attributes["nexus.source"] = "escalation"
| rate(duration, 1m)
```

---

## 4. Sampling Strategies

The exporter supports three samplers, selected by `NEXUS_TRACING_SAMPLE_RATE`:

| Rate | Sampler | Use case |
|------|---------|---------|
| `0` | `NeverSample` | Tracing fully disabled |
| `0 < rate < 1` | `ProbabilitySampler` | Representative sampling |
| `1` (default) | `AlwaysSample` | Full sampling |

### 4.1 Probability / head-based sampling

Probability sampling is **deterministic** — the decision is based on a hash of
the trace ID, so the same trace is always (or never) sampled across all
process replicas. This is important for distributed correlation: you either
see the whole trace or none of it.

```bash
# Sample 10 % of traces (deterministic — same 10 % across all replicas)
NEXUS_TRACING_SAMPLE_RATE=0.1
```

### 4.2 Adaptive sampling with Grafana Tempo

For high-volume production traffic, configure Tempo's **tail-based sampling**
to keep 100 % of errors and a configurable percentage of successful traces:

```yaml
# tempo.yaml — add the tail sampling processor
traces:
  tail_sampling:
    policies:
      # Always keep errors
      - type: status_code
        status_code: STATUS_CODE_ERROR
      # Keep 1 % of successful traces
      - type: probabilistic
        probabilistic: 0.01
```

Restart Tempo after editing the config. Tail sampling runs in the Tempo
ingester, after the trace is assembled from individual spans.

### 4.3 Boot-time log

When `NEXUS_TRACING_ENDPOINT` is non-empty, the proxy logs the endpoint on
boot:

```
level=INFO msg="tracing exporter registered" endpoint=http://collector:4318/v1/traces
```

If you do not see this line, the endpoint env var is empty or the exporter
failed to start.

---

## 5. Troubleshooting

### Spans are not appearing in the UI

1. **Check boot log** for `tracing exporter registered` — confirms the
   exporter goroutine started.
2. **Verify network**: the proxy must be able to reach the collector on
   `NEXUS_TRACING_ENDPOINT`. Try:
   ```bash
   curl -sv http://jaeger:4318/v1/traces -X POST \
     -H "Content-Type: application/json" \
     -d '{"resourceSpans":[]}'
   ```
   A `200` or `400` (empty body is valid but rejected) means the collector
   is reachable; a connection refused means a firewall or service-discovery
   issue.
3. **Check `nexus_tracing_dropped_total`** on `/metrics`:
   - Non-zero means the export queue is full (back-pressure from a stalled
     collector). Increase `NEXUS_TRACING_QUEUE_SIZE` or investigate collector
     availability.
4. **Check the queue depth gauge** `nexus_tracing_queue_depth` — if it is
   consistently near `NEXUS_TRACING_QUEUE_SIZE`, the collector is slower than
   the request rate.

### `nexus_tracing_dropped_total` is non-zero

The export buffer is full. Possible causes and fixes:

| Cause | Fix |
|-------|-----|
| Collector is slow or unreachable | Investigate collector health; check network path |
| Burst of traffic exceeding buffer | Increase `NEXUS_TRACING_QUEUE_SIZE` (default 256) |
| `NEXUS_TRACING_TIMEOUT` too small | Increase from the default 10 s if the collector is distant |

### Spans are not linked across services

Ensure `traceparent` propagation is working:
1. Check the outbound request headers from the proxy — the `traceparent`
   header must be present on Ollama and frontier API calls.
2. In the tracing UI, verify that the trace ID is consistent across all
   spans from the proxy and the upstream service.
3. If the upstream service is not recording the propagated traceparent,
   consult that service's tracing documentation for how to configure
   W3C Trace Context support.

---

## 6. Full Example: Docker Compose

A minimal end-to-end stack with Jaeger:

```yaml
# docker-compose.yaml
services:
  nexus:
    image: ghcr.io/anchapin/nexus-proxy:latest
    environment:
      NEXUS_TRACING_ENDPOINT: http://jaeger:4318/v1/traces
      NEXUS_TRACING_SAMPLE_RATE: 0.1
    ports:
      - "8080:8080"

  jaeger:
    image: jaegertracing/all-in-one:1.62
    environment:
      COLLECTOR_OTLP_ENABLED: "true"
    ports:
      - "4318:4318"    # OTLP HTTP
      - "16686:16686"  # UI
```

Start the stack and open `http://localhost:16686` to explore traces.
