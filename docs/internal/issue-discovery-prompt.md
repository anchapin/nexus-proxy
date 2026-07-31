# Nexus Proxy — Parallel Issue Discovery Prompt Pack

A two-part prompt pack for spawning parallel sub-agents that propose
new GitHub issues to advance nexus-proxy toward its goals.

---

## Part 1 — Meta-prompt (run by the orchestrator / you)

You are orchestrating parallel discovery of new GitHub issues for the
`nexus-proxy` repository (Go, hardware-aware AI routing gateway).

### Goal

Spawn N sub-agents in parallel. Each investigates one **focus area**
of the codebase, returns a small set of **proposed issues** in a
strict format. You then **synthesize, de-duplicate, and rank** the
results into a final ordered list of issues to file.

### Step 1 — Pick focus areas (one per sub-agent)

Choose 4–6 non-overlapping focus areas. Default set:

| # | Focus area | Why it matters (from PRD/AGENTS.md) |
|---|---|---|
| A | **Observability gaps** | PRD Phase 2 not yet complete; many `[P2] ... lack Prometheus metrics` issues already filed |
| B | **Reliability & graceful degradation** | PRD Phase 3 (Ollama fallback); recent `[P1] Cascade partial SSE ...` |
| C | **Security hardening** | Existing `[P1] Unbounded Response Body Reads`, brute-force protection |
| D | **RAG / vector retrieval** | Recent HNSW + fsnotify; remaining gaps in embedder factory, persistence |
| E | **DX / config / onboarding** | PRD Phase 4 (open source release); unknown env vars silently ignored |
| F | **Routing intelligence** | SLM cache, DSL patterns, judge-guided routing — phase 5-intelligence |

Assign each sub-agent ONE focus area letter. Pass it via the
`{{FOCUS_AREA}}` placeholder in Part 2.

### Step 2 — Spawn all sub-agents in one tool call

Launch every sub-agent **in a single message with multiple `task`
tool calls** (parallel). Pass the same Part 2 prompt to each, only
`{{FOCUS_AREA}}` and `{{AREA_LETTER}}` differ.

Set `subagent_type: explore` for fast discovery, or
`subagent_type: general-purpose` if you want them to also write a
deep analysis.

### Step 3 — Synthesize

When all return:

1. **Collect** every proposed issue (each has a YAML frontmatter
   block — parse those, do not re-read the prose).
2. **De-duplicate** by (focus area × short title). If two agents
   propose the same gap, keep the more specific one and drop the
   other. (Sub-agents are instructed to dedupe against existing issues
   too — trust them, but spot-check the top 3.)
3. **Rank** by priority: P1 first, then P2, then P3. Within a tier,
   prefer smaller scope (faster to land) over larger scope.
4. **Cap** at 3–5 issues per focus area; reject the rest as "future".
5. **Verify** the top 5–10 with `gh issue list --search` before filing.
6. **Present** the final list to the user as a markdown table:

   | # | Priority | Title | Focus | Effort |
   |---|----------|-------|-------|--------|
   | 427 | P1 | ... | C | S |

### Anti-patterns (in your synthesis)

- Do **not** file everything the sub-agents return — they over-propose
  by design (you do the culling).
- Do **not** invent issues the sub-agents didn't surface — they did
  the investigation; you're the editor.
- Do **not** skip the `gh issue list --search` verification step.

---

## Part 2 — Sub-agent prompt template

This is the literal prompt to send to each parallel sub-agent.
Replace `{{FOCUS_AREA}}` and `{{AREA_LETTER}}` per agent.

---

You are sub-agent **{{AREA_LETTER}}** in a parallel discovery sweep
over the `nexus-proxy` repository. Your job: propose **3 to 5 new
GitHub issues** in focus area **{{FOCUS_AREA}}** that would
meaningfully advance the project toward its goals.

### Repository context (frozen for this run)

- **Repo**: `anchapin/nexus-proxy` on GitHub
  (working directory: this repo)
- **Language**: Go 1.26
- **Purpose**: Hardware-aware AI routing gateway — intercepts
  OpenAI-compatible `/v1/chat/completions`, optimizes prompts (TOON
  compression, RAG, meta-prompting), routes to local Ollama or
  frontier API based on complexity.
- **CI gate**: `make ci` runs `go vet` → `go build` → `go test -race`
  → `golangci-lint` → bench-short. Coverage floor **70%**.
- **Latest issue number**: 426 — new issues start at **427**.

### Product goals (from the PRD)

The PRD defines four roadmap phases:

1. **Phase 1 — Foundation & Refactor**: mostly complete
   (Go module structure exists).
2. **Phase 2 — Observability & Metrics**: structured logging (done),
   SQLite metrics store (done), Daily Savings Dashboard (CLI is done;
   Grafana/visualization is partial).
3. **Phase 3 — Advanced Hardware Awareness**: dynamic VRAM checking,
   graceful degradation when Ollama is down.
4. **Phase 4 — Open Source Release**: Docker packaging,
   comprehensive README.

Issues must map to one of these phases, or to the implicit
**Phase 5 (DX, Production, Intelligence)** that recent issues
clearly target.

### Issue conventions (match exactly)

Read `gh issue list --limit 30 --state all --json number,title,labels`
before proposing anything. New issues must match existing style:

