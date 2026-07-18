# Nexus Proxy — Agent Guide

Hardware-aware AI routing gateway in Go. Intercepts OpenAI-compatible
`/v1/chat/completions`, optimizes prompts (TOON compression, RAG,
meta-prompting), and routes to local Ollama or a frontier API based
on complexity.

See `Nexus Proxy PRD and Architecture.md` for full design intent and
`README.md` for user-facing quickstart.

## Build / test / lint

```bash
make build          # → ./bin/nexus
make test           # unit tests
make test-race      # race detector (required to merge)
make lint           # golangci-lint v2.12.2 (.golangci.yml is v2 format)
make fmt            # gofmt -w (in place)
make ci             # vet + build + test + test-race + lint + bench-short
```

`go run ./cmd/nexus` also works.

**CI gate (what actually blocks a merge):** `.github/workflows/ci.yml`
runs three separate jobs — `test` (`go vet ./...` → `go build ./cmd/nexus`
→ `go test -race -coverprofile=coverage.txt -covermode=atomic ./...` then
the coverage gate), `bench` (non-blocking `make bench-short`, smoke only),
and `lint` (`golangci-lint-action@v7`). It does **not** invoke `make ci`;
that target is a local convenience wrapper that runs the same steps plus
bench-short on one machine.

**Coverage floor is 70%** — CI fails the `test` job if total coverage
drops below `COVERAGE_THRESHOLD`. Per-package numbers print for
visibility; only the total gates.

**Go version:** CI pins Go 1.26 in `.github/workflows/ci.yml` (and
`release.yml`). `go.mod` declares `go 1.25.0` — 1.26 is fully supported
because it is a strict superset.

