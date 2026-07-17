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
make lint           # golangci-lint
make fmt            # gofmt -w (in place)
make ci             # vet + build + test + test-race + lint (bench-short runs but does not block)
```

`go run ./cmd/nexus` also works.

**CLI subcommands:**
- `nexus` (no args) — start the proxy server
- `nexus check` / `nexus doctor` — boot-time diagnostics, exits 0 on success
- `nexus dashboard` — print daily savings summary to stdout
- `nexus --version` — print build version

**Go version:** `go.mod` declares `1.25.0`; CI builds with Go 1.26 (see `.github/workflows/ci.yml`). Local dev needs at least 1.25.

**CI coverage floor is 70%** — `make ci` will fail if total coverage
drops below 70%. Per-package numbers print for visibility; only the
total gates.

**Runtime dependency only:** `modernc.org/sqlite` (metrics store).
Everything else is stdlib.

## Running tests

```bash
make test                          # all packages
go test ./internal/handlers/...    # single package
go test -run TestChatHandler ./...  # single test
make test-race                     # race detector (required before merge)
```

Benchmarks are **non-blocking** in CI — the `bench` job has `continue-on-error: true`. `make ci` does not gate on them.

## Package layout

```
cmd/nexus/              # main: wires config → middleware → handlers → HTTP server
internal/
  auth/                 # inbound API-key middleware
  budget/               # 24h rolling frontier spend cap
  concurrencylimit/     # VRAM-aware local-route semaphore
  config/               # Load() parses all NEXUS_* env vars
  handlers/             # chat.go + health.go (only package that touches net/http)
  health/               # Ollama circuit breaker
  judge/                # async LLM-as-a-judge (sampled local completions)
  metrics/              # SQLite metrics store → savings dashboard
  middleware/           # prompt transforms + security headers
  observability/        # Prometheus collector + /metrics endpoint
  probe/                # nvidia-smi / AMD sysfs VRAM probe
  providers/            # multi-frontier registry + cost-latency selector
  quality/              # background cargo check / tsc verifier
  rag/                  # PersistentStore (SQLite) + Store + Watcher
  ratelimit/            # ClientIPResolver + HTTP middleware
  router/               # Guardrail → DSL → SLM.Decide routing pipeline
  telemetry/            # Recorder interface + JSONLRecorder
  tracing/              # W3C trace context + OTLP/JSON exporter
  transport/            # shared pooled http.Client (NEXUS_HTTP_* tuning)
  upstream/             # upstream.go, cascade.go (Panel + PanelStreaming)
```

**Dependency rule:** `internal/handlers` and `internal/upstream` must
**never** import `internal/judge`. The judge hooks in via
`JudgeObserver` on `handlers.Deps` — a function-typed field wired in
`cmd/nexus/main.go`. This keeps the hot path testable without spinning
up a worker pool.

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
(default 512 entries / 30s). Semantic dedup via
`NEXUS_SLMCACHE_SEMANTIC_THRESHOLD` uses the embedder.

**Fusion progressive delivery** (`NEXUS_FUSION_PROGRESSIVE=true`, default):
panels race local + frontier, stream the faster as speculative SSE, and
only invoke the arbiter when Jaccard similarity < `NEXUS_FUSION_AGREEMENT_THRESHOLD`
(default 0.85). Set `NEXUS_FUSION_PROGRESSIVE=false` for legacy blocking
Panel behavior.

## Middleware order (do not reorder)

**Inbound HTTP chain** (`cmd/nexus/main.go`, outermost → innermost):
1. Security headers (`X-Content-Type-Options`, CSP, HSTS, etc.)
2. Panic recovery
3. Auth (bearer token; no-op when `NEXUS_PROXY_API_KEY` unset)
4. Rate limiter (per-client-IP) wrapping the chat handler
5. Handler dispatch

Rate limiter fires **before** any prompt-pipeline work — a 429 terminates
at the HTTP layer. Exempt paths: `/healthz /livez /readyz /status /metrics`.

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

## Debug tracing (issue #33)

`NEXUS_DEBUG=true` emits five structured slog groups per request:
`request`, `transforms`, `routing`, `upstream`, `response`. Zero
overhead when off. API keys redacted; body preview capped at
`NEXUS_DEBUG_BODY_BYTES` (default 512).

## Adding new env vars

Update `internal/config/config.go` in three places:
1. Add field to `Config` struct
2. Add entry to `knownEnvVars` map (case-sensitive allowlist — catches typos)
3. Parse in `Load()` using a helper:
   - `getEnv("NEXUS_VAR", "default")` — string
   - `getEnvBool("NEXUS_VAR", false)` — bool
   - `getEnvInt("NEXUS_VAR", 0)` — int
   - `getEnvFloat("NEXUS_VAR", 0.0)` — float64
   - `getEnvDuration("NEXUS_VAR", 30*time.Second)` — duration
   - `getEnvAllowEmpty("NEXUS_VAR", "default")` — string (including empty)
4. Add the variable to `.env.example`

## Important env vars not mentioned elsewhere

- `NEXUS_MAX_BODY_BYTES` — max request body size (default 1 MiB). Rejected before allocation via `http.MaxBytesReader`.
- `NEXUS_MAX_UPSTREAM_RESPONSE_BYTES` — cap on buffered upstream responses (default 10 MiB). Prevents a malicious upstream from exhausting proxy memory.
- `NEXUS_SHUTDOWN_TIMEOUT` — graceful drain window (default 30s). Must accommodate long frontier SSE streams.

## Branch conventions

- **`develop`** is the default branch — base for all feature/fix branches
- **`main`** — only as PR target from `develop` for releases
- Naming: `fix/issue-<number>` or `feat/<short-description>`

## Logging

`log/slog` structured logging only. Use `slog.Info(...)` with
`slog.String("component", ...)` attributes. Never `fmt.Println` in
production paths.

## Testing

Tests use `httptest` + `RecordingTransport` in `internal/upstream/recording.go`
to record/replay HTTP calls. All tests run in <2s with `-race`.