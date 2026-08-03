# ADR-001: Agent Admission Control (Out-of-Band Trace Evaluation)

**Status:** Proposed
**Date:** 2026-08-02
**Authors:** Adrien
**References:** [OTel GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/), [Open Policy Agent](https://www.openpolicyagent.org/), [CUE](https://cuelang.org/), [wazero](https://wazero.io/)

## Context

### Agent assertions are written at the wrong altitude

The dominant way to test an agent today is a string comparison:

```python
assert response == expected_answer
```

This is brittle in the specific way that matters: it is simultaneously too strict and too weak. A correct answer that got reworded fails the test. An answer that got reworded *and leaked a card number* passes it. The assertion is anchored to the one property of the episode that is least stable and least important.

The properties teams actually care about are not properties of the final string. They are properties of the **episode**:

- it cited its sources
- it verified identity before taking an action
- it never called a tool outside the approved set
- the refund amount never exceeded the approved limit
- it did not invent a customer record

None of these can be expressed as an equality check on the output. All of them are trivially expressible over a trace.

### Evaluation is being built into the agent

The second failure is architectural. Frameworks increasingly ship in-process guardrails: callbacks, middleware, hooks, validators that run inside the agent's own event loop. This couples the checker to the checked, and every consequence of that coupling is bad:

- an agent can be configured (or can drift) into skipping its own guard
- the policy version is whatever the agent happened to deploy with, so tightening a rule requires a redeploy
- you cannot evaluate an agent you do not own
- you cannot evaluate a trace from last Tuesday
- the guard is written in the agent's language, so it cannot be shared across a polyglot fleet

We have solved this shape of problem before, and never by putting the check inside the thing being checked. Kubernetes does not ask pods to validate themselves; it has admission controllers. CI does not ask application code to scan itself; it has scanners. Runtime application security is a layer, not a library call.

### The prior art is inline, per-call, and single-engine

Policy-as-code for agents exists, but it has converged on one shape: an inline gate in front of a single tool call. TrueFoundry's OPA Guardrails evaluate Rego against LLM requests, responses, and MCP invocations at request time. Strata's Maverics AI Identity Gateway embeds OPA so every tool call must clear policy before reaching an upstream service. Microsoft's Agent Governance Toolkit (March 2026) ships framework middleware expressing policy in Rego or Cedar.

All of these answer the same question: **"may this call proceed?"**

None of them answers: **"did this episode satisfy its contract?"**

That second question is the one CI needs in order to gate a merge, and the one an incident review needs in order to explain what happened. It requires the whole trace, not a single call, and it requires more than one kind of check: a permission engine cannot tell you whether an answer was grounded, and an LLM judge cannot tell you whether a numeric invariant held.

### The substrate exists, and it is still moving

OpenTelemetry's GenAI semantic conventions now define agent, workflow, tool, and model spans, along with token-usage and latency metrics. Client spans exited experimental in early 2026. But agent, workflow, and tool spans (precisely the ones this tool depends on) remain at Development stability, and the conventions were moved out of the main semantic-conventions repository into a dedicated project (`gen_ai.*` deprecated there as of v1.42.0, June 2026).

So the substrate is real enough to build on and unstable enough that building directly against it would be a mistake. This tension drives Decision 2.

## Decision

Build **`axda`**, a single-binary Go CLI that evaluates a recorded agent trace against a versioned policy bundle and emits a reliability score, a violation list, and span-anchored evidence.

```
axda evaluate \
  --trace trace.json \
  --policy github.com/company/support-agent-evals@v1.2.0
```

The concept is **Agent Admission Control**: policy evaluation as an external control plane, with the agent as an unwitting subject.

### 1. Out-of-band by construction: the agent stays ignorant

There is no SDK to install, no callback to register, no middleware to mount, no import statement anywhere in the agent. The sole coupling between `axda` and the agent is the trace the agent already emits for observability reasons it had anyway.

Everything below is a consequence of this one decision, and each of these properties is unavailable to any in-process design:

| Property | Why it follows |
|---|---|
| The guard cannot be skipped by the guarded | The agent has no code path that touches the evaluator |
| Policy revs independently of deploys | Tightening a rule is a bundle bump, not a redeploy |
| Third-party agents are evaluable | You need their trace, not their source |
| History is evaluable | Last Tuesday's trace is still a file |
| One artifact covers CI, regression, and a future inline gate | The bundle does not know who is running it |
| Polyglot by default | Python, TypeScript, and hosted agents all emit OTLP |

### 2. Input is a normalized `Episode`, decoded by a versioned adapter

The canonical input is OTLP trace JSON: from a file (`--trace`), from stdin, or later from a span-export endpoint. But evaluators never see raw spans. The adapter decodes a trace into an **Episode**:

```
Episode
  ├─ Turns[]      user/assistant messages, model calls
  ├─ ToolCalls[]  name, args, result, error, span ref, timing
  ├─ Claims[]     assertions + citations extracted from output
  ├─ Metrics      latency, tokens, cost, retries, step count
  └─ Meta         agent id, model ids, trace id, timestamps
```

**Why normalize rather than expose spans directly.** Agent and tool span conventions are at Development stability and changed repositories two months ago. An ecosystem of policy bundles written against raw `gen_ai.*` attributes would rot on every convention revision, and the rot would be distributed across every bundle any user ever wrote. Against `Episode`, only the adapter moves.

The schema is versioned (`episode/v1`) and every bundle declares the version it targets, so an adapter change is a detectable, resolvable incompatibility rather than a silent misread.

Non-OTLP inputs (framework-native JSON exports) are additional adapters behind the same interface. They are never a second path into the core.

### 3. The contract is the authoring surface; evaluators are the compilation target

The headline artifact is a declarative contract with no framework vocabulary in it:

```yaml
apiVersion: axda.dev/v1
kind: AgentContract
metadata:
  name: support-agent
spec:
  allowed_tools:
    - crm.lookup
    - email.send
  must:
    - cite_sources
    - verify_identity_before_action
  must_not:
    - expose_pii
    - invent_customer_data
  invariants:
    - "refund.amount <= approved_limit"
```

The compiler lowers each clause onto a concrete evaluator:

| Clause | Lowers to | Engine |
|---|---|---|
| `allowed_tools` | set membership over `ToolCalls[].name` | Rego |
| `verify_identity_before_action` | temporal ordering predicate over the call sequence | Rego |
| `invariants` | constraint over extracted structured values | CUE |
| `expose_pii` | detector over assistant output and tool arguments | built-in detector + Rego verdict |
| `invent_customer_data` | every asserted record traces to a tool result | CUE (structural) |
| `cite_sources` | every claim has a supporting retrieval span | CUE (structural) + judge (advisory groundedness) |

The raw evaluator form remains available as the explicit escape hatch, and is simply the desugared representation:

```yaml
evaluators:
  - name: safety
    type: rego
    package: acme/security
  - name: factuality
    type: cue
    package: acme/facts
  - name: helpfulness
    type: llm
    model: claude-sonnet-5
  - name: latency
    type: metric
```

`axda explain --contract agent.yaml` prints the full lowering. This is not a nicety. The entire premise is that assertions should be written at a higher altitude than they are today; that premise collapses if the descent from contract to check is a black box the user cannot inspect, debug, or override.

### 4. Three engines, three questions, three epistemic classes

| Engine | Filters | Question |
|---|---|---|
| **CUE** | what the agent **believed** | Is this fact consistent? |
| **Rego** | what the agent **did** | Was this action allowed? |
| **LLM judge** | what the agent **said** | Was this useful? |
| **metric** | what the agent **cost** | Was this within budget? |

CUE is chosen for the belief layer specifically because unification is the right primitive for consistency: you are asking whether a set of extracted values can coexist with a schema and with each other, not writing imperative assertions one at a time. Rego is chosen for the action layer because permission-over-a-log is exactly the question OPA was built for: the only novelty is that the input is an episode rather than a single request. The judge exists because helpfulness is irreducibly subjective and pretending otherwise produces bad rules rather than no rules. `metric` is a built-in threshold checker rather than a real engine.

This split is not taxonomy for its own sake. It sorts verdicts by **how much they may be trusted**, which Decision 6 then consumes directly.

### 5. Built-in engines are compiled in; third-party evaluators are WASM

`axda` ships as one static binary with CUE (`cuelang.org/go`), Rego (`github.com/open-policy-agent/opa/rego`), the metric evaluator, and the LLM judge linked in. No CGO, no runtime dependencies, cross-compiles everywhere.

Third-party evaluators are **WASM modules executed via wazero** (a pure-Go runtime, which preserves both of those properties).

**Why WASM over the alternatives.** An evaluator is a pure function `Episode → []Verdict`, so it does not need the ambient authority that a subprocess model hands out for free. That matters here more than it usually does, because the whole point of `--policy github.com/some-org/evals` is that you run policy bundles **authored by other people**. A bundle is a distributable artifact pulled over the network and then handed your production trace, which contains, by construction, everything your agent said and every argument it passed to every tool.

Under wazero, a module gets no filesystem, no clock, no network, and no environment unless the host explicitly grants a capability. A hostile bundle therefore cannot exfiltrate the trace it was invited to inspect. The Terraform-style gRPC-subprocess model and the exec-a-binary model both give that away by default. Deterministic replay falls out of the same property for free.

ABI `axda/plugin/v1`: the guest exports `evaluate`; the host passes a length-prefixed protobuf `Episode` and reads back `[]Verdict`. Capabilities (`http`, `judge`, `read_file`) are declared in bundle metadata and granted per-evaluator. A plugin that wants to run its own LLM judge requests `judge` and calls back into the host's configured provider rather than opening a socket of its own.

**The cost, stated plainly:** plugin authors need a WASM toolchain, TinyGo, Rust, or Go 1.24+ with `GOOS=wasip1`. This is a real barrier. It is accepted because the built-in engines cover the common cases, making third-party WASM the extension path rather than the default path.

### 6. Verdicts are tiered: only deterministic ones fail the build

Every verdict carries a class:

- **`deterministic`**: CUE, Rego, metric. Same episode plus same bundle yields the same verdict, always. **Blocking.**
- **`probabilistic`**: LLM judge. **Advisory by default**; blocks only when the clause explicitly sets `blocking: true`.

The reasoning is operational rather than philosophical. An evaluation tool that turns CI red on a rerun with no code change gets disabled within a month, and then the org has a policy bundle nobody enforces. Determinism is the property that makes this gateable at all, so the default must protect it.

Advisory does not mean unactionable. Judge verdicts carry `model_id`, `prompt_hash`, `effort`, and the judge's own reasoning as evidence, so a failing quality signal can be reviewed and, if it proves stable, promoted to blocking.

> **Correction (2026-08-03).** This section originally named `temperature` as the recorded provenance field. That parameter is rejected outright by current frontier models (`temperature`, `top_p`, and `top_k` return a 400 on Claude Opus 5) so there is no temperature to record. The reproducibility knob is `output_config.effort`, and that is what the implementation stores. Verdicts are additionally cached by `(model, effort, prompt hash)`, which is what makes repeated runs over an unchanged trace stable in practice without pretending a judge is deterministic.

Report schema `axda.dev/report/v1`:

```json
{
  "schema": "axda.dev/report/v1",
  "reliability_score": 0.82,
  "gate": "fail",
  "violations": [
    {
      "clause": "must_not.expose_pii",
      "evaluator": "axda.pii",
      "class": "deterministic",
      "severity": "critical",
      "evidence": {
        "trace_id": "a19c...",
        "span_id": "7b3f...",
        "path": "turns[3].assistant.content",
        "excerpt": "…card ending 4242…"
      }
    }
  ]
}
```

Exit codes: `0` pass, `1` blocking violation, `2` bundle or trace error. Reporters: `human` (default), `json`, `sarif` (which lands violations in GitHub code scanning with no extra integration work), and `junit`.

**Every violation must point at a span.** A finding without evidence is not shippable output; it is a vibe with a severity label attached, and it is the reason eval scores get ignored.

### 7. Bundles are git-resolved and lockfile-pinned

Resolution goes through a `Resolver` interface. v1 ships two implementations: `file://` for local development, and `git` for everything else.

```
--policy github.com/company/support-agent-evals@v1.2.0
```

Git is chosen over an OCI registry for v1 because it reuses the user's existing SSH keys and tokens, which means private bundles work immediately with no new infrastructure and no publishing step standing between someone and their first run. OCI (with its stronger supply-chain story of immutable digests and cosign signatures) is a later implementation of the same interface, not a rewrite.

Resolved bundles are content-hashed into `axda.lock`. `--frozen` fails when the lock is stale, so CI is reproducible or it errors.

Bundle layout:

```
support-agent-evals/
  axda.yaml           # name, version, episode schema version, requested capabilities
  contract.yaml       # AgentContract
  policy/*.rego
  schema/*.cue
  judges/*.md         # judge prompt templates
  plugins/*.wasm      # optional third-party evaluators
  testdata/*.json     # golden traces
```

`testdata/` is not optional decoration. A policy bundle is code, so it ships with fixture traces and `axda test` runs the bundle against them. A policy that has never been exercised against a *failing* trace is how you end up with a green CI check that verifies nothing.

## Architecture Overview

```
   Agent Runtime  ──OTLP──>  trace.json / collector
   (ignorant)                        │
 ═════════════════════════ trust boundary ═════════════════════════
                                     v
                     OTLP → Episode adapter (episode/v1)
                                     │
                AgentContract ───────┴─────── compiles to evaluators
                                     │
          ┌──────────┬───────────────┼───────────────┬──────────┐
          v          v               v               v          v
         CUE       Rego           metric          judge      wasm/*
      believes      does        thresholds         says     3rd party
          │          │               │               │          │
          └───── deterministic ──────┘          probabilistic ───┘
                      │                               │
                   blocking                        advisory
                      └───────────────┬───────────────┘
                                      v
                    Report: score + violations + span evidence
                     exit 0 / 1 / 2 · human | json | sarif | junit
```

The trust boundary is the point of the diagram. Everything above it is the agent's world and is untouched. Everything below it is the control plane.

## Consequences

### Benefits

- The guard cannot be disabled by the thing it guards, because the thing it guards has no reference to it.
- Policy versions and agent deploy versions move independently; tightening a rule is a bundle bump.
- Agents you did not write, in languages you do not use, are evaluable from their traces alone.
- Deterministic-by-default verdicts make the tool safe to put in a merge gate, which is the only place it will actually change behavior.
- Every violation is anchored to a span, so a failure is a starting point for debugging rather than a score to argue with.
- One artifact serves CI gating, nightly regression, incident review, and (later) an inline admission gate.
- The three-engine split gives each question to the tool that can actually answer it, instead of forcing permissions into a judge prompt or quality into a Rego rule.

### Trade-offs

- **Trace quality is a hard dependency, and this is the largest adoption risk.** `axda` can only assert what the trace recorded; an agent that does not emit tool spans cannot be checked for tool-allowlist violations. Mitigation: `axda lint --trace` reports which contract clauses are unevaluable against a given trace's coverage, and unevaluable clauses report **`skipped`, never `passed`**. Silently passing a clause we could not evaluate would be the single worst failure mode available to this tool: it would hand someone a green check that verifies nothing and tell them they were safe.
- **Post-hoc means the damage already happened.** v1 detects, it does not prevent. Inline admission is deferred (ADR-005), not denied, and Decision 7's bundle format is designed so the same artifact drives both.
- **Semantic-convention churn lands on us.** The adapter absorbs it on users' behalf, but that means the adapter is a maintenance commitment that tracks a Development-stability spec.
- **A single `reliability_score` invites vanity-metric use**: the number will end up on a dashboard and someone will optimize it. Mitigation: the score is never the gate. The gate is the violation list; the score is a summary of it.
- **Three languages in one tool** (CUE, Rego, judge prompts) is a real learning cost. The contract layer hides it for the common cases; the escape hatch does not, and anyone who needs the escape hatch pays the full price.
- **WASM raises the floor for plugin authors** relative to "write a script that reads stdin". Accepted in exchange for being able to run strangers' policy bundles against production traces.

### Out of scope (v1)

- Inline or blocking admission at runtime: [ADR-005](005-inline-admission-gate.md).
- Trace **collection**. `axda` consumes traces; it is not a collector, an exporter, or a backend. Getting a trace out of a specific runtime is a configuration problem, documented per host: [ADR-007](007-agentcore-trace-acquisition.md) covers AWS Bedrock AgentCore.
- Fleet aggregation, trend dashboards, and cross-episode statistics.
- Auto-remediation or prompt-repair suggestions.
- OCI bundle distribution: [ADR-006](006-oci-distribution.md).
- Adapters beyond OTLP plus one reference framework adapter.

## Verification

These are the acceptance criteria for the implementation that follows this ADR:

- A committed golden trace with a planted PII leak evaluates to exit `1`, with one critical violation carrying a resolvable `trace_id`/`span_id` pair.
- The corresponding clean trace evaluates to exit `0`.
- **Determinism:** the same trace and bundle, judges disabled, run ten times, produce byte-identical reports modulo timestamps.
- **Skipped ≠ passed:** a trace stripped of tool spans, evaluated against a contract with `allowed_tools`, reports that clause as `skipped` and says so in the human reporter.
- `axda test` on the reference bundle requires every policy to have both a passing and a failing fixture, and fails the bundle if one is missing.
- Mutating a remote bundle and re-running with `--frozen` exits `2`.
- A WASM plugin attempting network or filesystem access without a declared capability traps; its evaluator is marked `errored` and never `passed`.

## Follow-up ADRs

| ADR | Scope |
|---|---|
| [002](002-episode-schema.md) | Episode schema v1: field-level specification and OTLP attribute mapping |
| [003](003-contract-lowering.md) | Contract lowering specification: the full clause vocabulary and its compilation |
| [004](004-wasm-plugin-abi.md) | WASM plugin ABI `axda/plugin/v1`: memory layout, capability grants, versioning |
| [005](005-inline-admission-gate.md) | Inline admission gate: partial-episode semantics, latency budget, fail-open policy |
| [006](006-oci-distribution.md) | OCI bundle distribution and signing |
| [007](007-agentcore-trace-acquisition.md) | Trace acquisition from AWS Bedrock AgentCore Runtime |

## Open items

- ~~**Go module path.**~~ **Resolved.** The repository's canonical name is lowercase (`AdrienFromToulouse/agentixdisciplina`), so the module path is `github.com/AdrienFromToulouse/agentixdisciplina`. This was not a design choice in the end: a mixed-case path would simply not have matched the repository, and `go get` would have failed. A vanity path such as `axda.dev/axda` remains available and would still need settling before the first tag, since the module path is effectively permanent afterwards.
- **License.** Apache-2.0 is the assumed default for an OSS policy tool with a plugin ecosystem: it grants a patent license that MIT does not, which matters when third parties are distributing bundles and modules. Not yet decided.

## References

- [OpenTelemetry GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/): agent, workflow, tool, and model spans; `gen_ai.*` moved out of the main semconv repo in v1.42.0 (June 2026), agent-layer spans still at Development stability
- [Open Policy Agent / Rego](https://www.openpolicyagent.org/): action-layer engine
- [CUE](https://cuelang.org/): belief-layer engine
- [wazero](https://wazero.io/): pure-Go WebAssembly runtime, plugin sandbox
- [TrueFoundry OPA Guardrails](https://www.truefoundry.com/docs/ai-gateway/opa-guardrails): prior art, inline per-call gating
- [Why OPA is the missing guardrail for AI agents](https://codilime.com/blog/why-use-open-policy-agent-for-your-ai-agents/): prior art, gateway-embedded policy
- [SARIF 2.1.0](https://docs.oasis-open.org/sarif/sarif/v2.1.0/sarif-v2.1.0.html): violation reporting format for code-scanning integration
