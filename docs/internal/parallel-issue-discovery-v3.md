# Nexus Proxy — Parallel Issue Discovery Orchestrator Prompt

A prompt for spawning **6 parallel sub-agents** to discover, score, and propose
new GitHub issues that advance `nexus-proxy` toward its product goals.

---

## Orchestrator Setup

### Repository Facts

| Field | Value |
|-------|-------|
| **Repo** | `anchapin/nexus-proxy` |
| **Working directory** | `/home/alex/AI/nexus-proxy` |
| **Language / CI pin** | Go 1.26 |
| **Build** | `make build` → `./bin/nexus` |
| **CI gate** | `make ci` — vet → build → `go test -race` → golangci-lint → bench-short |
| **Coverage floor** | 70 % total (CI fails below) |
| **Subcommands** | `nexus check` (doctor), `nexus config validate`, `nexus dashboard`, `nexus --version` |
| **Critical constraint** | `internal/handlers` and `internal/upstream` must **never** import `internal/judge` or `internal/quality` — they hook in via `JudgeObserver`/`QualityObserver` function-typed fields on `handlers.Deps` |

### Five Product Phases

| Phase | Description | Status | Key Remaining Work |
|-------|-------------|--------|---------------------|
| 1 | Foundation & Module Structure | Mostly complete | Tech-debt cleanup; dead-code removal |
| 2 | Observability & Metrics | Partial | Tracing queue gauge, Grafana dashboard, SQLite `auto_vacuum` |
| 3 | Hardware Awareness | Partial | Temperature-based throttling, dynamic VRAM probe improvements |
| 4 | OSS Release | Partial | Cross-compiled binaries with SLSA, Windows binary, Docker smoke tests |
| 5 | Intelligence / DX / Production | Active | RAG hardening, judge-guided routing, DSL Unicode case folding, SLM cache correctness |

### Six Focus Areas

