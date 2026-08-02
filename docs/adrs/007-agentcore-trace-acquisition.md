# ADR-007: Trace Acquisition from AWS Bedrock AgentCore Runtime

**Status:** Proposed
**Date:** 2026-08-02
**Authors:** Adrien
**References:** [ADR-001](001-agent-admission-control.md), [ADR-002](002-episode-schema.md), [ADR-003](003-contract-lowering.md), [AgentCore Observability](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/observability-configure.html), [CloudWatch Transaction Search](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch-Transaction-Search.html)

## Context

[ADR-001](001-agent-admission-control.md) declared trace collection out of scope — `axda` consumes traces, it is not a collector. That is the right boundary, but it leaves a gap that every user hits on day one: *I have an agent on AgentCore Runtime. Where do I get `trace.json`?*

This ADR answers that for the first reference runtime. It specifies configuration and one adapter, not a new subsystem.

### The starting point is already close to ideal

The reference deployment is a containerised AG-UI server whose image ends with:

```dockerfile
CMD ["opentelemetry-instrument", "python", "-m", "src.agui_server"]
```

`opentelemetry-instrument` is OpenTelemetry's zero-code auto-instrumentation launcher. With `aws-opentelemetry-distro` installed, it reads OTel configuration from environment variables and instruments the framework, Bedrock calls, tool invocations, and outbound HTTP without a line of application code.

This is exactly the coupling [ADR-001](001-agent-admission-control.md) asked for. The agent emits a trace because it was already instrumented for observability; `axda` is a consumer of that existing signal.

### The on-ramp has to be zero-infrastructure

[ADR-001 §7](001-agent-admission-control.md) chose git over OCI for policy bundles on the grounds that nothing should stand between someone and their first run. The same reasoning governs trace acquisition, and it is easy to get wrong: the architecturally cleanest answer is "run an OpenTelemetry Collector and fan telemetry out to a sink you control", which is also a deployment project. Requiring a new production component before the first evaluation would put the tool out of reach of exactly the person most likely to try it — one engineer with an agent already running.

So the question is whether telemetry AgentCore *already* emits, to a destination AgentCore *already* writes to, is good enough to gate on.

### It is, because `aws/spans` is not the X-Ray segment format

With CloudWatch Transaction Search enabled, AgentCore delivers spans to the shared **`aws/spans`** log group. Two properties of that path decide this ADR:

- Spans there are **stored in semantic-convention format with W3C trace IDs**, and all span attributes are searchable. These are OTel spans in a log group, not X-Ray segment documents.
- Transaction Search **ingests 100% of spans as structured logs**. The configurable indexing percentage governs X-Ray trace summaries for search and analytics; it does not sample what lands in the log group.

Semantic-convention fidelity plus complete ingestion is enough for a deterministic, gate-worthy Episode. The X-Ray *API* (`BatchGetTraces`) is a different and genuinely degraded path — OTel attributes land in segment `metadata`, and segments are capped at 64 kB — but it is not the path we need.

## Decision

### 1. The Dockerfile does not change

```dockerfile
CMD ["opentelemetry-instrument", "python", "-m", "src.agui_server"]
```

Byte-identical, on both acquisition paths. `aws-opentelemetry-distro` stays in `requirements.txt` — already required for AgentCore observability.

This is stated as a decision rather than an incidental outcome because it is the falsifiable prediction [ADR-001](001-agent-admission-control.md) makes. If adding evaluation to a real managed runtime required touching the agent image, the out-of-band claim would be marketing.

### 2. Two first-class paths; CloudWatch is the default

| | **Path A — CloudWatch Transaction Search** | **Path B — gateway collector** |
|---|---|---|
| New infrastructure | none | an OTel Collector to deploy and operate |
| Env-var changes | content capture only | endpoint, headers, `DISABLE_ADOT_OBSERVABILITY` |
| Adapter | `cloudwatch-spans/v1` | `otlp/v1.41` |
| Fidelity | semantic-convention spans, 100% ingestion | reference |
| Gate-worthy | **yes** | yes |
| Content redaction | no — content lands in CloudWatch | yes, per-branch at the collector |
| Non-AWS sinks | no | yes |
| Trace grouping | by query predicate | `groupbytrace` processor |
| Marginal cost | Logs ingestion + query scan | collector compute + sink |

