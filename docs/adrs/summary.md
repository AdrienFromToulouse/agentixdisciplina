# Architecture Decision Records

**`axda`** is an out-of-band evaluator for AI agents. It reads a recorded trace, checks it against a versioned policy bundle, and emits a reliability score, a violation list, and span-anchored evidence.

The organising principle (**Agent Admission Control**) is that the evaluator is an external control plane and the agent remains ignorant of it, in the same way pods are ignorant of Kubernetes admission controllers and application code is ignorant of CI security scanners.

## Vocabulary

- **Episode**: one normalized agent run, holding its turns, tool calls, claims, metrics, coverage, and metadata. Decoded from OTLP by a versioned adapter. The unit of evaluation, and the only thing evaluators ever see.
- **Contract**: a declarative statement of what an agent must, must not, and may do (`kind: AgentContract`). The authoring surface.
- **Clause**: one named, registered predicate in a contract. Never free text, never interpreted.
- **Evaluator**: a check that maps an Episode to verdicts. Built-in (CUE, Rego, metric, LLM judge) or third-party (WASM). The compilation target of a contract.
- **Bundle**: a distributable, versioned directory holding a contract, its policies, its judge prompts, its plugins, and its golden traces. Resolved from a local path, git, or an OCI registry.
- **Verdict**: the result of one evaluator, one of `pass`, `fail`, `skipped`, or `errored`, classed `deterministic` (blocking) or `probabilistic` (advisory by default).
- **Coverage**: what a given trace can and cannot support. A clause whose requirements exceed coverage is `skipped`, never `passed`.
- **Evidence**: the span a violation points at. A finding without it is not shippable output.

## Foundations

| ADR | Decision | Status |
|-----|----------|--------|
| [001](001-agent-admission-control.md) | Agent Admission Control: Out-of-Band Trace Evaluation | Proposed |
| [002](002-episode-schema.md) | Episode Schema v1 and OTLP Attribute Mapping | Proposed |
| [003](003-contract-lowering.md) | Contract Lowering Specification | Proposed |

## Extensibility and distribution

| ADR | Decision | Status |
|-----|----------|--------|
| [004](004-wasm-plugin-abi.md) | WASM Plugin ABI `axda/plugin/v1` | Proposed |
| [008](008-verbatim-gated-extraction.md) | Verbatim-Gated Fact Extraction | Proposed |
| [006](006-oci-distribution.md) | OCI Bundle Distribution and Signing | Proposed |

## Enforcement surfaces

| ADR | Decision | Status |
|-----|----------|--------|
| [005](005-inline-admission-gate.md) | Inline Admission Gate | Proposed |

## Runtime integration

| ADR | Decision | Status |
|-----|----------|--------|
| [007](007-agentcore-trace-acquisition.md) | Trace Acquisition from AWS Bedrock AgentCore Runtime | Proposed |

## Invariants

Decisions that later ADRs may not quietly undo:

1. **The agent stays ignorant.** No SDK, no callback, no middleware, no import. The only coupling is the trace the agent already emits. ([001](001-agent-admission-control.md) §1, upheld under inline enforcement in [005](005-inline-admission-gate.md) §1 and against a real runtime in [007](007-agentcore-trace-acquisition.md) §1.)
2. **`skipped` is never `passed`, and neither is `errored`.** A check that could not run, or that broke, must never read as a check that succeeded. ([003](003-contract-lowering.md) §5, [004](004-wasm-plugin-abi.md) §7.)
3. **Determinism is a property of the whole input closure.** A verdict is blocking only if every input it read was deterministic, which is why capability-holding plugins are forced advisory. An LLM-extracted claim counts as deterministic only where a verbatim gate proved its evidence exists, which makes a failure blocking and a pass advisory. ([002](002-episode-schema.md) §4, [004](004-wasm-plugin-abi.md) §5, [008](008-verbatim-gated-extraction.md) §2.)
4. **No finding without a span.** Every violation resolves to a `trace_id`/`span_id`, and a plugin that fabricates one is `errored`. ([001](001-agent-admission-control.md) §6, [004](004-wasm-plugin-abi.md) §7.)
5. **Contracts are compiled, not interpreted.** Clause names resolve against a closed registry; an unknown name is a compile error, never a prompt. ([003](003-contract-lowering.md) §1.)
6. **The tool must not leak what it was hired to detect.** Evidence is masked by default and content-bearing telemetry is confined to a short-retention, access-controlled sink. ([002](002-episode-schema.md) §7, [007](007-agentcore-trace-acquisition.md) §3.)

## Open items

- ~~**Go module path**~~: resolved to `github.com/AdrienFromToulouse/agentixdisciplina`, matching the repository's canonical lowercase name. A vanity path (`axda.dev/axda`) is still an option before the first tag. ([001](001-agent-admission-control.md))
- **License**: Apache-2.0 assumed, not decided. ([001](001-agent-admission-control.md))
- **AgentCore dual-export behaviour**: whether telemetry duplicates to AWS *and* a custom endpoint when `DISABLE_ADOT_OBSERVABILITY` is unset is undocumented. Affects only [007](007-agentcore-trace-acquisition.md) Path B; the default CloudWatch path does not touch that variable. Revisit §6 once tested.
