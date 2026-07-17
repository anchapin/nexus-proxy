# Routing Decision Debug Runbook

When `NEXUS_DEBUG=true` the proxy emits five structured slog groups per request.
This runbook explains what each field means and how to trace a routing decision
back through the Guardrail → DSL → SLM pipeline.

**Prerequisites:** a live proxy with `NEXUS_DEBUG=true` and a request that
produces the behaviour you are investigating. Use `request_id` as the grep
anchor — it appears in every group so you can isolate a single request.

---

## The Five Debug Groups

### 1. `[DEBUG] request`

Summarises the inbound OpenAI payload before any middleware runs.

```
[DEBUG] request request_id=<id> group: (
  messages=3
  estimated_tokens=842
  model=gpt-4o
  stream=true
  body_bytes=512
)
```

| Field | Meaning |
| ----- | ------- |
| `messages` | Number of messages in the `messages` array |
| `estimated_tokens` | Token estimate used by the VRAM guardrail; derived from `telemetry.EstimateTokens` |
| `model` | Raw model string the client sent (`model` field of the request body) |
| `stream` | Whether the client requested SSE streaming |
| `body_bytes` | Raw request body size; useful to confirm the proxy received the full payload |

---

### 2. `[DEBUG] transforms`

Post-middleware state: what RAG injected, whether TOON compression fired,
and whether the meta-prompt was applied.

```
[DEBUG] transforms request_id=<id> group: (
  prompt_engineering=true
  rag: (
    injected=true
    filename=sample.go
    cache_hit=false
    score=0.73
  )
  toon: (
    applied=true
    bytes_before=2048
    bytes_after=1048
    tokens_saved=312
  )
)
```

| Field | Meaning |
| ----- | ------- |
| `prompt_engineering` | `true` when a role / CoT / constraint block was prepended to the system prompt |
| `rag.injected` | `true` when a RAG example was appended (cosine score above `NEXUS_RAG_THRESHOLD`) |
| `rag.filename` | Filename of the matched RAG example |
| `rag.cache_hit` | `true` when the embedding was served from the embedder's in-memory LRU cache (#227) |
| `rag.score` | Cosine similarity score; higher = better match |
| `toon.applied` | `true` when any JSON array in the prompt was rewritten to CSV-like shape |
| `toon.bytes_before / bytes_after` | Raw byte sizes before and after TOON rewriting |
| `toon.tokens_saved` | Estimated tokens eliminated by the rewrite |

---

### 3. `[DEBUG] routing`  *(core of this runbook)*

The routing decision and the inputs that produced it.

```
[DEBUG] routing request_id=<id> group: (
  route=local
  reason=dsl
  budget_source=static-fallback
  budget_tokens=2048
  estimated_tokens=842
  slm_raw=
)
```

| Field | Meaning |
| ----- | ------- |
| `route` | Final route: `local`, `frontier`, or `fusion` |
| `reason` | Which pipeline stage chose this route: `guardrail`, `dsl`, `slm` |
| `budget_source` | Where the VRAM token budget came from (see Budget sources table below) |
| `budget_tokens` | Maximum tokens the local model can handle given available VRAM; `0` when no budget applies (frontier/fusion routes) |
| `estimated_tokens` | Token estimate for the prompt after all transforms (RAG, TOON, prompt engineering) |
| `slm_raw` | Raw JSON returned by the routing SLM; empty when `reason != "slm"` |

**Budget sources:**

| Value | Meaning |
| ----- | ------- |
| `static-fallback` | No VRAM probe result yet; used the compile-time fallback (`NEXUS_STATIC_FALLBACK_TOKENS`) |
| `ollama-ps` | VRAM probe succeeded; used `nvidia-smi` / AMD sysfs to compute available slots |
| `N/A` | Route is `frontier` or `fusion`; no local VRAM budget applies |

---

## Routing Pipeline Decision Tree

```
Prompt arrives
    │
    ├─► Guardrail: estimated_tokens > budget_tokens?
    │       YES → route=frontier, reason=guardrail  (VRAM budget exceeded)
    │       NO  ↓
    │
    ├─► DSL fast-pass (regex patterns)
    │       DSL hit on fusion patterns
    │           → route=fusion, reason=dsl
    │       DSL hit on formatting patterns
    │           → route=local, reason=dsl
    │       DSL hit on local patterns
    │           → route=local, reason=dsl
    │       DSL miss (no pattern matched) ↓
    │
    └─► SLM fallback (qwen3-coder:4b)
            route=fusion  (SLM returned fusion)
            route=local   (SLM returned local)
            route=frontier (SLM returned frontier OR SLM call failed OR confidence below threshold)
            reason=slm
```

