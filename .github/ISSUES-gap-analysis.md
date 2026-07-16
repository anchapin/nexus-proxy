# GitHub Issues — Nexus Proxy Gap Analysis

---

## [P1] Auth failure Prometheus counters are dead code

**Problem:** The `Collector` struct exposes `IncAuthRejectedInvalid()` and `IncAuthRejectedMissing()` methods (`internal/observability/collector.go:259-265`), but `internal/auth/auth.go:50` and `internal/auth/auth.go:57` return 401 responses without ever calling them. Auth rejections are completely invisible in the `/metrics` endpoint.

**Proposed Solution:**
- Call ` deps.Observability.IncAuthRejectedInvalid()` in the "invalid token" path at `auth.go:57`
- Call ` deps.Observability.IncAuthRejectedMissing()` in the "missing token" path at `auth.go:50`
- Add `nexus_auth_rejected_total{reason="invalid|missing"}` to the Prometheus metrics definition in `collector.go`
- Add a test in `internal/auth/auth_test.go` verifying the counter increments on each 401 path

**Related:** #37 (inbound auth)

**Acceptance Criteria:**
- [ ] `curl /metrics | grep auth_rejected` shows incrementing counters after each 401 response
- [ ] `nexus check` passes without new warnings
- [ ] `make test-race` passes with new counter wiring

---

## [P1] No brute-force protection on auth endpoint

**Problem:** `internal/auth/auth.go` validates API keys on every request but imposes no per-IP rate limit on failed attempts. An attacker with network access can fire unlimited credential-guessing requests against the proxy with no throttling.

**Proposed Solution:**
- Add an auth-failure rate limiter similar to the existing client-IP rate limiter in `internal/ratelimit/`
- New config env vars: `NEXUS_AUTH_RATE_LIMIT_RPM` (default 5), `NEXUS_AUTH_RATE_LIMIT_BURST` (default 3)
- When auth fails for a given client IP more than `NEXUS_AUTH_RATE_LIMIT_BURST` times within a sliding window, return 429 with `Retry-After` header
- This should be a separate rate limiter from the general request rate limiter, since auth failures need independent tracking

**Dependencies:** Requires the auth failure counter from the issue above

**Acceptance Criteria:**
- [ ] Rapid sequential bad tokens from same IP returns 429 after `NEXUS_AUTH_RATE_LIMIT_BURST` failures
- [ ] `Retry-After` header is set on 429 responses from auth endpoint
- [ ] `nexus_requests_rejected_total{reason="auth_rate_limit"}` counter increments
- [ ] `make test-race` passes

---

## [P1] Cascade and Panel use `context.Background()` instead of request context

**Problem:** `cascade.go:154`, `upstream.go:466,474,745,754,496` create independent `context.Background()` contexts with timeouts instead of deriving from `r.Context()`. When a client disconnects mid-request, the HTTP handler goroutine exits but the cascade/panel goroutines continue running to their timeout, wasting upstream resources and holding connections.

**Proposed Solution:**
- Replace `context.WithTimeout(context.Background(), ...)` with `context.WithTimeout(r.Context(), ...)` in:
  - `cascade.go:154` (`fetchCascadeStep`)
  - `upstream.go:466,474` (panel `localCtx`, `frontierCtx`)
  - `upstream.go:496` (arbiter context)
  - `upstream.go:745,754` (panel streaming contexts)
- Add `IsClientAbort(err error)` checks after SSE writes to short-circuit when the client has gone away
- The existing `upstream.go:171-173` `StreamWithContext` already uses `ctx` passed by caller — verify cascade/panel follow the same pattern

**Related:** #8 (health + circuit breaker)

**Acceptance Criteria:**
- [ ] When a client disconnects during cascade/panel, in-flight upstream calls are cancelled within 1 second
- [ ] No goroutine leak when client aborts mid-request (verified with `go test -race` in a streaming test)
- [ ] `make test-race` passes

---

## [P1] `LocalPatternsRegex` never wired to handler

