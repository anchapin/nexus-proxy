# Nexus Proxy

Hardware-aware AI routing gateway in Go. Intercepts OpenAI-compatible
`/v1/chat/completions` requests from coding agents (OpenCode, Aider, etc.),
optimizes the prompt (TOON compression, RAG few-shot, meta-prompting), and
routes to local Ollama or a frontier API based on complexity.

See `Nexus Proxy PRD and Architecture.md` for the full PRD, planned module
layout, and roadmap.

## Repo state — important

This is a **single-file prototype** committed as-is. It is not yet the
standard Go layout the PRD targets.

- One Go source: `harness_agnostic_go_router_proxy.go` (package `main`,
  ~640 lines, includes router, middleware, streaming, RAG, fusion all in one).
- **No `go.mod`, no `go.sum`, no tests, no README, no `.env`, no CI, no lint
  config.**
- Git: single branch `main`, remote `https://github.com/anchapin/nexus-proxy.git`,
  one initial commit. No tags, no release process.
- PRD roadmap Phase 1 is "break single file into standard Go module structure"
  and "extract hardcoded configs into `config.yaml` / `.env`". **The next
  feature work is the refactor, not new features** — appending to the
  single file moves the codebase away from the planned layout.

## Build / run

There is no module file. Two ways to run:

```bash
# From the repo root, with a modern Go toolchain (1.21+ per PRD):
go run harness_agnostic_go_router_proxy.go

# Or after `go mod init github.com/anchapin/nexus-proxy` and splitting files.
```

The binary listens on `:8000` (hardcoded `Port` constant) and exposes exactly
one endpoint: `POST /v1/chat/completions`. No `/healthz`, no metrics.

## Prerequisites before running

1. **Ollama running locally** on `http://localhost:11434` (hardcoded
   `LocalOllamaURL`). Override the constant if it lives elsewhere.
2. **Models pulled in Ollama:**
   - `qwen3-coder:4b` — routing SLM
   - `qwen3-coder:8b` — local execution model
   - `nomic-embed-text` — embeddings for RAG (RAG indexer logs `[RAG ERROR]`
     and silently skips files if this model is missing)
3. **Frontier API key.** `FrontierAPIKey = "your-api-key-here"` in the source
   is a placeholder. Replace it (or refactor to read from env — that's on the
   roadmap) before anything actually hits `api.openai.com`, or `localRes`
   requests to frontier will 401.

## Routing logic — quick reference

Decided in `evaluateDSL` first, then `getSLMRoutingDecision` (SLM) as fallback.
Output values: `"local"`, `"frontier"`, `"fusion"`.

| Trigger                                              | Route     |
| ---------------------------------------------------- | --------- |
| Estimated prompt tokens > 6000 (`len/4` heuristic)   | `frontier` (force — VRAM guardrail) |
| Prompt contains `"architectural design"` or `"system architecture"` | `fusion` |
| Prompt matches regex `\b(css\|format\|docstring\|lint\|typo\|boilerplate)\b` (case-insensitive) | `local` |
| Otherwise                                            | SLM decides (Qwen3-Coder-4B) |
| SLM call fails, times out (8s), or returns invalid JSON | `frontier` (safe default) |

Fusion runs local + frontier in parallel (`fetchPanelMember`, 120s timeout each),
then asks the frontier model to synthesize the two responses back.

## TOON compression gotchas

`serializeToTOON` rewrites JSON arrays into a CSV-like format
(`items[N]{k1,k2}:\n  v1,v2`). Two non-obvious behaviours:

- **Commas in values are replaced with full-width `，` (U+FF0C)** to protect
  the CSV structure. Downstream code that re-parses TOON must handle this.
- **Newlines in values are replaced with spaces.** Multi-line strings round-trip
  lossy.
- The regex only fires on `\`\`\`json\n[ ... ]\n\`\`\`` fenced blocks inside
  `user`/`assistant` message content (`jsonArrayBlockRegex`).

## Middleware order (do not reorder casually)

In `chatCompletionsHandler`:

1. `applyPromptEngineering` — injects role/CoT/constraints into system prompt
2. `applyRetrievalAugmentation` — embeds latest user prompt, finds best
   cosine match in `fewShotStore` (threshold 0.55), injects into last user msg
3. `optimizePromptContext` — TOON compresses JSON arrays, appends TOON
   instructions to system prompt
4. Then `evaluateDSL` → `getSLMRoutingDecision`
5. Then stream (`local` / `frontier`) or fusion (`fusion`)

RAG requires Ollama + `nomic-embed-text` reachable; if embedding fails the
prompt is passed through unchanged (silent skip, just a log line).

## Style / workflow notes

- Go 1.21+ per PRD. No formatting/lint tooling configured — if you add `gofmt`
  or `golangci-lint`, document it in this file.
- `few_shot_examples/` directory is auto-created on first run if missing
  (`initRAGIndex` `os.Mkdir`). Per PRD it should be gitignored; currently it
  isn't, so don't commit example files accidentally.
- All upstream HTTP traffic uses `http.DefaultClient`. No timeouts configured
  globally — only the SLM (8s) and fusion members (120s) get per-request
  contexts. Other calls inherit `DefaultClient`'s zero timeout.
- No structured logging — plain `log.Println` with bracketed prefixes
  (`[ROUTER]`, `[RAG INDEXER]`, etc.). PRD Phase 2 swaps this for `slog`.

## Where to extend

For new behaviour, follow the PRD's planned layout (don't keep growing the
single file):

```
cmd/nexus/main.go
internal/config/        # YAML + .env parsing — replaces hardcoded constants
internal/handlers/      # chat.go (HTTP handler)
internal/middleware/    # toon.go, prompt_engine.go, rag.go
internal/router/        # dsl.go, slm.go, guardrails.go
internal/upstream/      # stream.go, fusion.go
```

Existing functions map 1:1 to those packages (see function names above).