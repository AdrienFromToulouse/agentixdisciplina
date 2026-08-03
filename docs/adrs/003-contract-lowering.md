# ADR-003: Contract Lowering Specification

**Status:** Proposed
**Date:** 2026-08-02
**Authors:** Adrien
**References:** [ADR-001](001-agent-admission-control.md), [ADR-002](002-episode-schema.md), [ADR-004](004-wasm-plugin-abi.md), [ADR-005](005-inline-admission-gate.md)

## Context

[ADR-001](001-agent-admission-control.md) made the contract the headline authoring surface and showed six illustrative lowerings. It left three things unspecified, and each is a place where the design could quietly fail.

**What is a clause, exactly?** `must: [cite_sources]` looks like English. If it is English, something has to interpret it, and the only thing that can interpret arbitrary English is a model, which would make every contract clause probabilistic and destroy the blocking-verdict guarantee that makes the tool gateable at all. The resolution has to be explicit.

**Where do invariant operands come from?** The brief's example is `refund.amount <= approved_limit`. Neither identifier exists anywhere in an Episode. `refund.amount` is presumably an argument to some tool; `approved_limit` is presumably a field of some earlier tool's result. Nothing in the contract says which. This is the largest hole in the original sketch: the most compelling clause type in the pitch has no defined semantics.

**How does a clause become `skipped`?** [ADR-002](002-episode-schema.md) produces `Coverage`, but nothing yet connects a missing coverage flag to a clause that cannot run. Without that link, [ADR-001](001-agent-admission-control.md)'s central safety property (`skipped` is never `passed`) has no implementation.

## Decision

### 1. Clause names are a closed, versioned registry: never interpreted

A clause name resolves against a registry of `ClauseKind` definitions. An unknown name is a **compile error**, not a prompt.

```
$ axda explain --contract agent.yaml
error: unknown clause "be_polite_to_customers" (contract.spec.must[2])
  axda does not interpret free-text clauses.
  did you mean: quality.tone?
  to express a subjective requirement, declare a judge:
      must:
        - kind: quality.judge
          rubric: judges/politeness.md
```

This is the decision the whole system rests on. A contract that reads like prose but compiles to a fixed predicate is a usable abstraction; a contract that reads like prose and is *understood* like prose is a prompt with YAML syntax, and it would inherit every property (non-determinism, prompt injection, silent drift) that [ADR-001](001-agent-admission-control.md) built this tool to escape. Subjectivity is available, but it must be asked for by name.

Each `ClauseKind` declares:

```
ClauseKind
  name           string        namespaced: "tool.allowlist", "acme.verify_kyc"
  params         schema        typed parameter schema
  engine         enum{rego, cue, metric, judge, wasm, builtin}
  class          enum{deterministic, probabilistic}
  requires       []string      Coverage flags needed (§5)
  prefix_decidable bool        can this decide on a partial episode (ADR-005)
  default_severity enum{critical, major, minor}
  default_blocking bool
```

### 2. The v1 clause vocabulary

Shorthand string form desugars to the object form: `cite_sources` becomes `{kind: grounding.cite_sources}` with registry defaults filled in.

**Tool clauses**: over `Episode.tool_calls`, engine Rego, deterministic.

| Clause | Params | Requires |
|---|---|---|
| `tool.allowlist` (`allowed_tools`) | `tools[]`, `agent_path?` | none |
| `tool.denylist` (`denied_tools`) | `tools[]` | none |
| `tool.call_limit` | `tool?`, `max` | none |
| `tool.no_retry_after_error` | `tool?`, `max_retries` | none |
| `tool.args_match` | `tool`, `schema` (CUE) | `has_tool_args` |

**Ordering clauses**: over the total order from [ADR-002 §5](002-episode-schema.md), engine Rego, deterministic.

| Clause | Params | Requires |
|---|---|---|
| `order.before` | `first`, `then` | none |
| `order.requires_precondition` (`verify_identity_before_action`) | `action`, `precondition`, `within?` | none |
| `order.requires_confirmation` | `action`, `confirmation_turn_pattern` | `has_message_content` |

