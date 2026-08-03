<p align="center">
  <img src="docs/assets/logo.png" alt="AgentixDisciplina" width="380">
</p>

<h1 align="center">axda</h1>

<p align="center"><em>Agent Admission Control: out-of-band evaluation for AI agents.</em></p>

---

`axda` reads a recorded agent trace, checks it against a contract, and emits a reliability score, a violation list, and span-anchored evidence.

The agent never knows it exists. No SDK, no callback, no middleware, no import: the only coupling is the OpenTelemetry trace the agent already emits. The same relationship Kubernetes admission controllers have to pods, and CI security scanners have to application code.

```
axda evaluate --contract agent.yaml --trace trace.json
```

## Why not just assert on the output

```python
assert response == expected_answer
```

This is simultaneously too strict and too weak. A correct answer that got reworded fails. An answer that got reworded *and leaked a card number* passes.

The properties worth asserting are properties of the **episode**, not the final string:

```yaml
spec:
  allowed_tools: [crm.lookup, crm.verify_identity, billing.refund, email.send]

  # Every operand says where it comes from. `cardinality` is required, not
  # defaulted: a policy that silently checks only the first of five refunds is
  # the exact bug this field prevents.
  values:
    refund.amount:
      from: tool_call
      tool: billing.refund
      arg: amount
      cardinality: any                     # must hold for EVERY refund
    approved_limit:
      from: tool_result
      tool: crm.lookup
      path: $.customer.refund_limit
      cardinality: last

  invariants:
    - "refund.amount <= approved_limit"

  must:
    - kind: order.requires_precondition
      action: billing.refund
      precondition: crm.verify_identity

  must_not:
    - kind: content.no_pii
      types: [card, ssn, email]
      allow_in_tool_args: [email.send]     # sending an address to email.send is the job
```

Clause names resolve against a closed registry. An unknown name is a **compile error, never a prompt**: a contract that reads like prose but is *understood* like prose would just be a prompt with YAML syntax. The same applies to invariants: an operand you did not declare fails at compile time, before any trace is read.

Three engines, three questions ([ADR-003 §4](docs/adrs/003-contract-lowering.md)):

| Engine | Filters | Question |
|---|---|---|
| **Rego** (embedded OPA) | what the agent **did** | Was this action allowed? |
| **CUE** | what the agent **believed** | Is this value consistent? |
| **metric** | what the agent **cost** | Was this within budget? |

`axda explain` prints the full lowering, so the descent from contract to check is never a black box.

## Three rules it will not break

**A skip is never a pass.** GenAI message content is opt-in and off by default, so most real traces cannot support a PII check. When that happens the clause reports `SKIP` with the exact environment variable that would fix it. It never reports green.

**Determinism is a property of the whole input closure.** Only deterministic verdicts fail a build. Anything downstream of an LLM (a judge, an inferred claim) is advisory unless you explicitly opt in, because a check that goes red on a rerun gets disabled within a month.

**No finding without a span.** Every violation resolves to a `trace_id`/`span_id`. A finding you cannot navigate to is a vibe with a severity label.

## Install

```bash
go build -o axda ./cmd/axda
```

## Quickstart: a local trace

```bash
./axda explain  --contract examples/support-agent/contract.yaml
./axda evaluate --contract examples/support-agent/contract.yaml \
                --trace    examples/support-agent/testdata/violating.json
```

```
support-agent · 8 clauses · score 0.30 · FAIL
episode f3b5dec05a71… · adapter otlp/v1.41 · 8 spans · 4 tool calls · 7 turns

  FAIL  tool.allowlist                     critical  1 violation(s)
        └ tool "internal.debug_dump" is not in the allowed set
          tool_calls[2] · span 0000000000000005
  FAIL  order.requires_precondition        critical  1 violation(s)
        └ "billing.refund" ran with no completed "crm.verify_identity" before it
          tool_calls[1] · span 0000000000000004
  FAIL  must_not.content.no_pii            critical  1 violation(s)
        └ card exposed in assistant turn
          The refund went back to your Visa [redacted:card ****4242]. You should…
          turns[4].text · span 0000000000000007
  PASS  tool.call_limit                    major     ok
  ...

  3 passed · 5 failed · 0 skipped · 0 errored
```

Exit codes: `0` pass · `1` blocking violation · `2` contract or trace error.

## Quickstart: AWS Bedrock AgentCore

No collector, no sidecar, and **no change to your image**. If your Dockerfile already ends with

```dockerfile
CMD ["opentelemetry-instrument", "python", "-m", "src.agui_server"]
```

