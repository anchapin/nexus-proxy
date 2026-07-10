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
  handlers/                # chat.go: HTTP entry point for /v1/chat/completions
  middleware/              # toon.go, prompt_engine.go (prompt transforms)
  router/                  # dsl.go, slm.go, guardrails
  upstream/                # stream.go, fusion.go (panel + arbiter)
  rag/                     # in-memory vector store + Ollama embedder
few_shot_examples/         # (gitignored) user-curated snippets
.env.example               # all env vars with safe defaults
Makefile                   # build / test / lint / ci
.github/workflows/ci.yml   # CI: vet, build, test -race, golangci-lint
.golangci.yml              # lint config
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
  All 73 tests run in <1s with `-race`.

## Where to extend

For new behaviour, add tests first and follow the existing layout:

| New behaviour                        | Package                          |
| ------------------------------------ | -------------------------------- |
| New env var                          | `internal/config`                |
| New prompt transform                 | `internal/middleware`            |
| New routing rule                     | `internal/router`                |
| Different upstream protocol          | `internal/upstream`              |
| New HTTP endpoint                    | `internal/handlers`              |

Existing functions map 1:1 to those packages. The handler is the only
public entry point — keep middleware and router free of `net/http`
concerns so they stay unit-testable.