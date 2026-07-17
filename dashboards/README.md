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

### Variables

The dashboard declares three template variables:

| Variable | Type | Query | Purpose |
|----------|------|-------|---------|
| `datasource` | Datasource | `prometheus` | Prometheus datasource selector |
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

### Customization

The dashboard uses a restrained color palette aligned with Grafana's default
theme. To adapt to a dark/light preference, use **Dashboard Settings →
General → Toggle Dark/Light Theme** in the Grafana UI. All panel colors are
set via palette-classic or threshold modes and adapt automatically.

For multi-deployment setups, use the `instance` variable dropdown to switch
between Prometheus targets. If you run a single instance, leave `instance`
on **All** — all panels filter correctly by default.