**Path A is the default.** Start there. Move to Path B when a named driver appears — a compliance requirement that prompts must not enter CloudWatch, a second destination such as Langfuse or Elastic, or a volume where per-query log scanning costs more than running a collector.

Making the upgrade conditional on a named driver matters: a topology adopted because it is architecturally tidier, rather than because something required it, is a component someone has to operate forever for no stated reason.

Both paths produce the same Episode and the same report. [ADR-002 §3](002-episode-schema.md)'s versioned-adapter design is what makes them interchangeable — the adapter absorbs the format difference and nothing downstream of it knows which ran.

### 3. Path A configuration

Account-level, once: **enable CloudWatch Transaction Search**. AgentCore requires it to deliver spans at all.

On the agent runtime, the *only* variable added beyond AgentCore's own observability defaults:

```
OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true
```

Nothing else. Notably **not** `OTEL_EXPORTER_OTLP_ENDPOINT` and **not** `DISABLE_ADOT_OBSERVABILITY` — the default AgentCore telemetry path is left exactly as it is, so there is no way to misconfigure it and silently break the ops team's dashboards. That was the riskiest instruction in the collector topology, and Path A removes it rather than mitigating it.

Reading a trace back:

```bash
# by session — the reliable CI path, since you set the session id on the request
axda trace fetch --from cloudwatch \
  --session "$SESSION_ID" --region eu-west-1 --wait 30s --out trace.json

# by trace id — incident review
axda trace fetch --from cloudwatch --trace-id 68f0a4c2-... --out trace.json
```

`fetch` queries `aws/spans` filtered to the trace or session, polls until the span set is stable for a settle interval (default 3s) or `--wait` expires, and writes OTLP JSON. Time-window bounding is mandatory in the query — CloudWatch Logs bills by data scanned, and `aws/spans` is a shared account-wide log group.

Trace grouping comes free here: the query *is* the grouping predicate, so Path A does not need the `groupbytrace` processor Path B requires, and cannot produce a partial-batch Episode.

### 4. The `cloudwatch-spans/v1` adapter, and an honest fidelity ladder

A real, versioned, tested adapter per [ADR-002 §3](002-episode-schema.md) — not a reconstruction heuristic. It reads semantic-convention span records and maps them exactly as the OTLP adapter does; the difference is transport and envelope, not vocabulary.

Three tiers, and only the third is second-class:

| Adapter | Source | Fidelity | Gating |
|---|---|---|---|
| `otlp/v1.41` | collector (Path B) | reference | yes |
| `cloudwatch-spans/v1` | `aws/spans` (Path A) | semantic-convention spans, 100% ingestion | **yes** |
| `xray/v1` | `BatchGetTraces` | attributes in segment `metadata`, 64 kB cap | **no** — forensics |

`cloudwatch-spans/v1` sets `coverage` from what the records actually contain, exactly like the OTLP adapter. It does not set `coverage.degraded` merely for being the CloudWatch path, because it is not degraded — claiming otherwise would train users to ignore the flag that [ADR-003 §5](003-contract-lowering.md) depends on.

Where it does set `degraded`: a span whose attributes were truncated against the CloudWatch Logs event size limit, or a trace whose span set never stabilised within `--wait`.

`xray/v1` remains for traces predating Transaction Search or in accounts where it is not enabled. Any Episode from it is marked `adapter: xray/v1`, carries a forensics banner in the report, and `axda evaluate` refuses `--gate` on it. Degraded input must not be able to produce a green build.

### 5. Content capture, and what Path B actually buys