then you are already emitting everything `axda` needs.

**1. Enable CloudWatch Transaction Search** (account-level, once). AgentCore delivers spans to the shared `aws/spans` log group, in OTel semantic-convention format with W3C trace ids, at 100% ingestion.

**2. Turn on content capture**: the only environment variable you add:

```
OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true
```

Without it, tool and budget clauses still work; content clauses report `SKIP`. Note that with it, prompts and tool arguments land in CloudWatch: set a short retention on the log group.

**3. Fetch and evaluate:**

```bash
./axda trace fetch --from cloudwatch --session "$SESSION_ID" --out trace.json
./axda evaluate --contract examples/support-agent/contract.yaml --trace trace.json
```

Or in one step:

```bash
./axda evaluate --contract agent.yaml --from cloudwatch --session "$SESSION_ID"
```

> **The span record schema is unverified.** AWS does not publish a stable JSON schema for `aws/spans` records, so the decoder is written defensively against likely field namings. **Run this first:**
>
> ```bash
> ./axda trace fetch --from cloudwatch --session "$SESSION_ID" --raw | head -50
> ```
>
> If the mapping is wrong, that output is what fixes it.

## What works today

| | |
|---|---|
| Adapters | `otlp/v1.41` (file or stdin), `cloudwatch-spans/v1` (`aws/spans`) |
| Trace fetch | CloudWatch by session id or trace id, with settle-polling and `--raw` |
| **Rego** clauses | `tool.allowlist` · `tool.denylist` · `tool.call_limit` · `order.requires_precondition` · `order.before` |
| **CUE** clauses | `invariants` with declared value bindings · `tool.args_match` · `grounding.cite_sources` |
| **judge** clauses | `quality.judge` · `quality.helpful` · `quality.on_topic` · `quality.tone` · `grounding.judge`: advisory, cached |
| builtin clauses | `content.no_pii` (Luhn-checked) · `content.deny_patterns` · `grounding.no_unsourced_claims` |
| metric clauses | `budget.max_{duration_ms,steps,tokens,tool_errors}` |
| Claim extraction | `structural` (deterministic) or `llm` behind a **verbatim gate**: the model must quote character-for-character or the row is discarded |
| Value bindings | `from: tool_call \| tool_result \| metric \| literal`, JSONPath-lite, `cardinality: any \| first \| last \| exactly_one`, `default` |
| Custom clauses | namespaced Rego declared in the contract, compiled and checked at load |
| Coverage | `SKIP` with remediation hints; `--fail-on-skipped` to gate on instrumentation |
| Evidence | `--evidence masked` (default) `full` `none`; last-4 preserved on cards |
| Output | human, `--json` (`axda.dev/report/v1`) |

### Custom clauses

Bring your own Rego. It must be namespaced: the bare namespaces are reserved so a contract cannot shadow `tool.allowlist` and change what an existing clause means.

```yaml
clauses:
  - name: acme.no_weekend_refunds
    engine: rego
    source: policy/weekend.rego
    query: data.acme.weekend.violation
    requires: [has_tool_args]
spec:
  must:
    - kind: acme.no_weekend_refunds
      max_duration_ms: 50
```

A policy that does not compile is a **load-time** error, not a mid-run surprise. Custom findings carry span evidence like any built-in.

### Fact extraction and the verbatim gate

`structural` extraction is deterministic but only finds claims that assert a concrete value, so it misses anything stated in prose. `--extractor llm` closes that gap without handing fabrication to a model:

```bash
./axda evaluate -c agent.yaml -t trace.json --extractor llm
```

The model is shown the episode as addressable `<source>` blocks and must return, per fact, the source id and a snippet **copied character-for-character**. The code then locates that snippet: exact substring first, then allowing flexible whitespace *between* words for hard-wrapped lines, each word spelled exactly as the source spells it. A row whose snippet cannot be located is **discarded, never repaired**, and the stored citation is the source's own bytes rather than the model's retyping.

That buys precision, not recall, so the uncertainty is one-sided and the verdict class follows it:

| Outcome | Rests on | Class |
|---|---|---|
| a violation is found | a quote verified in code | **deterministic**, blocks |
| no violation is found | the extractor having looked everywhere | **probabilistic**, advisory |

A rejected row means the *extractor* quoted something absent from the trace. That is never charged to the agent: it lands in `coverage.degraded`. If the extractor cannot run, claim-reading clauses `SKIP` rather than silently falling back to `structural`.

### LLM judges