**Problem:** `internal/router/planner.go:163` declares `LocalPatternsRegex *regexp.Regexp` as a Planner field, and `planner.go:228` uses it in DSL matching. However, `internal/handlers/chat.go:452` (the `Deps` struct) has no `LocalPatternsRegex` field, and `cmd/nexus/main.go:709` only passes `FormattingRegex`. The result is that `planner.LocalPatternsRegex` is always `nil` at `chat.go:973`, making the DSL's second branch (local pattern routing) dead code.

**Proposed Solution:**
- Add `LocalPatternsRegex *regexp.Regexp` to `handlers/chat.go:452` (`Deps` struct)
- Pass it from `main.go:709` alongside `FormattingRegex`
- The regex already exists as `localPatternsRegex` at `main.go:46` — just wire it through

**Related:** #43 (routing DSL)

**Acceptance Criteria:**
- [ ] A prompt matching only `localPatternsRegex` (not `formattingRegex`) routes to local
- [ ] `make test-race` passes
- [ ] DSL local pattern routing has test coverage with a non-nil `LocalPatternsRegex`

---

## [P1] traceparent NOT propagated to upstream Ollama/frontier calls

**Problem:** `internal/tracing/propagation.go:16-17` claims "outbound upstream calls carry traceparent for distributed correlation," but `internal/upstream/upstream.go:144-146,290-292,342-344` and `internal/upstream/cascade.go:260-262` never set the `traceparent` header. Only the judge call propagates it. This breaks distributed tracing for the main request path — spans for local Ollama and frontier API calls are orphaned.

**Proposed Solution:**
- Add `traceparent` header to all upstream HTTP calls in `internal/upstream/`
- Create a `tracing outbound context` helper that extracts the current span and formats the `traceparent` header
- Apply to: `upstream.go:144-146` (StreamWithContext), `upstream.go:290-292` (BufferedFetchWithContext), `upstream.go:342-344` (FetchPanel), `cascade.go:260-262` (fetchCascadeStep)
- Use the existing `tracing.FormatTraceparent(span)` utility

**Related:** #41 (distributed tracing)

**Acceptance Criteria:**
- [ ] `curl -v /v1/chat/completions` with a traceparent header shows that header forwarded to Ollama and frontier
- [ ] Jaeger/OTLP collector receives child spans for upstream calls
- [ ] `make test-race` passes

---

## [P1] No per-stage latency breakdown in telemetry or metrics

**Problem:** The `ObservabilityEvent` (`internal/observability/collector.go:47-55`), `Record` (`internal/telemetry/recorder.go:65-80`), and `Request` (`internal/metrics/metrics.go:54-96`) structs only capture `TotalLatencyMs` and `TTFTMs`. RAG retrieval time, prompt engineering time, TOON compression time, SLM routing decision time, and cascade step times are all untracked. Operators cannot diagnose which stage is slow.

**Proposed Solution:**
- Add per-stage timing fields to `ObservabilityEvent`: `RAGRetrievalMs`, `PromptEngineeringMs`, `TOONCompressionMs`, `SLMRoutingMs`, `UpstreamFirstByteMs`
- Populate them in `internal/handlers/chat.go` using `time.Since()` around each stage
- Add `nexus_pipeline_stage_latency_ms` histogram metric with `stage` label
- Include per-stage timings in the JSONL telemetry record
- Add composite indexes to the SQLite metrics schema for `route, timestamp` and `model, timestamp`

**Dependencies:** Requires instrumentation points in `chat.go`

**Acceptance Criteria:**
- [ ] `/metrics` exposes `nexus_pipeline_stage_latency_ms{stage="rag|prompt_eng|toon|slm|upstream"}` histograms
- [ ] JSONL telemetry record includes all per-stage timings
- [ ] SQLite metrics DB has composite indexes on `(route, timestamp)` and `(model, timestamp)`
- [ ] `make test-race` passes

---

## [P2] RAG cache hit tracking is hardcoded to false (references issue #227)

**Problem:** At `internal/handlers/chat.go:911`, `trace.Transforms.RAGCacheHit = false` is set because the `CachedEmbedder` doesn't actually track cache hits (referenced issue #115). The RAG cache hit/miss metric is therefore always false, making it impossible to measure cache effectiveness.