### Identifying each stage in the logs

| Observed in debug trace | Stage |
| ------------------------| ------|
| `reason=guardrail` | VRAM guardrail tripped; prompt too large for local VRAM budget |
| `reason=dsl` and `route=local` | DSL formatting or local pattern matched; fast local route |
| `reason=dsl` and `route=fusion` | DSL fusion pattern matched (`architectural design`, `system architecture`) |
| `reason=slm` and `slm_raw={}` | SLM called; inspect `slm_raw` for the model's raw decision |
| `reason=slm` and `slm_raw=""` | SLM called but returned empty response; escalated to frontier (safe default) |

---

## Correlating `dsl_hit` and `slm_confidence`

The issue description uses `dsl_hit` and `slm_confidence` as shorthand for two
distinct concepts in the debug output:

### `dsl_hit`  →  `reason == "dsl"`

When the DSL fast-pass matches a prompt, `routing.reason` is set to `"dsl"`.
There is no separate `dsl_hit` boolean — the presence of `reason=dsl` in the
trace **is** the dsl-hit signal.

A DSL miss is indicated by `reason=slm` — the request fell through to the SLM.

### `slm_confidence`  →  adaptive confidence debug lines

When `NEXUS_ROUTING_CONFIDENCE_DB` is set and the judge is active, the
adaptive routing system emits two additional debug lines **before** the routing
group:

```
[DEBUG] dsl bypassed, asking slm request_id=<id>
[DEBUG] adaptive routing confidence request_id=<id> group: (
  category=refactoring
  confidence=0.72
)
```

| Field | Meaning |
| ----- | ------- |
| `category` | Task category assigned by `router.Categorize()` (e.g. `css`, `refactoring`, `architecture`) |
| `confidence` | 0.0..1.0 empirical estimate of how well local performs on this category; derived from historical judge scores |

The confidence value feeds into the SLM system prompt bias:

| Confidence range | SLM behaviour |
| ----------------| --------------|
| `< NEXUS_ROUTING_CONFIDENCE_FLOOR` (default 0.4) | Negative bias toward frontier appended to SLM prompt |
| `> NEXUS_ROUTING_CONFIDENCE_CEILING` (default 0.85) | Positive bias toward local appended to SLM prompt |
| Inside the band | No bias; byte-for-byte identical to the pre-adaptive-routing path |

---

## Fusion Progressive Delivery — Reading the `upstream` group

When `route=fusion` and `NEXUS_FUSION_PROGRESSIVE=true` (default), the
progressive delivery path races local and frontier in parallel. The `upstream`
group records the outcome:

```
[DEBUG] upstream request_id=<id> group: (
  route=fusion
  target_host=api.openai.com
  model=gpt-4o
  streaming=true
  cascade: (
    steps=[local, frontier, arbiter]
    served_by=frontier
    success=true
  )
)
```

| Field | Meaning |
| ----- | ------- |
| `route` | Always `fusion` for fusion requests |
| `target_host` | Frontier host only (query strings never logged — they may contain model identifiers) |
| `cascade.steps` | Ordered list of steps attempted |
| `cascade.served_by` | Which step produced the final answer |
| `cascade.success` | Whether at least one step succeeded |

### Fusion arbiter skip reasons

Three distinct log lines tell you **why** the arbiter was skipped:

#### 1. Agreement threshold — `similarity >= NEXUS_FUSION_AGREEMENT_THRESHOLD`

```
[DEBUG] upstream ... or ...
fusion agreement, arbiter skipped similarity=0.91 threshold=0.85 source=frontier
```

Both panel members agreed (Jaccard similarity above threshold). The speculative
frontier SSE chunk is the final answer. No arbiter was called.

**Action required:** None — this is the optimal fusion outcome.

#### 2. Tool calls detected — `len(winner.ToolCalls) > 0`

```
[DEBUG] upstream ... or ...
fusion tool-call winner, arbiter skipped source=local tool_calls=2
```

The winner carried tool calls (function_call / tool_use). The arbiter synthesises
text — it cannot merge structured tool call lists — so it was bypassed.

**Action required:** None — tool-call fusion intentionally skips arbitration.

#### 3. One member failed — `first.Err != nil || second.Err != nil`

```
[DEBUG] upstream ... or ...
(one_member skip — no dedicated log line; check cascade.served_by in trace)
```

One panel member errored. The survivor's response is used directly.

