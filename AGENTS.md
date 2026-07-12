# Nexus Proxy

Hardware-aware AI routing gateway in Go. Intercepts OpenAI-compatible
`/v1/chat/completions` requests from coding agents (OpenCode, Aider, etc.),
optimizes the prompt (TOON compression, RAG few-shot, meta-prompting), and
routes to local Ollama or a frontier API based on complexity.

See `Nexus Proxy PRD and Architecture.md` for the full PRD and roadmap,
and `README.md` for the user-facing quickstart.

## Repo state

Phases 1–5 are substantially shipped. The single-file prototype is gone;
the codebase follows standard Go layout. Structured logging (#3),
telemetry (#16), Prometheus metrics (#40), distributed tracing (#41),
Kubernetes health endpoints (#42), inbound auth (#37), rate limiting
(#38), TLS termination (#39), SQLite metrics store (#4), the
multi-frontier provider registry (#43), and VRAM-aware concurrency
gating (#35) have all landed.

```
cmd/nexus/                 # main: wires config + middleware + handlers + starts HTTP
internal/
  auth/                    # inbound API-key gateway middleware (#37)
  budget/                  # rolling 24h frontier spend guard (#38)
  concurrencylimit/        # VRAM-aware local-route semaphore (#35)
  config/                  # env loading, defaults, validation
  handlers/                # chat.go + health.go (HTTP entry points)
  health/                  # Ollama health poller + circuit breaker (#8)
  judge/                   # async LLM-as-a-judge evaluator (#15)
  metrics/                 # SQLite metrics store for savings dashboard (#4)
  middleware/              # toon.go, prompt_engine.go (prompt transforms)
  observability/           # Prometheus collector + /metrics (#40)
  probe/                   # VRAM probe for concurrency gating
  providers/               # multi-frontier provider registry (#43)
  quality/                 # AST/compiler verifier (#13)
  rag/                     # SQLite-backed store + Ollama embedder + file watcher (#46)
  ratelimit/               # clientip.go, middleware.go (trusted-proxy + per-client limiter, #75)
  router/                  # dsl.go, slm.go, guardrails
  telemetry/               # Recorder interface + JSONLRecorder + ObservingWriter
  tracing/                 # W3C trace context + OTLP/JSON export (#41)
  transport/               # shared pooled HTTP client (#34)
  upstream/                # stream.go, cascade.go, fusion.go (panel + arbiter)
few_shot_examples/         # (gitignored) user-curated snippets
.env.example               # all env vars with safe defaults
Makefile                   # build / test / lint / ci
```

## Build / run / test

```bash
make build && ./bin/nexus   # build & run
make test                   # unit tests
make test-race              # race detector
make lint                   # golangci-lint
make ci                     # vet + build + test + lint (what CI runs)
```

`go run ./cmd/nexus` works too; the `Makefile` is convenience, not a
runtime dependency.

The binary listens on `:8000` (env: `NEXUS_ADDR`) and exposes:
- `POST /v1/chat/completions` — the proxy
- `GET  /healthz` — liveness probe (legacy alias)
- `GET  /livez` — liveness probe (#42)
- `GET  /readyz` — readiness probe; honours `NEXUS_READINESS_MODE` (#42)
- `GET  /status` — detailed subsystem state (frontier, judge, VRAM)
- `GET  /metrics` — Prometheus-format metrics (#40; path via `NEXUS_METRICS_ENDPOINT`)

`/healthz`, `/livez`, `/readyz`, `/status`, and `/metrics` are exempt
from inbound auth and rate limiting. `/v1/chat/completions` requires
auth when `NEXUS_PROXY_API_KEY` is set (#37).

Telemetry is opt-out: by default the proxy appends one JSON object per
proxied request to `./nexus-telemetry.jsonl` (env: `NEXUS_TELEMETRY_PATH`,
empty disables). Records are pushed onto a buffered channel consumed by a
background goroutine — the request path never blocks on persistence.

**SQLite metrics store (#4).** When `NEXUS_METRICS_DB` is set, the proxy
also writes a structured row per request to a SQLite database (route,
model, tokens, TOON savings, RAG injection, estimated frontier cost).
This powers the savings dashboard. Parent directories are created on
demand; leave empty to disable.

**Prometheus metrics (#40).** `/metrics` exposes counters, gauges, and
histograms for auth, rate-limit, budget, TLS middleware, and per-route
latency. See `internal/observability`.

**Known gotcha (#112).** When the SQLite metrics store is enabled, the
JSONL telemetry log is currently silenced. Tracked as bug #112 — both
sinks should coexist.

## Prerequisites

1. **Ollama running locally** on `http://localhost:11434` (env:
   `NEXUS_OLLAMA_URL`).
2. **Models pulled:**
   - `qwen3-coder:4b` — routing SLM (`NEXUS_ROUTER_MODEL`)
   - `qwen3-coder:8b` — local execution model (`NEXUS_LOCAL_MODEL`)
   - `nomic-embed-text` — embeddings for RAG (`NEXUS_EMBEDDING_MODEL`)
   RAG silently skips files whose embedding call fails, so missing the
   embedding model degrades gracefully (no RAG injection).
3. **Frontier API key** in `NEXUS_FRONTIER_API_KEY`. Without it, frontier
   routing returns 401.

Copy `.env.example` to `.env` and edit.

## Routing logic — quick reference

Decided in `internal/router`:
- `Guardrail` first (token-budget heuristic, force frontier above budget)
- `DSL` second (regex fast-pass: architecture → fusion, formatting → local)
- `SLMClient.Decide` last (Qwen3-Coder JSON-decision)
- Every failure defaults to **frontier** (the safe choice)

Route constants are `router.RouteLocal`, `router.RouteFrontier`,
`router.RouteFusion`.

| Trigger                                                       | Route     |
| ------------------------------------------------------------- | --------- |
| `len(prompt)/4 > NEXUS_TOKEN_GUARDRAIL`                       | `frontier` (VRAM guardrail) |
| Prompt contains "architectural design" or "system architecture" | `fusion` |
| Prompt matches `\b(css\|format\|docstring\|lint\|typo\|boilerplate)\b` | `local` |
| Otherwise                                                     | SLM decides |
| SLM confidence < `NEXUS_SLM_CONFIDENCE_THRESHOLD` AND route is local/fusion | `frontier` (escalation, #44) |
| SLM call fails, times out, or returns invalid JSON            | `frontier` (safe default) |

## Fusion progressive delivery (issue #48)

`route=fusion` no longer blocks until both panel members complete.
`upstream.PanelStreaming` (in `internal/upstream/upstream.go`) races
the local + frontier fetches, streams the first to complete as a
speculative OpenAI-compatible SSE chunk tagged with `X-Nexus-Fusion-
Progressive: true`, then either:

- emits `data: [DONE]\n\n` when both members' Jaccard similarity is
  `>= NEXUS_FUSION_AGREEMENT_THRESHOLD` (default 0.85) — arbiter
  is **not** invoked; the user has already received the answer;
- streams the arbiter's synthesis as additional SSE chunks after
  the speculative one when the two members diverge (the "append"
  disagreement mode).

The legacy blocking `Panel` path remains for `stream=false`
harness requests and for operators who opt out via
`NEXUS_FUSION_PROGRESSIVE=false`. Both paths share the same
arbiter timeout (`NEXUS_ARBITER_TIMEOUT`) and the same per-fetch
timeout (`NEXUS_FUSION_TIMEOUT`); the only difference is whether
the user sees bytes before the arbiter runs.

`fusion_arbiter_skipped` is exposed on the telemetry record (and
the SQLite metrics row) so the dashboard can report the fraction
of fusion traffic that achieved agreement.

## TOON compression gotchas

`middleware.SerializeToTOON` rewrites JSON arrays into a CSV-like TOON
shape. Two non-obvious behaviours to remember when re-parsing downstream:

- **Commas in values are replaced with full-width `，` (U+FF0C)** to
  protect the CSV structure.
- **Newlines in values are replaced with spaces** — multi-line strings
  round-trip lossy.
- The regex `JSONArrayBlock` only fires on ` ```json\n[ ... ]\n``` `
  fenced blocks inside `user`/`assistant` messages.

## Middleware order (do not reorder casually)

### Inbound chain (wired in `cmd/nexus/main.go`, outermost → innermost)

1. **Security headers** (#39) — sanitizes `X-Request-Id`, sets
   `X-Content-Type-Options`, `X-Frame-Options`, etc. Outermost so
   every response including 401/429 gets headers.
2. **Rate limiting** (#38/#75) — per-client-IP + optional global
   token-bucket. Exempts `/healthz`, `/livez`, `/readyz`, `/status`,
   `/metrics`.
3. **Inbound auth** (#37) — bearer-token gate. Exempts the same health
   endpoints. No-op pass-through when `NEXUS_PROXY_API_KEY` is unset.
4. **Handler** — `handlers.Chat` dispatch.

The HTTP-layer rate limiter (`internal/ratelimit`) wraps the chat
handler **before** any prompt-pipeline step runs — a throttled request
is rejected with 429 before prompt engineering / RAG / routing do any
work. It is disabled (passthrough) when `NEXUS_RATE_LIMIT_RPM <= 0`.

### Prompt pipeline (inside `internal/handlers/chat.go`)

1. `ApplyPromptEngineering` — inject role/CoT/constraints into system prompt
2. `RAG.Retrieve` + `InjectRAG` — embed latest user prompt, inject best
   cosine match (threshold from `NEXUS_RAG_THRESHOLD`)
3. `CompressJSONBlocks` + `AppendSystemNote` — TOON compress JSON arrays,
   append TOON instructions to system
4. Then routing: `Guardrail` → `DSL` → `SLM` (with confidence escalation, #44)
5. Then dispatch:
   - `local` → `Cascade` (streaming, with fallback) or `BufferedFetch` (non-streaming)
   - `frontier` → `Stream` (streaming) or `BufferedFetch` (non-streaming)
   - `fusion` → `Panel` (local + frontier + arbiter)

## Trusted-proxy client-IP resolution (issue #75)

`internal/ratelimit.ClientIPResolver` is the single source of truth for
"who is the client?". It honours `X-Forwarded-For` / `X-Real-IP` **only**
when the direct TCP peer (`r.RemoteAddr`) is in the
`NEXUS_TRUSTED_PROXIES` CIDR allowlist; otherwise it uses the peer IP
and ignores the headers (spoofing defence). A multi-hop XFF chain is
walked right-to-left, skipping trusted hops, to find the first
untrusted client IP (same algorithm as nginx `real_ip_recursive`).

Gotchas to remember when extending:

- **Empty config = trust nobody** (the safe default). The zero-value /
  nil resolver always returns the direct peer IP.
- **Invalid CIDR fails boot** — `config.parseTrustedProxies` returns an
  error rather than silently falling back, so a typo doesn't quietly
  disable XFF honouiring.
- `config.IsLoopbackBind` classifies `:8000` as loopback-safe (dev
  default) but `0.0.0.0:8000` as non-loopback; the boot warning fires
  only for the latter + rate-limit-on + no-trusted-proxies.
- The `ClientIPResolver` and `Middleware` live in `internal/ratelimit`
  (NOT `internal/middleware`, which is prompt transforms only) so they
  can import `net/http` without polluting the prompt pipeline.

## Style / workflow notes

- Go 1.21+. **Near-stdlib**: the only runtime dependency is
  `modernc.org/sqlite` (pure-Go SQLite driver for the metrics store,
  #4). The dev-time toolchain (golangci-lint, Make) is documented but
  optional for contributors who just want to run the binary.
- Logging uses `log/slog` (structured logging, #3). Format and level
  are configurable via `NEXUS_LOG_FORMAT` (json|text) and
  `NEXUS_LOG_LEVEL` (debug|info|warn|error). The old bracketed
  `log.Println` prefixes (`[ROUTER]`, `[RAG INDEXER]`) are gone — use
  `slog.Info(...)` with `slog.String("component", ...)` attributes.
- HTTP timeouts: `http.Server.ReadHeaderTimeout` is 10s.
  `ReadTimeout`/`WriteTimeout`/`IdleTimeout` are still 0 — tracked in
  #106. Per-call timeouts are `NEXUS_SLM_TIMEOUT` (8s default),
  `NEXUS_FUSION_TIMEOUT` (120s default), and
  `NEXUS_CASCADE_TIMEOUT` (per-cascade-attempt, #14).
- HTTP client is a single shared pooled `*http.Client` configured via
  `internal/transport.New` (#34) — env vars `NEXUS_HTTP_*` tune
  `MaxIdleConnsPerHost`, `MaxConnsPerHost`, `IdleConnTimeout`, etc.
  The old `http.DefaultClient` references are gone.
- The `few_shot_examples/` directory is auto-created at first run if
  missing (`Store.IndexDir`). It's gitignored.
- Tests use `httptest` + a `RecordingTransport` helper in
  `internal/upstream/recording.go` to record and replay HTTP calls.
  All tests run in <2s with `-race`.

## Persistent RAG vector store (issue #46)

`internal/rag` now ships a SQLite-backed `PersistentStore` and a
background `Watcher` so the few-shot embeddings survive restarts and
new snippets can be indexed without restarting the proxy.

```
internal/rag/
  rag.go          # FewShotExample, Embedder, RAGStore interface, Store (in-memory)
  sqlite.go       # PersistentStore: SQLite-backed cache + Load/IndexDir/Upsert/Remove
  watcher.go      # Watcher: mtime+size polling goroutine, file -> PersistentStore
```

The chat handler still calls `d.RAG.Retrieve` against a `rag.RAGStore`
interface — both `*Store` and `*PersistentStore` satisfy it, so the
handler is unaware of which implementation is wired.

| Env var                       | Default                                  | Purpose                                  |
| ----------------------------- | ---------------------------------------- | ---------------------------------------- |
| `NEXUS_RAG_DB`                | `~/.cache/nexus-proxy/rag.db`            | On-disk SQLite cache; empty disables persistence |
| `NEXUS_RAG_POLL_INTERVAL`     | `30s`                                    | Watcher cadence; `0` disables the watcher but keeps the cache |

Boot path (when persistence is enabled):
1. `OpenPersistentStore` opens (or creates) the DB and the in-memory
   `*Store` it embeds.
2. `LoadOrIndex` calls `Load` first (O(rows) — no Ollama) and only
   falls back to `IndexDir` (which embeds + persists) if the DB is
   empty.
3. The watcher (when enabled) polls the examples dir every
   `NEXUS_RAG_POLL_INTERVAL` and reconciles via Upsert/Remove using
   (mtime, size) as the freshness signal — a re-write triggers
   re-embed, a deletion drops the row.

Embeddings are serialised with `encoding/gob` (stdlib, no extra
dependency). The store is concurrency-safe via an internal
`sync.RWMutex`; the watcher writes through `Upsert`/`Remove` and
chat handlers read through `Retrieve` without coordination beyond
the lock.

## Debug request/response tracing (issue #33)

`NEXUS_DEBUG=true` switches the chat handler into a structured
tracing mode that emits five `[DEBUG] <section>` slog lines per
proxied request: `request`, `transforms`, `routing`, `upstream`,
`response`. Each line carries a structured sub-group so operators
can grep a single request id and see the full lifecycle:

| Section       | Fields                                                                  |
| ------------- | ----------------------------------------------------------------------- |
| `request`     | message count, estimated tokens, model, stream flag, body bytes        |
| `transforms`  | prompt engineering applied, RAG injection (filename/score), TOON delta  |
| `routing`     | route, reason (guardrail / dsl / slm), budget source, estimated tokens |
| `upstream`    | target host, model, streaming, cascade steps + served-by (when applicable) |
| `response`    | status code, TTFT ms, total bytes, output tokens, truncated body preview |

Production has zero overhead when the flag is off: the handler
skips trace construction entirely and the existing fast path is
byte-for-byte identical. When on, the handler installs a
`captureWriter` even without judge/quality observers so the
response trace can include a body preview.

API keys are always redacted (`sk-...XYZ1` for a real
`sk-proj-...XYZ1`); `Authorization` headers are stripped from any
structured payload. The body preview is capped at
`NEXUS_DEBUG_BODY_BYTES` (default 512) so a runaway upstream cannot
flood the log. Traces are emitted **after** the response completes
so they never interleave with the SSE stream the harness is
consuming.

## Async LLM-as-a-Judge (issue #15)

`internal/judge` is the async quality evaluator. It samples ~10% of
completed `RouteLocal` requests and asks a frontier endpoint for a
1–5 score on the model's output. Tunables (all env vars):

| Variable                    | Default                       | Purpose                                  |
| --------------------------- | ----------------------------- | ---------------------------------------- |
| `NEXUS_JUDGE_URL`           | z.ai, falls back to frontier  | Judge endpoint                           |
| `NEXUS_JUDGE_MODEL`         | `NEXUS_FRONTIER_MODEL`        | Judge model name                         |
| `NEXUS_JUDGE_API_KEY`       | `NEXUS_FRONTIER_API_KEY`      | Bearer token (may be empty in dev)       |
| `NEXUS_JUDGE_SAMPLE_RATE`   | `0.1`                         | Fraction of completed local requests     |
| `NEXUS_JUDGE_CONCURRENCY`   | `2`                           | Max simultaneous judge calls             |
| `NEXUS_JUDGE_QUEUE`         | `64`                          | Buffered channel size; overflow drops    |
| `NEXUS_JUDGE_TIMEOUT`       | `30s`                         | Per-call timeout                         |
| `NEXUS_JUDGE_COST_PER_1K`   | `0.002`                       | USD per 1k tokens for cost estimates     |

The judge is **always disabled when `NEXUS_JUDGE_SAMPLE_RATE <= 0`**,
so a stock `.env.example` boots with the evaluator dormant. The chat
hot path is unaffected when the judge is dormant.

**Dependency rule.** `internal/handlers` and `internal/upstream` must
never import `internal/judge`. The handler exposes a
`JudgeObserver` hook (function-typed) on `handlers.Deps`, and
`cmd/nexus/main.go` plugs in a closure that adapts to the judge's
`Sample`/`Enqueue` entry points. This keeps the hot path lean and
unit-testable without spinning up a worker pool.

**Persistence.** `internal/judge` exposes a tiny `Storage` interface;
today it is backed by an in-memory `MemoryStorage`. The SQLite metrics
store (#4, `internal/metrics`) persists per-request data (route, model,
tokens, cost) to `NEXUS_METRICS_DB`. The judge's `JudgeScore` record
(request id, score 1–5, raw response, prompt/output token estimates,
USD cost) is designed to persist through the same Storage interface
without changing the wire shape.

## Where to extend

For new behaviour, add tests first and follow the existing layout:

| New behaviour                           | Package                          |
| --------------------------------------- | -------------------------------- |
| New env var                             | `internal/config`                |
| New prompt transform                    | `internal/middleware`            |
| New routing rule                        | `internal/router`                |
| Different upstream protocol / cascade   | `internal/upstream`              |
| New HTTP endpoint                       | `internal/handlers`              |
| Trusted-proxy / client-IP / rate limit  | `internal/ratelimit`             |
| Inbound auth / API key validation       | `internal/auth`                  |
| Spend guard / budget enforcement        | `internal/budget`                |
| TLS / security headers                  | `internal/handlers` (header mw)  |
| Prometheus metric / collector           | `internal/observability`         |
| Distributed tracing / span              | `internal/tracing`               |
| SQLite metrics store / savings query    | `internal/metrics`               |
| VRAM probe / concurrency gate           | `internal/probe`, `internal/concurrencylimit` |
| Health poller / circuit breaker         | `internal/health`                |
| Multi-frontier provider registry        | `internal/providers`             |
| AST/compiler quality check              | `internal/quality`               |
| Shared HTTP client tuning               | `internal/transport`             |
| Judge scoring / sampling logic          | `internal/judge`                 |
| Judge persistence (SQLite, etc.)        | `internal/judge` (Storage impl)  |
| New telemetry field or storage backend  | `internal/telemetry`             |
| RAG embedding cache / indexer           | `internal/rag`                   |

Existing functions map 1:1 to those packages. The handler is the only
public entry point — keep middleware and router free of `net/http`
concerns so they stay unit-testable. Telemetry's `Recorder` interface
(`internal/telemetry`) is the JSONL seam; the SQLite metrics store
(`internal/metrics`) is a separate sink with its own schema. Both are
wired via `handlers.Deps` so neither touches the hot-path handler
logic directly.