Without `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true`, message content and tool arguments are absent — they are opt-in because they carry user data and PII ([ADR-002 §1](002-episode-schema.md)). Against such a trace, `tool.allowlist`, ordering clauses, and budget clauses work; `content.no_pii`, `grounding.cite_sources`, and invariants over tool arguments report `skipped` ([ADR-003 §5](003-contract-lowering.md)). Useful, but half the contract.

Enabling it on Path A means prompts and tool arguments land in `aws/spans`. That is a real expansion of sensitive-data surface, and it is the honest trade for zero infrastructure. Controls that apply:

- set a short retention on the span log group
- restrict read access — note that `aws/spans` is **shared account-wide**, so a CI role reading it can read every service's spans, which is a broader grant than a per-agent log group and should be scoped with a resource policy
- treat evidence output as sensitive; `--evidence=masked` is already the default ([ADR-002 §7](002-episode-schema.md))

**This is what Path B is for.** Its advantage was never fidelity — it is that a collector can redact content on the CloudWatch branch while preserving it on a short-lived evaluation sink, so the observability store never sees a customer prompt. Teams for whom that is a compliance requirement should go straight to Path B. Teams for whom it is not should not pay for a collector to get it.

### 6. Path B configuration, unchanged in substance

```
OTEL_EXPORTER_OTLP_ENDPOINT=https://otel-gw.internal:4318
OTEL_EXPORTER_OTLP_HEADERS=authorization=Bearer ${GW_TOKEN}
DISABLE_ADOT_OBSERVABILITY=true
OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true
```

`DISABLE_ADOT_OBSERVABILITY=true` is set deliberately. The documented behaviour of a custom endpoint *without* it is ambiguous — whether telemetry duplicates to both destinations or the AWS path wins is unclear — and a compliance-motivated topology should not rest on ambiguity. Everything goes to the collector; the collector is solely responsible for fan-out, including back to CloudWatch.

```yaml
processors:
  groupbytrace: { wait_duration: 10s }
  redact/cloudwatch:
    allow_all_keys: true
    blocked_key_patterns:
      - "gen_ai\\.(input|output)\\.messages"
      - "gen_ai\\.tool\\.call\\.(arguments|result)"

exporters:
  awsxray: { region: eu-west-1 }
  awss3/eval:
    s3uploader: { s3_bucket: acme-axda-traces }
    marshaler: otlp_json

service:
  pipelines:
    traces/observability: { receivers: [otlp], processors: [groupbytrace, redact/cloudwatch, batch], exporters: [awsxray] }
    traces/evaluation:    { receivers: [otlp], processors: [groupbytrace, batch],                    exporters: [awss3/eval] }
```

The collector runs as a **gateway**, not a sidecar: a sidecar needs a supervisor in the image and therefore a changed `CMD`, giving up §1.

### 7. An aggressive flush, on both paths

AgentCore freezes or reclaims the process between invocations. The default `BatchSpanProcessor` delay is 5s — long enough to strand the tail of a short invocation, and the stranded spans are typically the final assistant turn, which is precisely what the content clauses evaluate.

```
OTEL_BSP_SCHEDULE_DELAY=200
OTEL_BSP_MAX_QUEUE_SIZE=4096
```

A truncated trace is not detectable as truncated after the fact. It would present as a passing evaluation over a partial episode — the exact false green [ADR-003 §5](003-contract-lowering.md) exists to eliminate. That undetectability is why this is worth paying for up front rather than tuning when something looks wrong.

### 8. One Episode per invocation; the session id is metadata

An AgentCore Runtime session (`X-Amzn-Bedrock-AgentCore-Runtime-Session-Id`) can span many invocations. [ADR-002 §6](002-episode-schema.md) defines an Episode as one root `invoke_agent` span and puts cross-episode identity out of scope.

**Episode = one invocation.** The session id is carried into `EpisodeMeta` so multi-turn correlation stays possible when the aggregation layer arrives, and so `axda trace fetch --session` can find the trace in the first place. No v1 clause spans invocations.

