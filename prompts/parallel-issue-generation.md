# Parallel Issue Generation Prompt for Nexus Proxy

> **Purpose:** Spawn N sub-agents in parallel, each exploring a different
> domain of the codebase, to discover gaps and file high-quality GitHub
> issues that advance the project toward its vision.

---

## Orchestrator Instructions (for the lead agent)

1. **Read this entire prompt.** Every sub-agent receives Sections 2–5
   verbatim plus exactly **one** domain brief from Section 6.

2. **Spawn the sub-agents in parallel** — one `task` call per domain.
   Suggested count: 8 agents (adjust up/down as needed). Use
   `subagent_type: "explore"` for read-only investigation agents.

3. **Collect results.** Each sub-agent returns a JSON array of issue
   proposals. The orchestrator deduplicates, validates against existing
   closed issues, and files the survivors via `gh issue create`.

4. **Dedup pass (critical):** Before filing, the orchestrator must:
   ```bash
   gh issue list --state all --limit 200 --search "<keywords>" --json number,title,state
   ```
   for each proposed issue's core concept. **Do not file duplicates.**

5. **Rate-limit filing:** No more than 15 new issues per batch. Prioritize
   P0/P1 first. Stagger creation (sleep 2s between calls) to avoid GitHub
   secondary rate limits.

---

## Section 2 — Project Context (shared, read-only)

### What Nexus Proxy Is

A hardware-aware AI routing gateway written in Go (module
`github.com/anchapin/nexus-proxy`). It sits between AI coding agents
(OpenCode, Aider, OpenHands) and model backends (local Ollama, frontier
APIs). It intercepts OpenAI-compatible `/v1/chat/completions` requests,
optimizes the prompt (TOON compression, RAG injection, meta-prompting),
and routes to the cheapest capable model based on prompt complexity and
local VRAM constraints.

### Architecture at a Glance

```
Agent → [Auth] → [Security Headers] → [Rate Limit] → Prompt Pipeline:
  1. PromptEngineering (role/CoT injection)
  2. RAG Retrieve + Inject (cosine similarity)
  3. TOON Compression (JSON→CSV)
  4. Routing: Guardrail → DSL regex → SLM decide → Fusion
→ Local (Ollama) / Frontier (API) / Fusion (parallel + arbiter)
→ Stream SSE back to agent
```

### Key Subsystems (all in `internal/`)

| Package | Responsibility |
|---------|---------------|
| `config` | Env + YAML config with SIGHUP hot-reload |
| `handlers` | HTTP handlers, security headers, recovery |
| `middleware` | Prompt transforms (no net/http import) |
| `router` | Guardrail → DSL → SLM routing pipeline |
| `upstream` | Cascade, fusion panels, arbiter cache, recording |
| `rag` | SQLite-backed persistent RAG, HNSW index, watcher |
| `providers` | Multi-frontier registry + cost-latency selector |
| `health` | Ollama circuit breaker |
| `circuit` | Local-route cooldown after cascade failure |
| `concurrencylimit` | VRAM-aware local semaphore |
| `budget` | 24h rolling frontier spend cap |
| `ratelimit` | Client-IP API-key-aware rate limiter |
| `judge` | Async LLM-as-a-judge scoring |
| `quality` | Background cargo/tsc verifier |
| `tracing` | W3C trace context + OTLP exporter |
| `observability` | Prometheus metrics, route metrics, budget metrics |
| `metrics` | SQLite metrics store |
| `probe` | nvidia-smi / AMD sysfs VRAM probe |
| `diag` | Boot-time diagnostics (`nexus check`) |
| `tokenizer` | tiktoken wrapper |
| `transport` | Pooled HTTP client |
| `auth` | Inbound API-key middleware |

### PRD Roadmap Phases

- **Phase 1 (Foundation):** DONE — proper Go module structure, config system
- **Phase 2 (Observability):** DONE — slog, SQLite metrics, dashboard
- **Phase 3 (Hardware Awareness):** DONE — VRAM probing, graceful degradation, circuit breakers
- **Phase 4 (Open Source Release):** PARTIAL — Docker image exists, README exists, needs polish

