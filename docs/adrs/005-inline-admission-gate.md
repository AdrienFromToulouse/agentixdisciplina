# ADR-005: Inline Admission Gate

**Status:** Proposed
**Date:** 2026-08-02
**Authors:** Adrien
**References:** [ADR-001](001-agent-admission-control.md), [ADR-002](002-episode-schema.md), [ADR-003](003-contract-lowering.md), [ADR-004](004-wasm-plugin-abi.md)

## Context

[ADR-001](001-agent-admission-control.md) scoped v1 to post-hoc evaluation and named the obvious limitation in its trade-offs: *post-hoc means the damage already happened*. A CI gate stops a bad agent from shipping. It does not stop a shipped agent from issuing a refund above the approved limit at 3am.

The name of the concept is Agent Admission Control, and admission control is inherently inline. This ADR specifies the inline gate: deferred from v1, not abandoned.

The design constraint that makes this hard is that [ADR-001](001-agent-admission-control.md) also promised **the same artifact drives both**. An inline gate with its own policy language would mean two sources of truth that drift, and the drift would be silent until an incident. So the gate must consume the same bundle, the same contract, and the same clause registry: while operating on information that does not yet exist.

Three problems follow:

- **A running episode is a prefix.** Most contracts contain positive obligations (`must: cite_sources`) that cannot be violated until the episode ends. Evaluating them mid-run would either block everything or mean nothing.
- **The latency budget is two orders of magnitude tighter.** A CI run can take ten seconds. A gate in front of a tool call has single-digit milliseconds before it is the reason the agent feels slow.
- **A failing gate is an outage.** This is the classic Kubernetes admission-webhook failure: a validating webhook that fails closed and then becomes unavailable takes down every deployment in the cluster. The failure semantics have to be designed first, not discovered.

## Decision

### 1. The gate is a tool-call proxy, not an SDK

The gate runs as a process between the agent and its tools: an MCP proxy for MCP-based agents, an HTTP forward proxy for direct tool APIs. The agent's tool endpoint URL changes. Nothing else about the agent changes.

This preserves [ADR-001](001-agent-admission-control.md)'s central property under inline enforcement. A gate delivered as a framework callback would put the checker back inside the checked, and every consequence [ADR-001](001-agent-admission-control.md) enumerated would return: skippable by configuration, versioned with the agent, unavailable for agents you did not write.

A proxy keeps the agent ignorant. It also keeps the enforcement point at the only place where blocking is actually meaningful: the network hop where the side effect would occur. The agent never learns whether it was evaluated; it learns that a tool call returned an error.

```
agent ──► axda gate ──► tool / MCP server
             │
             └── denied: return a tool error to the agent
```

Returning a **tool error** rather than a transport failure is deliberate: agents already have a code path for "the tool refused", and it produces a graceful degradation ("I'm not able to issue a refund of that size") instead of a crash. The denial reason is passed back in the error payload, so the agent can explain itself.

### 2. The Episode is a prefix, and only prefix-decidable clauses run

The gate maintains an incrementally-built Episode per session, flagged `sealed: false`. [ADR-003](003-contract-lowering.md) already requires every `ClauseKind` to declare `prefix_decidable`; this is what consumes it.

The distinction that matters is not clause-by-clause taste, it is **which direction is decidable on a prefix**:

| Clause shape | On a prefix | Inline? |
|---|---|---|
| `tool.allowlist` | a disallowed call is a violation the moment it is attempted | **yes** |
| `content.no_pii` on tool args | the PII is in the outbound arguments, now | **yes** |
| `budget.max_cost_usd` | the running total either has or has not exceeded | **yes** |
| `invariants` over tool args | operands are bound if their source calls already happened | **conditionally** |
| `order.requires_precondition` | see below | **yes, one-directionally** |
| `grounding.cite_sources` | cannot be violated until the agent stops talking | no |
| `quality.*` | judges are banned inline (§3) | no |

`order.requires_precondition` is the interesting case. "Verify identity before refunding" cannot be *satisfied* on a prefix: the agent might verify later. But it can be *violated* on a prefix, precisely at the moment `billing.refund` is attempted with no prior `crm.verify_identity`. The gate evaluates the violating direction only. This is the shape of most useful safety rules, and it is why the gate is worth building despite being unable to evaluate positive obligations.

Invariants are conditional: if `approved_limit` comes from a `crm.lookup` that has already returned, the constraint is decidable when `billing.refund` is attempted. If the lookup has not happened, the operand is unbound and the clause is `skipped` inline, and per [ADR-003 §5](003-contract-lowering.md), skipped is not passed, so it is still enforced post-hoc.

