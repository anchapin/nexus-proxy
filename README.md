# Nexus Proxy

Hardware-aware AI routing gateway in Go. Sits between your coding agent
(OpenCode, Aider, OpenHands) and your model providers: intercepts
OpenAI-compatible `/v1/chat/completions`, optimizes the prompt, and routes
each request to either a local Ollama model or a frontier API based on
complexity.

## Features

- **Harness-agnostic.** Any tool that speaks the OpenAI chat completions
  protocol works. Point it at `http://localhost:8000` instead of
  `api.openai.com`.
- **Hardware guardrails.** Estimates token cost from prompt length and
  force-routes oversized requests to the frontier API to avoid local
  VRAM OOMs.
- **TOON compression.** Detects fenced ` ```json [...] ``` ` blocks in
  messages and rewrites them as Token-Oriented Object Notation
  (`items[N]{k1,k2}:\n  v1,v2`), saving context tokens on
  data-heavy outputs.
- **In-memory RAG.** Indexes code snippets from `few_shot_examples/` and
  injects the highest-cosine-similarity example into prompts that look
  similar.
- **Meta-prompting.** Appends role/CoT/constraint instructions to the
  system prompt so the model behaves like a senior engineer from the
  first token.
- **Three-tier routing.**
  1. DSL regex fast-pass for obvious cases (formatting → local,
     architecture → fusion).
  2. SLM fallback (Qwen3-Coder-4B) for anything the DSL doesn't catch.
  3. Fusion panel — local and frontier run in parallel; a frontier
     arbiter synthesizes the final answer.
- **Async LLM-as-a-Judge.** Samples ~10% of completed local-route
  requests and asks a frontier endpoint for a 1–5 score on the
  model's output. Bounded concurrency (default 2 simultaneous calls)
  and a buffered drop-on-overflow queue so the judge can never stall
  the chat hot path. Disabled by default; enable via
  `NEXUS_JUDGE_SAMPLE_RATE > 0`. The judge output is a structured
  `JudgeScore` record ready to be persisted by a future telemetry
  layer.

## Quickstart

### Prerequisites

- Go 1.21+
- [Ollama](https://ollama.com) running locally on `:11434`
- The following models pulled:
  ```bash
  ollama pull qwen3-coder:4b      # routing SLM
  ollama pull qwen3-coder:8b      # local execution model
  ollama pull nomic-embed-text    # embeddings for RAG
  ```
- A frontier API key (OpenAI or any OpenAI-compatible endpoint)

### Configure

```bash
cp .env.example .env
# Edit .env and set NEXUS_FRONTIER_API_KEY
```

### Build and run

```bash
make build && ./bin/nexus
```

Or directly:

```bash
go run ./cmd/nexus
```

The server listens on `:8000` by default. Set `NEXUS_ADDR=:9000` (or any
other address) to change it.

### Point your agent at the proxy

In OpenCode's `~/.config/opencode/config.toml`:

```toml
[provider.openai]
baseURL = "http://localhost:8000/v1"
apiKey = "any-non-empty-string"
```

Replace `baseURL` with whatever your agent uses for the OpenAI provider.

### Add few-shot examples

Drop code snippets into `few_shot_examples/`. They're indexed at boot via
`nomic-embed-text` and injected into prompts whose cosine similarity is
above `NEXUS_RAG_THRESHOLD` (default 0.55).

## Architecture

```
cmd/nexus/                 # main: wires config + handlers + starts HTTP
internal/
  config/                  # env loading, defaults, validation
  handlers/                # chat.go: HTTP entry point
  middleware/              # toon.go, prompt_engine.go
  router/                  # dsl.go, slm.go, guardrails
  upstream/                # stream.go, fusion.go (panel + arbiter)
  rag/                     # in-memory vector store + Ollama embedder
  judge/                   # async LLM-as-a-judge evaluator
few_shot_examples/         # (gitignored) user-curated snippets
.env.example               # all env vars with safe defaults
Makefile                   # build / test / lint / ci
```

### Request lifecycle

1. **Ingress.** `POST /v1/chat/completions` (OpenAI spec).
2. **Middleware chain (in order — do not reorder):**
   1. `applyPromptEngineering` — inject role/CoT/constraints into system
   2. `applyRetrievalAugmentation` — embed latest user prompt, inject
      best cosine match
   3. `optimizePromptContext` — TOON-compress JSON arrays in user msgs,
      append TOON notice to system
3. **Routing.** DSL → SLM fallback. Failed SLM → frontier (safe default).
4. **Execution.** Stream local, stream frontier, or run the fusion panel.

## Development

```bash
make test       # unit tests
make test-race  # race detector
make lint       # golangci-lint
make ci         # vet + build + test + lint (what CI runs)
```

The race detector and a healthy test suite are required to merge — see
`.github/workflows/ci.yml`.

## Configuration

All knobs are env vars; see `.env.example` for the full list with
defaults. The most useful ones:

| Variable                  | Default                       | Purpose                                  |
| ------------------------- | ----------------------------- | ---------------------------------------- |
| `NEXUS_ADDR`              | `:8000`                       | HTTP listen address                      |
| `NEXUS_FRONTIER_API_KEY`  | *(empty)*                     | Required for frontier routing to work    |
| `NEXUS_TOKEN_GUARDRAIL`   | `6000`                        | Estimated tokens that force-route to frontier |
| `NEXUS_RAG_THRESHOLD`     | `0.55`                        | Cosine similarity floor for RAG injection |
| `NEXUS_SLM_TIMEOUT`       | `8s`                          | Qwen3-Coder routing call timeout         |
| `NEXUS_FUSION_TIMEOUT`    | `120s`                        | Per-panel-member timeout in fusion       |
| `NEXUS_JUDGE_SAMPLE_RATE` | `0.1`                         | Fraction of local-route completions judged |
| `NEXUS_JUDGE_CONCURRENCY` | `2`                           | Max simultaneous judge calls            |
| `NEXUS_JUDGE_URL`         | z.ai (fallback frontier)      | Judge endpoint                          |
| `NEXUS_JUDGE_API_KEY`     | `NEXUS_FRONTIER_API_KEY`      | Judge bearer token                      |

## Status

This is the Phase 1 refactor (see `Nexus Proxy PRD and Architecture.md`).
Phase 2 (structured logging, SQLite metrics, savings dashboard) is the
next planned body of work.

## License

MIT — see [LICENSE](LICENSE).