### Conventions for Issues in This Repo

- **Title prefix:** `[ENHANCEMENT]` or `[bug]` or `[BUG]`
- **Body:** Problem description → Proposed solution → Alternatives considered → Acceptance criteria
- **Labels:** `bug`, `enhancement`, `documentation`, `testing`, `security`, `performance`
- **Every commit resolves an issue** via `fix: resolve #N — ...` or `feat: resolve #N — ...`
- **Conventional Commits** are enforced
- **Coverage floor is 70%**
- **golangci-lint v2.12.2** is the linter
- **Go 1.26** (CI pin), `go.mod` declares 1.25
- `develop` is the default branch; `main` is release-only

### What NOT to File Issues About (Already Implemented)

Do NOT propose issues for features that already exist. The following are
DONE and working:

- TOON compression (fenced + unfenced)
- Three-tier routing (DSL → SLM → Fusion)
- Ollama circuit breaker + health polling
- RAG with SQLite persistence + HNSW + embedder circuit breaker
- LLM-as-a-judge with adaptive routing feedback
- Budget cap (24h rolling)
- Rate limiting (per-IP + per-API-key, auth brute-force protection)
- Distributed tracing (OTLP/JSON)
- Prompt injection hardening (off/warn/strict modes)
- Config hot-reload via SIGHUP
- Multi-provider selector (cost-latency optimized)
- VRAM probe (nvidia-smi + AMD sysfs)
- Concurrency limiter (VRAM-aware semaphore)
- `nexus check` / `nexus doctor` diagnostics
- `nexus config validate`
- `nexus dashboard`
- `GET /v1/models` endpoint
- Prometheus metrics at `/metrics`
- Security headers (HSTS conditional on TLS)
- Graceful shutdown with drain timeout
- Progressive fusion delivery
- Arbiter synthesis cache
- SLM decision cache with semantic dedup
- Trusted-proxy client-IP resolution

---

## Section 3 — Output Format (mandatory)

Each sub-agent must return a JSON array of issue proposals:

```json
[
  {
    "title": "[ENHANCEMENT] <concise title>",
    "domain": "<assigned domain>",
    "priority": "P0|P1|P2|P3",
    "labels": ["enhancement"],
    "body": "## Problem\n\n<2-4 sentences describing the gap or opportunity>\n\n## Proposed Solution\n\n<detailed technical approach — specific packages, functions, env vars>\n\n## Alternatives Considered\n\n- <alt 1 and why rejected>\n- <alt 2 and why rejected>\n\n## Acceptance Criteria\n\n- [ ] <testable condition 1>\n- [ ] <testable condition 2>\n- [ ] <testable condition 3>\n- [ ] Tests added or updated\n- [ ] Documentation updated in `.env.example` / `AGENTS.md` / `docs/`\n\n## Context\n\n- **Files likely affected:** `internal/<pkg>/...`\n- **Config vars to add:** `NEXUS_...`\n- **Estimated effort:** S (1-2h) | M (half day) | L (1-2 days)"
  }
]
```

---

## Section 4 — Quality Rules (mandatory)

1. **No duplicates.** Search existing issues before proposing:
   ```bash
   gh issue list --state all --limit 50 --search "<keyword>" --json number,title
   ```

2. **No already-implemented features.** Re-read Section 2's "Already
   Implemented" list. If unsure whether something exists, check:
   ```bash
   rg -l "<feature_name>" --type go internal/
   ```

3. **Every issue must be independently actionable.** No issue should
   depend on another proposed issue being resolved first (unless the
   dependency is explicitly stated and the dependent issue is P0).

4. **Testable acceptance criteria.** Every issue must have ≥3 checkboxes
   that can be verified by running a command or inspecting output.

5. **Specific packages and files.** Every issue must name the exact
   `internal/` packages it touches and any new env vars it introduces.

6. **Scope discipline.** Each issue should be completable in <2 days by
   a single agent. If bigger, split it into multiple issues.