Overlapping spans are treated as **unordered**: `order.before` is violated only when `then` starts strictly after `first` ends. This is what keeps policies from depending on [ADR-002](002-episode-schema.md)'s arbitrary tie-break rule.

**Grounding clauses**: over `Episode.claims`.

| Clause | Params | Engine | Requires |
|---|---|---|---|
| `grounding.cite_sources` | `min_support` | CUE | `has_message_content` |
| `grounding.no_unsourced_claims` (`invent_customer_data`) | `value_types[]` | CUE | `has_message_content`, `has_tool_results` |
| `grounding.judge` | `rubric` | judge | `has_message_content` |

Per [ADR-002 §4](002-episode-schema.md), these emit `probabilistic` verdicts whenever the claims they read came from an LLM extractor, regardless of engine.

**Content clauses**: over turn content and tool arguments.

| Clause | Params | Engine | Requires |
|---|---|---|---|
| `content.no_pii` (`expose_pii`) | `types[]`, `allow_in_tool_args?` | builtin + Rego | `has_message_content` |
| `content.no_secrets` | `patterns[]` | builtin | `has_message_content` |
| `content.deny_patterns` | `patterns[]` (RE2) | builtin | `has_message_content` |

`allow_in_tool_args` exists because sending a card number to `billing.charge` is the job, and flagging it would train users to disable the check. Policies express *where* PII may travel, not merely whether it appears.

**Budget clauses**: over `Episode.metrics`, engine metric, deterministic.

| Clause | Params | Requires |
|---|---|---|
| `budget.max_duration_ms` | `value` | none |
| `budget.max_tokens` | `value` | `has_token_usage` |
| `budget.max_cost_usd` | `value` | `has_cost` |
| `budget.max_steps` | `value` | none |

**Quality clauses**: engine judge, **probabilistic**, non-blocking unless `blocking: true`.

`quality.judge` (free-form rubric), `quality.tone`, `quality.on_topic`, `quality.helpful`.

**Invariant clauses**: engine CUE, deterministic, covered in §4.

### 3. `must`, `must_not`, and severity defaults

`must_not: [x]` compiles to clause `x` with its polarity inverted, the clause reports a violation when its predicate *holds*. Not every clause is invertible (`budget.max_tokens` is already a bound); non-invertible kinds are rejected under `must_not` at compile time.

| Position | Default severity | Default blocking |
|---|---|---|
| `must_not` | critical | yes |
| `must` | major | yes |
| `allowed_tools` | critical | yes |
| any probabilistic clause | minor | **no** |

Any of these is overridable per clause. A probabilistic clause set to `blocking: true` emits a warning naming the reproducibility risk once per run, allowed, but not silently.

### 4. Invariants operate over an explicit value binding

An invariant is a CUE constraint over **named values that the contract declares how to extract**. Identifiers are not guessed from the trace.

```yaml
spec:
  values:
    refund.amount:
      from: tool_call
      tool: billing.refund
      arg: amount
      cardinality: any        # any | first | last | exactly_one
    approved_limit:
      from: tool_result
      tool: crm.lookup
      path: $.customer.refund_limit
      cardinality: last
    customer.tier:
      from: tool_result
      tool: crm.lookup
      path: $.customer.tier
      default: "standard"

  invariants:
    - "refund.amount <= approved_limit"
    - 'customer.tier == "enterprise" || refund.amount <= 500'
```

Extraction sources: `tool_call` (argument), `tool_result` (JSONPath into a result), `metric`, `claim_value`, `literal`.

The rules that make this safe:

