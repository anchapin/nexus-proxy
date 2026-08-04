# Nexus Proxy — Parallel Gap-Finding Issue Discovery Prompt

A prompt for spawning N sub-agents in parallel to discover new GitHub issues
that fill gaps in the nexus-proxy codebase.

---

## Orchestrator Instructions

### Purpose
Spawn parallel sub-agents to investigate distinct focus areas of the codebase
and propose **3-5 net-new GitHub issues per area** that would meaningfully
advance the project. You synthesize and deduplicate results into a final
ranked list.

### Context (frozen for this run)
- **Repo**: `anchapin/nexus-proxy` (Go, hardware-aware AI routing gateway)
- **Latest issue number**: Check via `gh issue list --state all --limit 1 --json number --sort created`
- **CI gate**: `make ci` = vet → build → `go test -race` → golangci-lint → bench-short; **70% coverage floor**
- **Dependency rule**: `internal/handlers` and `internal/upstream` must NEVER import `internal/judge` or `internal/quality`
- **Critical rule**: `internal/middleware` has ZERO `net/http` imports

### Focus Areas (assign ONE per sub-agent)

| Area | Letter | Description |
|------|--------|-------------|
| **Observability Gaps** | A | Missing Prometheus metrics, un-instrumented hot paths, tracing blind spots |
| **Reliability & Graceful Degradation** | B | Cascade failures, circuit breakers, fallback gaps |
| **Security Hardening** | C | Auth gaps, SSRF, injection, mTLS, brute-force |
| **RAG & Vector Retrieval** | D | Embedder factory, persistence, HNSW, semantic dedup |
| **DX / Config / Onboarding** | E | `nexus init`, `nexus check`, env var UX, silent ignores |
| **Routing Intelligence** | F | SLM cache, DSL patterns, judge-guided routing, fusion |

### Step 1 — Survey existing issues
Before spawning agents, run:
```bash
gh issue list --state all --limit 100 --json number,title,labels,state --sort created
```
Note which issue numbers are already taken and what gaps already have issues filed.

### Step 2 — Spawn all sub-agents in ONE message
Use the `task` tool with `subagent_type: general-purpose` for deep analysis.
Pass the **same prompt below** to each agent, only substituting:
- `{{AREA_LETTER}}` — one of A-F
- `{{FOCUS_AREA}}` — the full area name from the table above

### Step 3 — Synthesize (after all return)
1. Parse every YAML frontmatter block from all agent responses
2. Deduplicate by (area × title); keep the more specific one
3. Rank: P1 first, then P2, then P3; within tiers prefer smaller scope
4. Cap at 3-5 issues per area; reject the rest
5. Verify top issues aren't already filed: `gh issue list --search "<title>" --state all`
6. Present as a markdown table

---

## Sub-Agent Prompt Template

---

You are sub-agent **{{AREA_LETTER}}** in a parallel gap-finding sweep.
Your assigned focus area is: **{{FOCUS_AREA}}**

### Your Mission
Propose **3 to 5 net-new GitHub issues** that fill gaps in your focus area.
Each issue must be:
- **Concrete**: cites specific file:line or function name
- **Scoped**: achievable in 1-2 days of focused work
- **Non-duplicative**: not already filed (verify with `gh issue list --search`)
- **Meaningful**: advances a PRD phase or fixes a real defect

### Repository Context

**What nexus-proxy does**: Hardware-aware AI routing gateway. Intercepts
OpenAI-compatible `/v1/chat/completions`, optimizes prompts (TOON compression,
RAG, meta-prompting), and routes to local Ollama or frontier API based on
complexity.

**Key packages**:
- `cmd/nexus/` — main, wires config → middleware → handlers → HTTP server
- `internal/handlers/` — chat.go (HTTP entry), health.go, recover/security/sanitize
- `internal/middleware/` — prompt transforms only (NO net/http imports)
- `internal/router/` — Guardrail → DSL → SLM.Decide pipeline
- `internal/upstream/` — Cascade, fusion panel, arbiter cache
- `internal/rag/` — PersistentStore (SQLite), Store, embedders
- `internal/judge/` — async LLM-as-a-judge
- `internal/health/` — Ollama circuit breaker, frontier health poller
- `internal/providers/` — multi-frontier registry, adapter interface
- `internal/observability/` — Prometheus, OTLP tracing, routemetrics
- `internal/metrics/` — SQLite metrics store
- `internal/circuit/` — local-route cooldown after cascade failure
- `internal/concurrencylimit/` — VRAM-aware local-route semaphore
- `internal/auth/` — inbound API-key middleware
- `internal/ratelimit/` — ClientIPResolver + HTTP middleware
- `internal/config/` — Load() env parsing + LoadYAML() file+env