7. **Follow repo conventions:**
   - New env vars need BOTH config struct field + YAML mirror + `.env.example`
   - `internal/middleware` must NEVER import `net/http`
   - `internal/handlers` and `internal/upstream` must NEVER import `internal/judge` or `internal/quality`
   - Structured logging only (`log/slog`)
   - CGO-free pure-Go deps only

8. **Diversity of proposals.** Within your domain, cover at least 3
   distinct sub-areas. Don't propose 5 variations of the same thing.

9. **Realistic count.** Return 3–6 issues per domain. Quality over
   quantity.

---

## Section 5 — Investigation Methodology (per sub-agent)

Each sub-agent should:

1. **Read the code** in its assigned domain's packages. Use `read`,
   `grep`, and `glob` to understand current implementation.

2. **Check test coverage** gaps:
   ```bash
   go test -cover ./internal/<pkg>/... 2>/dev/null
   ```

3. **Look for TODOs/FIXMEs/HACKs:**
   ```bash
   rg -n 'TODO|FIXME|HACK|XXX|OPTIMIZE' --type go internal/<pkg>/
   ```

4. **Review recent git history** for context on what was recently fixed:
   ```bash
   git log --oneline -20 -- internal/<pkg>/
   ```

5. **Check `.env.example`** for knobs that are documented but not yet
   wired or have placeholder behavior.

6. **Read `docs/`** for documented-but-unimplemented features.

7. **Think about edge cases, failure modes, scaling limits, security
   gaps, developer ergonomics, and missing integrations.**

8. **Verify each idea isn't already done** before including it.

---

## Section 6 — Domain Briefs (one per sub-agent)

The orchestrator assigns each sub-agent exactly **one** domain brief
below. Copy the brief into the sub-agent's prompt.

---

### Domain A: Routing Intelligence & Decision Quality

**Scope:** `internal/router/`, `internal/upstream/`

Investigate:
- SLM routing accuracy — can the DSL pattern set be expanded for new
  programming languages, task types, or agent workflows?
- Routing feedback loop — how does judge quality data feed back into
  routing confidence? Are there blind spots?
- Fusion panel strategy — is progressive delivery optimal? Could
  weighted panel voting or tiered arbitration improve quality?
- Arbiter synthesis — are there cases where the arbiter adds latency
  without improving quality?
- Multi-turn conversation routing — does the router consider the full
  conversation history, or only the latest message?
- New routing tiers — should there be a "code review" or "security
  audit" route that always uses frontier?
- DSL pattern discoverability — can patterns be auto-discovered from
  historical routing outcomes instead of manually configured?

**Focus:** Issues that improve routing accuracy, reduce unnecessary
frontier spend, and handle more prompt categories correctly.

---

### Domain B: RAG & Knowledge Management

**Scope:** `internal/rag/`

Investigate:
- Embedding model lifecycle — is there a migration path when the
  embedding model changes? Does the HNSW index need rebuilding?
- Incremental indexing — does the watcher handle file moves/renames
  correctly, or just creates/deletes?
- Multi-language code support — does RAG indexing handle non-Go
  codebases (Python, TypeScript, Rust)?
- Cross-project RAG — could RAG indexes be shared across projects?
- Embedding dimension validation — what happens when different embedder
  models produce different vector sizes?
- RAG quality metrics — is there a way to measure RAG injection
  effectiveness (did the injected context actually help)?
- Batch embedding efficiency — can embedding throughput be improved?
- Vector store alternatives — should FAISS or a dedicated vector DB be
  supported as a backend?
- RAG filtering — can users scope which files/documents are indexed?

**Focus:** Issues that improve retrieval quality, index maintenance,
and support more diverse codebases.

---

### Domain C: Observability, Tracing & Metrics

**Scope:** `internal/observability/`, `internal/tracing/`,
`internal/metrics/`, `internal/telemetry/`

Investigate:
- Grafana dashboard templates — is there a pre-built dashboard JSON for
  the Prometheus metrics?
- Alerting rules — are there recommended alert thresholds (budget
  exceeded, circuit breaker open, high error rate)?
- Distributed tracing completeness — do all critical spans carry the
  right attributes? Are there missing spans in the routing pipeline?
