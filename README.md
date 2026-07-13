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
- **SQLite metrics store (issue #4).** Per-request routing and
  savings events land in a local SQLite database
  (`$XDG_CACHE_HOME/nexus-proxy/metrics.db` by default, override via
  `NEXUS_METRICS_DB`). One row per proxied request with route,
  model, input/output tokens, TOON savings, RAG injection, and
  rough frontier-cost estimate. Writes are pushed onto a buffered
  channel consumed by a background goroutine so the request path
  never blocks on disk I/O. The store also satisfies
  `telemetry.Recorder` for backwards compatibility with the v0
  JSONL pipeline.

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

> **Graceful degradation (issue #8).** If Ollama becomes unreachable
> after the proxy has booted, a background health poller pings
> `GET /api/tags` every `NEXUS_HEALTH_POLL_INTERVAL` (default 30s).
> After `NEXUS_HEALTH_BREAKER_THRESHOLD` (default 3) consecutive
> failed probes the breaker trips and:
>
> - `route=local` requests are rerouted to the configured frontier
>   endpoint (the local step is omitted from the cascade).
> - `route=fusion` requests still run the arbiter synthesis, but
>   the local panel member is skipped — the arbiter sees a synthetic
>   "[local failed: ollama unavailable (degraded)]" marker and
>   synthesises from the frontier candidate alone.
> - Every proxied response carries `X-Nexus-Degraded: true` while
>   the breaker is open (and `X-Nexus-Degraded: false` once Ollama
>   recovers — the breaker reopens on the first successful probe).
>
> Set `NEXUS_HEALTH_POLL_INTERVAL=0` to disable the poller entirely
> (the proxy then assumes local Ollama is always healthy and will
> pay the per-request upstream timeout when it isn't).

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

### Running behind a reverse proxy (nginx / Cloudflare / cloud LB)

When Nexus sits behind a reverse proxy, the proxy appends the real
client IP to `X-Forwarded-For` on each request. By default Nexus
**ignores** that header and uses the direct peer IP — so a direct
attacker cannot spoof per-client rate limits by rotating the header.

To honour `X-Forwarded-For` / `X-Real-IP`, whitelist the reverse
proxy's CIDR via `NEXUS_TRUSTED_PROXIES`. Forwarded headers are then
honoured **only** when the direct peer is in the list:

```bash
# Trust the private networks your reverse proxy uses.
NEXUS_TRUSTED_PROXIES=10.0.0.0/8,172.16.0.0/12
# Or a single loopback proxy:
NEXUS_TRUSTED_PROXIES=127.0.0.1
```

A multi-hop `X-Forwarded-For` chain is walked right-to-left, skipping
trusted hops, to find the first untrusted (real client) IP — the same
algorithm nginx's `real_ip_recursive` uses.

The boot log emits a warning when rate limiting is enabled, the bind
address is non-loopback, and no trusted proxies are configured — that
combination usually means a forgotten reverse-proxy whitelist (so every
client behind the proxy shares one bucket).

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

## Docker

A multi-stage `Dockerfile` ships at the repo root: stage 1 builds a static
binary in `golang:1.21-alpine` and stage 2 copies it into
[`gcr.io/distroless/static-debian12:nonroot`](https://github.com/GoogleContainerTools/distroless).
The final image runs as UID 65532 with no shell and no package manager,
uses env-only configuration, and listens on `:8000`. Final image size is
well under 30 MB.

### Run the prebuilt image

```bash
docker run --rm -p 8000:8000 \
  -e NEXUS_FRONTIER_API_KEY=sk-... \
  -e NEXUS_OLLAMA_URL=http://host.docker.internal:11434 \
  ghcr.io/anchapin/nexus-proxy:latest
```

Then point your agent at `http://localhost:8000/v1` (see
[Point your agent at the proxy](#point-your-agent-at-the-proxy)).

`host.docker.internal` is how the container reaches Ollama running on
the host. On Linux without Docker Desktop you may need
`--add-host=host.docker.internal:host-gateway` on the `docker run` line.

### Build it locally

```bash
docker build -t nexus-proxy:dev .
docker run --rm -p 8000:8000 \
  -e NEXUS_FRONTIER_API_KEY=sk-... \
  nexus-proxy:dev
```

### Compose (proxy + Ollama)

For a fully-local stack, including Ollama in the same network:

```bash
NEXUS_FRONTIER_API_KEY=sk-... docker compose up -d
docker compose exec ollama ollama pull qwen3-coder:4b qwen3-coder:8b nomic-embed-text
```

The proxy container talks to the `ollama` service by name over the
compose network — leave `NEXUS_OLLAMA_URL` unset and the default in
`docker-compose.yml` will kick in.

### Health check

The proxy serves `GET /healthz` returning `ok`. The Dockerfile uses
`HEALTHCHECK NONE` (distroless has no shell/curl) so orchestrators like
Kubernetes or Compose should probe `/healthz` from outside the image.
For Compose users, the bundled `ollama` service already healthchecks
itself; the proxy's own liveness is the operator's responsibility.

### Persisting state

Two paths the proxy writes to at runtime:

| Path (env var)            | Default                       | Persist with |
| ------------------------- | ----------------------------- | ------------ |
| `NEXUS_TELEMETRY_PATH`    | `./nexus-telemetry.jsonl`     | `/tmp` in the container is writable but not persisted — bind-mount `/tmp` or set the env var to a mounted volume |
| `NEXUS_EXAMPLES_DIR`      | `./few_shot_examples`         | Bind-mount a host directory so your curated snippets survive container restarts |

Example:

```bash
docker run --rm -p 8000:8000 \
  -v "$PWD/few_shot_examples:/few_shot_examples" \
  -e NEXUS_EXAMPLES_DIR=/few_shot_examples \
  -e NEXUS_FRONTIER_API_KEY=sk-... \
  nexus-proxy:dev
```

## Releases

Every semver tag (`v1.0.0`, `v1.2.3`, …) triggers the
[release workflow](.github/workflows/release.yml), which publishes:

- **Cross-compiled binaries** — `linux/amd64`, `linux/arm64`,
  `darwin/arm64`
- **SHA256 checksums** — `checksums-sha256.txt`
- **GHCR multi-arch image** — `ghcr.io/anchapin/nexus-proxy:<tag>`
  (amd64 + arm64), also tagged `latest`
- **SBOM** — SPDX JSON attached to the release
- **Cosign signature** — keyless (OIDC) signature on the image,
  recorded in the Rekor transparency log

### Install a prebuilt binary

```bash
# Download the binary for your platform from the GitHub Release page,
# then verify its checksum:
sha256sum -c checksums-sha256.txt

# Make it executable and run:
chmod +x nexus-*-linux-amd64
./nexus-*-linux-amd64 --version
```

### Pull the container image

```bash
# Replace <tag> with a release version (e.g. v1.0.0) or "latest"
docker pull ghcr.io/anchapin/nexus-proxy:<tag>

# Check the version:
docker run --rm ghcr.io/anchapin/nexus-proxy:<tag> --version
```

### Verify the image signature

The image is signed with cosign keyless signing. Verify it against the
GitHub Actions OIDC identity:

```bash
cosign verify ghcr.io/anchapin/nexus-proxy:<tag> \
  --certificate-identity "https://github.com/anchapin/nexus-proxy/.github/workflows/release.yml@refs/tags/<tag>" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
```

### `--version`

```bash
$ ./nexus --version
nexus v1.0.0
```

`--version` (or `-v`) prints the build version and exits. The version is
injected at compile time via `-ldflags`; a local `make build` reports
`nexus dev` unless you override it with `make build VERSION=v1.2.3`.

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
  judge/                   # async LLM-as-a-judge evaluator (issue #15)
  quality/                 # async AST/compiler verifier (issue #13)
  metrics/                 # SQLite-backed metrics store (issue #4)
  telemetry/               # JSONL recorder (issue #16)
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

## Security & CI

The following automated checks run on every pull request and push to
`main`:

| Check | Workflow | What it catches |
| ----- | -------- | --------------- |
| **CI** (vet, build, test, lint, bench) | [`ci.yml`](.github/workflows/ci.yml) | Build breakage, race conditions, lint regressions |
| **Coverage gate** | [`ci.yml`](.github/workflows/ci.yml) | Fails if total coverage drops below `70%` |
| **CodeQL** | [`codeql.yml`](.github/workflows/codeql.yml) | SAST for Go — taint flow, hard-coded creds, SQL injection (`security-and-quality` pack) |
| **Secret scan** | [`secret-scan.yml`](.github/workflows/secret-scan.yml) | `gitleaks` scan for accidentally committed API keys / tokens |
| **Dependabot** | [`dependabot.yml`](.github/dependabot.yml) | Weekly update PRs for Go modules + GitHub Actions |

The coverage threshold is deliberately conservative so the gate
activates without churning existing PRs; raise `COVERAGE_THRESHOLD` in
`ci.yml` as coverage improves. `make ci` (local) stays green — the new
jobs are workflow-only.

## Configuration

All knobs are env vars; see `.env.example` for the full list with
defaults. The most useful ones:

| Variable                  | Default                       | Purpose                                  |
| ------------------------- | ----------------------------- | ---------------------------------------- |
| `NEXUS_ADDR`              | `:8000`                       | HTTP listen address                      |
| `NEXUS_SERVER_READ_TIMEOUT` | `30s`                       | Inbound request read deadline (issue #77) |
| `NEXUS_SERVER_WRITE_TIMEOUT` | `0` (disabled)             | Inbound response write deadline; 0 = streaming-safe (issue #77) |
| `NEXUS_SERVER_IDLE_TIMEOUT` | `120s`                      | Keep-alive idle wait (issue #77)         |
| `NEXUS_SERVER_MAX_HEADER_BYTES` | `1048576` (1 MiB)        | Max request header bytes (issue #77)     |
| `NEXUS_FRONTIER_API_KEY`  | *(empty)*                     | Required for frontier routing to work    |
| `NEXUS_TOKEN_GUARDRAIL`   | `6000`                        | Estimated tokens that force-route to frontier |
| `NEXUS_RAG_THRESHOLD`     | `0.55`                        | Cosine similarity floor for RAG injection |
| `NEXUS_SLM_TIMEOUT`       | `8s`                          | Qwen3-Coder routing call timeout         |
| `NEXUS_FUSION_TIMEOUT`    | `120s`                        | Per-panel-member timeout in fusion       |
| `NEXUS_JUDGE_SAMPLE_RATE` | `0.1`                         | Fraction of local-route completions judged |
| `NEXUS_JUDGE_CONCURRENCY` | `2`                           | Max simultaneous judge calls            |
| `NEXUS_JUDGE_URL`         | z.ai (fallback frontier)      | Judge endpoint                          |
| `NEXUS_JUDGE_API_KEY`     | `NEXUS_FRONTIER_API_KEY`      | Judge bearer token                      |
| `NEXUS_METRICS_DB`        | `~/.cache/nexus-proxy/metrics.db` | SQLite metrics store (issue #4)      |
| `NEXUS_TRUSTED_PROXIES`   | *(empty = trust nobody)*      | CIDR allowlist for X-Forwarded-For (issue #75) |
| `NEXUS_RATE_LIMIT_RPM`    | `0` (disabled)                | Per-client requests/min ceiling (issue #75) |
| `NEXUS_RATE_LIMIT_BURST`  | `0` (= RPM)                  | Token-bucket burst capacity (issue #75) |
| `NEXUS_COST_BASELINE_PROVIDER` | `frontier`               | Provider name for the cost-avoidance baseline |
| `NEXUS_COST_BASELINE_MODEL`   | `NEXUS_FRONTIER_MODEL`    | Model name for the cost-avoidance baseline |
| `NEXUS_COST_BASELINE_RATE_PER_1K` | `NEXUS_FRONTIER_COST_PER_1K` | USD per 1k tokens for baseline valuation |

## Cost Savings

Nexus Proxy records a **savings USD** figure for every proxied request, so
operators can track cost-avoidance over time.

### How savings are calculated

The formula is:

```
savings_usd = max(baseline_cost - actual_cost, 0)
```

**Baseline cost** is what the request *would have* cost if sent to the
configured baseline provider at the baseline rate — regardless of which
route the request actually took. It is computed from the total token count
(input + output) at the baseline rate, so even a `route=local` request
that incurred zero actual cost still shows a positive baseline.

**Actual cost** is the real cost incurred:

| Route       | Actual cost basis                                     |
| ----------- | ---------------------------------------------------- |
| `local`     | $0 (Ollama is free; GPU is a sunk cost)              |
| `frontier`  | frontier rate × total tokens                         |
| `fusion`    | $0 — both panel members run locally; the arbiter is a fast local call |

Because actual cost for `local` and `fusion` is $0, the full baseline
cost appears as savings. For `frontier` requests the savings is
`baseline − frontier`, which is zero when both use the same rate.

### Baseline configuration

Three env vars control the baseline:

| Variable                        | Purpose                                          |
| ------------------------------- | ------------------------------------------------ |
| `NEXUS_COST_BASELINE_PROVIDER` | Provider name for baseline valuation (default: `frontier`) |
| `NEXUS_COST_BASELINE_MODEL`    | Model name for baseline (default: `NEXUS_FRONTIER_MODEL`) |
| `NEXUS_COST_BASELINE_RATE_PER_1K` | USD per 1k tokens for baseline valuation (default: `NEXUS_FRONTIER_COST_PER_1K`) |

Set `NEXUS_COST_BASELINE_RATE_PER_1K` to your actual frontier provider's
per-token rate to make the savings figure realistic. If the rate is `0`
or unset, baseline cost is recorded as `0` and the savings field is
omitted for that request.

### Currency and precision

All monetary values are in **USD**. The `savings_usd` column in the
SQLite metrics store holds values with 6 decimal places of precision
(e.g. `0.001200`). The `nexus-dashboard` command rounds to 2 decimal
places for display.

### Aggregation

The SQLite metrics store (`NEXUS_METRICS_DB`) persists one row per
request with `baseline_cost_usd`, `actual_cost_usd`, and `savings_usd`.
Sum `savings_usd` over any time window to obtain the total cost
avoided. The `nexus-dashboard` command (`cmd/nexus-dashboard/main.go`)
produces a daily rollup using this data.

## Status

This is the Phase 1 refactor (see `Nexus Proxy PRD and Architecture.md`).
Phase 2 is landing incrementally: structured logging (#9, MIT-licensed),
the SQLite metrics store (#4), and the savings dashboard are still in
progress. The metrics DB lives in the user's cache directory by default
(~/.cache/nexus-proxy/metrics.db on Linux, %LocalAppData%\nexus-proxy
\metrics.db on Windows); set `NEXUS_METRICS_DB` to relocate it or
empty the variable to disable.

## License

MIT — see [LICENSE](LICENSE).