| Area | Letter | Primary Packages | Adjacent Recent Work |
|------|--------|------------------|----------------------|
| RAG & Vector Store | **A** | `internal/rag/`, `internal/middleware/rag.go` | Batch embedding (#781), PersistentStore fixes, HNSW benchmarks |
| Routing Intelligence | **B** | `internal/router/`, `internal/judge/` | Routing-preview (#783), adaptive confidence (#784), arbiter cache (#782), SLM cache fixes |
| Observability & Tracing | **C** | `internal/observability/`, `internal/tracing/`, `internal/telemetry/` | Latency percentiles (#777), Prometheus histograms, queue gauges |
| Upstream & Cascade | **D** | `internal/upstream/`, `internal/handlers/chat.go` | Cascade fallback (#765, #767), MaxResponseBytes (#742, #766) |
| Security & Auth | **E** | `internal/auth/`, `internal/ratelimit/`, `internal/handlers/security.go`, `internal/handlers/recover.go` | API-key rate limiting (#778), brute-force limiter, panic redaction |
| Config & Developer Experience | **F** | `internal/config/`, `internal/middleware/toon.go`, `internal/middleware/injection.go` | TOON fixes (#780), prompt injection hardening, `.env.example` audit |

### Deduplication Checklist (run before spawning)

1. `gh issue list --state all --limit 200` — see every open/closed issue
2. `gh issue list --search "<title keywords>"` — verify each proposed title is new
3. `gh pr list --state merged --limit 50` — confirm no recent duplicate work
4. Cross-check the **Known Open Issues** table in the sub-agent prompt below

---

## Spawn Instructions

1. Spawn **all 6 sub-agents simultaneously** in a single message using the `task`
   tool with `subagent_type: general-purpose`.
2. Substitute `{{FOCUS_AREA}}` and `{{AREA_LETTER}}` in the sub-agent prompt
   template for each agent.
3. Each sub-agent **investigates and proposes only** — it does not file issues,
   create branches, or write code.
4. Collect all 6 responses, de-duplicate across areas, rank by priority, and
   cap at **3–4 issues per focus area**.

---

## Sub-Agent Prompt Template

---

You are **Sub-agent {{AREA_LETTER}}** conducting an issue discovery sweep over the
`nexus-proxy` repository — a hardware-aware AI routing gateway in Go.

**Your focus area**: `{{FOCUS_AREA}}`
**Your area letter**: `{{AREA_LETTER}}`
**Your job**: Propose **3 to 4 new GitHub issues** that would meaningfully advance the project.

---

### What `nexus-proxy` Does

Nexus Proxy sits between AI coding agents (OpenCode, Aider, OpenHands) and model
providers. It intercepts OpenAI-compatible `/v1/chat/completions` requests,
optimizes prompts (TOON compression, RAG injection, meta-prompting), and routes
each request to either a local Ollama model or a frontier API based on complexity.

**Core routing pipeline** (`internal/router`):
1. **Guardrail** — force-route oversized prompts to frontier (VRAM OOM prevention)
2. **DSL** — regex fast-pass for obvious patterns (formatting → local, architecture → fusion)
3. **SLM** — Qwen3-Coder-4B semantic routing decision
4. **Execution** — local stream, frontier stream, or fusion panel (parallel + arbiter synthesis)

**Key packages**:
- `cmd/nexus/` — main entry point, wires config → middleware → handlers → HTTP server
- `internal/handlers/` — HTTP handlers (`chat.go`, `health.go`, `recover.go`, `security.go`, `sanitize.go`)
- `internal/middleware/` — prompt transforms (`toon.go`, `prompt_engine.go`, `rag.go`, `injection.go`) — **zero `net/http` imports**
- `internal/router/` — routing pipeline (`dsl.go`, `slm.go`, `slm_cache.go`, `guardrails.go`, `confidence.go`)
- `internal/upstream/` — upstream calls (`cascade.go`, `upstream.go`, `fusion.go`, `arbiter_cache.go`, `recording.go`)
- `internal/rag/` — vector store + persistent store + embedders (`store.go`, `persistent.go`, `embedders.go`, `watcher.go`)
- `internal/judge/` — async LLM-as-a-judge (`judge.go`, `eval.go`, `confidence.go`)
- `internal/quality/` — async AST/compiler verifier
- `internal/auth/` — inbound API-key auth middleware
- `internal/ratelimit/` — `ClientIPResolver` + HTTP middleware (separate from `internal/middleware`)
- `internal/config/` — env loading, YAML config, hot-reload via SIGHUP
- `internal/observability/` — Prometheus metrics (no `client_golang`)
- `internal/tracing/` — OTLP/W3C distributed tracing
- `internal/telemetry/` — JSONL recorder interface + `JSONLRecorder`
- `internal/metrics/` — SQLite metrics store + `SQLiteStore`
- `internal/probe/` — NVIDIA VRAM probe + AMD sysfs probe
- `internal/concurrencylimit/` — VRAM-aware local-route semaphore
- `internal/circuit/` — local-route cooldown after cascade failure
- `internal/budget/` — 24 h rolling frontier spend cap

**CRITICAL dependency rule**:
- `internal/handlers` and `internal/upstream` must **never** import `internal/judge`
  or `internal/quality`.
- Judge/quality hook in via `JudgeObserver`/`QualityObserver` function-typed fields
  on `handlers.Deps`, wired in `cmd/nexus/main.go`.
- This keeps the hot path testable without spinning up worker pools.

---

### Product Roadmap

| Phase | Description | Key Remaining |
|-------|-------------|---------------|
| 1 | Foundation | Mostly complete; dead-code cleanup |
| 2 | Observability & Metrics | Tracing queue gauge, Grafana dashboard, SQLite vacuum |
| 3 | Hardware Awareness | Temperature throttling, dynamic VRAM probe |
| 4 | OSS Release | SLSA binaries, Windows binary, Docker smoke tests |
| 5 | Intelligence/DX/Production | RAG hardening, judge-guided routing, DSL Unicode, SLM cache fixes |

---

### Recently Merged PRs (do not file duplicates)

| PR | Summary |
|----|---------|
| #784 | Expose adaptive routing confidence via `nexus judge stats` |
| #783 | Add `nexus routing-preview` dry-run command |
| #782 | Enable arbiter synthesis cache by default |
| #781 | Add batch embedding support to RAG embedders |
| #780 | Improve unfenced TOON array detection |
| #779 | Round-robin GPU selection for multi-GPU nodes |
| #777 | Add per-request latency percentiles to Prometheus metrics |
| #778 | Add API-key-aware rate limiting |
| #767 | HTTP 429 emits distinct `rate_limited` cascade fallback reason |
| #766 | Add `NEXUS_CASCADE_MAX_RESPONSE_BYTES` env var |
| #765 | Set FallbackReason on terminal cascade failure |
| #764 | Add `nexus_slm_cache_embedding_errors_total` counter |
| #763 | Stop/Close to `ratelimit.Middleware` to prevent reaper goroutine leak |
| #762 | Add `nexus_auth_limiter_tracked_ips` and `nexus_auth_limiter_blocked_ips` gauges |
| #761 | Add `nexus_rate_limit_bucket_utilization` histogram |
| #760 | Remove redundant O(n log n) `sortExpiry` call from SLMCache |

---

### Known Open Issues (for deduplication)

**Routing (B)**:
- #563: SLM cache returns expired entries as valid hits
- #587: Categorize uses ASCII-only case folding (DSL uses Unicode)
- #588: DSL package uses MustCompile — invalid pattern = boot panic
- #591: SQLiteConfidenceStore coerces empty category to `"other"`

**RAG (A)**:
- #592: HNSWIndex Serialize/Deserialize structurally broken
- #593: `probeEmbedderDims` error discarded at boot
- #594: RAG injection has no size guard (can overflow context window)
- #536: PersistentStore loads stale embeddings with no model/dimension validation

**Observability (C)**:
- #596: `nexus_tracing_spans_in_queue` gauge missing
- #566: OTLP batch flush has zero retry — drops up to 64 spans
- #565: nil pointer panic when collector unreachable at network level
- #681: JSONLRecorder.Record and Dropped at 0% coverage

**Upstream (D)**:
- #533: NEXUS_MAX_RESPONSE_BYTES parsed but never wired
- #570: `buildCascadeFromProviders` dead code at 0% coverage
- #569: `ConfigureTimeouts` is dead code
- #564: panel panic invisible (no metric)

**Security (E)**:
- #687: `genericSecretRe` partial-match leaves trailing fragments
- #577: panic value logged via `slog.Any` — potential secret leakage
- #578: AuthLimiter reaper has 23.1% coverage

**Config (F)**:
- #535: NEXUS_TOON_UNFENCED parsed but never wired
- #603: TrustedProxiesRaw preserved for diagnostics but never read
- #573: `parseBoolEnvStr` silent fallback on typos
- #574: `clampFloat` boundary branches untested

---

### Investigation Steps

1. **Survey your focus area**:
   ```bash
   gh issue list --state all --limit 200 --search "{{FOCUS_AREA}}"
   gh pr list --state merged --limit 50
   ```

2. **Read the relevant source code**:
   | Area | Read first |
   |------|-----------|
   | A — RAG | `internal/rag/*.go`, `internal/middleware/rag.go` |
   | B — Routing | `internal/router/*.go`, `internal/judge/*.go` |
   | C — Observability | `internal/observability/*.go`, `internal/tracing/*.go`, `dashboards/` |
   | D — Upstream | `internal/upstream/*.go`, `internal/handlers/chat.go` |
   | E — Security | `internal/auth/*.go`, `internal/ratelimit/*.go`, `internal/handlers/recover.go`, `internal/handlers/security.go` |
   | F — Config | `internal/config/*.go`, `.env.example`, `internal/middleware/toon.go`, `internal/middleware/injection.go` |

3. **Look for**:
   - `TODO`, `FIXME`, `XXX`, `HACK` comments
   - Unbounded allocations (`io.ReadAll`, no size limits)
   - Silent error swallowing (`_ = err`, missing `return`)
   - Hardcoded values that should be configurable via env var
   - Missing metrics/tracing on hot paths
   - Race conditions (shared mutable state without locks)
   - Dead code (functions called by nothing)
   - Missing graceful shutdown paths
   - Untested error branches
   - Security issues (header injection, secret redaction gaps, auth bypass)
   - Performance issues (allocations in hot paths, unnecessary copies)

4. **Cross-reference the PRD** for unchecked or partially-done items

5. **Score candidates**:
   - Does it fix a real bug or advance a PRD phase?
   - Can it be completed in 1–2 days?
   - Are acceptance criteria unambiguous and testable?
   - Is it not already filed?
   - P1 = correctness bug, security hole, memory/IO unboundedness, data loss,
     or blocking a PRD phase
   - P2 = observability gap, missing metric, configurability hole,
     non-blocking reliability
   - P3 = DX polish, nice-to-have, internal refactor

6. **Propose 3–4 issues** (fewer is fine; "nothing new" is valid if evidence is strong)

---

### Issue Format (strict compliance required)

Each proposed issue must be a **separate fenced code block**:

```markdown
## {{AREA_LETTER}}.N — [P1/P2/P3] Concise Title — Elaboration

**Focus area**: {{FOCUS_AREA}}
**PRD phase**: Phase X — Description
**Estimated effort**: S (1 day) / M (2–3 days) / L (3–5 days)
**Labels**: `bug` | `enhancement` | `hardening` | `documentation`, `area:xxx`, `phase:X-xxx`

### Problem
What is broken or missing? Be specific. Cite `file:line` references.

### Proposed solution
Concrete change. Name new functions, metrics, config vars explicitly.

### Acceptance criteria
- [ ] Criterion 1 (must be verifiable)
- [ ] Criterion 2 (must be verifiable)
- [ ] Criterion 3 (must be verifiable)
- [ ] `make ci` passes with coverage ≥ 70 %

### References
- `internal/pkg/file.go:123` — relevant code
- `#123` — adjacent/related issue
- PRD section — roadmap alignment
```

After all issue blocks, add:

```markdown
### Why I rejected the others
- **Rejected X** — reason; could be filed later as #XXX
- **Rejected Y** — scope too large (>2 days), split into A + B
```

---

### What to Return to the Orchestrator

Return ONLY:
1. Your fenced markdown blocks (3–4 proposed issues)
2. A one-line `VERIFIED: <n>` confirming you checked `gh issue list` for duplicates
3. A one-line `NEXT: <number>` indicating the next available issue number for your area

**Do NOT file issues yourself. Do NOT create branches. Investigation and proposal only.**

---

### Quality Standards

- Every issue must cite **at least one specific `file:line` reference**
- Every issue must have **at least 3 testable acceptance criteria**
- No issue may duplicate an open or recently-closed issue/PR
- No issue larger than ~2 days of focused work — split if larger
- No vague "improve X" — concrete change with concrete observable outcome
- Avoid feature creep beyond PRD scope unless it's a correctness defect
- P1 = correctness bug, security hole, memory/IO unboundedness, data loss, or blocking a PRD phase
- P2 = observability gap, missing metric, configurability hole, non-blocking reliability
- P3 = DX polish, nice-to-have, internal refactor

---

## Orchestrator Synthesis Instructions

After receiving all 6 sub-agent responses:

1. **Parse** all proposed issues (ignore prose, extract fenced blocks only)
2. **De-duplicate**:
   - Run `gh issue list --search "<title keywords>"` for each candidate
   - Check for overlap across focus areas (an issue might belong to a different area)
3. **Rank**: P1 correctness/security → P1 observability → P2 → P3
4. **Cap**: Maximum 3–4 issues per focus area; reject the rest with clear rationale
5. **Verify**: Confirm each final candidate with `gh issue list --search` before presenting
6. **Present** as a final markdown table:

```
| #  | Priority | Title | Focus | Effort | Labels |
|----|----------|-------|-------|--------|--------|
| 786 | P1 | ... | A | S | bug, area:rag, phase:5-intelligence |
```

7. **Write the result** to `.agents/results/issue-discovery-{sessionId}.md`
8. **Return** a summary: issue count by priority, by area, and the file path

---

## Anti-Patterns (do not follow)

- ❌ Filing every possible issue — cull aggressively to highest-signal candidates
- ❌ Inventing issues not found in the codebase
- ❌ Skipping `gh issue list --search` verification
- ❌ Sub-agents citing no specific `file:line` references
- ❌ Proposing issues without 3+ testable acceptance criteria
- ❌ Scope larger than 2 days per issue
- ❌ Ignoring the dependency rule (`handlers`/`upstream` must not import `judge`/`quality`)
- ❌ Proposing issues already covered by the Known Open Issues table
