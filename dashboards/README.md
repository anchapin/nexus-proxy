# Grafana Dashboards

This directory contains pre-built Grafana dashboard JSON exports for Nexus Proxy
observability.

## Dashboard: Nexus Proxy — Metrics Dashboard

**File:** `nexus-proxy.json`

Importable into Grafana via `grafana dashboard import`. Provides four rows of
panels covering overview, routing, RAG, and infrastructure metrics.

### Rows

| Row | Panels |
|-----|--------|
| **Row 1 — Overview** | Total requests, TOON cost savings %, average TTFT, request rate by route |
| **Row 2 — Routing** | Route decision pie chart, SLM cache hit rate, SLM low-confidence escalations by task type |
| **Row 3 — RAG** | RAG retrieval latency, RAG hit/miss rate pie, top 10 retrieved filenames |
| **Row 4 — Infrastructure** | VRAM usage, circuit breaker state, embedder/Ollama failures |
| **Row 5 — Traces** | Trace span timeline, span duration by stage, error traces rate (per 5m) |

### Variables

The dashboard declares three template variables:

| Variable | Type | Query | Purpose |
|----------|------|-------|---------|
| `datasource` | Datasource | `prometheus` | Prometheus datasource selector |
| `tempo` | Datasource | `tempo` | Tempo (OTLP) datasource selector for trace panels |
| `instance` | Query (multi) | `label_values(nexus_requests_total, instance)` | Filter by deployment instance |
| `route` | Query (multi) | `label_values(nexus_requests_total, route)` | Filter by route (`local`, `frontier`, `fusion`) |

Both `instance` and `route` default to **All**, so the dashboard is
functional without any variable selection.

### Importing

**Via Grafana UI:**

1. Navigate to **Dashboards → Import**.
2. Upload `nexus-proxy.json` or paste its contents.
3. Select your Prometheus datasource.
4. Click **Import**.

**Via `grafana-cli`:**

```bash
grafana-cli dashboards import /path/to/nexus-proxy.json
```

**Via `curl` and the Grafana API:**

```bash
curl -X POST \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $GRAFANA_API_KEY" \
  --data @nexus-proxy.json \
  https://your-grafana-host/api/dashboards/db
```

### Required Metrics

All panels use metrics from the `/metrics` endpoint. No additional exporters
or instrumentation are required. Verify your Prometheus scraper is pointed at
`http://your-nexus-instance:8080/metrics` (or the port configured via
`NEXUS_HTTP_PORT`).

### Required Traces

Row 5 panels use Grafana's built-in **Tempo** datasource (traceql queries).
No additional exporters are required — the OTLP endpoint is already configured
via `NEXUS_TRACING_ENDPOINT`. Tempo must be configured in Grafana with an
OTLP receiver pointing to your `NEXUS_TRACING_ENDPOINT`.

#### TraceQL query reference

| Panel | Query | Description |
|-------|-------|-------------|
| Trace Span Timeline | `{}` | All recent traces — opens trace list with timeline view |
| Span Duration by Stage | `{span.name} \| avg(duration) by (span.name)` | Per-span-name average latency |
| Error Traces Rate | `{span.status = STATUS_CODE_ERROR}` | Count of errored spans per 5m window |

#### Tempo datasource setup

1. In Grafana, go to **Connections → Data Sources → Add data source**.
2. Select **Tempo**.
3. Set the URL to your Tempo OTLP receiver address
   (e.g. `http://your-tempo-instance:4317` for gRPC or `:4318` for HTTP).
4. Set **TraceID** as the primary lookup field.
5. Click **Save & Test**.

Row 5 will not render until a Tempo datasource is configured and `${tempo}` is
set to point to it.

### Cost Savings Calculation

The **TOON cost savings %** panel computes:

```
100 * (nexus_toon_savings_tokens_total / (nexus_input_tokens_total + nexus_output_tokens_total))
```

This reflects tokens saved by TOON compression relative to total tokens
processed. It is a proxy for cost reduction; actual cost savings depend on
your frontier provider's token pricing.

### Alerting

Example Grafana alert rules you may want to add:

| Alert | Condition | Severity |
|-------|-----------|----------|
| High circuit breaker failure rate | `rate(nexus_circuit_breaker_failures_total[5m]) > 0.1` | warning |
| Low SLM cache hit rate | `nexus_slm_cache_hits_total / (nexus_slm_cache_hits_total + nexus_slm_cache_misses_total) < 0.5` | info |
| High RAG embed error rate | `rate(nexus_rag_retrieval_total{hit="false",reason="embed_error"}[5m]) > 0.05` | warning |
| VRAM exhaustion | `nexus_vram_free_bytes < 500_000_000` (500 MB) | critical |
| High error trace rate | `count_over_time({span.status = STATUS_CODE_ERROR}[5m]) > 10` | warning |

### Customization

The dashboard uses a restrained color palette aligned with Grafana's default
theme. To adapt to a dark/light preference, use **Dashboard Settings →
General → Toggle Dark/Light Theme** in the Grafana UI. All panel colors are
set via palette-classic or threshold modes and adapt automatically.

For multi-deployment setups, use the `instance` variable dropdown to switch
between Prometheus targets. If you run a single instance, leave `instance`
on **All** — all panels filter correctly by default.