**Startup diagnostics:** `nexus check` (alias `nexus doctor`) runs the
boot-time diagnostic suite and exits without starting the server. Use
this to verify Ollama reachability, model availability, frontier key
validity, VRAM probe budget, and RAG directory state before routing
any traffic (issue #32). The README Quickstart intentionally puts this
*before* `Build and run` — guarded by `cmd/nexus/doc_test.go` (issue #455).

**Runtime dependency only:** `modernc.org/sqlite` (metrics store).
Everything else is stdlib.

## Package layout

```
cmd/nexus/              # main: wires config → middleware → handlers → HTTP server
internal/
  auth/                 # inbound API-key middleware
  budget/               # 24h rolling frontier spend cap
  circuit/              # local-route cooldown after cascade failure (issue #80)
  concurrencylimit/     # VRAM-aware local-route semaphore
  config/               # Load() (env parsing) + LoadYAML() (file + env override)
  diag/                 # boot-time diagnostics (nexus check / nexus doctor)
  handlers/             # chat.go + health.go + recover/security/sanitize (only pkg that touches net/http in the request hot path)
  health/               # Ollama circuit breaker (separate from internal/circuit)
  ioutils/              # shared io helpers (decompression, etc.)
  judge/                # async LLM-as-a-judge (sampled local completions)
  metrics/              # SQLite metrics store → savings dashboard
  middleware/           # prompt transforms only (toon, prompt_engine, chain) — NO net/http
  observability/        # collector.go, prometheus.go, routemetrics.go → /metrics endpoint
  probe/                # nvidia-smi / AMD sysfs VRAM probe
  providers/            # multi-frontier registry + cost-latency selector
  quality/              # background cargo check / tsc verifier
  rag/                  # PersistentStore (SQLite) + Store + Watcher + embedders
  ratelimit/            # ClientIPResolver + HTTP middleware (NOT in middleware/)
  router/               # Guardrail → DSL → SLM.Decide routing pipeline
  telemetry/            # Recorder interface + JSONLRecorder
  tokenizer/            # tiktoken wrapper (single shared instance)
  tracing/              # W3C trace context + OTLP/JSON exporter
  tracingtest/          # test helpers for the tracing package
  transport/            # shared pooled http.Client (NEXUS_HTTP_* tuning)
  upstream/             # upstream.go, cascade.go, arbiter_cache.go, similarity.go, recording.go
```

**Critical: `internal/ratelimit` ≠ `internal/middleware`.** Rate limiting
lives in `internal/ratelimit` because it imports `net/http`. The
`internal/middleware` package has no `net/http` dependency — keep it that
way for unit-testability.

**Critical dependency rule:** `internal/handlers` and `internal/upstream`
must **never** import `internal/judge` or `internal/quality`. Both hook
in via observer interfaces on `handlers.Deps` — `JudgeObserver` and
`QualityObserver` are function-typed fields wired in `cmd/nexus/main.go`.
This keeps the hot path testable without spinning up worker pools.

## Routing pipeline

`internal/router`: Guardrail → DSL → SLM.Decide. Every failure defaults
to **frontier** (safe choice).

| Trigger | Route |
| ------- | ----- |
| `len(prompt)/4 > NEXUS_TOKEN_GUARDRAIL` | `frontier` (VRAM guardrail) |
| Prompt matches `NEXUS_DSL_FUSION_PATTERNS` (default: `architectural design\|system architecture`) | `fusion` |
| Prompt matches `NEXUS_DSL_FORMATTING_PATTERNS` (default: `css\|format\|docstring\|lint\|typo\|boilerplate\|debug\|fix bug\|git commit\|sql query\|parse json\|validate input\|regex\|api endpoint\|test\|optimize\|readme`) | `local` |
| Prompt matches `NEXUS_DSL_LOCAL_PATTERNS` (default: `refactor\|security scan\|generate tests\|explain this code\|performance analysis`) | `local` |
| Otherwise | SLM decides (qwen3-coder:4b JSON decision) |
| SLM confidence < threshold OR SLM fails | `frontier` (escalation) |

DSL patterns are **comma-separated regexes** (set via env var, not a map).
`NEXUS_DSL_FUSION_PATTERNS` and `NEXUS_DSL_LOCAL_PATTERNS` accept Go
regex syntax.

**SLM decision cache:** `NEXUS_SLM_CACHE_MAX_ENTRIES` + `NEXUS_SLM_CACHE_TTL`
(default 512 entries / 30s). Set `NEXUS_SLM_CACHE_TTL=0` to disable.
Semantic dedup via `NEXUS_SLMCACHE_SIMILARITY_THRESHOLD` (yaml key
`slm_cache_similarity_threshold`, range 0..1) — `0.0` disables semantic
matching.

**Fusion progressive delivery** (`NEXUS_FUSION_PROGRESSIVE=true`, default):
panels race local + frontier, stream the faster as speculative SSE, and
only invoke the arbiter when Jaccard similarity < `NEXUS_FUSION_AGREEMENT_THRESHOLD`
(default 0.85). Set `NEXUS_FUSION_PROGRESSIVE=false` for legacy blocking
Panel behavior.

## Middleware order (do not reorder)

**Inbound HTTP chain** (`cmd/nexus/main.go`, outermost → innermost):
1. Security headers (`X-Request-Id` sanitize, `X-Content-Type-Options`, etc.)
2. Panic recovery (turns panics into structured 500 JSON envelopes)
3. Inbound auth (bearer token; exempts `/healthz`, `/metrics`; also `/status` when `NEXUS_STATUS_PUBLIC=true`; no-op when `NEXUS_PROXY_API_KEY` unset)
4. mux routing (rate limiting is **not** a global header — it is path-specific)

Rate limiter wraps only `/v1/chat/completions`. Health, status, and metrics
endpoints are registered on the unprotected mux and are never rate-limited.
A 429 from the rate limiter terminates at the HTTP layer, before any
prompt-pipeline work runs.

**Prompt pipeline** (`internal/handlers/chat.go`):
1. `ApplyPromptEngineering` — role/CoT/constraints into system prompt
2. `RAG.Retrieve` + `InjectRAG` — embed + cosine match (threshold `NEXUS_RAG_THRESHOLD`)
3. `CompressJSONBlocks` + `AppendSystemNote` — TOON compression + system note
4. Guardrail → DSL → SLM routing
5. Dispatch: local → Cascade/BufferedFetch; frontier → Stream/BufferedFetch; fusion → Panel

## Ollama degradation (issue #8)

If Ollama goes down after boot, the health poller trips the circuit
breaker after `NEXUS_HEALTH_BREAKER_THRESHOLD` (default 3) failed probes:
- `RouteLocal` → transparently rerouted to frontier
- `RouteFusion` → local panel skipped, arbiter synthesizes from frontier alone
- Response carries `X-Nexus-Degraded: true`
- Circuit recloses on next successful probe

Set `NEXUS_HEALTH_POLL_INTERVAL=0` to disable the poller (assumes
Ollama always healthy, pays per-request timeout on failure).

## Trusted-proxy client-IP resolution (issue #75)

`internal/ratelimit.ClientIPResolver` is the single source of truth.
`X-Forwarded-For` / `X-Real-IP` are honoured **only** when the direct
TCP peer is in `NEXUS_TRUSTED_PROXIES` CIDR allowlist. Empty =
trust nobody (safe default). Invalid CIDR **fails boot** (not silent).

Boot warning fires when: rate limiting on + non-loopback bind + no
trusted proxies configured.

## TOON compression (issue #123)

`middleware.SerializeToTOON` rewrites JSON arrays into CSV-like shape.
Two non-obvious round-trip rules:
- **Commas in values → full-width `，` (U+FF0C)**
- **Newlines in values → spaces** (multi-line strings lose newlines)

The `JSONArrayBlock` regex only fires on fenced ` ```json\n[...]\n``` ` blocks.
A second pass (`CompressUnfencedJSONArrays`) handles bare and prose-
embedded arrays of ≥2 objects — single-row arrays are skipped. Set
`NEXUS_TOON_UNFENCED=false` to restrict to fenced-only.

## Persistent RAG (issue #46)

`internal/rag`: `PersistentStore` (SQLite-backed) embeds an in-memory
`Store`. Both satisfy the `RAGStore` interface. The chat handler is
unaware which is wired.

Boot: `OpenPersistentStore` → `LoadOrIndex` (loads from SQLite, falls
back to full embed if DB is empty). `Watcher` reconciles on mtime+size
changes. Embeddings use `encoding/gob`. Set `NEXUS_RAG_DB=` to disable
persistence (legacy in-memory path).

**RAG embedder is pluggable** (`NEXUS_EMBEDDER_TYPE`): `ollama` (default),
`openai`, or `cohere`. Set the matching API key env var.

## Judge (issue #15)

Async LLM-as-a-judge samples ~10% of `RouteLocal` completions and scores
them 1–5 via a frontier endpoint. Disabled when `NEXUS_JUDGE_SAMPLE_RATE <= 0`.

Judge output (`JudgeScore`) is stored via a `Storage` interface (today:
in-memory `MemoryStorage`). The SQLite metrics store (`internal/metrics`)
persists per-request rows independently.

**Judge-guided adaptive routing** (`NEXUS_ROUTING_CONFIDENCE_DB`):
historical scores aggregated by task category feed back to the SLM as a
confidence signal. Dormant when judge is off — routing is byte-for-byte
identical to non-adaptive path.

## Request body and response guards

`NEXUS_MAX_BODY_BYTES` (default 1 MiB) caps inbound request bodies. The
chat handler rejects oversized POSTs with 413 before any allocation
happens — zero overhead on normal traffic.

`NEXUS_MAX_RESPONSE_BYTES` (default 64 MiB) caps upstream response bodies
read into memory. Prevents a malicious upstream from exhausting proxy
memory on large completions.

`NEXUS_SHUTDOWN_TIMEOUT` (default 30s) is the graceful drain window.
The prior 10s hardcoded value truncated frontier SSE streams mid-token.
Set this and your K8s `terminationGracePeriodSeconds` consistently when
running long-streaming workloads. A warning fires at boot if
`SHUTDOWN_TIMEOUT < SERVER_READ_TIMEOUT` since in-flight uploads could
be truncated.

## Security headers (issue #444)

`handlers.SecurityHeaders(tlsActive bool)` — defined in
`internal/handlers/security.go` — is the **single source of truth**
for response hardening. Wired as the outermost layer in
`cmd/nexus/main.go`:

```go
Handler: handlers.SecurityHeaders(cfg.TLSEnabled)(handlers.Recover()(rootHandler)),
```

HSTS (`Strict-Transport-Security: max-age=31536000`) is only emitted
when `cfg.TLSEnabled` is true. Default false: a stock plaintext bind
must not advertise HSTS (spec violation, silently ignored by browsers).
The mirror env var is `NEXUS_TLS_ENABLED` and the YAML key is
`tls_enabled:`.

`internal/middleware/security.go` does not exist — there is no duplicate.
Do not add it; `internal/middleware` is intentionally net/http-free
(kept pure for unit-testability, mirroring `internal/ratelimit`'s split).
Any response-header middleware belongs in `internal/handlers`.

## Debug tracing (issue #33)

`NEXUS_DEBUG=true` emits five structured slog groups per request:
`request`, `transforms`, `routing`, `upstream`, `response`. Zero
overhead when off. API keys redacted; body preview capped at
`NEXUS_DEBUG_BODY_BYTES` (default 512).

## Adding new env vars

Config env vars are split across two files. New vars need **both**:

1. **Struct field** in `internal/config/config.go` (`Config` struct) +
   parsed inline in `Load()` using one of these helpers (lines
   ~1707–1789):
   - `getEnv(key, def)` — string
   - `getEnvAllowEmpty(key, def)` — string (including empty)
   - `getEnvInt(key, def)` — int
   - `getEnvBool(key, def)` — bool
   - `getEnvFloat(key, def)` — float64
   - `getEnvDuration(key, def)` — duration
   - `getEnvRegexps(key, default)` — `[]*regexp.Regexp`
2. **YAML mirror** in `internal/config/yaml.go` (`YAMLConfig` struct
   field, snake_case) + an env-overrides-yaml branch in `LoadYAML()` so
   file-based config users get the same knob.

No central registry — but `internal/config/env_example_audit_test.go`
enforces that any `NEXUS_*` var referenced by the parser is documented
in `.env.example`. Add it there too.

For hot-reloadable knobs (rate limit, log level, log format, debug) add
the field to `ReloadHotReloadable()` in `config.go` — knobs not in that
list require a server restart.

## Branch conventions

- **`develop`** is the default branch — base for all feature/fix branches
- **`main`** — only as PR target from `develop` for releases
- Naming: `fix/issue-<number>` or `feat/<short-description>`
- **Conventional Commits** for messages: `feat:`, `fix:`, `docs:`, etc.
  Commit subject should reference the issue (e.g. `feat: resolve #123 — …`)
- **PR body must link the issue** with `Fixes #N` / `Closes #N` /
  `Resolves #N` (any case). The `auto-close` workflow
  (`.github/workflows/auto-close.yml`) closes the linked issue when the
  PR merges — but only if at least one such token is in the body. Use
  `scripts/check_pr_closing_refs.sh <PR_NUMBER> <EXPECTED_COUNT>` to
  verify when a PR title says `resolve #N` it actually closes exactly N.

## Operator / load-test scripts (`scripts/`)

- `scripts/check_pr_closing_refs.sh` — gates a PR's closing refs.
  See the comment at the top for exit codes.
- `scripts/loadtest.sh` — burst-load a live `/v1/chat/completions`,
  prefers `hey` (auto-downloaded), falls back to curl. Outputs
  p50/p90/p99 + throughput for comparison against `docs/BENCHMARKS.md`.

## Contributor docs (`docs/`)

- `docs/runbook.md` — operational triage (Ollama down, RAG breaker,
  DSL mis-routes, trace flags).
- `docs/observability-surface.md` — Prometheus metrics + `/status`
  field inventory.
- `docs/BENCHMARKS.md` — baseline numbers + how to re-run.
- `docs/tracing.example.md` — W3C trace context + OTLP/JSON setup.

## Release flow

`.github/workflows/release.yml` triggers on `v*` tags or
`workflow_dispatch`. Produces linux/amd64, linux/arm64, darwin/arm64
binaries, SHA256 checksums, multi-arch GHCR image, keyless cosign
signature (requires `id-token: write`), and SPDX SBOM. The
`docker/build-push-action` block uses `provenance: true` /
`sbom: true` and consumes `cache-from: type=gha` / `cache-to:
type=gha,mode=max`. Do **not** set `outputs: push-by-digest` — it
silently overrides `tags` and leaves the image untagged.

## Logging

`log/slog` structured logging only. Use `slog.Info(...)` with
`slog.String("component", ...)` attributes. Never `fmt.Println` in
production paths.

## Testing

Tests use `httptest` + `RecordingTransport` in `internal/upstream/recording.go`
to record/replay HTTP calls. All tests run in <2s with `-race`.

`make test-race` is required to pass before merging — race conditions in
transport, metrics, budget tracker, and VRAM limiter are easy to miss
in manual testing.

**Test infrastructure:** `RecordingTransport` (internal/upstream/recording.go)
records/replays HTTP calls so unit tests have no external dependencies.

## Local-route cooldown (issue #80)

After the cascade detects an Ollama failure and falls back, `circuit.Cooldown`
arms a short cooldown so subsequent requests skip local and go directly to
fallback — closing the gap between cascade failure detection and the health
poller tripping the breaker. Set `NEXUS_LOCAL_COOLDOWN=0` to disable
(pre-issue-#80 behaviour).