**PRD Phases** (from `Nexus Proxy PRD and Architecture.md`):
1. **Phase 1 — Foundation & Refactor**: mostly complete
2. **Phase 2 — Observability & Metrics**: structured logging (done), SQLite metrics (done),
   savings dashboard (CLI done, visual partial)
3. **Phase 3 — Hardware Awareness**: VRAM probing, graceful Ollama degradation (done)
4. **Phase 4 — OSS Release**: Docker, README, config wizard (mostly done)
5. **Phase 5 — Production, DX, Intelligence**: ongoing work

### Investigation Steps

**Step 1: Survey your focus area**
```bash
gh issue list --state all --limit 50 --search "{{FOCUS_AREA}}"
gh issue list --state all --limit 50 --search "area:{{AREA_LETTER}}"
```
Note which gaps already have issues.

**Step 2: Read the relevant code**
Minimum to read (per area):

| Area | Read first |
|------|-----------|
| A — Observability | `internal/observability/`, `internal/metrics/`, `docs/observability-surface.md` |
| B — Reliability | `internal/circuit/`, `internal/health/`, `internal/concurrencylimit/`, `internal/upstream/cascade.go` |
| C — Security | `internal/auth/`, `internal/ratelimit/`, `internal/handlers/security.go`, SSRF guard in `internal/transport/` |
| D — RAG | `internal/rag/`, embedder factory, `internal/middleware/prompt_engine*` |
| E — DX | `cmd/nexus/init.go`, `internal/diag/`, `internal/config/`, `.env.example` |
| F — Routing | `internal/router/`, `internal/judge/`, `internal/upstream/fusion*.go` |

Look for:
- `TODO`, `FIXME`, `XXX`, `HACK` comments
- Unbounded allocations (`io.ReadAll`, no size limits)
- Missing error propagation or silent error swallowing (`_ = ...`)
- Hardcoded values that should be config env vars
- Missing metrics / tracing spans on hot paths
- Race conditions
- Missing graceful shutdown paths
- Silent fallback behavior with no observability

**Step 3: Cross-reference PRD**
Find checkbox items that are unchecked or partially done.

**Step 4: Score candidates**
- Does it advance a PRD phase or fix a real bug?
- Is scope ≤ 2 days of focused work?
- Are acceptance criteria unambiguous (at least 3)?
- Is it not already filed?

**Step 5: Propose 3-5 issues**

### Output Format (STRICT — follow exactly)

Return your entire response as **one fenced markdown block** with YAML
frontmatter per issue:

```markdown
## {{AREA_LETTER}}.1 — [P2] Add Prometheus histogram for cascade fallback latency

**Focus**: {{FOCUS_AREA}}
**PRD phase**: Phase 2 — Observability
**Estimated effort**: S (1 day)
**Labels**: `enhancement`, `area:upstream`, `phase:2-observability`

### Problem
When the cascade falls back from local Ollama to frontier, the latency
contribution of the fallback hop is not exported. Operators can see total
latency but cannot distinguish primary vs fallback latency.

### Proposed solution
Add `nexus_cascade_fallback_duration_seconds` histogram with labels
`from_route`, `to_route`, `reason`. Emit on every fallback transition
in `internal/upstream/cascade.go`.

### Acceptance criteria
- [ ] Histogram emitted with the three labels documented above
- [ ] Unit test asserts emission on timeout-driven fallback
- [ ] `make ci` passes with coverage ≥ 70%
- [ ] Metric appears in `docs/observability-surface.md`

### References
- `internal/upstream/cascade.go:142`
- `docs/observability-surface.md` (existing metric catalog)
```

Repeat the block for each issue. After all blocks, add:

```markdown
### Why I rejected the others
- **Candidate X** — rejected because [one-line reason]
- **Candidate Y** — rejected because [one-line reason]
```

### What to return to the orchestrator

Return ONLY:
1. The fenced markdown block above (proposed issues)
2. A one-line `VERIFIED: <n>` count confirming you checked `gh issue list --search`
   for each issue title and none are duplicates

**Do NOT file issues yourself. Do NOT create branches. Investigation only.**

### Quality Standards (reject work failing these)
- Every issue cites **at least one specific file:line**
- Every issue has **at least 3 acceptance criteria**
- No issue duplicates an open or closed issue
- No issue exceeds ~2 days of work
- No vague "improve X" — concrete change with concrete metric or file
- No feature creep beyond PRD scope unless it's a real defect