**Action required:** Investigate why the failing member errored (check the
member's upstream trace or the proxy error log).

### When the arbiter ran — `fusion disagreement`

```
[DEBUG] upstream ... or ...
fusion disagreement, invoking arbiter similarity=0.61 threshold=0.85 source=frontier
```

Both members succeeded but their content diverged below the agreement threshold.
The arbiter synthesised a final answer and streamed it as additional SSE chunks
after the speculative frontier chunk. The proxy marks
`X-Nexus-Fusion-Arbiter-Ran: true` on the response.

**Action:** If this happens frequently and the arbiter's synthesis is
unsatisfactory, lower `NEXUS_FUSION_AGREEMENT_THRESHOLD` (e.g. from `0.85` to
`0.75`) so the arbiter is invoked less often. Raising the threshold has the
opposite effect — more requests are arbitrated.

---

## Worked Examples

### Example 1 — Guardrail forced frontier

```
[DEBUG] request   ... estimated_tokens=3100 ...
[DEBUG] routing   ... route=frontier reason=guardrail budget_source=ollama-ps budget_tokens=2048 estimated_tokens=3100 ...
[DEBUG] upstream  ... route=frontier target_host=api.openai.com ...
```

**Interpretation:** The VRAM budget for the local model is 2048 tokens, but the
prompt is ~3100 tokens. Guardrail escalated to frontier.

**Fix:** Reduce prompt size (increase RAG threshold, trim few-shot examples) or
run a smaller local model with a larger VRAM budget.

---

### Example 2 — DSL fast-pass to local

```
[DEBUG] routing ... route=local reason=dsl budget_source=ollama-ps budget_tokens=2048 estimated_tokens=612 ...
```

**Interpretation:** DSL matched a formatting/local pattern (e.g. `css`,
`docstring`). The request went to local.

**Fix:** No fix needed — this is correct behaviour.

---

### Example 3 — SLM escalated to frontier

```
[DEBUG] routing ... route=frontier reason=slm budget_source=ollama-ps budget_tokens=2048 estimated_tokens=900 slm_raw={"route":"frontier"}
[DEBUG] adaptive routing confidence ... category=other confidence=0.31
```

**Interpretation:** DSL missed; SLM returned `frontier`. The low confidence
(0.31, below the 0.4 floor) biased the SLM toward frontier. `estimated_tokens`
(900) is below the VRAM budget (2048), so guardrail did not fire.

**Fix:** If you believe this prompt should go local, either (a) add a DSL
pattern to `NEXUS_DSL_LOCAL_PATTERNS`, or (b) improve the judge confidence
for the `other` category by letting more local completions be judged.

---

### Example 4 — Fusion with agreement

```
[DEBUG] routing ... route=fusion reason=dsl ...
[DEBUG] upstream ... cascade: (steps=[local, frontier] served_by=frontier success=true)
[DEBUG] upstream ... fusion agreement, arbiter skipped similarity=0.92 threshold=0.85 source=frontier
```

**Interpretation:** DSL matched a fusion pattern; both panel members ran; the
Jaccard similarity (0.92) exceeded the threshold; the frontier response was
streamed speculatively; arbiter was never called.

**Fix:** None — this is the optimal fusion outcome (cost of frontier with local
quality signal available for future confidence scoring).

---

## Quick Reference — Debug Lines Reference Table

| Log line | Group | Meaning |
| -------- | ----- | ------- |
| `guardrail forced frontier` | (slog direct) | VRAM budget exceeded; escalated |
| `dsl match` | (slog direct) | DSL fast-pass hit |
| `dsl bypassed, asking slm` | (slog direct) | DSL missed; falling through to SLM |
| `adaptive routing confidence` | (slog direct) | Confidence value being fed to SLM |
| `slm error, defaulting to frontier` | (slog direct) | SLM call failed; safe fallback to frontier |
| `slm decision` | (slog direct) | SLM returned; shows chosen route |
| `fusion agreement, arbiter skipped` | upstream | Both fusion members agreed above threshold |
| `fusion tool-call winner, arbiter skipped` | upstream | Tool calls detected; arbiter bypassed |
| `fusion disagreement, invoking arbiter` | upstream | Content diverged below threshold; arbiter ran |
| `starting fusion panel` | (slog direct) | Fusion routing chosen; panel dispatch beginning |

---

## Related Documents

- `docs/runbook.md` — General operator runbook (health checks, budget, RAG, DSL)
- `docs/observability-surface.md` — Prometheus metrics and SQLite schema reference
- `internal/handlers/debug.go` — Source-of-truth for trace struct field semantics
- `internal/router/dsl.go` — DSL fast-pass pattern definitions
- `internal/router/slm.go` — SLM routing client and cache behaviour