- Metrics cardinality — could high-cardinality labels (per-model,
  per-prompt-hash) cause Prometheus memory bloat?
- pprof/debug endpoints — is there a secure way to expose runtime
  profiling?
- Savings dashboard improvements — can the CLI dashboard show weekly/
  monthly trends, not just daily?
- Log correlation — can trace IDs be injected into slog output for
  correlation?
- OpenTelemetry migration — should the custom OTLP exporter be replaced
  with the official OTel SDK?
- Health endpoint richness — does `/healthz` expose enough detail for
  load balancer integration?

**Focus:** Issues that make the system more observable, debuggable,
and operationally mature.

---

### Domain D: Security Hardening & Access Control

**Scope:** `internal/auth/`, `internal/ratelimit/`,
`internal/handlers/security.go`, `internal/middleware/injection.go`

Investigate:
- mTLS support — could the proxy require client certificates?
- OAuth2 / OIDC inbound auth — beyond static API keys, could the proxy
  validate JWTs from identity providers?
- Audit logging — is there a tamper-evident audit trail of who routed
  what and when?
- Secret management — is there a path to integrate with Vault, AWS
  Secrets Manager, or similar for API key storage?
- Response sanitization — could the proxy redact PII or secrets from
  upstream responses before returning to the agent?
- SSRF protection — are there guards against the proxy being used to
  reach internal network addresses?
- CSP completeness — does the Content-Security-Policy cover all
  necessary directives?
- Multi-tenant isolation — if multiple users share a proxy instance,
  is their data isolated?
- Rate limit evasion — are there bypass paths that skip rate limiting?

**Focus:** Issues that close security gaps and support enterprise
deployment scenarios.

---

### Domain E: Performance & Resource Management

**Scope:** `internal/concurrencylimit/`, `internal/transport/`,
`internal/probe/`, `internal/upstream/cascade.go`

Investigate:
- Memory profiling — has the proxy been profiled under load? Are there
  allocation hotspots in the streaming path?
- Connection pooling — is the HTTP client pool optimally sized for both
  local Ollama and frontier APIs?
- SSE buffering — is there unnecessary buffering in the streaming path
  that adds latency?
- Cache warming — can the SLM decision cache and arbiter cache be
  pre-warmed on boot from historical data?
- Goroutine leak detection — are there code paths that leak goroutines
  on error or timeout?
- VRAM probe overhead — how often is nvidia-smi polled? Can it use
  NVML shared library instead of subprocess invocation?
- Cascade timeout tuning — are cascade timeouts adaptive based on
  prompt size, or fixed?
- TOON compression CPU cost — for very large JSON arrays, is
  compression a measurable CPU bottleneck?
- Request coalescing — can identical in-flight requests be deduplicated?

**Focus:** Issues that reduce latency, memory footprint, and CPU
overhead in the hot path.

---

### Domain F: Developer Experience & Onboarding

**Scope:** `cmd/nexus/`, `README.md`, `CONTRIBUTING.md`, `docs/`,
`.env.example`, `config.yaml`

Investigate:
- Config wizard — could `nexus init` walk a new user through setup
  interactively (detect Ollama, test API keys, choose models)?
- Homebrew formula — is there a `brew install` path?
- IDE integration — could there be a VS Code extension or language
  server that surfaces routing decisions inline?
- Web dashboard — beyond the CLI dashboard, could there be a lightweight
  web UI for metrics and routing history?
- Migration guide — when breaking config changes happen, is there an
  automated migration path?
- Error message quality — do boot-time errors include actionable
  remediation hints?
- Quickstart templates — are there `docker-compose.yml` examples for
  common setups (Ollama + proxy + agent)?
- Config validation depth — does `nexus config validate` catch semantic
  errors (e.g., missing model in Ollama) or just syntax?
- `.env.example` completeness — are all vars documented with
  meaningful examples and comments?

**Focus:** Issues that reduce time-to-first-request and improve
day-to-day operator experience.

---

### Domain G: Provider Ecosystem & Frontier Integration

**Scope:** `internal/providers/`

