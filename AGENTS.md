# Nexus Proxy — Agent Guide

Hardware-aware AI routing gateway in Go (module
`github.com/anchapin/nexus-proxy`). Intercepts OpenAI-compatible
`/v1/chat/completions`, optimizes prompts (TOON compression, RAG,
meta-prompting), and routes to local Ollama or a frontier API based
on complexity.

See `Nexus Proxy PRD and Architecture.md` for design intent and
`README.md` for user-facing quickstart.

## Build / test / lint

```bash
make build          # → ./bin/nexus
make test           # unit tests
make test-race      # race detector — required to merge
make lint           # golangci-lint v2.12.2
make fmt            # gofmt -w (in place)
make bench-baseline # regenerate bench/baseline.txt for benchstat (issue #1186)
make ci             # vet + build + test + test-race + lint + bench-short
```

`go run ./cmd/nexus` also works. **Go 1.26** (CI pin); `go.mod` declares 1.25.

**Coverage floor is 70%** — CI fails if total drops below `COVERAGE_THRESHOLD`.
Per-package numbers print for visibility; only the total gates.

**CI runs five jobs** (`.github/workflows/ci.yml`): `test` (vet → build →
`go test -race -coverprofile=coverage.txt -covermode=atomic ./...` + coverage
gate), `bench` (non-blocking `bench-short`, `continue-on-error: true`),
`bench-regression` (benchstat comparison against `bench/baseline.txt`, posts
PR comment, `continue-on-error: true` — issue #1186), `lint`
(`golangci-lint-action@v9`, golangci-lint **v2.12.2**), and `docker`
(smoke `make docker-build` — catches Dockerfile↔go.mod Go-version drift,
issue #541). `make ci` is a local convenience wrapper; CI does not invoke it.

**golangci-lint exclusions** (`.golangci.yml`): `cmd/` and `internal/observability/prometheus.go`
ignore `errcheck` on `fmt.Fprint*` writes intentionally — write errors cannot be handled
after headers are committed (issue #276). `resp.Body.Close`, `rows.Close`, `stmt.Close`
are also excluded in non-critical paths.

**Subcommands** (`cmd/nexus/dispatch.go` dispatches on `os.Args[1]`; no args =
start the proxy):
- `nexus check` (alias `nexus doctor`) — boot-time diagnostic suite. Exits
  **0 when every check passes** (warn/skip are fine), **1 when at least one
  fails**. `--json` for machine-readable output. Guarded by
  `cmd/nexus/doc_test.go` (issue #455).
- `nexus config validate <file>` — parse + validate a YAML config against
  the same rules as `Load()`. Exits 0/1.
- `nexus dashboard` — daily savings summary view.
- `nexus --version` (`-v` / `version`) — build version (`dev` unless
  `-ldflags -X main.version=...` overrides it; Makefile + release.yml set it).

**Runtime deps are pure-Go / CGO-free:** `modernc.org/sqlite`,
`fsnotify`, `tiktoken-go`, `golang.org/x/sync`, `gopkg.in/yaml.v3`.

## Package layout

```
cmd/nexus/              # main: wires config → middleware → handlers → HTTP server
internal/
  auth/                 # inbound API-key middleware
  budget/               # 24h rolling frontier spend cap
  circuit/              # local-route cooldown after cascade failure (issue #80)
  concurrencylimit/     # VRAM-aware local-route semaphore
  config/               # Load() (env parsing) + LoadYAML() (file + env override)
  diag/                  # boot-time diagnostics (nexus check / nexus doctor)
  handlers/             # chat.go + health.go + recover/security/sanitize
  health/               # Ollama circuit breaker (separate from internal/circuit)
  ioutils/              # shared io helpers (decompression, etc.)
  judge/                # async LLM-as-a-judge (sampled local completions)
  metrics/              # SQLite metrics store → savings dashboard
  middleware/           # prompt transforms only — NO net/http
  observability/        # collector.go, prometheus.go, routemetrics.go → /metrics
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
imports `net/http`; `internal/middleware` intentionally does not — keep it
that way for unit-testability. `internal/middleware` has **zero** `net/http`
imports.

**Critical dependency rule:** `internal/handlers` and `internal/upstream`
must **never** import `internal/judge` or `internal/quality`. Both hook
in via `JudgeObserver` / `QualityObserver` function-typed fields on
`handlers.Deps` wired in `cmd/nexus/main.go`. This keeps the hot path
testable without spinning up worker pools.

**RAG embedder circuit breaker** (`NEXUS_RAG_CIRCUIT_BREAKER_THRESHOLD`, default 3):
trips after N consecutive Ollama `/api/embeddings` failures; recloses on first
success. Separate from the Ollama health breaker that governs chat routing.

**SLM decision cache semantic dedup** (`NEXUS_SLMCACHE_SEMANTIC_SCAN_LIMIT`):
when > 0, `getSemantic` stops scanning after examining this many entries,
bounding O(n) cosine similarity to a cap. Default 0 = unlimited.

**Models discovery endpoint** (`GET /v1/models`): served when
`NEXUS_MODELS_ENDPOINT=true` (default). Lists configured local/router/frontier
models plus cached Ollama `/api/tags` results (TTL: `NEXUS_MODELS_CACHE_TTL`,
default 5m). Set `NEXUS_MODELS_ENDPOINT=false` to disable entirely.

**Model aliasing** (issue #1184): `NEXUS_MODEL_ALIASES` is a JSON map
(`{"gpt-4":"anthropic/claude-3-5-sonnet"}`) that translates
client-requested model names to `"providerName/upstreamModel"`. When a
request's `model` field matches an alias, the proxy rewrites the body
model and routes directly to the target provider (bypassing the SLM
routing pipeline). The `providerName` must match a provider registered
via `NEXUS_FRONTIER_PROVIDERS`. Aliases are surfaced in
`GET /v1/models` (owned_by: `"alias"`). `NEXUS_MODEL_ALIASES_STRICT=true`
rejects unknown models with HTTP 400; default false passes through.

**Distributed tracing config**: `NEXUS_TRACING_ENDPOINT` enables OTLP/JSON export.
Tune with `NEXUS_TRACING_TIMEOUT` (default 10s), `NEXUS_TRACING_MAX_RETRIES` (3),
`NEXUS_TRACING_RETRY_BASE_DELAY` (100ms), `NEXUS_TRACING_RETRY_MAX_DELAY` (2s),
`NEXUS_TRACING_QUEUE_SIZE` (256), `NEXUS_TRACING_BATCH_SIZE` (64), and
`NEXUS_TRACING_SAMPLE_RATE` (1.0 = record all). Dropped spans appear as
`nexus_tracing_dropped_total`; flush failures as `nexus_tracing_flush_failures_total`.

## Routing pipeline

`internal/router`: Guardrail → DSL → SLM.Decide. Every failure defaults
to **frontier** (safe choice).

| Trigger | Route |
| ------- | ----- |
| `len(prompt)/4 > NEXUS_TOKEN_GUARDRAIL` | `frontier` (VRAM guardrail) |
| Prompt matches `NEXUS_DSL_FUSION_PATTERNS` (default: `architectural design\|system architecture`) | `fusion` |
| Prompt matches `NEXUS_DSL_FORMATTING_PATTERNS` (default: `css\|format\|docstring\|lint\|typo\|boilerplate\|debug\|fix bug\|git commit\|sql query\|parse json\|validate input\|regex\|api endpoint\|test\|optimize\|readme`) | `local` |
| Prompt matches `NEXUS_DSL_LOCAL_PATTERNS` (default: `refactor\|security scan\|generate tests\|explain this code\|performance analysis`) | `local` |
| Prompt matches `NEXUS_DSL_UNICODE_PATTERNS` (non-ASCII text categories like `\p{Han}`, issue #422) | `local` |
| Otherwise | SLM decides (qwen3-coder:4b JSON decision) |
| SLM confidence < threshold OR SLM fails | `frontier` (escalation) |

DSL patterns are **comma-separated regexes** (set via env var, not a map).

**Pattern precedence** (issue #876): patterns are checked in fixed order; first match wins:
1. `NEXUS_DSL_FUSION_PATTERNS` → `fusion`
2. `NEXUS_DSL_FORMATTING_PATTERNS` → `local`
3. `NEXUS_DSL_LOCAL_PATTERNS` → `local`
4. `NEXUS_DSL_UNICODE_PATTERNS` → `local`

**SLM decision cache:** `NEXUS_SLM_CACHE_MAX_ENTRIES` + `NEXUS_SLM_CACHE_TTL`
(default 512 entries / 30s). Set `NEXUS_SLM_CACHE_TTL=0` to disable.
Semantic dedup via `NEXUS_SLMCACHE_SIMILARITY_THRESHOLD` (range 0..1).

**Fusion progressive delivery** (`NEXUS_FUSION_PROGRESSIVE=true`, default):
panels race local + frontier, stream the faster as speculative SSE, and
only invoke the arbiter when Jaccard similarity < `NEXUS_FUSION_AGREEMENT_THRESHOLD`
(default 0.85).

**Arbiter synthesis cache** (`NEXUS_ARBITER_CACHE_TTL`, default 5m): when > 0, arbiter
responses are cached keyed by a hash of both panel members' content. Set to 0
to disable — every disagreement triggers a fresh frontier call.

## Middleware order (do not reorder)

**Inbound HTTP chain** (`cmd/nexus/main.go`, outermost → innermost):
1. Security headers
2. Panic recovery (turns panics into structured 500 JSON envelopes)
3. Inbound auth (bearer token; exempts `/healthz`, `/metrics`; `/status` when `NEXUS_STATUS_PUBLIC=true`; no-op when `NEXUS_PROXY_API_KEY` unset)
4. mux routing (rate limiting is **not** a global header — it is path-specific)

Rate limiter wraps only `/v1/chat/completions`. Health, status, and metrics
endpoints are registered on the unprotected mux and are never rate-limited.

**Prompt pipeline** (`internal/handlers/chat.go`):
1. `ApplyPromptEngineering` — role/CoT/constraints into system prompt
2. `RAG.Retrieve` + `InjectRAG` — embed + cosine match (threshold `NEXUS_RAG_THRESHOLD`)
3. `CompressJSONBlocks` + `AppendSystemNote` — TOON compression + system note
4. Guardrail → DSL → SLM routing
5. Dispatch: local → Cascade/BufferedFetch; frontier → Stream/BufferedFetch; fusion → Panel

**Middleware chain customization:** `NEXUS_MIDDLEWARE_CHAIN` (default:
`promptEngineering,rag,compressJSONBlocks,appendSystemNote`) lets operators
reorder or omit steps. Available names: `promptEngineering`, `rag`,
`compressJSONBlocks`, `appendSystemNote`.

## Ollama degradation (issue #8)

If Ollama goes down after boot, the health poller trips the circuit
breaker after `NEXUS_HEALTH_BREAKER_THRESHOLD` (default 3) failed probes:
- `RouteLocal` → transparently rerouted to frontier
- `RouteFusion` → local panel skipped, arbiter synthesizes from frontier alone
- Response carries `X-Nexus-Degraded: true`
- Circuit recloses on next successful probe

Set `NEXUS_HEALTH_POLL_INTERVAL=0` to disable the poller.

## Trusted-proxy client-IP resolution (issue #75)

`internal/ratelimit.ClientIPResolver` is the single source of truth.
`X-Forwarded-For` / `X-Real-IP` are honoured **only** when the direct
TCP peer is in `NEXUS_TRUSTED_PROXIES` CIDR allowlist. Empty =
trust nobody (safe default). **Invalid CIDR fails boot** (not silent).

## TOON compression (issue #123)

`middleware.SerializeToTOON` rewrites JSON arrays into CSV-like shape.
Two non-obvious round-trip rules:
- **Commas in values → full-width `，` (U+FF0C)**
- **Newlines in values → spaces** (multi-line strings lose newlines)

The `JSONArrayBlock` regex only fires on fenced ` ```json\n[...]\n``` ` blocks.
A second pass handles bare/prose-embedded arrays of ≥2 objects. Set
`NEXUS_TOON_UNFENCED=false` to restrict to fenced-only.

## Persistent RAG (issue #46)

`internal/rag`: `PersistentStore` (SQLite-backed) embeds an in-memory
`Store`. Both satisfy the `RAGStore` interface. Boot:
`OpenPersistentStore` → `LoadOrIndex` (loads from SQLite, falls back to
full embed if DB is empty). `Watcher` reconciles on mtime+size changes.
Set `NEXUS_RAG_DB=` to disable persistence (legacy in-memory path).

**RAG embedder is pluggable** (`NEXUS_EMBEDDER_TYPE`): `ollama` (default),
`openai`, or `cohere`.

## Judge (issue #15)

Async LLM-as-a-judge samples ~10% of `RouteLocal` completions and scores
them 1–5 via a frontier endpoint. Disabled when `NEXUS_JUDGE_SAMPLE_RATE <= 0`.

**Judge-guided adaptive routing** (`NEXUS_ROUTING_CONFIDENCE_DB`):
historical scores aggregated by task category feed back to the SLM as a
confidence signal. Dormant when judge is off — routing is byte-for-byte
identical to non-adaptive path.

## Request body and response guards

`NEXUS_MAX_BODY_BYTES` (default 1 MiB) caps inbound request bodies.
413 rejection before any allocation — zero overhead on normal traffic.

`NEXUS_MAX_RESPONSE_BYTES` (default 64 MiB) caps upstream response bodies.
`NEXUS_CASCADE_MAX_RESPONSE_BYTES` (default 64 MiB) caps cascade response bodies
independently (issue #742).

`NEXUS_SHUTDOWN_TIMEOUT` (default 30s) is the graceful drain window.
A warning fires at boot if `SHUTDOWN_TIMEOUT < SERVER_READ_TIMEOUT`.

## Security headers (issue #444)

`handlers.SecurityHeaders(tlsActive bool)` in `internal/handlers/security.go`
is the **single source of truth** for response hardening. Wired as the
outermost layer:

```go
Handler: handlers.SecurityHeaders(cfg.TLSEnabled)(handlers.Recover(handlerPanicObs)(rootHandler)),
```

HSTS is only emitted when `cfg.TLSEnabled` is true. Default false: a
stock plaintext bind must not advertise HSTS.

`internal/middleware/security.go` does not exist — do not add it.
`internal/middleware` is intentionally net/http-free. Any response-header
middleware belongs in `internal/handlers`.

## Auth brute-force protection (issue #296)

After `NEXUS_AUTH_RATE_LIMIT_BURST` auth failures from the same client IP
within the `NEXUS_AUTH_RATE_LIMIT_WINDOW` sliding window, the proxy returns
429 with `Retry-After`. Disabled when `NEXUS_AUTH_RATE_LIMIT_RPM <= 0`.

**Rate limiting** (`NEXUS_RATE_LIMIT_RPM` / `NEXUS_RATE_LIMIT_BURST`) is
hot-reloadable and buckets by client IP by default. Set
`NEXUS_RATE_LIMIT_BY_API_KEY=true` to bucket by `SHA256(IP + ":" + APIKey)`
instead — prevents shared-IP abuse across different API keys (issue #776).

## Prompt injection hardening (issue #76)

`NEXUS_PROMPT_INJECTION_MODE` controls policy-text isolation:
- `off` (default): legacy append behaviour, backward compatible.
- `warn`: proxy text in `[NEXUS PROXY POLICY]` delimiters in a leading
  system message; suspicious user patterns are **logged but requests proceed**.
- `strict`: same as warn, plus requests with suspicious user patterns are
  rejected with a 400.

`NEXUS_INJECTION_SCAN_ROLES` (default `system`) controls which roles are
scanned (`system`, or `system,user`). Proxy-injected policy blocks are never
flagged regardless of this setting.

## Observability

**Prometheus metrics** are served at `GET /metrics` (no auth required):
- `nexus_tracing_dropped_total` — OTLP spans dropped
- `nexus_budget_*` — spend/budget guard counters when `NEXUS_BUDGET_ALERT_ENABLED`
- `nexus_health_circuit_*` — Ollama circuit breaker state transitions
- `nexus_rag_circuit_*` — RAG embedder circuit breaker state transitions
- `nexus_upstream_*` — per-route/upstream counters and histograms

See `docs/observability-surface.md` for the full metric reference.

**Structured logging** (`log/slog`): `NEXUS_LOG_LEVEL` (debug/info/warn/error)
and `NEXUS_LOG_FORMAT` (json/text). Both are hot-reloadable via SIGHUP.

**Debug tracing** (`NEXUS_DEBUG=true`): emits five slog groups per request:
`request`, `transforms`, `routing`, `upstream`, `response`. API keys redacted;
body preview capped at `NEXUS_DEBUG_BODY_BYTES` (default 512).

**Distributed tracing** (`NEXUS_TRACING_ENDPOINT`): OTLP/JSON exporter.
See `docs/tracing.example.md` for setup.

## Provider selector (issue #45)

The multi-provider registry picks the cheapest provider based on observed
latency + cost. Tunable via `NEXUS_SELECTOR_WINDOW` (look-back window),
`NEXUS_SELECTOR_MIN_SAMPLES` (observations before a provider is trusted),
`NEXUS_SELECTOR_REFRESH` (recompute cadence), and `NEXUS_PROVIDER_TAIL_WEIGHT`
(P95 blend factor, range 0–1). When multiple providers are registered via
`NEXUS_PROVIDERS`, the legacy `NEXUS_FRONTIER_*` vars are ignored.

## Provider adapter interface (issue #1185)

`internal/providers/adapter.go` defines a `ProviderAdapter` that translates
between the proxy's canonical OpenAI request/response shape and a
non-OpenAI provider's native API. The proxy's hot path always speaks
OpenAI internally; the adapter is the single place that knows the
provider's auth headers, request path, request-body schema, and SSE
event shape.

- Methods: `AuthHeaders(apiKey)`, `RequestPath(baseURL)`,
  `TransformRequest(body)`, `NormalizeSSE(io.Reader) io.Reader`.
- `NewAdapter(type)` resolves the type (default `openai` is a byte-for-byte
  no-op so the existing OpenAI path is unchanged); unknown types error.
- Allowed types: `openai`, `anthropic`, `azure`, `gemini`
  (`providers.ValidAdapterTypes()`).
- Per-provider selection: `NEXUS_PROVIDER_<NAME>_TYPE` (env) or the
  `type` field on a YAML `providers:` list entry.
- The `anthropic` adapter authenticates via `x-api-key` +
  `anthropic-version`, POSTs to `/v1/messages`, and normalises Anthropic's
  `content_block_delta` SSE events into OpenAI `chat.completion.chunk`
  frames.

Config validation rejects an unknown `type` at boot
(`LoadFromEnv` / `nexus config validate` both enforce the closed set).

## Per-provider cost model (issue #1183)

`NEXUS_COST_USE_OUTPUT_TOKENS` (default `false`) switches the per-request
cost estimate from the legacy flat input-only rate to a per-provider
input/output token split. When enabled, `frontierCostEstimate` looks up the
serving provider via `ProviderRegistry.ByModel(model)` and computes
`inputTokens*inputRate/1000 + outputTokens*outputRate/1000`, counting output
tokens with the tiktoken tokenizer. When disabled (default), the estimate is
byte-for-byte identical to the pre-issue-#1183 single-rate path.

`NEXUS_FRONTIER_PROVIDERS` JSON entries accept `inputCostPer1K` and
`outputCostPer1K` (the flat `costPer1K` still parses and seeds the input rate
when the split keys are absent). `ProviderConfig` exposes
`InputCostPer1KUSD()` / `OutputCostPer1KUSD()`; `CostPer1KUSD()` is retained
as the selector weight. The metrics row surfaces `input_cost_usd` and
`output_cost_usd` as distinct SQLite columns.

## Adding new env vars

Config env vars are split across two files. New vars need **both**:

1. **Struct field** in `internal/config/config.go` (`Config` struct) +
   parsed inline in `Load()` using: `getEnv`, `getEnvAllowEmpty`,
   `getEnvInt`, `getEnvBool`, `getEnvFloat`, `getEnvDuration`,
   `getEnvRegexps`.
2. **YAML mirror** in `internal/config/yaml.go` (`YAMLConfig` struct
   field, snake_case) + an env-overrides-yaml branch in `LoadYAML()`.

Config file: `NEXUS_CONFIG_FILE` if set, else
`$XDG_CONFIG_HOME/nexus-proxy/config.yaml` if `XDG_CONFIG_HOME` is set,
else `./config.yaml`. Env vars always override file values.
`nexus config validate <file>` checks a YAML file before use (exits 0/1).

`internal/config/env_example_audit_test.go` **enforces** the `.env.example` ↔
parser contract bidirectionally (issue #478). Adding a var without both the
struct field and the `.env.example` entry will fail the test. Four prefixes
are exempt: `NEXUS_PROVIDER_`, `NEXUS_FRONTIER_`, `NEXUS_ZAI_`, `NEXUS_HTTP_`;
plus `NEXUS_QUALITY_TEST_HOOK` (test-only).

For hot-reloadable knobs add the field to `ReloadHotReloadable()` in
`config.go`. Sending **SIGHUP** re-reads exactly:
- `NEXUS_LOG_LEVEL`, `NEXUS_LOG_FORMAT`, `NEXUS_DEBUG`
- `NEXUS_RATE_LIMIT_RPM`, `NEXUS_RATE_LIMIT_BURST`
- `NEXUS_AUTH_RATE_LIMIT_RPM`, `NEXUS_AUTH_RATE_LIMIT_BURST`, `NEXUS_AUTH_RATE_LIMIT_WINDOW`
- `NEXUS_SHUTDOWN_TIMEOUT`, `NEXUS_SERVER_READ_TIMEOUT`
- `NEXUS_BUDGET_ALERT_THRESHOLD`, `NEXUS_FUSION_AGREEMENT_THRESHOLD`, `NEXUS_TRACING_SAMPLE_RATE`
- `NEXUS_TRUSTED_PROXIES` (issue #896 — re-parsed without restart)
Everything else requires a full restart.
The `env_example_audit_test.go` bidirectional test enforces that every
var listed in `ReloadHotReloadable()` carries the `# hot-reloadable via
SIGHUP` annotation in `.env.example` — omitting the annotation from a new
hot-reloadable var will fail the test.

## Branch conventions

- **`develop`** is the default branch — base for all feature/fix branches
- **`main`** — **release-only branch**. Changes land on `main` **only** via a
  PR merged from `develop` after all CI checks pass and at least one approving
  review. Direct pushes to `main` are blocked by branch protection (enforce_admins,
  require_code_owner_reviews, required_status_checks). Never commit directly to
  `main` — treat it as a read-only release artifact.
- Naming: `fix/issue-<number>` or `feat/<short-description>`
- **Conventional Commits:** `feat:`, `fix:`, `docs:`, etc. Reference the
  issue in the subject (e.g. `feat: resolve #123 — …`)
- **PR body must link the issue** with `Fixes #N` / `Closes #N` /
  `Resolves #N`. Run `scripts/check_pr_closing_refs.sh <PR_NUMBER> <EXPECTED_COUNT>`
  to verify the link count is exact before merging.

## Logging

`log/slog` structured logging only. Use `slog.Info(...)` with
`slog.String("component", ...)` attributes. Never `fmt.Println` in
production paths.

## Testing

Tests use `httptest` + `RecordingTransport` in `internal/upstream/recording.go`
to record/replay HTTP calls. All tests run in <2s with `-race`.

`make test-race` is required to pass before merging.

**Focused testing:** `go test ./internal/packagename` runs a single package.
Prefix with `-v` for verbose output.

**Pre-commit hook** (`make install-hooks` once after cloning): runs `gofmt -l`
on staged `.go` files and fails the commit if any need formatting. The hook
lives in `.githooks/pre-commit`; `make install-hooks` sets `git
core.hooksPath` to point at it.

## Local-route cooldown (issue #80)

After the cascade detects an Ollama failure and falls back, `circuit.Cooldown`
arms a short cooldown so subsequent requests skip local and go directly to
fallback. Set `NEXUS_LOCAL_COOLDOWN=0` to disable (pre-issue-#80 behaviour).

## Newer routing and RAG knobs

Key knobs not covered elsewhere (verify defaults in `.env.example`):
- **`NEXUS_SLM_CONFIDENCE_THRESHOLD`** (default 0.3): SLM decisions below this bypass DSL/SLM and go to frontier.
- **`NEXUS_SLMCACHE_SEMANTIC_SCAN_LIMIT`** (default 0): retained for backward compat; fix for issue #1038 makes semantic dedup always scan all entries and exit early only on perfect score=1.0, so this var has no effect.
- **`NEXUS_RAG_EMBED_CACHE_*`** (size 256, TTL 24h): LRU cache for prompt embeddings — repeat prompts skip Ollama entirely.
- **`NEXUS_RAG_EMBED_CACHE_WAIT_TIMEOUT`** (default 5s): max waiter time for concurrent in-flight Embeds; 0 = wait indefinitely (issue #800).
- **`NEXUS_RAG_CIRCUIT_BREAKER_THRESHOLD`** (default 3): consecutive embed failures before RAG circuit trips.
- **`NEXUS_ARBITER_CACHE_MAX_ENTRIES`** (default 512): LRU cap for arbiter synthesis cache.
- **`NEXUS_READINESS_MODE`** (`degraded`|`strict`): `/readyz` returns 503 in `strict` mode when Ollama is degraded or down.

## `nexus check` exit codes

| Code | Meaning |
| ---- | ------- |
| `0`  | Every check passed (warnings/skip are fine) |
| `1`  | At least one check failed — read `[FAIL]` lines for remediation |

`nexus check --json` emits machine-readable output for CI gates.
