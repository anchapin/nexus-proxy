# Alerting

nexus-proxy ships production-ready Prometheus alerting and recording
rules so an operator can go from `git clone` to a paging Alertmanager
without hand-authoring a single PromQL expression. The rules consume
the existing `/metrics` exposition (see
[observability-surface.md](observability-surface.md)) — **no new env
vars or config is required on the proxy side**.

## Files

| File | Contents |
|------|----------|
| [`../deploy/prometheus/recording-rules.yaml`](../deploy/prometheus/recording-rules.yaml) | 5 precomputed series (error rate, p99 latency, budget utilization, cooldown fraction, cascade fallback rate) |
| [`../deploy/prometheus/alerts.yaml`](../deploy/prometheus/alerts.yaml) | 9 alerting rules covering budget, circuit breakers, error rate, trace loss, cooldown, and the p99 latency SLO |

## Load path

1. **Copy** the two rule files to your Prometheus host (or mount them
   via your config-management system):

   ```sh
   cp deploy/prometheus/recording-rules.yaml /etc/prometheus/nexus-recording-rules.yaml
   cp deploy/prometheus/alerts.yaml           /etc/prometheus/nexus-alerts.yaml
   ```

2. **Reference** them in `prometheus.yml`:

   ```yaml
   rule_files:
     - /etc/prometheus/nexus-recording-rules.yaml
     - /etc/prometheus/nexus-alerts.yaml

   scrape_configs:
     - job_name: nexus-proxy
       metrics_path: /metrics
       static_configs:
         - targets: ["nexus-proxy:8080"]
   ```

   `/metrics` is exempt from auth and rate limiting, so no credentials
   are needed in the scrape config.

3. **Validate** before reloading Prometheus:

   ```sh
   make check-rules
   # equivalent to:
   promtool check rules deploy/prometheus/*.yaml
   ```

   `make check-rules` auto-detects `promtool` on `PATH`; if it is absent
   it prints install instructions and exits non-zero so CI fails loudly.

4. **Reload** Prometheus (send `SIGHUP` or `POST /-/reload` if the
   `--web.enable-lifecycle` flag is set). Confirm the rules loaded:

   ```sh
   curl -s http://prometheus:9090/api/v1/rules | jq '.data.groups[].name'
   # → ["nexus-recording.rules", "nexus-alerts"]
   ```

## Required operator edit

`recording-rules.yaml` computes `nexus:budget_utilization_ratio` by
dividing `nexus_budget_spend_usd` by a **constant**. The proxy exposes
the rolling 24h spend as a metric but does **not** expose the configured
daily cap (it is a static config value, not a runtime signal). Before
relying on the `NexusBudgetApproaching` alert, edit the divisor in
`recording-rules.yaml` to match your `NEXUS_BUDGET_ALERT_THRESHOLD`:

```yaml
- record: nexus:budget_utilization_ratio
  expr: nexus_budget_spend_usd / 10.0   # ← edit this to your cap (USD)
```

The default `10.0` is an intentionally low placeholder so a forgotten
edit surfaces as a noisy-but-harmless alert rather than a silent gap.

## Alertmanager routing

Every rule carries a `severity` label (`critical` or `warning`) and a
`component` label. A minimal Alertmanager route that pages on critical
and posts warnings to Slack:

```yaml
route:
  receiver: default
  routes:
    - matchers: ['severity="critical"']
      receiver: pagerduty
    - matchers: ['severity="warning"']
      receiver: slack
receivers:
  - name: pagerduty
    pagerduty_configs:
      - routing_key: "<key>"
  - name: slack
    slack_configs:
      - api_url: "<webhook>"
  - name: default
    slack_configs:
      - api_url: "<webhook>"
```

## Alert reference

| Alert | Severity | Component | Trigger | Impact |
|-------|----------|-----------|---------|--------|
| `NexusBudgetExceeded` | warning | budget | `increase(nexus_budget_exceeded_total[5m]) > 0` | Frontier requests are being rejected; spend at cap |
| `NexusBudgetApproaching` | warning | budget | `nexus:budget_utilization_ratio > 0.8 for 10m` | Spend within 20% of cap; rejection imminent |
| `NexusCircuitBreakerOpen` | critical | circuit-breaker | `nexus_circuit_breaker_state{circuit="ollama"} == 2 for 5m` | All local/fusion traffic rerouted to paid frontier |
| `NexusRAGCircuitOpen` | warning | circuit-breaker | `nexus_rag_circuit_state == 2 for 5m` | RAG context injection disabled; answers degrade silently |
| `NexusHighErrorRate` | critical | routing | `nexus:error_rate5m > 0.10 for 5m` | >10% of a route's requests failing over to frontier |
| `NexusTraceDropsSpiking` | warning | tracing | `rate(nexus_tracing_dropped_total[5m]) > 1 for 5m` | Trace queue saturated; spans shed (back-pressure) |
| `NexusTraceFlushFailures` | warning | tracing | `increase(nexus_tracing_flush_failures_total[15m]) > 3` | OTLP collector unreachable; batches permanently lost |
| `NexusLocalCooldownStuck` | warning | circuit-breaker | `nexus_local_cooldown_active == 1 for 30m` | Local arm bypassed for 30m (misconfig or flapping Ollama) |
| `NexusSLOp99Breach` | warning | slo | `nexus_upstream_request_latency_p99_seconds{route="local"} > 2 for 10m` | Local p99 exceeds the 2s SLO; latency advantage lost |

