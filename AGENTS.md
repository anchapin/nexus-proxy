# Nexus Proxy

Hardware-aware AI routing gateway in Go. Intercepts OpenAI-compatible
`/v1/chat/completions` requests from coding agents (OpenCode, Aider, etc.),
optimizes the prompt (TOON compression, RAG few-shot, meta-prompting), and
routes to local Ollama or a frontier API based on complexity.

See `Nexus Proxy PRD and Architecture.md` for the full PRD and roadmap,
and `README.md` for the user-facing quickstart.

## Repo state

Phase 1 refactor is complete. The single-file prototype is gone; the
codebase now follows standard Go layout. Telemetry was added in #16.

```
cmd/nexus/                 # main: wires config + handlers + starts HTTP
internal/
  config/                  # env loading, defaults, validation
  handlers/                # chat.go: HTTP entry point
  middleware/              # toon.go, prompt_engine.go
  router/                  # dsl.go, slm.go, guardrails
  upstream/                # stream.go, fusion.go (panel + arbiter)
  ratelimit/               # clientip.go, middleware.go (trusted-proxy + per-client limiter, issue #75)
  rag/                     # SQLite-backed store + Ollama embedder + file watcher (issue #46)
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
- `GET  /healthz` — liveness probe

Telemetry is opt-out: by default the proxy appends one JSON object per
proxied request to `./nexus-telemetry.jsonl` (env: `NEXUS_TELEMETRY_PATH`,
empty disables). Records are pushed onto a buffered channel consumed by a
background goroutine — the request path never blocks on persistence. Set
`NEXUS_TELEMETRY_PATH=/tmp/nexus.db` or similar to relocate the log.

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

In `internal/handlers/chat.go`:

1. `ApplyPromptEngineering` — inject role/CoT/constraints into system prompt
2. `RAG.Retrieve` + `InjectRAG` — embed latest user prompt, inject best
   cosine match (threshold from `NEXUS_RAG_THRESHOLD`)
3. `CompressJSONBlocks` + `AppendSystemNote` — TOON compress JSON arrays,
   append TOON instructions to system
4. Then routing: `Guardrail` → `DSL` → `SLM`
5. Then stream (`local` / `frontier`) or `Panel` (`fusion`)

The HTTP-layer rate limiter (`internal/ratelimit`, issue #75) wraps the
chat handler **before** any of the above runs — a throttled request is
rejected with 429 before prompt engineering / RAG / routing do any work.
It is disabled (passthrough) when `NEXUS_RATE_LIMIT_RPM <= 0`.

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

- Go 1.21+. Stdlib-only by design (PRD: "zero-dependency"). The
  dev-time toolchain (golangci-lint, Make) is documented but optional
  for contributors who just want to run the binary.
- Logging is plain `log.Println` with bracketed prefixes (`[ROUTER]`,
  `[RAG INDEXER]`, etc.). PRD Phase 2 swaps this for `slog`.
- HTTP timeouts: `http.Server.ReadHeaderTimeout` is 10s; per-call
  timeouts are `NEXUS_SLM_TIMEOUT` (8s default) and
  `NEXUS_FUSION_TIMEOUT` (120s default). Other upstream calls use the
  shared `http.DefaultClient`.
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
today it is backed by an in-memory `MemoryStorage`. Issue #16
(SQLite-backed telemetry) will provide a SQLite implementation that
satisfies the same interface. The judge stores a structured
`JudgeScore` record (request id, score 1–5, raw response, prompt/
output token estimates, USD cost) so the future PR can persist it
without changing the wire shape.

## Where to extend

For new behaviour, add tests first and follow the existing layout:

| New behaviour                        | Package                          |
| ------------------------------------ | -------------------------------- |
| New env var                          | `internal/config`                |
| New prompt transform                 | `internal/middleware`            |
| New routing rule                     | `internal/router`                |
| Different upstream protocol          | `internal/upstream`              |
| New HTTP endpoint                    | `internal/handlers`              |
| Trusted-proxy / client-IP / rate limit | `internal/ratelimit`           |
| Judge scoring / sampling logic       | `internal/judge`                 |
| Judge persistence (SQLite, etc.)     | `internal/judge` (Storage impl)  |
| New metric field or storage backend  | `internal/telemetry`             |
| RAG embedding cache / indexer        | `internal/rag`                   |

Existing functions map 1:1 to those packages. The handler is the only
public entry point — keep middleware and router free of `net/http`
concerns so they stay unit-testable. Telemetry's `Recorder` interface is
the only seam: swap the JSONL implementation for SQLite by satisfying
`Record(Record)` + `Close()` without touching the handler.