**Proposed Solution:**
- Implement cache hit tracking in `internal/rag/` `CachedEmbedder` — track embedding lookups with a `map[string] EmbedResult` and record whether the lookup was a cache hit
- Surface hits via `RAGCacheHit bool` in the observability event
- Expose `nexus_rag_cache_hits_total` and `nexus_rag_cache_misses_total` Prometheus counters

**Related:** #46 (RAG persistence), #227

**Acceptance Criteria:**
- [ ] `trace.Transforms.RAGCacheHit` reflects actual cache state
- [ ] Prometheus counters `nexus_rag_cache_hits_total` and `nexus_rag_cache_misses_total` increment correctly
- [ ] `make test-race` passes

---

## [P2] Circuit breaker has no Prometheus metrics

**Problem:** `internal/circuit/` manages circuit breaker state but exposes nothing to Prometheus. Operators cannot see whether a circuit is currently closed/open/half-open, how many consecutive failures have occurred, or time-since-last-failure — they can only infer state from log messages or observe actual request failures.

**Proposed Solution:**
- Add `nexus_circuit_breaker_state{circuit="ollama|rag|...",state="closed|open|half_open"}` gauge (value 0/1)
- Add `nexus_circuit_breaker_failures_total{circuit="..."}` counter
- Add `nexus_circuit_breaker_last_failure_seconds{circuit="..."}` gauge (unix timestamp of last failure)
- Expose these in `internal/observability/collector.go` and update at each state transition

**Acceptance Criteria:**
- [ ] `/metrics` shows current circuit breaker state for all circuits
- [ ] State transitions are visible in Prometheus as gauge flips
- [ ] `make test-race` passes

---

## [P1] `NEXUS_SLM_CONFIDENCE_THRESHOLD` documented but not implemented

**Problem:** `AGENTS.md:129` documents a hard escalation rule: "SLM confidence < `NEXUS_SLM_CONFIDENCE_THRESHOLD` AND route is local/fusion → frontier (escalation, #44)". However, this env var does not exist in config, and `planner.go:279-285` only applies a soft bias via `negativeBiasNote` — there is no hard override that escalates to frontier when confidence is low.

**Proposed Solution:**
- Add `SLMConfidenceThreshold float64` to `Config` with env var `NEXUS_SLM_CONFIDENCE_THRESHOLD` (default `0.3`)
- Add post-SLM hard override in `planner.go:279-285`: if `route == RouteLocal || route == RouteFusion` AND `confidence < cfg.SLMConfidenceThreshold`, override to `RouteFrontier`
- Set `RouteSource` to `SourceSLMEscalation` (new source) and record in telemetry
- Expose `nexus_slm_escalations_total{reason="low_confidence"}` metric

**Related:** #44 (SLM escalation), #47 (adaptive confidence)

**Acceptance Criteria:**
- [ ] `NEXUS_SLM_CONFIDENCE_THRESHOLD=0.5` causes routes with confidence < 0.5 to escalate to frontier
- [ ] `nexus_slm_escalations_total{reason="low_confidence"}` increments on escalation
- [ ] `make test-race` passes with new escalation logic

---

## [P2] DSL formatting regex not env-configurable

**Problem:** `cmd/nexus/main.go:46` declares `formattingRegexPattern` as a hardcoded `const`. The DSL fast-pass keywords that determine local routing (e.g., `css`, `format`, `docstring`, `lint`) cannot be tuned by operators without a code change. Similarly, the fusion trigger keywords at `internal/router/dsl.go:48-49` are hardcoded.

**Proposed Solution:**
- Add `NEXUS_DSL_FORMATTING_PATTERNS` env var (comma-separated regex patterns, default: current hardcoded value)
- Add `NEXUS_DSL_FUSION_PATTERNS` env var (comma-separated, default: `"architectural design|system architecture"`)
- Parse both in `internal/config/config.go` and expose as `[]*regexp.Regexp` on the config struct
- Pass to `planner.go` alongside existing DSL patterns

**Related:** #43 (multi-frontier provider registry)