### 9. Consumption modes

**CI gate** — a pre-production endpoint driven with fixture inputs:

```bash
SESSION_ID=$(uuidgen)
curl -H "X-Amzn-Bedrock-AgentCore-Runtime-Session-Id: $SESSION_ID" ... 
axda trace fetch --from cloudwatch --session "$SESSION_ID" --wait 60s --out trace.json
axda evaluate --trace trace.json --policy oci://ghcr.io/acme/support-agent-evals:v2.1.0 --frozen
```

Exit code gates the pipeline per [ADR-001 §6](001-agent-admission-control.md).

**Production sampling** — a scheduled job fetches recent traces by service and evaluates a sample; violations alert. Sampled, because the LLM judge costs money per run and evaluating every episode is unnecessary.

## Architecture Overview

```
  AgentCore Runtime  (image UNCHANGED on both paths)
  ┌────────────────────────────────────────────────┐
  │ CMD ["opentelemetry-instrument",               │
  │      "python", "-m", "src.agui_server"]        │
  │   aws-opentelemetry-distro                     │
  │   GENAI_CAPTURE_MESSAGE_CONTENT=true           │
  │   OTEL_BSP_SCHEDULE_DELAY=200                  │
  └───────┬────────────────────────────────┬───────┘
          │                                │
   PATH A │ (default, no new infra)        │ PATH B (redaction / non-AWS sinks)
   AgentCore's own telemetry path          │ OTLP → gateway, DISABLE_ADOT=true
          │                                v
          v                     ┌────────────────────┐
   ┌──────────────┐             │  OTel Collector    │
   │  aws/spans   │             │  groupbytrace      │
   │  log group   │             └───┬────────────┬───┘
   │  · semconv   │          redact │            │ content kept
   │    format    │                 v            v
   │  · W3C ids   │          CloudWatch     S3 eval sink
   │  · 100%      │          (no content)   (otlp_json)
   └──────┬───────┘                              │
          │ axda trace fetch --from cloudwatch   │
          v                                      v
   cloudwatch-spans/v1                     otlp/v1.41
          └──────────────┬───────────────────────┘
                         v
              Episode ──► axda evaluate ──► exit 0/1/2
                         ▲
        xray/v1 ─────────┘  forensics only · banner · --gate refused
        (BatchGetTraces, attrs in metadata, 64 kB cap)
```

## Consequences

### Benefits

- **Usable today with no new infrastructure.** Enable Transaction Search, set one environment variable, run `axda trace fetch`. That is the whole setup, and it is a setup one engineer can do in an afternoon.
- The default path touches neither `OTEL_EXPORTER_OTLP_ENDPOINT` nor `DISABLE_ADOT_OBSERVABILITY`, so it cannot break existing observability — the largest operational risk in the collector topology is absent rather than mitigated.
- `aws/spans` carries semantic-convention spans at 100% ingestion, so Path A is gate-worthy rather than advisory. Users are not asked to trust a lossy reconstruction.
- Query-based grouping means Path A cannot produce a partial-batch Episode.
- The fidelity ladder is explicit and only its bottom rung is restricted, so `coverage.degraded` keeps meaning something.
- The agent image is untouched on both paths — [ADR-001](001-agent-admission-control.md)'s central claim, demonstrated on a real managed runtime.
- Path B remains available with its benefit stated precisely (redaction and fan-out, not fidelity), so upgrading is a decision with a reason.

### Trade-offs