Judges filter what the agent *said*: helpfulness, tone, groundedness. They are **advisory**: a judge verdict never fails the build unless the clause sets `blocking: true`, and because `Blocks()` requires the deterministic class, even that cannot make a probabilistic verdict gate.

```yaml
  must:
    - kind: quality.judge
      rubric_file: judges/politeness.md    # inlined at load; a missing file fails there
    - kind: quality.tone
      style: warm but concise
```

```bash
export ANTHROPIC_API_KEY=...
./axda evaluate -c agent.yaml -t trace.json          # judges run
./axda evaluate -c agent.yaml -t trace.json --no-judge
./axda evaluate -c agent.yaml -t trace.json --judge-model claude-opus-5 --judge-effort medium
```

With no credentials, judge clauses report `SKIP` with the reason: never `PASS`. Verdicts carry `model_id`, `prompt_hash`, and `effort` as provenance, and are cached in `.axda/judge-cache.json` keyed by all three, so re-running over an unchanged trace is stable and free. Caching does not make a judge deterministic, which is exactly why they stay advisory.

Default model is `claude-opus-5` at `effort: low`, since scoring a transcript against a rubric is a scoped classification task. Both are per-run flags.

## Not built yet

Specified in the ADRs, absent from the binary:

- **Extractor recall is unmeasured.** Nothing reports how many claims the LLM extractor missed; the advisory class on a pass is an admission of that, not a fix ([ADR-008](docs/adrs/008-verbatim-gated-extraction.md)).
- **`from: claim_value` value bindings**, which would let an invariant read an extracted fact ([ADR-003 §4](docs/adrs/003-contract-lowering.md)).
- **Policy bundles**: v0 takes `--contract FILE`, and custom clauses live in the contract rather than in bundle metadata. Git and OCI resolution, lockfiles, and signing are [ADR-001 §7](docs/adrs/001-agent-admission-control.md) and [ADR-006](docs/adrs/006-oci-distribution.md).
- **WASM plugins** ([ADR-004](docs/adrs/004-wasm-plugin-abi.md)) and the **inline admission gate** ([ADR-005](docs/adrs/005-inline-admission-gate.md)).
- `axda test`, `axda lint`, SARIF and JUnit reporters.
- **`must_not` polarity inversion.** Every registered kind is a violation predicate and aliases carry the polarity (`expose_pii` → `content.no_pii`), so position sets severity defaults rather than inverting a clause ([ADR-003 §3](docs/adrs/003-contract-lowering.md)).
- **`content.no_pii` is builtin-only.** [ADR-003 §2](docs/adrs/003-contract-lowering.md) pairs the detector with a Rego verdict; regex and Luhn are not expressible as policy, and the second hop bought nothing.

Embedding OPA and CUE costs real weight: the binary is ~47 MB and evaluation takes ~40 ms per run, most of it one-time Rego compilation.

## Design

The architecture is settled in [docs/adrs](docs/adrs/summary.md): read [the index](docs/adrs/summary.md) first; it lists six invariants later work may not quietly undo.

| ADR | |
|---|---|
| [001](docs/adrs/001-agent-admission-control.md) | Agent Admission Control: out-of-band trace evaluation |
| [002](docs/adrs/002-episode-schema.md) | Episode schema v1 and OTLP attribute mapping |
| [003](docs/adrs/003-contract-lowering.md) | Contract lowering specification |
| [004](docs/adrs/004-wasm-plugin-abi.md) | WASM plugin ABI `axda/plugin/v1` |
| [005](docs/adrs/005-inline-admission-gate.md) | Inline admission gate |
| [006](docs/adrs/006-oci-distribution.md) | OCI bundle distribution and signing |
| [007](docs/adrs/007-agentcore-trace-acquisition.md) | Trace acquisition from AWS Bedrock AgentCore |

## Tests

```bash
go test ./...
```

The suite asserts the three rules above directly rather than testing around them:

- a clause that fails with content present reports `SKIP` (not `PASS`) when content is stripped, and an invariant whose operand never bound does the same
- `cardinality: any` catches a bad value in *third* position, not just the first
- ten runs over one trace produce byte-identical reports, and shuffling span order changes nothing: including through Rego, whose result sets have no inherent order
- a masked report does not contain the card number it reports on
- an undeclared invariant operand, a missing `cardinality`, a non-namespaced custom clause, and a Rego module that does not compile all fail at **load**, before any trace is read

## Status

Pre-alpha. The contract surface, report schema, and Episode model follow the ADRs and are meant to be stable; everything else moves.