Investigate:
- New provider support — are there providers that should be added
  (Groq, Together AI, Fireworks, DeepSeek, Mistral, Cohere Generate)?
- Streaming protocol divergence — do all providers implement SSE
  identically? Are there format incompatibilities?
- Provider health checking — is there a proactive health check for
  frontier providers, or only reactive failure detection?
- Cost model accuracy — is the per-provider cost model up to date with
  current pricing? Is there a way to auto-fetch pricing?
- Provider-specific features — should the proxy expose provider-specific
  capabilities (function calling, vision, long context)?
- Failover chains — when the primary frontier provider is down, does
  the selector automatically try alternatives?
- Token counting per provider — different providers count tokens
  differently; does the cost model account for this?
- Provider rate limit awareness — could the proxy respect
  provider-specific rate limits (e.g., Anthropic's 50 RPM on some tiers)?
- Model aliasing — can users define custom model names that map to
  provider-specific model IDs?

**Focus:** Issues that expand provider coverage and make multi-provider
routing more robust.

---

### Domain H: Testing, CI & Quality Assurance

**Scope:** All test files, `.github/workflows/`, `Makefile`,
`internal/tracingtest/`, `internal/testutil/`

Investigate:
- Integration test suite — are there end-to-end tests that exercise the
  full routing pipeline with a mock Ollama and mock frontier?
- Property-based testing — could fuzz testing catch edge cases in TOON
  compression, DSL regex matching, or RAG similarity scoring?
- Benchmark regression detection — does CI track benchmark results over
  time and flag regressions?
- Test fixture management — are large test fixtures stored efficiently,
  or checked into git as raw files?
- Chaos testing — could the CI inject failures (Ollama crash, network
  partition, slow responses) to verify degradation paths?
- Mutation testing — has mutation testing been considered for critical
  packages (router, upstream, budget)?
- Coverage gaps — are there packages below the 70% floor?
- Race condition testing — beyond `make test-race`, are there
  stress-test scenarios that exercise concurrent access patterns?
- Release pipeline — is there an automated release process (goreleaser,
  GitHub Releases with changelog)?

**Focus:** Issues that strengthen the testing pyramid, catch
regressions earlier, and automate release quality.

---

## Section 7 — Orchestrator Post-Processing

After all sub-agents return their issue arrays:

1. **Merge** all arrays into a single list.

2. **Deduplicate** by semantic similarity:
   - Compare titles + body keywords across all proposals.
   - If two issues target the same gap, keep the more specific one.

3. **Cross-check against closed issues:**
   ```bash
   gh issue list --state closed --limit 500 --json number,title | \
     python3 -c "import json,sys; [print(f'#{i[\"number\"]} {i[\"title\"]}') for i in json.load(sys.stdin)]"
   ```

4. **Sort by priority:** P0 → P1 → P2 → P3.

5. **Cap at 15 issues** per batch (avoids issue tracker flooding).

6. **File each issue:**
   ```bash
   gh issue create \
     --title "[ENHANCEMENT] ..." \
     --body-file /tmp/issue-body.md \
     --label "enhancement"
   ```

7. **Report a summary table:**

   | # | Title | Domain | Priority |
   |---|-------|--------|----------|
   | 1147 | ... | Routing | P1 |
   | 1148 | ... | RAG | P0 |
   | ... | ... | ... | ... |

---

## Example Sub-Agent Prompt (copy-paste template)

```
You are a code analysis sub-agent for the Nexus Proxy project.

Your domain: **<DOMAIN NAME>**
Your scope: <SCOPE PACKAGES>

Read the project context and conventions below, then investigate your
domain's packages to find gaps, improvement opportunities, and missing
features.

PROJECT CONTEXT:
<paste Section 2>

OUTPUT FORMAT:
<paste Section 3>

QUALITY RULES:
<paste Section 4>

INVESTIGATION METHOD:
<paste Section 5>

YOUR DOMAIN BRIEF:
<paste the specific domain brief from Section 6>

Return ONLY a JSON array of 3–6 issue proposals. Do not file the issues
yourself — just return the proposals. Be specific, be technical, and
verify each idea isn't already implemented before proposing it.
```
