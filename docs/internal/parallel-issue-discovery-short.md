# Nexus Proxy — Parallel Issue Discovery

## Context

**Repo**: anchapin/nexus-proxy (Go, hardware-aware AI routing gateway)
**Next issue #**: 786 (confirm: `gh issue list --state all --limit 5 --sort created`)
**CI**: `make ci` — 70% coverage floor, race detector required
**Critical rule**: `internal/handlers` and `internal/upstream` must NEVER import `internal/judge` or `internal/quality`

## Product Phases

| Phase | Description | Status |
|-------|-------------|--------|
| 1 | Foundation | Mostly complete |
| 2 | Observability & Metrics | Partial — tracing queue gauge, Grafana |
| 3 | Hardware Awareness | Partial — temperature throttling, VRAM |
| 4 | OSS Release | Partial — SLSA, Windows, Docker smoke |
| 5 | Intelligence/DX/Production | Active — RAG hardening, DSL Unicode, SLM cache |

## Your Assignment

**Area**: `{{FOCUS_AREA}}` (Area {{AREA_LETTER}})

Investigate the relevant packages, find 3–4 issues that would meaningfully advance the project,
then output them in the format specified below.

## Investigation Steps

1. **Check existing issues**: `gh issue list --state all --limit 200 --search "{{FOCUS_AREA}}"`
2. **Check recent PRs**: `gh pr list --state merged --limit 30`
3. **Read the code**: See focus area table below
4. **Look for**: TODO/FIXME comments, unbounded allocations, silent error swallowing, missing metrics/tracing, hardcoded values, race conditions, dead code
5. **Cross-reference PRD** for unchecked items

## Focus Area Package Mapping

| Area | Packages to Read |
|------|-----------------|
| A — RAG | `internal/rag/*.go`, `internal/middleware/rag.go` |
| B — Routing | `internal/router/*.go`, `internal/judge/*.go` |
| C — Observability | `internal/observability/*.go`, `internal/tracing/*.go`, `dashboards/` |
| D — Upstream | `internal/upstream/*.go`, `internal/handlers/chat.go` |
| E — Security | `internal/auth/*.go`, `internal/ratelimit/*.go`, `internal/handlers/recover.go`, `internal/handlers/security.go` |
| F — Config | `internal/config/*.go`, `.env.example`, `internal/middleware/toon.go` |

## Known Issues (do not duplicate)

**Routing (B)**: #563 SLM cache stale entries, #587 ASCII-only case fold, #588 MustCompile panic, #591 empty category coerce
**RAG (A)**: #592 HNSW serialization broken, #593 embedder dims error discarded, #594 no RAG size guard, #536 stale embeddings no validation
**Observability (C)**: #596 spans-in-queue gauge missing, #566 OTLP no retry drops spans, #565 nil panic on unreachable collector, #681 JSONLRecorder 0% coverage
**Upstream (D)**: #533 MaxResponseBytes not wired, #570 buildCascade dead code, #569 ConfigureTimeouts dead code, #564 panel panic invisible
**Security (E)**: #687 secret redaction partial, #577 panic value logged, #578 AuthLimiter reaper 23% coverage
**Config (F)**: #535 TOON_UNFENCED not wired, #603 TrustedProxiesRaw unused, #573 parseBoolEnvStr silent fallback, #574 clampFloat untested

## Recent Merged PRs (do not duplicate)

#784 judge stats, #783 routing-preview, #782 arbiter cache default, #781 batch embedding,
#780 TOON fix, #779 multi-GPU round-robin, #777 latency percentiles, #778 API-key rate limiting,
#767 429 cascade reason, #766 CASCADE_MAX_RESPONSE_BYTES, #765 FallbackReason terminal failure

## Output Format (strict)

Return **3–4 fenced code blocks**, one per issue:

````markdown
```yaml
## {{AREA_LETTER}}.N — [P1/P2] Issue title — brief elaboration

focus: {{FOCUS_AREA}}
phase: Phase X — Description
effort: S (1d) | M (2-3d) | L (3-5d)
labels: bug|enhancement|hardening|documentation, area:xxx, phase:X-xxx

problem: |
  Specific description of the bug or gap. Cite file:line references.

solution: |
  Concrete proposed change. Name functions, metrics, config vars explicitly.

acceptance:
  - Criterion 1 (must be verifiable)
  - Criterion 2 (must be verifiable)
  - Criterion 3 (must be verifiable)
  - make ci passes with coverage ≥ 70%

references:
  - internal/pkg/file.go:123 — relevant code
  - #123 — adjacent issue
```
````

After all blocks, add:

```markdown
### Rejected candidates
- **X** — rejected because [reason]; consider later as #XXX
- **Y** — rejected because scope too large, split into A + B

VERIFIED: N (checked gh issue list for duplicates)
NEXT: <next available issue number for this area>
```

## Quality Standards

- **Every issue must cite at least one specific `file:line` reference**
- **Every issue must have at least 3 testable acceptance criteria**
- **No duplication** — verify with `gh issue list --search`
- **Max scope: ~2 days** — split larger issues
- **P1 = correctness, security, unbounded memory/IO, data loss, or blocking a PRD phase**
- **P2 = observability gap, missing metric, configurability hole, non-blocking reliability**
- **P3 = DX polish, nice-to-have, internal refactor**

## What to Return

1. Your 3–4 fenced issue blocks
2. Rejected candidates section
3. VERIFIED and NEXT lines

**Do NOT file issues. Do NOT create branches. Propose only.**