**Acceptance Criteria:**
- [ ] Operator can add `"refactor"` to local patterns via env var
- [ ] Operator can add `"database schema"` to fusion patterns via env var
- [ ] Invalid regex patterns fail boot with a clear error message
- [ ] `make test-race` passes

---

## [P1] `NEXUS_READINESS_MODE` documented but completely unwired

**Problem:** `internal/handlers/health.go:19,45` references `NEXUS_READINESS_MODE` (strict/degraded), and `/readyz` has mode-dependent behavior (`strict` returns 503 when Ollama is unreachable and no frontier key exists). However, `internal/config/config.go` never reads this env var — it is absent from the `Config` struct. The `/readyz` endpoint always uses default (degraded) behavior regardless of what operators set.

**Proposed Solution:**
- Add `ReadinessMode string` to `Config` struct in `internal/config/config.go`
- Add `NEXUS_READINESS_MODE` env var parsing (values: `"strict"`, `"degraded"`; default `"degraded"`)
- Validate in `config.Validate()` — reject unrecognized values
- Pass `ReadinessMode` to `ReadyzDeps` in `cmd/nexus/main.go:1095`
- Add `NEXUS_READINESS_MODE` to `.env.example`

**Related:** #42 (Kubernetes health endpoints)

**Acceptance Criteria:**
- [ ] `NEXUS_READINESS_MODE=strict` makes `/readyz` return 503 when Ollama is down
- [ ] Unrecognized values fail boot with validation error
- [ ] `nexus check` can verify readiness mode configuration
- [ ] `make test-race` passes

---

## [P2] No config hot reload

**Problem:** Every configuration change (rate limits, budget guards, log level, debug flags) requires a full proxy restart. There is no `SIGHUP` handler or other mechanism to reload config at runtime. Production operators cannot tune `NEXUS_RATE_LIMIT_RPM`, `NEXUS_BUDGET_DAILY_LIMIT`, or `NEXUS_LOG_LEVEL` without dropping traffic.

**Proposed Solution:**
- Implement `SIGHUP`-based config reload in `cmd/nexus/main.go`
- Settings eligible for hot reload (no restart required): `NEXUS_RATE_LIMIT_RPM`, `NEXUS_RATE_LIMIT_BURST`, `NEXUS_LOG_LEVEL`, `NEXUS_LOG_FORMAT`, `NEXUS_DEBUG`
- Settings requiring restart (document in error message): `NEXUS_OLLAMA_URL`, `NEXUS_FRONTIER_API_KEY`, `NEXUS_METRICS_DB`
- Reload updates the relevant components in-place (rate limiter re-configures its token bucket, logger level is updated via `slog`)
- Emit a log line at INFO level when config is reloaded: `"config reloaded", changed_fields: ["rate_limit_rpm", "log_level"]`

**Acceptance Criteria:**
- [ ] `kill -HUP <nexus-pid>` reloads eligible config without restart
- [ ] Ineligible settings produce a log message directing operator to restart
- [ ] `make test-race` passes

---

## [P2] `nexus check` missing critical diagnostic checks

**Problem:** The boot-time diagnostic command (`nexus check`, `internal/diag/diag.go:119-131`) runs 11 checks but omits several that could catch misconfigurations before they cause production issues:

- RAG circuit breaker threshold (`NEXUS_RAG_CIRCUIT_BREAKER_THRESHOLD=0` means breaker is disabled)
- Quality verifier concurrency (`NEXUS_QUALITY_CONCURRENCY=0` means verifier is dormant)
- Budget guard enabled state (`NEXUS_BUDGET_DAILY_LIMIT=0` means guard is disabled — operator may not realize)
- Rate limiter misconfiguration (RPM>0 + no `NEXUS_TRUSTED_PROXIES` configured = spoofing vulnerability)
- Provider registry JSON validity (`NEXUS_FRONTIER_PROVIDERS` with malformed JSON)
- Middleware chain validity (unknown middleware names silently falling back to default)

**Proposed Solution:**
- Add `checkRAGCircuitBreaker` — warns if threshold is 0
- Add `checkQualityVerifier` — warns if concurrency is 0
- Add `checkBudgetGuard` — warns if daily limit is 0
- Add `checkRateLimitProxyConfig` — errors if RPM > 0 and no trusted proxies configured
- Add `checkProviderRegistry` — validates `NEXUS_FRONTIER_PROVIDERS` JSON parsing
- Add `checkMiddlewareChain` — validates all middleware names resolve

