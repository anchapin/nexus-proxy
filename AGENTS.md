# Nexus Proxy — Agent Guide

Hardware-aware AI routing gateway in Go. Intercepts OpenAI-compatible
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
make ci             # vet + build + test + test-race + lint + bench-short
```

`go run ./cmd/nexus` also works. **Go 1.26** (CI pin); `go.mod` declares 1.25.

**Coverage floor is 70%** — CI fails if total drops below `COVERAGE_THRESHOLD`.
Per-package numbers print for visibility; only the total gates.

**CI runs four jobs** (`.github/workflows/ci.yml`): `test` (vet → build →
`go test -race -coverprofile=coverage.txt -covermode=atomic ./...` + coverage
gate), `bench` (non-blocking `bench-short`, `continue-on-error: true`),
`lint` (`golangci-lint-action@v9`, golangci-lint **v2.12.2**), and `docker`
(smoke `make docker-build` — catches Dockerfile↔go.mod Go-version drift,
issue #541). `make ci` is a local convenience wrapper; CI does not invoke it.

**Subcommands** (`cmd/nexus/main.go` dispatches on `os.Args[1]`; no args =
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
  diag/                 # boot-time diagnostics (nexus check / nexus doctor)
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
that way for unit-testability.

**Critical dependency rule:** `internal/handlers` and `internal/upstream`
must **never** import `internal/judge` or `internal/quality`. Both hook
in via `JudgeObserver` / `QualityObserver` function-typed fields on
`handlers.Deps` wired in `cmd/nexus/main.go`. This keeps the hot path
testable without spinning up worker pools.

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

**SLM decision cache:** `NEXUS_SLM_CACHE_MAX_ENTRIES` + `NEXUS_SLM_CACHE_TTL`
(default 512 entries / 30s). Set `NEXUS_SLM_CACHE_TTL=0` to disable.
Semantic dedup via `NEXUS_SLMCACHE_SIMILARITY_THRESHOLD` (range 0..1).

**Fusion progressive delivery** (`NEXUS_FUSION_PROGRESSIVE=true`, default):
panels race local + frontier, stream the faster as speculative SSE, and
only invoke the arbiter when Jaccard similarity < `NEXUS_FUSION_AGREEMENT_THRESHOLD`
(default 0.85).

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
trust nobody (safe default). Invalid CIDR **fails boot** (not silent).

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

## Debug tracing (issue #33)

`NEXUS_DEBUG=true` emits five structured slog groups per request:
`request`, `transforms`, `routing`, `upstream`, `response`. Zero
overhead when off. API keys redacted; body preview capped at
`NEXUS_DEBUG_BODY_BYTES` (default 512).

## Adding new env vars

Config env vars are split across two files. New vars need **both**:

1. **Struct field** in `internal/config/config.go` (`Config` struct) +
   parsed inline in `Load()` using: `getEnv`, `getEnvAllowEmpty`,
   `getEnvInt`, `getEnvBool`, `getEnvFloat`, `getEnvDuration`,
   `getEnvRegexps`.
2. **YAML mirror** in `internal/config/yaml.go` (`YAMLConfig` struct
   field, snake_case) + an env-overrides-yaml branch in `LoadYAML()`.

Config file: `$XDG_CONFIG_HOME/nexus-proxy/config.yaml` or
`~/.config/nexus-proxy/...` or `./config.yaml`. Env vars always override
file values.

`internal/config/env_example_audit_test.go` enforces the `.env.example` ↔
parser contract bidirectionally (issue #478): code vars must have a
canonical `.env.example` entry, and vice versa. Four skip prefixes
exempt dynamic-construction vars (`NEXUS_PROVIDER_`, `NEXUS_FRONTIER_`,
`NEXUS_ZAI_`, `NEXUS_HTTP_`), plus one exact-match skip
(`NEXUS_QUALITY_TEST_HOOK`, test-only).

For hot-reloadable knobs add the field to `ReloadHotReloadable()` in
`config.go`. Sending **SIGHUP** re-reads exactly: log level, log format,
debug, rate-limit RPM, rate-limit burst. Everything else requires a full
restart.

## Branch conventions

- **`develop`** is the default branch — base for all feature/fix branches
- **`main`** — only as PR target from `develop` for releases
- Naming: `fix/issue-<number>` or `feat/<short-description>`
- **Conventional Commits:** `feat:`, `fix:`, `docs:`, etc. Reference the
  issue in the subject (e.g. `feat: resolve #123 — …`)
- **PR body must link the issue** with `Fixes #N` / `Closes #N` /
  `Resolves #N`. Use
  `scripts/check_pr_closing_refs.sh <PR_NUMBER> <EXPECTED_COUNT>` to
  verify the count is exact.

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

## Local-route cooldown (issue #80)

After the cascade detects an Ollama failure and falls back, `circuit.Cooldown`
arms a short cooldown so subsequent requests skip local and go directly to
fallback. Set `NEXUS_LOCAL_COOLDOWN=0` to disable (pre-issue-#80 behaviour).