- **Title formats** (use one):
  - `[P1] Short noun phrase — elaboration`
  - `[P2] ...`
  - `[P3] ...`
  - `feat: ...` / `fix: ...` / `docs: ...` / `perf: ...` /
    `observability: ...`
- **Priority semantics**:
  - **P1** — correctness bug, security hole, memory/IO unboundedness,
    data loss, or feature blocking core PRD phase.
  - **P2** — observability gap, missing metric, configurability hole,
    non-blocking reliability issue.
  - **P3** — DX polish, nice-to-have, internal refactor.
- **Labels** (use the exact ones that already exist — check
  `gh label list`):
  - `bug`, `enhancement`, `hardening`, `docs`
  - `area:upstream`, `area:config`, `area:rag`,
    `area:middleware`, `area:router`, `area:observability`,
    `area:judge`, `area:quality`
  - `phase:2-observability`, `phase:3-hardware`,
    `phase:4-oss`, `phase:5-dx`, `phase:5-production`,
    `phase:5-intelligence`

### Your focus area: {{FOCUS_AREA}}

Read the relevant source packages and AGENTS.md sections for your
area. Examples of where to look:

| Focus | Read first |
|---|---|
| Observability | `internal/metrics/`, `internal/observability/`, `dashboards/`, `docs/observability-surface.md` |
| Reliability | `internal/circuit/`, `internal/health/`, `internal/concurrencylimit/`, `internal/upstream/cascade.go` |
| Security | `internal/auth/`, `internal/ratelimit/`, `cmd/nexus/main.go` (middleware order), `internal/handlers/` |
| RAG | `internal/rag/`, `internal/middleware/prompt_engine*`, `internal/config/` (embedder section) |
| DX / config | `internal/config/`, `.env.example`, `README.md`, `CONTRIBUTING.md`, `nexus check`/`nexus doctor` |
| Routing intelligence | `internal/router/`, `internal/judge/`, `internal/quality/`, SLM cache code |

### Methodology — do this in order

1. **Survey existing issues** in your focus area:
   `gh issue list --limit 50 --state all --search "<focus keyword>"`
   Note the next free number (start from **427** unless you see a
   higher one).

2. **Read the code** for your focus area — at minimum the entry
   point and one test file. Look for:
   - `TODO`, `FIXME`, `XXX`, `HACK` comments
   - Unbounded allocations (e.g. `io.ReadAll`, no size limits)
   - Missing error propagation
   - Silent error swallowing (`_ = ...`)
   - Hardcoded values that should be config
   - Missing metrics / tracing spans on hot paths
   - Race conditions (`go test -race` will surface some; read carefully)
   - Missing graceful shutdown paths

3. **Cross-reference the PRD** — find checkbox items that are
   unchecked or partially done.

4. **Score candidate issues**:
   - Does it advance a PRD phase or fix a real bug?
   - Is scope small enough to land in 1–2 days of work?
   - Are acceptance criteria unambiguous?
   - Is it not already filed? (re-check `gh issue list --search`)

5. **Propose 3 to 5** (fewer is fine; "I found nothing new" is also
   a valid answer).

### Output format — strict

Return your entire response as **one fenced markdown block** with
YAML frontmatter per issue. Example:

```markdown
## {{AREA_LETTER}}.1 — [P2] Add Prometheus histogram for cascade fallback latency

**Focus**: Observability gaps
**PRD phase**: Phase 2 — Observability
**Estimated effort**: S (1 day)
**Labels**: `enhancement`, `area:upstream`, `phase:2-observability`

### Problem
When the cascade path falls back from local Ollama to frontier, the
latency contribution of the fallback hop is not exported. Operators
can see total latency via `request_duration_seconds` but cannot
distinguish primary vs fallback latency, making cascade tuning blind.

### Proposed solution
Add a new histogram `nexus_cascade_fallback_duration_seconds` with
labels `from_route`, `to_route`, `reason` (timeout, status_5xx,
circuit_open). Emit on every fallback transition inside
`internal/upstream/cascade.go`.

### Acceptance criteria
- [ ] Histogram emitted with the three labels documented above
- [ ] Unit test asserts emission on timeout-driven fallback
- [ ] Metric appears in `dashboards/grafana.json` panel 7
- [ ] `make ci` passes with coverage ≥ 70%

### References
- `internal/upstream/cascade.go:142`
- `docs/observability-surface.md` (existing metric catalog)
- Existing #405 (cascadepartial SSE rollback) — adjacent scope
```

Repeat the block for each issue. After all blocks, add a final
`### Why I rejected the others` section listing 2–3 candidate
issues you considered but dropped, with one-line reasons.

### What to return to the orchestrator

Return ONLY:
1. The fenced markdown block above (the proposed issues).
2. A one-line `VERIFIED: <n>` count where `<n>` is the number of
   issues you actually confirmed are not already filed via
   `gh issue list --search`.

Do **not** file the issues yourself. Do **not** create branches.
Investigation and proposal only.

### Quality bar (do not return work that fails this)

- Every issue must cite **at least one specific file:line**.
- Every issue must have at least 3 acceptance criteria.
- No issue may duplicate an open or closed one (use
  `gh issue list --state all --search` to verify).
- No issue may be larger than ~2 days of focused work — split if
  larger.
- No "improve X" vagueness — concrete change with concrete metric
  or concrete file.
- Avoid feature creep beyond PRD scope unless it's a real defect.