- **Undeclared identifier is a compile error.** `axda explain` fails loudly rather than evaluating against an absent value.
- **Cardinality is explicit.** `refund.amount` with `cardinality: any` means the constraint must hold for *every* matching call, the reading a reviewer expects. `exactly_one` fails the clause if the tool was called zero or several times. Making this a required choice avoids the entire class of bug where a policy silently checks only the first of five refunds.
- **Missing value with no `default` → the clause is `skipped`, not passed.** Consistent with §5.
- **Values inherit provenance.** A value from an `extractor: llm` claim makes its invariants probabilistic ([ADR-002 §4](002-episode-schema.md)).

Constraints are evaluated by CUE with the bound values unified into a scope. CUE, not a bespoke expression language, because these are exactly value constraints under unification, and reusing it keeps one dependency doing one job.

### 5. Coverage requirements produce `skipped`

Every `ClauseKind` declares `requires`. At plan time the compiler intersects each clause's requirements with `Episode.coverage`:

```
requires ⊆ coverage  →  clause runs
requires ⊄ coverage  →  clause reports SKIPPED, listing the missing flags
```

A skipped clause is never `pass`. It appears in the report as its own state, is counted separately, and the human reporter prints it above the passes so it cannot be scrolled past:

```
support-agent · 11 clauses · score 0.78 · FAIL

  FAIL  must_not.expose_pii            critical   turns[3].assistant.content
  PASS  tool.allowlist                 4 calls, all permitted
  SKIP  grounding.cite_sources         needs: has_message_content
        └ trace captured no message content
          enable: OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true
  ...
```

`--fail-on-skipped` promotes any skip to a blocking failure, for teams that want their CI to assert instrumentation coverage as well as behaviour. Off by default (it would fail every first run), but it is the setting a mature deployment should end up at.

### 6. Compilation produces an inspectable plan

`axda explain --contract agent.yaml` emits the full lowering as a plan document: the artifact a reviewer reads to confirm the contract means what they think:

```
plan: support-agent (contract axda.dev/v1, episode/v1)

  must_not.expose_pii
    ├─ kind      content.no_pii {types: [card, ssn, email]}
    ├─ engine    builtin:axda.pii → rego:axda.content.deny
    ├─ class     deterministic     blocking: yes     severity: critical
    ├─ requires  has_message_content
    ├─ reads     turns[].content, tool_calls[].arguments
    └─ inline    yes (prefix-decidable)

  invariants[0]  "refund.amount <= approved_limit"
    ├─ engine    cue
    ├─ binds     refund.amount   ← tool_call(billing.refund).arg(amount) [any]
    │            approved_limit  ← tool_result(crm.lookup).$.customer.refund_limit [last]
    ├─ class     deterministic     blocking: yes
    └─ inline    yes
```

The plan is content-hashed into the report, so a report always states which lowering produced it. A contract change that alters the plan is visible in a diff.

### 7. Bundles extend the vocabulary under their own namespace

```yaml
# axda.yaml
clauses:
  - name: acme.verify_kyc
    engine: rego
    entrypoint: policy/kyc.rego:deny
    requires: [has_tool_results]
    params:
      tier: {type: string, enum: [basic, enhanced]}
    class: deterministic
    prefix_decidable: true
```

Custom kinds must be namespaced; the bare namespace is reserved for built-ins so a bundle cannot shadow `tool.allowlist` and change what an existing contract means. A custom kind backed by a capability-holding WASM plugin is forced to `probabilistic` regardless of what it declares ([ADR-004](004-wasm-plugin-abi.md)).

## Architecture Overview

```
  contract.yaml ──► parse ──► resolve clause names against registry
                                  │            │
                          unknown │            │ known
                                  v            v
                          COMPILE ERROR   bind params (typed)
                                               │
                                  ┌────────────┼────────────┐
                                  v            v            v
                          value bindings   coverage      polarity
                          (§4)             check (§5)    must/must_not
                                  │            │            │
                                  └────────────┼────────────┘
                                               v
                                      evaluation plan  ──► axda explain
                                               │
                          ┌────────────────────┼────────────────────┐
                          v                    v                    v
                     runs: rego/cue/       skipped:            probabilistic:
                     metric/builtin        requires ⊄ coverage  judge / llm-extracted
                          │                    │                    │
                       blocking            never "pass"          advisory
```