**Related:** #32 (boot-time diagnostics)

**Acceptance Criteria:**
- [ ] `nexus check` reports warnings for all dormant features (RAG breaker disabled, verifier disabled, budget guard disabled)
- [ ] `nexus check` reports error when rate limiting is on without trusted proxies
- [ ] `nexus check` fails boot if provider registry JSON is malformed
- [ ] `make test-race` passes

---

## [P2] `/status` endpoint incomplete

**Problem:** The detailed status endpoint (`internal/handlers/status.go:76-126`) exposes judge, quality, RAG, and routing subsystem state, but omits several operator-critical dimensions:

- Rate limiter enabled state and current RPM limit
- Budget guard enabled state and current rolling spend
- Metrics DB writability (live check)
- SLM decision cache enabled state
- Arbiter synthesis cache enabled state

**Proposed Solution:**
- Add `rate_limiter` section to status response: `{enabled: bool, rpm: int, burst: int}`
- Add `budget` section: `{enabled: bool, daily_limit_usd: float, current_spend_usd: float, reset_at: timestamp}`
- Add `metrics_db` section: `{writable: bool, path: string}`
- Add `slm_cache` section: `{enabled: bool, ttl_seconds: int}`
- Add `arbiter_cache` section: `{enabled: bool, ttl_seconds: int}`

**Acceptance Criteria:**
- [ ] `GET /status` includes all new sections
- [ ] Values reflect current runtime state (not just config defaults)
- [ ] `make test-race` passes

---

## [P2] Panel goroutines have no panic recovery

**Problem:** `internal/upstream/upstream.go:465-479` and `upstream.go:744-760` launch goroutines for panel members (local and frontier fetches) with no `defer recover()`. If `FetchPanel` panics due to an unexpected JSON shape from an upstream, the goroutine dies and the `results` channel send at line 470/750 blocks forever — the main goroutine never receives and the request hangs.

**Proposed Solution:**
- Add `defer func() { if r := recover(); r != nil { results <- PanelResult{Err: fmt.Errorf("panic: %v", r)} } }()` inside the goroutines at `upstream.go:465-479` and `upstream.go:744-760`
- The panic error should be sent as a `PanelResult{Err: ...}` so the main goroutine can handle it via the existing error path
- Add a Prometheus counter `nexus_panel_panics_total` to track these events
- Apply the same pattern to `cascade.go` goroutines if any exist

**Acceptance Criteria:**
- [ ] A panic in a panel goroutine produces a traceable error in logs and returns an error to the client
- [ ] `nexus_panel_panics_total` counter increments on panic
- [ ] `make test-race` passes with panic recovery in place

---

## [P2] No `ResponseHeaderTimeout` in HTTP transport

**Problem:** `internal/transport/transport.go:81-90` does not set `ResponseHeaderTimeout` on the HTTP client. A slow or malicious upstream server could hold the connection open after sending headers but send response body data very slowly (slowloris variant). The client's only bound is the TCP stack's keepalive timeout.

**Proposed Solution:**
- Add `ResponseHeaderTimeout` to `TransportConfig` and the `*http.Client` in `transport.go`
- Default to `NEXUS_HTTP_RESPONSE_HEADER_TIMEOUT` env var (default `30s`)
- Ensure this timeout is applied per-request via the custom `Transport` (not on the client directly)
- The existing `ReadTimeout` (`NEXUS_SERVER_READ_TIMEOUT`) guards header+body read, but `ResponseHeaderTimeout` specifically bounds the headers-read phase independent of body transfer

**Related:** #34 (shared HTTP client)

**Acceptance Criteria:**
- [ ] A upstream server that sends headers but no body triggers timeout within `ResponseHeaderTimeout`
- [ ] `NEXUS_HTTP_RESPONSE_HEADER_TIMEOUT` env var controls the timeout
- [ ] `make test-race` passes

---

