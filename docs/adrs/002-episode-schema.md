# ADR-002: Episode Schema v1 and OTLP Attribute Mapping

**Status:** Proposed
**Date:** 2026-08-02
**Authors:** Adrien
**References:** [ADR-001](001-agent-admission-control.md), [ADR-003](003-contract-lowering.md), [ADR-004](004-wasm-plugin-abi.md), [OTel GenAI semconv](https://opentelemetry.io/docs/specs/semconv/gen-ai/)

## Context

[ADR-001](001-agent-admission-control.md) decided that evaluators never see raw spans — an adapter decodes OTLP into a normalized `Episode`, and evaluators are written against `episode/v1`. It sketched five field groups and stopped. This ADR specifies the schema, the mapping from OTel GenAI attributes onto it, and the three problems that mapping surfaces.

### Problem 1: the content we need is opt-in and off by default

The GenAI conventions define `gen_ai.input.messages` and `gen_ai.output.messages` as JSON arrays of `{role, parts[]}` objects (superseding the deprecated `gen_ai.prompt` / `gen_ai.completion`). Tool arguments and results have equivalent attributes.

**All of this content is off by default.** It is explicitly recognised as carrying user data and PII, so instrumentations gate it behind opt-in configuration. A perfectly conforming, production-grade trace routinely contains span names, tool names, timings, and token counts — and no message bodies and no tool arguments at all.

This is not an edge case to handle defensively. It is the modal trace. It means:

- `expose_pii` cannot run without message content
- `invariants` over tool arguments cannot run without argument capture
- `cite_sources` cannot run without output content
- `allowed_tools` runs fine, because tool *names* are in the span name

An evaluator that reports "pass" because the content it needed was absent is worse than no evaluator. So coverage is not metadata about the Episode — it is part of the Episode, and it is the input to the `skipped` machinery in [ADR-003](003-contract-lowering.md).

### Problem 2: traces are trees, policies are queries over sequences

OTLP gives a span tree. Almost every policy question is a query over a *sequence*: "was any tool called that isn't in this set", "did a `billing.refund` ever precede a `crm.verify_identity`", "how many steps did this take". Expressing tree traversal in Rego is possible and unpleasant, and every bundle author would reimplement it.

### Problem 3: determinism requires a total order, and OTLP does not provide one

[ADR-001](001-agent-admission-control.md) commits to byte-identical reports across runs. Spans in an OTLP payload have no guaranteed order, timestamps collide at millisecond resolution, and concurrent tool calls genuinely overlap. Without a defined total order, `ToolCalls[2]` means different things on different runs and the determinism guarantee is void.

## Decision

### 1. `Episode` is a flat, ordered, immutable document with span back-references

The adapter flattens the span tree into ordered lists and keeps a reference back to the originating span on every element, so evidence can always be resolved to a `trace_id`/`span_id` pair.

```
Episode (episode/v1)
  ├─ meta       EpisodeMeta      identity, agents, models, timing
  ├─ turns      []Turn           ordered conversational units
  ├─ tool_calls []ToolCall       ordered, flattened across all agents
  ├─ claims     []Claim          extracted assertions + their support
  ├─ metrics    Metrics          latency, tokens, cost, counts
  ├─ coverage   Coverage         what this trace can and cannot support
  └─ spans      []SpanRef        raw span index, for evidence resolution
```

Nothing in the Episode is nullable-by-convention. Absent data is absent *and* recorded in `coverage`, so an evaluator can distinguish "no tool calls happened" from "tool calls happened but were not captured" — a distinction that determines whether a clause passes or is skipped.

### 2. Field-level schema

```
EpisodeMeta
  episode_id      string     derived, stable (see §5)
  trace_id        string
  schema_version  string     "episode/v1"
  root_agent      string     gen_ai.agent.name of the root invoke_agent span
  agents          []AgentRef name, id, path (see §6)
  models          []string   distinct gen_ai.request.model values
  provider        string     gen_ai.provider.name
  started_at      timestamp
  ended_at        timestamp
  adapter         string     e.g. "otlp/v1.41"

Turn
  index      int
  role       enum{user, assistant, system, tool}
  agent_path string        which agent produced it (see §6)
  content    []ContentPart text | image | audio | tool_use | tool_result
  span       SpanRef
  captured   bool          false when the span existed but content was not recorded

ToolCall
  index        int
  name         string      e.g. "crm.lookup"
  kind         enum{tool, agent, retrieval, unknown}
  arguments    json        {} when not captured
  result       json        {} when not captured
  error        string      empty when the call succeeded
  agent_path   string      the caller
  started_at   timestamp
  duration_ms  int
  span         SpanRef
  args_captured   bool
  result_captured bool

Claim
  index      int
  text       string
  support    []SpanRef   retrieval / tool-result spans backing it
  values     map[string]json  extracted structured values (see ADR-003 §4)
  extractor  enum{structural, llm, plugin}
  turn       int

Metrics
  duration_ms        int
  latency_p50_ms     int    across model calls
  latency_p95_ms     int
  input_tokens       int
  output_tokens      int
  cost_usd           decimal   nil when pricing is unknown
  model_calls        int
  tool_calls         int
  tool_errors        int
  retries            int
  steps              int

Coverage
  has_message_content bool
  has_tool_args       bool
  has_tool_results    bool
  has_token_usage     bool
  has_cost            bool
  has_retrieval_spans bool
  has_agent_spans     bool
  degraded            []string   human-readable notes, e.g.
                                 "3 of 11 tool calls have no captured arguments"
```

`SpanRef` is `{trace_id, span_id, name, path}` where `path` is a JSON-Pointer-ish accessor into the Episode (`turns[3].content[0].text`) so a violation can point at both the span and the exact field.

### 3. OTLP attribute mapping

The adapter targets the GenAI conventions as of v1.41 and is versioned (`adapter: otlp/v1.41`) so a convention revision is a new adapter, not a silent behaviour change.

Versioning the adapter also lets the same attribute vocabulary arrive in a different envelope. [ADR-007 §4](007-agentcore-trace-acquisition.md) adds `cloudwatch-spans/v1`, which reads semantic-convention spans out of a CloudWatch log group rather than an OTLP payload; the mapping below is identical and nothing downstream of the adapter knows which one ran. `meta.adapter` records it, and a report always states how its Episode was produced.

| Episode field | Source |
|---|---|
| `meta.root_agent` | `gen_ai.agent.name` on the outermost `invoke_agent` span |
| `meta.provider` | `gen_ai.provider.name` |
| `meta.models` | distinct `gen_ai.request.model` / `gen_ai.response.model` |
| `turns[].content` | `gen_ai.input.messages` / `gen_ai.output.messages` (`{role, parts[]}`) |
| `tool_calls[].name` | `gen_ai.tool.name`, falling back to parsing `execute_tool {name}` |
| `tool_calls[].kind` | `gen_ai.tool.type`, plus `agent` for nested `invoke_agent` spans |
| `tool_calls[].arguments` | `gen_ai.tool.call.arguments` |
| `tool_calls[].result` | `gen_ai.tool.call.result` |
| `tool_calls[].error` | span status `ERROR` + `error.type` |
| `metrics.*_tokens` | `gen_ai.usage.input_tokens` / `gen_ai.usage.output_tokens` |
| `metrics.model_calls` | count of spans with `gen_ai.operation.name = chat` |
| `metrics.tool_calls` | count of spans with `gen_ai.operation.name = execute_tool` |
| `metrics.latency_*` | span durations over `chat` spans |

Span-name parsing is a deliberate fallback, not the primary path: v1.41 requires the tool name in the span name (`execute_tool {gen_ai.tool.name}`), which makes tool identity recoverable even from traces that captured nothing else. That single guarantee is why `allowed_tools` — the highest-value clause — works on the modal trace.

Deprecated `gen_ai.prompt` / `gen_ai.completion` are read as a compatibility fallback and set `coverage.degraded`.

### 4. Extraction provenance propagates to verdict class

`Claim` is the only Episode field that is *inferred* rather than *read*. Claims can be extracted three ways, and the extractor is recorded:

- **`structural`** — citation markers in the output, tool-result references, fields of a structured response. Deterministic.
- **`llm`** — an extractor model reads the output and emits claims. Not deterministic.
- **`plugin`** — a WASM extractor ([ADR-004](004-wasm-plugin-abi.md)); deterministic iff it holds no capabilities.

**Rule: a verdict is `deterministic` only if every Episode field it read is deterministic.** A CUE evaluator is a deterministic engine, but a CUE rule reading `claims[].text` where `extractor = llm` produces a `probabilistic` verdict and therefore does not block the build.

This is the same principle [ADR-004](004-wasm-plugin-abi.md) applies to capability-holding plugins, and it is what stops the determinism guarantee from being laundered: you cannot obtain a blocking verdict by running a strict engine over a fuzzy input. The default extractor is `structural` precisely so that the default path stays blocking.

### 5. Deterministic identity and total ordering

`episode_id` is `sha256` over the trace id plus the sorted span-id set — stable across re-runs, and changes if the trace changes.

Ordering is a defined total order applied to every list, so index-based references are stable:

1. `start_time_unix_nano` ascending
2. tie → parent-before-child (topological, by span parentage)
3. tie → `span_id` lexicographic ascending

Rule 3 is arbitrary but total, which is the only property that matters. Concurrent tool calls therefore get a stable, if semantically meaningless, relative order — and [ADR-003](003-contract-lowering.md) ordering clauses are specified to treat overlapping spans as *unordered*, so no policy can accidentally depend on rule 3.

### 6. Multi-agent traces flatten into one Episode with `agent_path`

Nested `invoke_agent` spans do not create nested Episodes. They flatten into the parent's lists, and every element carries `agent_path` — a slash-joined chain of agent names (`support/billing-specialist`).

A nested agent invocation appears **twice**, deliberately: once as a `ToolCall` with `kind: agent` (so `allowed_tools` can govern which sub-agents may be delegated to — delegation *is* an action), and once as the source of the turns and tool calls it contributed (each tagged with its `agent_path`).

Contracts scope with `agent_path` selectors. One Episode per root `invoke_agent`; a trace containing several roots yields several Episodes and `axda evaluate` reports on each.

### 7. Content is bounded and maskable at the report boundary

The Episode holds full content in memory because evaluators need it. The *report* does not: evidence excerpts are capped (256 bytes) and pass through `--evidence`:

- `full` — verbatim excerpt
- `masked` (default) — detected sensitive spans replaced with `[redacted:card]`, surrounding context kept
- `none` — span reference and path only, no excerpt

`masked` is the default because the highest-value finding this tool produces is "your agent leaked a card number", and writing that card number into a CI log that gets archived would reproduce the exact harm being reported.

## Architecture Overview

```
  OTLP trace (JSON / protobuf)
            │
            v
  ┌─────────────────────┐
  │ adapter otlp/v1.41  │   span tree ──► flat ordered lists
  │                     │   attribute mapping (§3)
  │                     │   total order (§5)
  │                     │   agent_path flattening (§6)
  └──────────┬──────────┘
             │
             ├──► coverage probe ──► Coverage{...}  ──┐
             │                                        │
             ├──► claim extractor ──► Claim{extractor}│
             │      structural | llm | plugin         │
             v                                        v
        Episode (episode/v1) ─────────────────► verdict class
             │                                  deterministic iff
             v                                  all inputs deterministic
        evaluators (CUE · Rego · metric · judge · wasm)
```

## Consequences

### Benefits

- Bundles are written against a schema we control, so a GenAI convention revision is one adapter change rather than an ecosystem-wide break.
- Flat ordered lists make the common policy shapes — set membership, ordering, counting — one-liners in Rego instead of tree walks.
- `Coverage` turns "we couldn't check this" from an invisible pass into an explicit, reportable state.
- Provenance-propagates-to-class closes the loophole where a strict engine over an inferred input would yield a falsely blocking verdict.
- Every field carries a `SpanRef`, so [ADR-001](001-agent-admission-control.md)'s "no finding without evidence" rule is structurally enforceable rather than a convention.
- Masked-by-default evidence means the tool does not re-leak what it was hired to detect.

### Trade-offs

- **Opt-in content caps what is checkable out of the box.** Most users' first run will skip the content clauses. Mitigation is documentation and `axda lint --trace`, which prints the exact instrumentation flag needed to enable each skipped clause — turning the limitation into an actionable setup step rather than a dead end. It remains the top adoption friction.
- **Flattening discards tree structure.** A policy that genuinely needs nesting depth has only `agent_path` to work with. Accepted: no v1 clause needs more, and `spans[]` is retained as an escape hatch.
- **Claim extraction is the weakest link in the schema.** Structural extraction only finds claims the agent bothered to mark, so `cite_sources` under-detects on unstructured prose. The LLM extractor covers that but downgrades the verdict to advisory. There is no third option; this is an honest limit of the approach.
- **The adapter is a permanent maintenance commitment** tracking a Development-stability spec that changed repositories two months ago.
- **Full content lives in memory**, so a very long episode is a large object. No streaming evaluation in v1; large-trace behaviour is untested and bounded only by a size guard.

### Out of scope

- Streaming or incremental Episode construction — [ADR-005](005-inline-admission-gate.md) needs it and specifies prefix semantics there.
- Adapters for framework-native formats. Backend-envelope adapters over the same semantic-convention vocabulary are in scope and specified per host ([ADR-007](007-agentcore-trace-acquisition.md)); adapters that would require a *different* vocabulary are not.
- Cross-episode identity (session stitching, user journeys).
- Metric-signal ingestion; v1 reads spans only.
- Cost derivation for unknown models — `cost_usd` is nil rather than guessed.

## Verification

- A golden OTLP trace with full content capture decodes to a fixture Episode, compared byte-for-byte.
- The same trace with content attributes stripped decodes to the same tool-call sequence, with `coverage.has_message_content = false` and identical `tool_calls[].name` values.
- Shuffling span order in the input payload produces a byte-identical Episode.
- A trace using deprecated `gen_ai.prompt` decodes with content populated and a `coverage.degraded` entry naming the deprecation.
- A two-level multi-agent trace produces one Episode where the sub-agent appears as a `kind: agent` tool call and its own tool calls carry the nested `agent_path`.
- A CUE rule reading an `extractor: llm` claim yields a verdict with `class: probabilistic`, and the run exits `0` despite the failure.
- `--evidence=masked` on the PII fixture emits `[redacted:card]` and the raw digits appear nowhere in the report.

## References

- [ADR-001](001-agent-admission-control.md) — Agent Admission Control (mandates the normalization layer)
- [ADR-003](003-contract-lowering.md) — Contract lowering (consumes `Coverage` to produce `skipped`)
- [ADR-004](004-wasm-plugin-abi.md) — Plugin ABI (Episode is the wire type)
- [OTel GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/) — `invoke_agent` / `chat` / `execute_tool` spans, `gen_ai.input.messages` schema, opt-in content capture
- [GenAI attribute registry](https://opentelemetry.io/docs/specs/semconv/registry/attributes/gen-ai/) — canonical attribute list