## Consequences

### Benefits

- Contracts read at the altitude [ADR-001](001-agent-admission-control.md) promised while compiling to fixed predicates, so the blocking guarantee survives the abstraction.
- Rejecting unknown clause names means a typo is caught at compile time instead of becoming a check that silently never fires.
- Invariants have real semantics: explicit bindings, explicit cardinality, compile-time errors for missing operands.
- `requires`-versus-`coverage` makes `skipped ≠ passed` a mechanism rather than an intention, and `--fail-on-skipped` gives teams a path to enforcing coverage.
- `axda explain` makes the lowering reviewable, which is what keeps the abstraction from being magic.
- Namespaced extension lets organisations add domain clauses without forking or shadowing built-ins.

### Trade-offs

- **A closed vocabulary means the tool says no.** Users will want clauses that do not exist and will be told to write Rego or a judge. Mitigation: the registry is data, additions are cheap, and `acme.*` extension is a documented first-class path. The friction is the price of determinism and is charged deliberately.
- **Value bindings are verbose.** Declaring an extraction for every operand is more typing than the one-line `refund.amount <= approved_limit` in the original sketch. There is no way to be both explicit and implicit; explicit wins because the failure mode of guessing is a policy that checks the wrong field and passes.
- **The registry becomes a compatibility surface.** Changing a clause's default severity or `requires` set changes behaviour for every existing contract. Clause kinds are versioned with the contract `apiVersion` and changes are breaking changes.
- **`must_not` inversion is not universal**, so the contract has kinds that work in one position and not the other. Compile-time rejection makes this discoverable, not silent, but it is still a wart.
- **Two lowering targets for one clause** (`content.no_pii` runs a builtin detector and a Rego verdict) means debugging crosses an engine boundary. `axda explain` shows the chain; it is still two things.

### Out of scope

- Natural-language clause authoring, in any form, including "compile English to a clause with a model at author time". The compiled artifact would be unreviewable against its source.
- Clause composition operators (`any_of`, `all_of`, nesting). v1 clauses are a flat list.
- Cross-episode clauses ("this must hold for 95% of episodes"): needs the aggregation layer [ADR-001](001-agent-admission-control.md) put out of scope.
- Automatic contract inference from observed traces.
- Contract inheritance or bundle-to-bundle imports.

## Verification

- Unknown clause name exits `2` with a suggestion, and no evaluation runs.
- Every registry entry has a fixture contract, a violating trace, and a clean trace; `axda test` fails the bundle if any kind lacks both.
- An invariant referencing an undeclared identifier fails at `axda explain`, before any trace is read.
- `cardinality: exactly_one` against a trace with two matching calls reports a violation naming both spans.
- A declared value that is absent from the trace, with no `default`, reports `skipped`: asserted explicitly against a report where the same clause would otherwise have passed vacuously.
- A contract with `grounding.cite_sources` against a content-free trace reports `skipped`, exits `0`; the same run with `--fail-on-skipped` exits `1`.
- `must_not: [budget.max_tokens]` is rejected at compile time as non-invertible.
- A bundle declaring a clause named `tool.allowlist` is rejected for shadowing a reserved namespace.
- The plan hash in the report changes when the contract changes and is stable when it does not.

## References

- [ADR-001](001-agent-admission-control.md): Agent Admission Control (contract as the authoring surface)
- [ADR-002](002-episode-schema.md): Episode schema (`Coverage`, extraction provenance, total ordering)
- [ADR-004](004-wasm-plugin-abi.md): Plugin ABI (capability-holding plugins are forced probabilistic)
- [ADR-005](005-inline-admission-gate.md): Inline gate (consumes `prefix_decidable`)
- [CUE](https://cuelang.org/): constraint evaluation for invariants
- [Rego](https://www.openpolicyagent.org/docs/latest/policy-language/): tool and ordering clauses