**The gate is strictly weaker than the post-hoc run and never replaces it.** `axda explain --mode inline` prints which clauses are enforceable inline and which are deferred, so the coverage gap is visible at authoring time rather than discovered after an incident.

### 3. Hard latency budget: deterministic engines only, no judges

Default budget is **50ms** per tool call, wall-clock, from proxy ingress to decision.

- Rego and CUE evaluate against a prefix Episode in well under that on realistic episode sizes.
- The metric evaluator is arithmetic.
- WASM plugins get a tighter deadline than the post-hoc 2s ([ADR-004 §6](004-wasm-plugin-abi.md)) (default 10ms) and a plugin that cannot make it is excluded from inline enforcement at bundle load, with a warning, rather than being allowed to blow the budget at runtime.
- **LLM judges are prohibited inline**, unconditionally. Not budget-limited: prohibited. A judge is a network round-trip to a model, which is at minimum hundreds of milliseconds, and it is probabilistic, which means it would block a legitimate action nondeterministically. Both properties are disqualifying, and making it a tunable would invite someone to turn it on.

Exceeding the budget is an error condition and resolves through §4, not by waiting.

> **Narrowed (2026-08-03).** [ADR-009 §5](009-rebac-authorization-clauses.md) permits the gate one class of network call: the configured authorization checker, under a 10ms sub-budget. A Zanzibar-model check is neither slow nor probabilistic at check time, so neither disqualifying property of the judge applies.

### 4. Failure semantics are per-clause and default by severity

The admission-webhook footgun is a single global fail-closed switch. `axda` makes it a per-clause property with a severity-derived default:

```yaml
must_not:
  - kind: content.no_pii
    on_error: deny        # default for critical
  - kind: budget.max_cost_usd
    value: 5.00
    on_error: allow       # default for non-critical
```

| Clause severity | Default `on_error` |
|---|---|
| critical | `deny` |
| major, minor | `allow` |
| any probabilistic clause | n/a: not evaluated inline |

The reasoning: for a critical `must_not`, the whole point is that the action is unacceptable, so an unevaluated critical rule must not become an allowed action. For everything else, an agent that stops working because the gate had a bad minute is a worse outcome than a budget overrun.

Three further protections, because per-clause defaults are not enough on their own:

- **Circuit breaker.** If the gate's error rate over a rolling window exceeds a threshold, it trips and applies `on_error` uniformly without evaluating: so a sick gate degrades predictably instead of adding latency to every call on its way to the same answer.
- **Kill switch.** `axda gate --disable` and an equivalent control-plane flag put the proxy into pass-through. Operators need a documented way to get out of the way at 3am, and if we do not provide one they will delete the deployment.
- **Fail-open is loud.** Every allowed-on-error decision emits a span and a counter. Silent fail-open is how a gate becomes decorative without anyone noticing.

### 5. Decisions are cached and idempotent

Identical `(bundle digest, plan hash, clause, tool name, canonicalized arguments, relevant prefix state)` yields a cached decision. Retries (which agents do constantly) do not re-evaluate.

The cache key includes the plan hash from [ADR-003 §6](003-contract-lowering.md), so a contract change invalidates every entry rather than serving decisions from a policy that is no longer in force.

### 6. The gate emits spans, closing the loop with post-hoc evaluation

Every gate decision emits its own span: clause, verdict, latency, cache hit, and whether it was an `on_error` fallback. Those spans land in the same trace the agent is already producing, which means the post-hoc run ([ADR-001](001-agent-admission-control.md)) reads them as ordinary Episode content.

Two things fall out. Post-hoc evaluation can assert on the gate itself: "no critical clause fell open this week" is a contract clause like any other. And an incident review sees the enforcement decisions in the same timeline as the agent behaviour, rather than in a separate system that has to be correlated by hand.

### 7. Contract selection is by session, not by agent identity

The proxy maps an incoming request to a contract via, in order: an explicit `x-axda-contract` header, the session's agent identity from the transport, or a configured default. Sessions are keyed by the transport's session identifier so the prefix Episode accumulates correctly across calls.

An unmapped session with no default configured is a **configuration error that denies**, not a silent pass-through. A gate that quietly stops governing traffic it does not recognise is the same failure as a clause that passes because its data was missing.

## Architecture Overview

