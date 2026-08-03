# ADR-008: Verbatim-Gated Fact Extraction

**Status:** Proposed
**Date:** 2026-08-03
**Authors:** Adrien
**References:** [ADR-001](001-agent-admission-control.md), [ADR-002](002-episode-schema.md), [ADR-003](003-contract-lowering.md)

## Context

[ADR-002 §4](002-episode-schema.md) allows three claim extractors and records which one ran, because a verdict is deterministic only if every input it read was deterministic. Only `structural` is implemented. It finds a claim by looking for concrete tokens (a number, amount, date, or identifier) in an assistant sentence, which means it silently misses everything asserted in prose: *"the customer is eligible for a full refund"* asserts a checkable fact and contains no token the extractor can see.

The obvious fix is an LLM extractor. The obvious objection is that it hands the fabrication problem to a model: an extractor that can invent a claim can also invent the evidence for it, and a grounding finding whose quoted source does not exist is worse than no finding at all. That is the same failure this tool was built to catch, reintroduced inside the tool.

[kai](https://github.com/AdrienFromToulouse/kai) resolves this for document extraction with a **verbatim gate**: the model must quote a snippet that appears character-for-character in the source, the code locates that snippet rather than trusting the model's line numbers, and a row whose snippet cannot be located is discarded rather than repaired. A model that tidies a quote, fixes OCR damage, or converts a unit is rejected on the same footing as one that fabricates outright.

That mechanism transfers directly. What needs deciding is what it buys, because it does not simply make LLM extraction deterministic.

## Decision

### 1. An extracted fact must quote its source, and the code locates the quote

The extractor is given the Episode as a set of **addressable sources**: each turn and each captured tool result, with a stable id. It must return, per fact, the source id and a snippet copied from that source.

The gate then runs in code:

1. Look for the snippet in the named source, exact substring first.
2. Failing that, allow flexible whitespace *between* words, each word spelled exactly as the source spells it. Only whitespace bends, because a snippet spanning a wrapped line cannot be quoted contiguously.
3. If neither locates it, the fact is **rejected**, with a reason. Never repaired.
4. If located, store **the source's bytes for that span**, not the model's retyping, along with the resolved `SpanRef` and character offsets.

Storing the source's text rather than the model's is what makes a citation verbatim by construction instead of by diligence. Computing offsets by locating, rather than accepting offsets the model reports, is the same idea applied to position.

### 2. The gate buys precision, not recall, and the verdict class follows

This is the part that is easy to get wrong. A verbatim gate does **not** make the extractor deterministic. Run it twice and it may surface different facts. What it makes impossible is a *fabricated* fact: anything it surfaces provably appears in the trace.

So the uncertainty is entirely one-sided:

| Outcome | What it rests on | Class |
|---|---|---|
| **A violation is found** | the quoted text provably exists in the trace, and the support check that condemned it runs in code | **deterministic** — blocks |
| **No violation is found** | the extractor found nothing to condemn, which may mean nothing is wrong, or may mean it missed something | **probabilistic** — advisory |

A clause using the LLM extractor therefore emits a **deterministic verdict when it fails and a probabilistic verdict when it passes**. `Verdict.Class` is already per-verdict rather than per-clause, so this needs no new machinery.

This is a refinement of summary invariant 3, not an exception to it. The invariant says a verdict is blocking only if every input it read was deterministic. A verified fact *is* a deterministic input: its existence in the trace is established by code, not asserted by a model. What the model contributes is *which* facts it looked at, and that only affects the outcomes where the model found nothing.

### 3. Rejected rows are a signal about the extractor, not about the agent

A rejected row means the extraction model tried to quote something that is not there. That says nothing about the agent under evaluation, so it must not become a contract violation: the agent would be blamed for the evaluator's misbehaviour.

Rejections are recorded on `Coverage.Degraded` and surfaced in the report, because an extractor rejecting a large fraction of its own output is a reason to distrust the recall side of every grounding verdict in that run. A rejection rate above a threshold marks the affected clauses degraded.

### 4. The extractor is opt-in and never silently substituted

`structural` stays the default. The LLM extractor is selected explicitly (`--extractor llm`, or per-bundle configuration), because it costs money, needs credentials, and changes the class of some verdicts. A run that cannot reach the extractor reports the affected clauses `skipped`, never falling back to `structural` and reporting as though the better extractor had run.

## Architecture Overview

```
  Episode
    |
    +--> addressable sources: turn[i], tool_result[j]  (stable ids, exact bytes)
              |
              v
       extractor model  -->  [{source_id, snippet, claim}]
              |
              v
       VERBATIM GATE (code)
         exact substring, else whitespace-flexible, else REJECT
              |
      +-------+---------------------------+
      |                                   |
   located                            rejected
      |                                   |
   store SOURCE bytes                 Coverage.Degraded
   + SpanRef + offsets                (about the extractor,
      |                                not the agent)
      v
   Claim{extractor: llm, verified: true}
      |
      v
   grounding clauses
      |
      +-- violation found  -> deterministic, blocks
      +-- nothing found    -> probabilistic, advisory
```

## Consequences

### Benefits

- Claims asserted in prose become checkable, which is the whole gap `structural` leaves.
- A fabricated citation cannot reach a report: the quote is located in the trace or the fact is discarded.
- Stored citations are the source's bytes, so a finding quotes what the agent actually said rather than a tidied paraphrase of it.
- The precision/recall asymmetry is encoded in the verdict class rather than left as a caveat in prose, so a blocking failure still means what it always meant.
- Extractor misbehaviour is visible and attributed to the extractor instead of being charged to the agent.

### Trade-offs

- **Recall is unmeasured.** Nothing here tells you how many claims the extractor missed, and the advisory class on a pass is an admission of that rather than a fix. Measuring it needs labelled episodes, which is a separate piece of work.
- **The gate rejects some true evidence.** A model that quotes across a truncated tool result, or normalises a unicode quote character, loses a real fact. That is the intended direction of the trade: a false rejection costs recall, a false acceptance costs the tool its credibility.
- **Cost and latency** scale with episode size, on top of the judge.
- **Two extractors to maintain**, with different verdict-class behaviour, which is a real subtlety for anyone reading a report.
- **The extraction model is a second model in the trust path.** It cannot fabricate evidence, but it chooses what to look at, and a prompt-injected trace could plausibly steer it toward looking away. The gate does not address that.

### Out of scope

- Measuring extractor recall against labelled episodes.
- Extraction plugins (`extractor: plugin` remains reserved for [ADR-004](004-wasm-plugin-abi.md)).
- Cross-episode fact reconciliation.
- Using the extractor for anything but claims: value bindings stay declarative ([ADR-003 §4](003-contract-lowering.md)).

## Verification

- A snippet that appears exactly in the named source is located, and the stored text is the source's bytes.
- A snippet differing only in whitespace, including across a newline, is located.
- A snippet that paraphrases, corrects a typo, or converts a unit is **rejected**, not repaired.
- A snippet that exists in a *different* source than the one named is rejected.
- A rejected row appears in `Coverage.Degraded` and produces no contract violation.
- A grounding clause backed by the LLM extractor emits `class: deterministic` on a failure and `class: probabilistic` on a pass, in the same run.
- With the extractor unreachable, affected clauses report `skipped`, and the report never claims `structural` results as LLM ones.

## References

- [ADR-002](002-episode-schema.md) — claim extractors and the provenance rule this refines
- [ADR-003](003-contract-lowering.md) — grounding clauses that consume claims
- [kai](https://github.com/AdrienFromToulouse/kai) — prior art: the verbatim gate on document extraction