- **Path A puts prompts in CloudWatch.** Content capture is required for half the contract, and on Path A that content lands in the account's span log group. Retention and access scoping are load-bearing controls, and teams that cannot accept this must run Path B. This is the honest cost of zero infrastructure, not an oversight.
- **`aws/spans` is shared account-wide.** Read access for CI is broader than a per-agent log group, and needs a resource policy rather than a blanket grant.
- **Query cost scales with log volume.** CloudWatch Logs bills by data scanned; frequent CI polling over a busy account is a real bill. Time-window bounding is mandatory, and at high volume Path B's collector may be cheaper than the queries.
- **Transaction Search is an account-level prerequisite** with its own span-ingestion cost, and it must be enabled before any of this works.
- **Fetch is a poll with a settle window**, so CI waits a few seconds per trace and a long-tail export can still be missed if `--wait` is too short. `fetch` reports a non-stable span set rather than returning a possibly-partial trace.
- **Two adapters to maintain** against a moving convention, not one.
- **AgentCore's env-var surface is not a stable contract**; ADOT configurator names and `DISABLE_ADOT_OBSERVABILITY` are AWS implementation details that can change without an OTel spec revision.
- **The Path B dual-export ambiguity is routed around, not resolved.** If telemetry does duplicate cleanly to both destinations without `DISABLE_ADOT_OBSERVABILITY`, a simpler Path B becomes available and §6 should be revisited.

### Out of scope

- Non-Python AgentCore runtimes; the variable names are Python-distro specific, the topology is not.
- AgentCore Gateway, Memory, and Identity telemetry — Runtime spans only.
- Other hosts (Lambda, ECS, EKS, self-hosted). Both patterns transfer; the variables do not.
- Live tailing or streaming fetch; `fetch` is request/response.
- Production sampling strategy and cost modelling.
- Session stitching across invocations, consistent with [ADR-002](002-episode-schema.md).

## Verification

- The image builds and runs with a byte-identical `CMD`; `git diff` on the Dockerfile is empty across the whole change, on both paths.
- **Path A, no collector:** one invocation, then `axda trace fetch --from cloudwatch --session …` yields a trace containing the root `invoke_agent` span and every child, and `axda evaluate` gates on it.
- The same trace fetched via Path A and captured via Path B produces byte-identical Episodes modulo `meta.adapter`.
- With content capture on, a Path A Episode reports `coverage.has_message_content = true` and runs the content clauses.
- With it off, the same contract reports those clauses `skipped` and exits `0` — the degradation is visible, not silent.
- `cloudwatch-spans/v1` does **not** set `coverage.degraded` on a healthy trace; it does set it on a span truncated at the log event size limit.
- A short invocation (<500ms) exports all spans including the final assistant turn — span counts compared at `OTEL_BSP_SCHEDULE_DELAY=200` versus the 5s default.
- Concurrent invocations: no fetched trace contains spans from another trace id.
- `fetch` against a still-exporting trace waits for the settle window; with `--wait` too short it reports a non-stable span set rather than writing a partial trace.
- `axda evaluate --gate` on an `adapter: xray/v1` Episode is refused, and the report carries the forensics banner.
- Path B only: spans in CloudWatch contain no `gen_ai.*.messages`; spans in the evaluation sink do.
- Path B only: killing the gateway raises the span-delivery alarm on the CloudWatch exporter.

## References

- [ADR-001](001-agent-admission-control.md) — trace collection is out of scope; the agent stays ignorant; on-ramp beats ideal
- [ADR-002](002-episode-schema.md) — versioned adapters, Episode boundaries, coverage flags, opt-in content
- [ADR-003](003-contract-lowering.md) — how missing coverage becomes `skipped`
- [CloudWatch Transaction Search](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch-Transaction-Search.html) — `aws/spans`, semantic-convention storage, W3C trace ids
- [Ingesting spans for complete visibility](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch-Transaction-Search-ingesting-spans.html) — 100% ingestion versus indexing percentage
- [Add observability to your AgentCore resources](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/observability-configure.html) — environment variables, `DISABLE_ADOT_OBSERVABILITY`
- [AgentCore observability telemetry](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/observability-telemetry.html) — ADOT requirement, Transaction Search prerequisite
- [X-Ray segment documents](https://docs.aws.amazon.com/xray/latest/devguide/xray-api-segmentdocuments.html) — metadata handling and the 64 kB cap behind the `xray/v1` restriction