```
                    ┌─────────── same bundle, same contract, same registry ───────────┐
                    │                                                                  │
   CI (ADR-001)     │   axda evaluate ── sealed Episode ── ALL clauses ── exit 0/1/2   │
                    │                                                                  │
   Runtime          │   axda gate                                                      │
                    └──────────────────────────┬───────────────────────────────────────┘
                                               │
   agent ──tool call──► ┌─────────────────────────────────────┐ ──allow──► tool
                        │ append to prefix Episode (sealed=0) │
                        │              │                       │
                        │   prefix_decidable clauses only      │
                        │   rego · cue · metric · wasm(10ms)   │
                        │   judges PROHIBITED                  │
                        │              │                       │
                        │   ≤50ms ─────┴─── over budget ──┐    │
                        └───────────────────────────┬─────┘    │
                                    │               │          │
                                  deny         on_error: deny/allow
                                    │           (critical→deny)
                                    v                 │
                        tool error to agent ◄─────────┘
                                    │
                        emits decision span ──► same trace ──► post-hoc evaluation
                                                                (deferred clauses +
                                                                 assertions on the gate)
```

## Consequences

### Benefits

- Violations are prevented rather than reported, for the clause shapes where prevention is possible.
- One bundle, one contract, one registry across CI and runtime: no second policy language, no drift.
- The agent stays ignorant: a URL change, no SDK, no callback, no redeploy when policy changes.
- Denials arrive as tool errors, so agents degrade gracefully instead of crashing.
- `order.requires_precondition` being violation-decidable on a prefix means the most valuable safety rules ("never do X without first doing Y") work inline despite being unsatisfiable on a prefix.
- Per-clause `on_error` avoids the all-or-nothing webhook trap; the circuit breaker and kill switch give operators a way out.
- Gate spans make enforcement auditable by the same tool that evaluates the agent, and make "the gate fell open" itself assertable.

### Trade-offs

- **The gate is a new component in the request path.** It can fail, it adds a hop, it needs deploying and monitoring. [ADR-001](001-agent-admission-control.md)'s CLI has none of these properties, and some users should stay with CI-only enforcement.
- **Only a subset of clauses is enforceable**, and the subset is not obvious from reading a contract. `axda explain --mode inline` makes it visible, but a user who does not run it may believe they have runtime enforcement of `cite_sources`. They do not.
- **Tool-call granularity only.** The gate sees calls, not the final answer. Content clauses apply to outbound tool arguments; they cannot stop the agent from saying something bad to a user.
- **Fail-open on non-critical clauses is a real hole**, chosen deliberately over availability loss. A degraded gate silently permits budget overruns. Counters and spans make it visible after the fact, which is not the same as preventing it.
- **50ms is a budget, not a guarantee.** Large prefix Episodes on long sessions will approach it. Prefix Episodes are bounded and truncated by age, which is itself a soundness compromise: a clause over a very long session may not see the whole prefix.
- **Session state makes the gate stateful**, so horizontal scaling requires session affinity or shared state. The CLI is a pure function; this is not.

### Out of scope

- **Gating model output.** Tokens that have streamed cannot be recalled, and buffering the full response to evaluate it destroys the streaming UX that agent products depend on. A response-path hook is a separate decision with a genuinely different cost/benefit, not an extension of this one.
- LLM judges inline, in any configuration.
- Human-in-the-loop escalation on denial (deny is automatic and final; approval workflows are a different product).
- Automatic remediation or argument rewriting: the gate allows or denies, it does not edit the agent's calls.
- Cross-session or fleet-level rate limiting.
- Multi-tenant gate deployment and per-tenant contract isolation.

## Verification

- An agent calling a tool outside `allowed_tools` receives a tool error naming the clause; the tool is never reached (asserted at the upstream, not just at the proxy).
- `order.requires_precondition` denies `billing.refund` with no prior `crm.verify_identity` in the same session, and allows it after one.
- An invariant whose operand source has not yet been called reports `skipped` inline, and the same clause reports a violation in the post-hoc run over the sealed trace.
- p99 decision latency stays under the budget across a benchmark of realistic prefix sizes; a plugin declared inline but exceeding 10ms is rejected at bundle load.
- A contract containing a `quality.*` clause loads with a warning and the clause is absent from the inline plan.
- Forced evaluator failure: a critical clause denies, a minor clause allows, both emit spans, and the fail-open counter increments.
- Tripping the circuit breaker applies `on_error` uniformly and decisions return in under 1ms.
- `axda gate --disable` passes traffic through with no evaluation and logs that it is disabled on every request.
- A session with no contract mapping and no default is denied, not passed through.
- Gate decision spans appear in the trace and are readable as Episode content by a post-hoc contract asserting on them.

## References

- [ADR-001](001-agent-admission-control.md): Agent Admission Control; the deferred inline gate
- [ADR-002](002-episode-schema.md): Episode schema; `sealed` and incremental construction
- [ADR-003](003-contract-lowering.md): `prefix_decidable`, severity defaults, plan hash
- [ADR-004](004-wasm-plugin-abi.md): plugin deadlines and capability-derived class
- [Kubernetes admission webhook failure policy](https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/#failure-policy): the prior art for §4
