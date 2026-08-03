# ADR-009: ReBAC Authorization Clauses Backed by SpiceDB

**Status:** Proposed
**Date:** 2026-08-03
**Authors:** Adrien
**References:** [ADR-001](001-agent-admission-control.md), [ADR-002](002-episode-schema.md), [ADR-003](003-contract-lowering.md), [ADR-005](005-inline-admission-gate.md), [SpiceDB](https://authzed.com/spicedb), [Zanzibar (Pang et al., 2019)](https://www.usenix.org/conference/atc19/presentation/pang)

## Context

### The question Rego cannot be given

The v1 action vocabulary answers "was this action allowed?" with pattern lists and ordering: `tool.allowlist` is a set of tool-name globs, `order.requires_precondition` is an interval comparison. Both are predicates over data the trace already contains.

Real organisations have a second kind of allowed, and it is the kind that governs documents, accounts, and customer records: **may this principal invoke this tool on this resource?** The answer is not in the trace and not in the contract. It is reachability in a relationship graph: alice may share the report because bob, who owns it, granted her editor, and editors may share. The graph lives in the application's authorization system, it changes minute to minute, and revocation is the moment it matters most.

Rego cannot be given this question, and not because of the language. The Rego engine deliberately evaluates with no store, no `data` document, and no network ([ADR-001 §5](001-agent-admission-control.md)); everything a policy may read arrives in `input`. A relationship graph cannot ride in `input`: snapshotting it into the Episode fails on size, and any copy is stale the moment a grant is revoked. Replicating the graph into bundle `data` fails the same way and additionally couples graph changes to policy releases. This is the known boundary of policy-as-code, and it is the problem Google's Zanzibar was built to solve: a dedicated service answering `CheckPermission(subject, permission, resource)` over a stored graph, with an explicit consistency model. SpiceDB is the reference open-source implementation.

So [ADR-001 §4](001-agent-admission-control.md)'s table gains a fifth row:

| Engine | Filters | Question |
|---|---|---|
| CUE | what the agent **believed** | Is this fact consistent? |
| Rego | what the agent **did** | Was this action allowed? |
| LLM judge | what the agent **said** | Was this useful? |
| metric | what the agent **cost** | Was this within budget? |
| **checker** | what the agent was **entitled** to do | Was this within the principal's rights? |

### The tension this ADR resolves

A live check against the graph is the only *correct* way to gate an action: the graph at execution time **is** the policy. But a live network call inside the evaluator would destroy the properties everything else stands on: determinism as a property of the whole input closure ([summary](summary.md) invariant 3), byte-identical replay of last Tuesday's trace, and a single static binary that evaluates offline.

The resolution is the same move [ADR-005 §6](005-inline-admission-gate.md) already made for gate verdicts generally: **check live at the gate, record the decision as a span, evaluate the record post-hoc.** The gate owns the network; the evaluator owns the record; the trace is the interface between them, as it is everywhere else in this design.

## Decision

### 1. The seam is a Zanzibar-model checker interface; SpiceDB is the reference implementation

The gate depends on a small interface, not a vendor:

```go
type Checker interface {
    Check(ctx, subject, permission, resource string, c Consistency) (Decision, ConsistencyToken, error)
}
```

Everything that outlives a run is checker-agnostic: contract YAML names no vendor, decision-span attributes carry `axda.authz.checker` as a recorded label (`"spicedb"`), and `consistency_token` is an opaque string (a ZedToken for SpiceDB, its equivalent elsewhere).

SpiceDB is the reference implementation because it has the strongest consistency-token story (per-request `Consistency`: `minimize_latency`, `at_least_as_fresh`, `at_exact_snapshot`), a mature Zanzibar-faithful model, and a pure-Go gRPC client (`authzed-go`) that preserves [ADR-001 §5](001-agent-admission-control.md)'s no-CGO single binary. Stated honestly: **OpenFGA is a credible drop-in**, and the interface, not the vendor, is the decision.

**Rejected: Cedar / Amazon Verified Permissions.** Cedar is policy-as-code, like OPA: the relationship graph would have to be marshalled into the request context, which is precisely the inexpressibility this ADR exists to escape. **Rejected: Ory Keto**, Zanzibar-model but with a weaker consistency-token and ecosystem story. **Rejected: SpiceDB-shaped Episode fields**, which would turn a vendor swap into a schema migration.

### 2. Record-then-evaluate, never evaluate-live

This is the decision every other one in this ADR depends on. The gate performs the live check and emits a decision span (§6). The post-hoc clause (§7) reads only the recorded decisions. `axda evaluate` therefore needs no checker connectivity at all: it remains a pure function of trace plus bundle, replay stays byte-identical, and an auditor who wants to know *why* a decision went the way it did can query SpiceDB at the recorded token out-of-band.

**Rejected: evaluator re-checks pinned to the recorded token** (`at_exact_snapshot`). Superficially deterministic, but the input closure now contains a network service's availability *and its garbage-collection window*: SpiceDB reclaims old revisions, so an expired snapshot errors, and the same trace evaluates differently on Tuesday and Friday. **Rejected: shipping the graph as Rego `data`** for the Context reasons: stale on arrival, unbounded, and graph ownership migrates into policy releases.

### 3. Principal identity lands in `EpisodeMeta`, mapped from `enduser.id`

The Episode gains the minimal identity surface, additive within `episode/v1`:

```
EpisodeMeta
  principal   *Principal    nil when the trace carries no identity

Principal
  type  string    checker subject type, e.g. "user"
  id    string    stable identifier, e.g. "alice@acme.com"

Coverage
  has_principal        bool
  has_authz_decisions  bool
```

The OTLP mapping extends [ADR-002 §3](002-episode-schema.md):

| Episode field | Source |
|---|---|
| `meta.principal.id` | `enduser.id` on the root `invoke_agent` span; fallback `axda.principal.id` |
| `meta.principal.type` | `axda.principal.type`, default `"user"` |
| `authz_decisions[]` | gate-emitted `axda.authz.check {permission}` spans (§6) |

`enduser.id` is the existing stable semconv for exactly this, and reusing it preserves invariant 1: an agent already instrumented for end-user attribution needs zero changes, and the only coupling remains telemetry the agent emits anyway. The `axda.*` namespace appears only where no semconv exists (the subject *type*) and on the gate's own spans, which axda authors.

The gate resolves the principal per session from a configured source, a trusted `x-axda-principal` header or a claim extracted from the transport's auth token, mirroring [ADR-005 §7](005-inline-admission-gate.md)'s contract selection. Whichever source is configured, the deployment must ensure the agent cannot assert its own identity: a gate that trusts a header the agent can set is checking the agent's homework against the agent's answer key. A session that reaches an authz-mapped tool with **no resolvable principal is an error resolving through `on_error`** ([ADR-005 §4](005-inline-admission-gate.md), critical → deny), not a skip and not a new knob.

**Rejected: a parallel `axda.user.id` attribute** (inventing a convention where a standard one exists). **Rejected: per-tool-call principal** for delegation and impersonation chains; deferred, because each decision record (§6) carries its own subject, so the post-hoc clause never needs it, and only `authz.recheck` (§7) reads `meta.principal`.

### 4. The tool→(permission, resource) binding is declared in the contract

```yaml
spec:
  authz:
    subject:
      type: user                    # checker subject type for meta.principal
    tools:
      - tool: docs.share            # tool-name pattern, same wildcards as tool.allowlist
        permission: share
        resource:
          type: document
          id: $.document_id         # JSONPath into canonicalized tool-call arguments
      - tool: billing.refund
        permission: approve_refund
        resource:
          type: account
          id: $.account_id

  must:
    - kind: authz.tool_permitted    # every mapped call has a recorded permitted decision
```

This is deliberately the same explicit-binding discipline as [ADR-003 §4](003-contract-lowering.md) values: `from`-style declarations, JSONPath extraction, and nothing guessed. Tools absent from the mapping are simply outside this clause's scope; they remain governed by `tool.allowlist`. High-assurance contracts may set `unmapped: deny` on the block to refuse any tool call that has no mapping.

**Rejected: CEL for resource extraction.** More expressive (composed resource ids), but a fourth language in a tool that already charges for three ([ADR-001](001-agent-admission-control.md) trade-offs), and [ADR-003](003-contract-lowering.md) already standardized JSONPath for exactly this job. **Rejected: convention-based extraction** (guess the `*_id` argument): the policy-that-checks-the-wrong-field-and-passes failure that explicit bindings exist to prevent.

### 5. Live checks run at the gate under a 10ms network sub-budget

[ADR-005 §3](005-inline-admission-gate.md) prohibits judges inline for two reasons: hundreds of milliseconds of latency, and probabilistic blocking. A Zanzibar check has neither property. It is engineered for single-digit-millisecond answers (the Zanzibar paper's core operational claim), and its answer is authoritative rather than probabilistic: the graph at check time *is* the policy. So the carve-out is narrow and named: **the gate may make network calls to the configured authorization checker only**, under a **10ms sub-budget** inside the 50ms envelope, the same deadline discipline as inline WASM. A check that misses the deadline is an error resolving through `on_error` (§8); nothing waits.

Default consistency is `minimize_latency` (SpiceDB's cached path). The mode and the returned token are recorded on every decision (§6), so bounded staleness is a visible, auditable property rather than a silent one. A deployment that cannot tolerate the staleness window can configure `fully_consistent` and accept the latency risk; the recorded token makes either choice reviewable after the fact.

Authz decisions are **excluded from [ADR-005 §5](005-inline-admission-gate.md)'s idempotency cache** except for exact-retry dedup with a short TTL (default 10s): that cache assumes decisions are pure functions of prefix state, and a permission check is not.

`axda evaluate` never dials the checker (§2), so [ADR-001 §5](001-agent-admission-control.md)'s no-network-in-evaluators posture survives intact; the binary merely gains a pure-Go gRPC client that only `axda gate` exercises.

**Rejected: post-hoc only.** Detection is not admission; [ADR-005](005-inline-admission-gate.md) exists because the damage already happened. **Rejected: quarantining the checker inline like the judge.** That would make the entire capability advisory, which is to say useless as admission control, for a check that is deterministic at the moment it runs.

### 6. The decision record: one span per check, one Episode list

The gate emits one span per check, `axda.authz.check {permission}`:

```
axda.authz.subject             "user:alice@acme.com"
axda.authz.permission          "share"
axda.authz.resource            "document:report-q3"
axda.authz.decision            "permitted" | "denied" | "errored"
axda.authz.fell_open           bool      the on_error: allow path was taken
axda.authz.checker             "spicedb"
axda.authz.consistency         "minimize_latency"
axda.authz.consistency_token   opaque string
axda.authz.tool                "docs.share"
axda.authz.args_hash           sha256 of canonicalized arguments
```

The adapter maps these into a new ordered Episode list, following [ADR-002](002-episode-schema.md)'s conventions (total order, span back-reference):

```
AuthzDecision
  index              int
  subject            string
  permission         string
  resource           string
  decision           enum{permitted, denied, errored}
  fell_open          bool
  checker            string
  consistency_token  string
  tool_call_index    int      -1 when uncorrelated
  span               SpanRef
```

Correlation between a decision and its tool call is primarily by trace context: the gate parents its decision span into the trace the agent propagates through the proxied call. The fallback is a match on `(tool name, args_hash, temporal adjacency)`. An unmatched decision keeps `tool_call_index: -1` and adds a `coverage.degraded` note; it is never dropped.

**Rejected: decision fields inline on `ToolCall`.** A denied call never reaches the tool, so there can be a decision with *no* tool-call span; a separate list represents both halves honestly.

### 7. Two clause kinds: verify the record, and re-check today's graph

| Kind | Engine | Class | Prefix-decidable | Requires | Default severity |
|---|---|---|---|---|---|
| `authz.tool_permitted` | `rego:axda.authz.tool_permitted_violation` | deterministic | yes | `has_authz_decisions` | critical |
| `authz.recheck` | `checker` | probabilistic | no (never inline) | `has_principal`, `has_tool_args` | minor |

**`authz.tool_permitted`** is the post-hoc workhorse. It reports findings on three shapes, each anchored to the executed call's span (invariant 4):

1. **Denied but executed.** A `denied` decision correlated with a tool call that ran anyway: the gate was bypassed or misrouted. The most severe finding this clause produces.
2. **Fell open and executed.** A `decision: errored, fell_open: true` record whose call executed. An errored check must never read as permitted (invariant 2), so fell-open detection is folded into this clause (`allow_fell_open: false` by default) rather than becoming a separate kind; the *generic* "no clause of any kind fell open this week" assertion is [ADR-005 §6](005-inline-admission-gate.md)'s territory and stays there.
3. **Unchecked.** A mapped tool call with no correlated decision, in a trace that has decisions: evidence the gate did not govern that call.

A trace with **zero** decisions fails the `has_authz_decisions` requirement and the whole clause is `skipped`, never passed ([ADR-003 §5](003-contract-lowering.md)): an agent that ran without a gate is honestly unverified, and `--fail-on-skipped` upgrades that for deployments that require gating.

This is the first clause whose inline and post-hoc lowerings are **different programs asserting the same predicate**: inline it lowers to *perform-and-record* (the live check, §5); post-hoc it lowers to *verify-the-record* (Rego over `authz_decisions`). Both compile from the same `spec.authz` block, and `axda explain` prints both lowerings; that shared source and visible descent is what contains the two-sources-of-truth drift [ADR-005](005-inline-admission-gate.md) warned about.

**`authz.recheck`** answers a different question: would this call be permitted under *today's* graph? It exists for audits ("re-run last quarter's traces against the tightened schema") and for traces from agents that ran without a gate. It dials the checker from the evaluator against a moving target, so it is classed probabilistic, advisory always, and, stricter than the judge, **`blocking: true` is rejected at compile time**. The asymmetry is deliberate: a judge verdict is about the immutable trace and can prove stable enough to promote ([ADR-001 §6](001-agent-admission-control.md)); a recheck verdict is about the current organisational graph, so promoting it would make CI redness depend on org-chart changes: nondeterminism by construction, not by sampling. Inline it is prohibited like the judge, and vacuously so: live checking *is* the gate's job there.

`checker` becomes the seventh `engine` enum value in the [ADR-003 §1](003-contract-lowering.md) registry schema. That is the registry-surface change ADR-003 classes as breaking; it is accepted explicitly here, while the stack is Proposed and the vocabulary is pre-1.0.

**Rejected naming: `tool.authz`** under the existing namespace. The `authz.*` namespace groups the kinds with their shared `spec.authz` binding block and leaves room for successors (`authz.delegation`, caveat-carrying checks) without crowding `tool.*`.

### 8. Failure semantics: ADR-005 §4 verbatim, and fail-open is durable

Checker unreachable, over deadline, or erroring resolves through [ADR-005 §4](005-inline-admission-gate.md) unchanged: per-clause `on_error` with severity-derived defaults (critical → deny), the circuit breaker counts checker errors, the kill switch applies, and fail-open is loud. A resource-extraction failure (the JSONPath misses in the captured arguments) takes the same error path: a check that cannot name its resource has not been performed.

What this ADR adds is that **fail-open is durable**: the errored decision span with `fell_open: true` is Episode content, so the post-hoc run convicts the executed call (§7, shape 2). The gate's worst minute is not a silent hole; it is a finding waiting in the trace.

**Rejected: a fail-static mode** (serve the last-known decision for the same check on error). It converts an outage into silently stale authorization, which is exactly the decorative-gate failure [ADR-005 §4](005-inline-admission-gate.md) exists to prevent.

## Architecture Overview

```
   agent ──tool call──► ┌──────────── axda gate ─────────────┐ ──allow──► tool
                        │ resolve principal (session, §3)     │
                        │ map tool → permission, resource (§4)│
                        │              │                      │
                        │   Check(subject, permission,        │     ┌───────────┐
                        │         resource) ──── ≤10ms ───────┼────►│  SpiceDB  │
                        │              │           (§5)       │◄────│  (graph)  │
                        │   permitted / denied / on_error     │     └───────────┘
                        └──────────────┬──────────────────────┘
                                       │
                          decision span: axda.authz.check
                          {subject, resource, decision,
                           fell_open, consistency_token}  (§6)
                                       │
                                       v
                          same trace ──► adapter ──► Episode.authz_decisions[]
                                                            │
   CI / post-hoc                                            v
   (checker unreachable):   axda evaluate ─► authz.tool_permitted   deterministic,
                                     │       reads the record only  blocking
                                     └· · ·► authz.recheck          probabilistic,
                                             dials today's graph    advisory forever
```

## Consequences

### Benefits

- Graph-shaped authorization ("granted by someone with grant rights") becomes assertable, the question neither Rego nor CUE could be given, without weakening either.
- One `spec.authz` block drives both admission (live, at the gate) and audit (deterministic, post-hoc), preserving [ADR-005](005-inline-admission-gate.md)'s one-artifact promise.
- The evaluator stays a pure function of trace plus bundle: post-hoc runs need no checker connectivity, and replay stays byte-identical.
- A gate that fell open is convicted by the trace it produced, so the failure mode of the new network dependency is visible in the same report as everything else.
- The checker interface keeps the vocabulary vendor-neutral; SpiceDB is a reference implementation, not a lock-in.
- The recorded consistency token turns staleness from an invisible risk into an auditable property of every decision.

### Trade-offs

- **The gate gains a stateful network dependency in its critical path.** SpiceDB is now infrastructure that must be deployed, monitored, and kept fast. Mitigation: the 10ms sub-budget bounds the damage, `on_error` semantics are already designed, and deployments that do not declare `spec.authz` pay nothing.
- **`minimize_latency` means a just-revoked permission can briefly still pass.** Chosen over `fully_consistent` because a deadline-missing check resolves through `on_error` and a critical mapping would deny, trading a staleness window for availability. The token is recorded, so the window is auditable; deployments can opt into full consistency per gate.
- **Correlation by `(tool, args_hash, adjacency)` is a heuristic** when trace-context propagation fails. Mitigation: uncorrelated decisions degrade coverage loudly rather than disappearing, and the primary path is exact.
- **The checker-agnostic seam is aspirational until a second implementation exists.** The interface is shaped by SpiceDB's API; OpenFGA support is the test that it was actually neutral.
- **`authz.recheck` verdicts go stale by design**: they describe the graph at evaluation time, not at execution time. That is the point of the clause, but a reader who confuses it with `authz.tool_permitted` will draw wrong conclusions; `axda explain` labels the difference.

### Out of scope

- **Writing the graph.** `WriteRelationships` stays in the application; axda is a `CheckPermission` consumer only. If the graph is wrong, axda detects the consequences; it does not prevent or repair them.
- `LookupResources` / `LookupSubjects` fan-out ("assert alice can see nothing outside her team"): a different query shape with a different cost model.
- SpiceDB schema authoring, validation, or migration: that is `zed`'s job.
- A general-purpose authorization sidecar or PDP for non-agent traffic.
- SpiceDB caveats (context-conditional relationships): a plausible v2 in which tool arguments feed caveat context, not v1.
- Delegation and impersonation chains ("may agent A act as this principal at all"): the natural follow-up kind, needing per-call principals (§3).
- Cross-episode principal identity, already excluded by [ADR-002](002-episode-schema.md).

## Verification

- A mapped tool call by a permitted principal is forwarded, and its decision span carries a non-empty consistency token; the post-hoc clause passes reading spans only, asserted with SpiceDB stopped.
- A denied principal receives a tool error naming the clause; the tool is never reached (asserted at the upstream); the post-hoc run reports no denied-but-executed finding.
- SpiceDB killed mid-session: a critical mapping denies, a non-critical mapping allows with `fell_open: true`, and the post-hoc run over that trace produces the fell-open finding anchored to the executed call's span.
- A gateless trace evaluates `authz.tool_permitted` as `skipped`, exit `0`; the same run with `--fail-on-skipped` exits `1`.
- `authz.recheck` with `blocking: true` is a compile error, before any trace is read.
- Ten post-hoc runs over a trace containing decision spans produce byte-identical reports.
- `axda explain` prints both lowerings of `authz.tool_permitted` (inline: perform-and-record; post-hoc: verify-the-record) from one `spec.authz` block.
- A `spec.authz` entry whose JSONPath matches nothing in a fixture call's arguments produces an `on_error` decision at the gate, not a vacuous allow.

## References

- [SpiceDB](https://authzed.com/spicedb): Zanzibar-model ReBAC database, reference checker implementation
- [Zanzibar: Google's Consistent, Global Authorization System (Pang et al., USENIX ATC 2019)](https://www.usenix.org/conference/atc19/presentation/pang): the model and its latency/consistency claims
- [authzed-go](https://github.com/authzed/authzed-go): pure-Go gRPC client, preserves the static-binary property
- [SpiceDB consistency documentation](https://authzed.com/docs/spicedb/concepts/consistency): `minimize_latency`, `at_exact_snapshot`, ZedTokens
- [OpenFGA](https://openfga.dev/): the credible alternative behind the same interface
- [OTel `enduser.id` semantic convention](https://opentelemetry.io/docs/specs/semconv/registry/attributes/enduser/): principal identity mapping
- [ADR-005](005-inline-admission-gate.md): the gate this ADR's live checks run in; §3 narrowed by §5 here