## [P2] Health probe circuit breaker has no backoff

**Problem:** `internal/health/health.go:304-327` — after the circuit breaker trips (Ollama deemed unhealthy), the poller continues probing at the same `PollInterval`. If Ollama is down for an extended period (hours), the poller logs a warning every 30 seconds indefinitely, flooding logs with identical messages.

**Proposed Solution:**
- Implement exponential backoff on the probe interval after consecutive failures:
  - After first failure: probe at normal `PollInterval`
  - After 3 consecutive failures: double interval
  - After 5 consecutive failures: quadruple interval
  - Cap at a maximum of `15 * PollInterval` (15 minutes if PollInterval=60s)
- On a successful probe: reset failure count and interval to normal
- Log at INFO level when entering backoff and when recovering: `"ollama health: entering backoff mode, interval=%.1fs"` / `"ollama health: recovered"`

**Related:** #8 (Ollama health + circuit breaker)

**Acceptance Criteria:**
- [ ] Log volume from health poller decreases after extended Ollama outage
- [ ] Recovery is logged at INFO level (not just debug)
- [ ] `make test-race` passes

---

## [P2] `make test-race` not in `ci` target

**Problem:** `Makefile:75` defines `ci` as `vet build test lint bench-short` — it does not run `test-race`. Race conditions may not be caught in CI, and contributors may not know to run `test-race` manually.

**Proposed Solution:**
- Add `test-race` to the `ci` target in `Makefile:75`: `ci: vet build test test-race lint bench-short`
- Add a comment explaining that `test-race` is in CI specifically because race conditions in concurrent code (transport, metrics, budget tracker, VRAM limiter) are easy to miss in manual testing
- Optionally add `race-ci` as a separate target for faster CI runs without race detection, keeping `ci` comprehensive

**Related:** #3 (structured logging)

**Acceptance Criteria:**
- [ ] `make ci` runs `test-race` and fails if any race conditions are detected
- [ ] `make help` documents `test-race` as the recommended pre-merge check

---

## [P2] DSL nil regex edge cases not tested

**Problem:** `internal/router/dsl_test.go` always passes both `formattingRegex` and `localPatternsRegex` as non-nil values. The `DSL()` function at `internal/router/dsl.go:52` and `internal/router/dsl.go:55` has explicit nil checks (`if formattingRegex != nil`), but these branches have zero test coverage. This is especially important given that `LocalPatternsRegex` is currently always `nil` in production (see dead code issue above).

**Proposed Solution:**
- Add test cases in `internal/router/dsl_test.go`:
  - `DSL(prompt, nil, nil)` — should return `("", false)` with no panic
  - `DSL(prompt, nil, localPatternsRegex)` — formatting branch skipped, local patterns should still match
  - `DSL(prompt, formattingRegex, nil)` — local patterns branch skipped, formatting should still match
- These tests validate the nil guards work correctly and document the expected fallback behavior

**Related:** #43 (routing DSL), dead code issue above

**Acceptance Criteria:**
- [ ] All nil-regex branches have explicit test cases
- [ ] `make test-race` passes

---

## [P2] Cascade `Content-Type` not validated before JSON parsing

**Problem:** `internal/upstream/cascade.go:305-308` (`BufferedFetchWithContext`) validates that the response body is valid JSON by unmarshaling into `map[string]interface{}`, but it does NOT check the `Content-Type` header. If an upstream returns `200 OK` with `Content-Type: text/html` containing HTML error pages (e.g., from a reverse proxy), the HTML passes JSON validation and gets forwarded as a valid response.

**Proposed Solution:**
- Add `Content-Type` validation before JSON parsing: require `application/json` or `text/event-stream` (for SSE endpoints)
- If `Content-Type` is missing or unexpected, return a `cascadeContentTypeMismatch` error with the actual content type logged
- Add test case in `internal/upstream/cascade_test.go` for 200 OK with `text/html` content type

**Related:** #8 (cascade fallback)

**Acceptance Criteria:**
- [ ] A 200 response with `Content-Type: text/html` is treated as an error, not forwarded
- [ ] The actual content type appears in the error log for debugging
- [ ] `make test-race` passes
