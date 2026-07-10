# Nexus Proxy

Hardware-aware AI routing gateway in Go. Intercepts OpenAI-compatible
`/v1/chat/completions` requests from coding agents (OpenCode, Aider, etc.),
optimizes the prompt (TOON compression, RAG few-shot, meta-prompting), and
routes to local Ollama or a frontier API based on complexity.

See `Nexus Proxy PRD and Architecture.md` for the full PRD and roadmap,
and `README.md` for the user-facing quickstart.

## Repo state

Phase 1 refactor is complete. The single-file prototype is gone; the
codebase now follows standard Go layout.

```
cmd/nexus/                 # main: wires config + handlers + starts HTTP
internal/
  config/                  # env loading, defaults, validation
  handlers/                # chat.go: HTTP entry point
  middleware/              # toon.go, prompt_engine.go
  router/                  # dsl.go, slm.go, guardrails
  upstream/                # stream.go, fusion.go (panel + arbiter)
  rag/                     # in-memory vector store + Ollama embedder
  judge/                   # async LLM-as-a-judge evaluator (issue #15)
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
| Judge scoring / sampling logic       | `internal/judge`                 |
| Judge persistence (SQLite, etc.)     | `internal/judge` (Storage impl)  |

Existing functions map 1:1 to those packages. The handler is the only
public entry point — keep middleware and router free of `net/http`
concerns so they stay unit-testable.