## Remediation runbook

### NexusBudgetExceeded / NexusBudgetApproaching
- **Symptom:** Frontier requests are being (or about to be) rejected by
  the daily budget gate.
- **Quick fix:** Raise `NEXUS_BUDGET_ALERT_THRESHOLD` (hot-reloadable via
  `SIGHUP`) to admit more spend.
- **Root cause:** Sustained traffic routed to the frontier — check
  whether the Ollama circuit breaker is open (rerouting everything to
  frontier) or the SLM is over-escalating (`nexus_slm_low_confidence_escalations_total`).
- **Long term:** Investigate why local routing fell below expectations.

### NexusCircuitBreakerOpen
- **Symptom:** Ollama health breaker is open; `X-Nexus-Degraded: true`
  on responses.
- **Quick fix:** Verify Ollama is reachable
  (`curl $NEXUS_OLLAMA_BASE_URL/api/tags`) and responsive.
- **Root cause:** `NEXUS_HEALTH_BREAKER_THRESHOLD` (default 3) failed
  probes in a row. Check Ollama logs for OOM, model load failures, or
  VRAM exhaustion.
- **Recovery:** Automatic — the breaker recloses on the next successful
  health probe (`NEXUS_HEALTH_POLL_INTERVAL`).

### NexusRAGCircuitOpen
- **Symptom:** RAG retrieval skipped; answers lack injected context.
- **Quick fix:** Verify the embedder endpoint
  (`NEXUS_EMBEDDER_TYPE` / `NEXUS_RAG_EMBEDDER_URL`).
- **Recovery:** Automatic — the breaker recloses after the first
  successful embed once `NEXUS_RAG_CIRCUIT_BREAKER_THRESHOLD` failures
  reset.

### NexusHighErrorRate
- **Symptom:** >10% of a route's requests cascading to frontier.
- **Diagnosis:** Break down by reason —
  `sum by(reason)(rate(nexus_cascade_fallback_total[5m]))`:
  - `timeout` → Ollama too slow; raise cascade timeout or shrink model.
  - `transport_error` → Ollama unreachable; network/DNS/TLS.
  - `http_error` → Ollama returning 5xx; overloaded or buggy.
  - `malformed_response` → upstream returned invalid JSON / empty choices.

### NexusTraceDropsSpiking
- **Symptom:** Trace spans shed at Submit time (queue back-pressure).
- **Distinguish** from flush failures: if
  `nexus_tracing_flush_failures_total` is also rising, the collector is
  the bottleneck (see `NexusTraceFlushFailures`). If it is flat, the
  proxy is producing spans faster than it can drain.
- **Fix:** Raise `NEXUS_TRACING_QUEUE_SIZE`, increase
  `NEXUS_TRACING_BATCH_SIZE`, or reduce `NEXUS_TRACING_SAMPLE_RATE`.

### NexusTraceFlushFailures
- **Symptom:** OTLP collector rejecting/unreachable; batches lost
  permanently (no retry).
- **Fix:** Check `NEXUS_TRACING_ENDPOINT` reachability and collector
  health. Verify `NEXUS_TRACING_TIMEOUT` is not shorter than the
  collector's response time.

### NexusLocalCooldownStuck
- **Symptom:** Cooldown armed for 30m; local arm bypassed.
- **Diagnosis:**
  - If `nexus_local_cooldown_triggers_total` is climbing: Ollama is
    flapping (repeated failures re-arming the cooldown). Fix Ollama.
  - If the trigger counter is flat: `NEXUS_LOCAL_COOLDOWN` is
    misconfigured to an excessively long window (should be seconds, not
    minutes).

### NexusSLOp99Breach
- **Symptom:** Local p99 latency above the 2s SLO.
- **Diagnosis:** Check Ollama saturation
  (`nexus_local_concurrency_in_flight` vs
  `nexus_local_concurrency_effective_slots`), model size vs available
  VRAM, and whether a larger model was recently loaded. A cold model
  load will spike p99 transiently and clear within the `for` window.
