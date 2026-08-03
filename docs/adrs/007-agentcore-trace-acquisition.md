# ADR-007: Trace Acquisition from AWS Bedrock AgentCore Runtime

**Status:** Proposed
**Date:** 2026-08-02
**Revised:** 2026-08-03 (§3, §5, trade-offs: AgentCore's per-agent span destination)
**Authors:** Adrien
**References:** [ADR-001](001-agent-admission-control.md), [ADR-002](002-episode-schema.md), [ADR-003](003-contract-lowering.md), [AgentCore Observability](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/observability-configure.html), [CloudWatch Transaction Search](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch-Transaction-Search.html)

## Context

[ADR-001](001-agent-admission-control.md) declared trace collection out of scope: `axda` consumes traces, it is not a collector. That is the right boundary, but it leaves a gap that every user hits on day one: *I have an agent on AgentCore Runtime. Where do I get `trace.json`?*

This ADR answers that for the first reference runtime. It specifies configuration and one adapter, not a new subsystem.

### The starting point is already close to ideal

The reference deployment is a containerised AG-UI server whose image ends with:

```dockerfile
CMD ["opentelemetry-instrument", "python", "-m", "src.agui_server"]
```

`opentelemetry-instrument` is OpenTelemetry's zero-code auto-instrumentation launcher. With `aws-opentelemetry-distro` installed, it reads OTel configuration from environment variables and instruments the framework, Bedrock calls, tool invocations, and outbound HTTP without a line of application code.

This is exactly the coupling [ADR-001](001-agent-admission-control.md) asked for. The agent emits a trace because it was already instrumented for observability; `axda` is a consumer of that existing signal.

### The on-ramp has to be zero-infrastructure

[ADR-001 §7](001-agent-admission-control.md) chose git over OCI for policy bundles on the grounds that nothing should stand between someone and their first run. The same reasoning governs trace acquisition, and it is easy to get wrong: the architecturally cleanest answer is "run an OpenTelemetry Collector and fan telemetry out to a sink you control", which is also a deployment project. Requiring a new production component before the first evaluation would put the tool out of reach of exactly the person most likely to try it: one engineer with an agent already running.

So the question is whether telemetry AgentCore *already* emits, to a destination AgentCore *already* writes to, is good enough to gate on.

### It is, because a span log group is not the X-Ray segment format

With CloudWatch Transaction Search enabled, AgentCore delivers spans to a CloudWatch **span log group** (§3 covers which one). Two properties of that path decide this ADR:

- Spans there are **stored in semantic-convention format with W3C trace IDs**, and all span attributes are searchable. These are OTel spans in a log group, not X-Ray segment documents.
- Transaction Search **ingests 100% of spans as structured logs**. The configurable indexing percentage governs X-Ray trace summaries for search and analytics; it does not sample what lands in the log group.

Semantic-convention fidelity plus complete ingestion is enough for a deterministic, gate-worthy Episode. The X-Ray *API* (`BatchGetTraces`) is a different and genuinely degraded path (OTel attributes land in segment `metadata`, and segments are capped at 64 kB) but it is not the path we need.

## Decision

### 1. The Dockerfile does not change

```dockerfile
CMD ["opentelemetry-instrument", "python", "-m", "src.agui_server"]
```

Byte-identical, on both acquisition paths. `aws-opentelemetry-distro` stays in `requirements.txt`: already required for AgentCore observability.

This is stated as a decision rather than an incidental outcome because it is the falsifiable prediction [ADR-001](001-agent-admission-control.md) makes. If adding evaluation to a real managed runtime required touching the agent image, the out-of-band claim would be marketing.

### 2. Two first-class paths; CloudWatch is the default

| | **Path A (CloudWatch Transaction Search)** | **Path B (gateway collector)** |
|---|---|---|
| New infrastructure | none | an OTel Collector to deploy and operate |
| Env-var changes | content capture only | endpoint, headers, `DISABLE_ADOT_OBSERVABILITY` |
| Adapter | `cloudwatch-spans/v1` | `otlp/v1.41` |
| Fidelity | semantic-convention spans, 100% ingestion | reference |
| Gate-worthy | **yes** | yes |
| Content redaction | no: content lands in CloudWatch | yes, per-branch at the collector |
| Non-AWS sinks | no | yes |
| Trace grouping | by query predicate | `groupbytrace` processor |
| Marginal cost | Logs ingestion + query scan | collector compute + sink |

**Path A is the default.** Start there. Move to Path B when a named driver appears: a compliance requirement that prompts must not enter CloudWatch, a second destination such as Langfuse or Elastic, or a volume where per-query log scanning costs more than running a collector.

Making the upgrade conditional on a named driver matters: a topology adopted because it is architecturally tidier, rather than because something required it, is a component someone has to operate forever for no stated reason.

Both paths produce the same Episode and the same report. [ADR-002 §3](002-episode-schema.md)'s versioned-adapter design is what makes them interchangeable: the adapter absorbs the format difference and nothing downstream of it knows which ran.

### 3. Path A configuration

Account-level, once: **enable CloudWatch Transaction Search**. AgentCore requires it to deliver spans at all.

#### Which log group

AgentCore has two span destinations, and which one applies is not a choice this ADR gets to make:

| Destination | Location | Applies to |
|---|---|---|
| per-agent | the `spans` stream of `/aws/bedrock-agentcore/runtimes/<agent-id>-<endpoint>` | newly created agents in regions that support it, by default |
| shared | the `default` stream of `aws/spans` | agents created before their region supported the per-agent destination |

`UNIFIED_TRACES_DESTINATION_ENABLED=true|false` overrides the default per agent, and the per-agent destination needs `aws-opentelemetry-distro>=0.18.0`: earlier versions ignore the setting and deliver to `aws/spans` regardless. Switching does not move spans already delivered.

Both destinations hold the same records, so this is a `--log-group` argument and nothing more. It is called out because the failure mode is silent: a bounded query against the wrong group returns zero events rather than an error, which reads as "the agent emitted nothing".

Discover which one an agent uses before the first fetch:

```bash
aws logs describe-log-streams \
  --log-group-name "/aws/bedrock-agentcore/runtimes/<agent-id>-<endpoint>" \
  --query 'logStreams[].[logStreamName,lastEventTimestamp]' --output text
```

**Amended.** This originally said that a `spans` stream in that output means the per-agent destination. That test is not sufficient: the stream can be *present and empty*, with `lastEventTimestamp` null, while the agent in fact delivers to `aws/spans`. Observed on a deployed agent. Read the timestamp, not just the name, and treat an empty `spans` stream as evidence for the shared destination. Scoping `--log-stream` to a stream that was never written returns zero events, which is the same silent failure this section exists to prevent — so `--log-stream` is an optimization to apply *after* the destination is confirmed, never a default.

#### Where the message content is

**Amended.** Spans and message content arrive in *different* places, which this ADR originally conflated.

| | lands in | carries |
|---|---|---|
| spans | the span log group (§3 above) | the trajectory: ids, parentage, timings, tool names, token counts, `session.id` |
| message content | the `otel-rt-logs` stream of `/aws/bedrock-agentcore/runtimes/<agent-id>-<endpoint>` | `{input, output}` bodies, as OTel **log records** under scope `strands.telemetry.tracer` |

Content capture does not put prompts on the spans. Current ADOT/Strands releases follow the newer GenAI convention of emitting message content as log records correlated by `traceId`/`spanId`, so a span-only fetch yields `coverage.has_message_content = false` even with capture enabled. Recovering content therefore takes a **second bounded query, joined on span id** — which is what `fetch` now does.

The join needs no configuration: the spans name their own content source in `resource.attributes`, via `aws.log.group.names` and `aws.log.stream.names`. `fetch` reads the destination off the trace rather than deriving it from an agent id it was never given, so no account-specific log group name is ever hardcoded, and a trace that does not say degrades to spans-only instead of guessing.

#### Content capture

On the agent runtime, the *only* variable added beyond AgentCore's own observability defaults:

```
OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true
```

Nothing else. Notably **not** `OTEL_EXPORTER_OTLP_ENDPOINT` and **not** `DISABLE_ADOT_OBSERVABILITY`: the default AgentCore telemetry path is left exactly as it is, so there is no way to misconfigure it and silently break the ops team's dashboards. That was the riskiest instruction in the collector topology, and Path A removes it rather than mitigating it.

#### Reading a trace back

```bash
# by session: the reliable CI path, since you set the session id on the request
axda trace fetch --from cloudwatch \
  --session "$SESSION_ID" --region eu-west-1 --wait 30s --out trace.json

# per-agent span destination (confirm the stream is written before scoping to it)
axda trace fetch --from cloudwatch --session "$SESSION_ID" \
  --log-group /aws/bedrock-agentcore/runtimes/<agent-id>-<endpoint> \
  --out trace.json

# by trace id: incident review
axda trace fetch --from cloudwatch --trace-id <trace-id> --out trace.json
```

`fetch` queries the span log group filtered to the trace or session, polls until the span set is stable for a settle interval (default 3s) or `--wait` expires, then runs the content query described above and writes both record sets. Time-window bounding is mandatory in the query: CloudWatch Logs bills by data scanned. `--log-stream` cuts what gets scanned on the per-agent destination for the same reason, since that log group also carries the agent's stdout and its OTEL structured logs.

Two window notes, both learned the hard way:

- **A trace id bounds its own scan.** AWS-generated ids carry the trace's start time in their first 8 hex characters, so `--trace-id` narrows the query to the minutes around the trace rather than sweeping `--since`. The `--since` range is retried as a fallback, because the embedded timestamp is a convention rather than a guarantee and a wrong guess must degrade to a wider search, not to "no spans found".
- **`--since` has to outlive the incident.** The default was two hours, which cannot reach a trace worth reviewing days later. It is now 24 hours.

#### The pagination false negative

`FilterLogEvents` with a `filterPattern` over a wide window returns an **empty first page plus a `nextToken`** — the scan simply had not reached a match yet. Any caller that reads the first page and stops concludes "no data" for a trace that is present. In the AWS CLI this shows up as `--query 'length(events)'` printing `0`.

`fetch` is not exposed to this because it drains `NewFilterLogEventsPaginator`, but anything hand-written alongside it is. When a manual query comes back empty, narrow the window before believing it.

`--log-group` defaults to `aws/spans` rather than deriving the per-agent name, because the agent id is not otherwise an input to `fetch` and asking for it would make the shared case harder to serve than it is. The empty-result error names the per-agent form instead.

Trace grouping comes free here: the query *is* the grouping predicate, so Path A does not need the `groupbytrace` processor Path B requires, and cannot produce a partial-batch Episode.

### 4. The `cloudwatch-spans/v1` adapter, and an honest fidelity ladder

A real, versioned, tested adapter per [ADR-002 §3](002-episode-schema.md): not a reconstruction heuristic. It reads semantic-convention span records and maps them exactly as the OTLP adapter does; the difference is transport and envelope, not vocabulary.

Three tiers, and only the third is second-class:

| Adapter | Source | Fidelity | Gating |
|---|---|---|---|
| `otlp/v1.41` | collector (Path B) | reference | yes |
| `cloudwatch-spans/v1` | a span log group **plus the agent's content stream** (Path A) | semantic-convention spans, 100% ingestion | **yes** |
| `xray/v1` | `BatchGetTraces` | attributes in segment `metadata`, 64 kB cap | **no**: forensics |

**Amended.** The middle row originally read "a span log group", which was the whole mistake: it implied one query is enough. `cloudwatch-spans/v1` is gate-worthy for the *whole* contract only when the content records are joined in. Spans alone are gate-worthy for the trajectory half and honestly SKIP the rest.

The join happens in the adapter, before the Episode is built: content lands in the same `gen_ai.input.messages` / `gen_ai.output.messages` / `gen_ai.tool.call.*` attributes an OTLP trace would have carried it in. So the Episode model, the evaluators, and the clause registry stay unaware that this path exists, and the two paths remain comparable rather than forked. A span that carried its own content keeps it: the span is the more canonical source and a log record never silently overwrites it.

`cloudwatch-spans/v1` sets `coverage` from what the records actually contain, exactly like the OTLP adapter. It does not set `coverage.degraded` merely for being the CloudWatch path, because it is not degraded: claiming otherwise would train users to ignore the flag that [ADR-003 §5](003-contract-lowering.md) depends on.

Where it does set `degraded`: a span whose attributes were truncated against the CloudWatch Logs event size limit, or a trace whose span set never stabilised within `--wait`.

Span ids are unique only within a trace, so a content record whose `traceId` disagrees with the span it would attach to is dropped rather than merged. Grafting one trace's content onto another would fabricate evidence, which is worse than having none.

`xray/v1` remains for traces predating Transaction Search or in accounts where it is not enabled. Any Episode from it is marked `adapter: xray/v1`, carries a forensics banner in the report, and `axda evaluate` refuses `--gate` on it. Degraded input must not be able to produce a green build.

`BatchGetTraces` is a weaker fallback than the 64 kB cap alone suggests: Transaction Search indexes only a sampled percentage of traces into X-Ray (1% under the default probabilistic rule), so most trace ids are simply absent from it. It also takes the `1-<8 hex>-<24 hex>` presentation rather than the bare 32-hex W3C id. The span log group is the complete record; X-Ray is not a substitute for it.

### 5. Content capture, and what Path B actually buys

Without `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true`, message content and tool arguments are absent: they are opt-in because they carry user data and PII ([ADR-002 §1](002-episode-schema.md)). Against such a trace, `tool.allowlist`, ordering clauses, and budget clauses work; `content.no_pii`, `grounding.cite_sources`, and invariants over tool arguments report `skipped` ([ADR-003 §5](003-contract-lowering.md)). Useful, but half the contract.

**Amended.** Setting that variable is necessary but not sufficient, and the failure it leaves behind looks identical to not setting it at all. Content capture writes the bodies to the agent's `otel-rt-logs` stream, not to the spans (§3), so a fetch that reads only the span log group reports `has_message_content = false` on an agent that is capturing content correctly. The same half-a-contract symptom therefore has two causes — capture off, or content not joined — and the second is invisible from the span records. `fetch` warns when it recovers a span set with no content records rather than letting this surface only as skipped clauses at evaluation time.

Enabling it on Path A means prompts and tool arguments land in CloudWatch. That is a real expansion of sensitive-data surface, and it is the honest trade for zero infrastructure. Controls that apply:

- set a short retention on the span log group **and on the agent's own log group**, since after this amendment the prompts live in the latter
- restrict read access, and note that this is where the two destinations in §3 differ materially. On the per-agent destination, read access, retention, and KMS encryption are all scoped to one agent, so a CI role granted `logs:FilterLogEvents` on that group sees only that agent. On the shared `aws/spans` group the same grant lets CI read **every service's spans in the account**, and needs a resource policy rather than a blanket grant. Where the per-agent destination is available, prefer it for this reason and not for tidiness
- grant `logs:FilterLogEvents` on **both** groups when the spans are shared and the content is per-agent, which is the common case after the §3 amendment. A role holding only the span grant fetches a trajectory and silently loses the content clauses; `fetch` treats that as degraded rather than fatal, so the gate still runs on what it could read
- treat evidence output as sensitive; `--evidence=masked` is already the default ([ADR-002 §7](002-episode-schema.md))

**This is what Path B is for.** Its advantage was never fidelity: it is that a collector can redact content on the CloudWatch branch while preserving it on a short-lived evaluation sink, so the observability store never sees a customer prompt. Teams for whom that is a compliance requirement should go straight to Path B. Teams for whom it is not should not pay for a collector to get it.

### 6. Path B configuration, unchanged in substance

```
OTEL_EXPORTER_OTLP_ENDPOINT=https://otel-gw.internal:4318
OTEL_EXPORTER_OTLP_HEADERS=authorization=Bearer ${GW_TOKEN}
DISABLE_ADOT_OBSERVABILITY=true
OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true
```

`DISABLE_ADOT_OBSERVABILITY=true` is set deliberately. The documented behaviour of a custom endpoint *without* it is ambiguous (whether telemetry duplicates to both destinations or the AWS path wins is unclear) and a compliance-motivated topology should not rest on ambiguity. Everything goes to the collector; the collector is solely responsible for fan-out, including back to CloudWatch.

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

AgentCore freezes or reclaims the process between invocations. The default `BatchSpanProcessor` delay is 5s: long enough to strand the tail of a short invocation, and the stranded spans are typically the final assistant turn, which is precisely what the content clauses evaluate.

```
OTEL_BSP_SCHEDULE_DELAY=200
OTEL_BSP_MAX_QUEUE_SIZE=4096
```

A truncated trace is not detectable as truncated after the fact. It would present as a passing evaluation over a partial episode: the exact false green [ADR-003 §5](003-contract-lowering.md) exists to eliminate. That undetectability is why this is worth paying for up front rather than tuning when something looks wrong.

### 8. One Episode per invocation; the session id is metadata

An AgentCore Runtime session (`X-Amzn-Bedrock-AgentCore-Runtime-Session-Id`) can span many invocations. [ADR-002 §6](002-episode-schema.md) defines an Episode as one root `invoke_agent` span and puts cross-episode identity out of scope.

**Episode = one invocation.** The session id is carried into `EpisodeMeta` so multi-turn correlation stays possible when the aggregation layer arrives, and so `axda trace fetch --session` can find the trace in the first place. No v1 clause spans invocations.

### 9. Consumption modes

**CI gate**: a pre-production endpoint driven with fixture inputs:

```bash
SESSION_ID=$(uuidgen)
curl -H "X-Amzn-Bedrock-AgentCore-Runtime-Session-Id: $SESSION_ID" ...
axda trace fetch --from cloudwatch --session "$SESSION_ID" --wait 60s --out trace.json
axda evaluate --trace trace.json --policy oci://ghcr.io/acme/support-agent-evals:v2.1.0 --frozen
```

Exit code gates the pipeline per [ADR-001 §6](001-agent-admission-control.md).

**Production sampling**: a scheduled job fetches recent traces by service and evaluates a sample; violations alert. Sampled, because the LLM judge costs money per run and evaluating every episode is unnecessary.

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
   ┌───────────────┐            │  OTel Collector    │
   │ span log group│            │  groupbytrace      │
   │ per-agent, or │            └───┬────────────┬───┘
   │ shared        │         redact │            │ content kept
   │ aws/spans     │                v            v
   │  · semconv    │         CloudWatch     S3 eval sink
   │  · W3C ids    │         (no content)   (otlp_json)
   │  · 100%       │                             │
   └──────┬────────┘                             │
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
- The default path touches neither `OTEL_EXPORTER_OTLP_ENDPOINT` nor `DISABLE_ADOT_OBSERVABILITY`, so it cannot break existing observability: the largest operational risk in the collector topology is absent rather than mitigated.
- The span log group carries semantic-convention spans at 100% ingestion, so Path A is gate-worthy rather than advisory. Users are not asked to trust a lossy reconstruction. **Amended:** gate-worthy for the whole contract takes the content join as well; the spans alone are gate-worthy for the trajectory and honestly skip the content clauses.
- On the per-agent span destination, spans, structured logs, and stdout share one log group, so retention, encryption, and CI read access scope to a single agent. That removes most of the force of the account-wide-access trade-off below without introducing a collector.
- Query-based grouping means Path A cannot produce a partial-batch Episode.
- The fidelity ladder is explicit and only its bottom rung is restricted, so `coverage.degraded` keeps meaning something.
- The agent image is untouched on both paths: [ADR-001](001-agent-admission-control.md)'s central claim, demonstrated on a real managed runtime.
- Path B remains available with its benefit stated precisely (redaction and fan-out, not fidelity), so upgrading is a decision with a reason.

### Trade-offs

- **Path A puts prompts in CloudWatch.** Content capture is required for half the contract, and on Path A that content lands in the agent's own log group (§3, amended — not the span log group as this originally said). Retention and access scoping are the controls that make this acceptable, and teams that cannot accept this must run Path B. This is the honest cost of zero infrastructure, not an oversight.
- **Two queries, not one.** Path A reads two log groups per trace, so it costs two bounded scans and two read grants where this ADR originally promised one. `--no-content` buys the old single-scan behaviour back at the price of the content clauses, and the trace-id-bounded window keeps the added scan small.
- **The shared `aws/spans` group is account-wide.** Where an agent uses it, read access for CI covers every service's spans and needs a resource policy rather than a blanket grant. Agents on the per-agent destination avoid this.
- **The span destination is not something the tool can infer.** Which group an agent writes to depends on its creation date, its region, an environment variable, and its ADOT version, and querying the wrong one returns zero events rather than an error. `fetch` cannot detect this, only name it in the empty-result message, so the first fetch against a new agent may need one `describe-log-streams` call to resolve. Read `lastEventTimestamp` in that call, not just the stream names: a present-but-empty `spans` stream means the shared destination, and scoping to it is the same silent zero-events failure.
- **The content destination, by contrast, is inferred** — from `aws.log.group.names` on the spans. That asymmetry is not elegant, but it falls out of what each query already has in hand: the span destination has to be known *before* there are any spans to ask, and the content destination does not.
- **Query cost scales with log volume.** CloudWatch Logs bills by data scanned; frequent CI polling over a busy account is a real bill. Time-window bounding is mandatory, and at high volume Path B's collector may be cheaper than the queries.
- **Transaction Search is an account-level prerequisite** with its own span-ingestion cost, and it must be enabled before any of this works.
- **Fetch is a poll with a settle window**, so CI waits a few seconds per trace and a long-tail export can still be missed if `--wait` is too short. `fetch` reports a non-stable span set rather than returning a possibly-partial trace.
- **Two adapters to maintain** against a moving convention, not one.
- **AgentCore's env-var surface is not a stable contract**; ADOT configurator names and `DISABLE_ADOT_OBSERVABILITY` are AWS implementation details that can change without an OTel spec revision.
- **The Path B dual-export ambiguity is routed around, not resolved.** If telemetry does duplicate cleanly to both destinations without `DISABLE_ADOT_OBSERVABILITY`, a simpler Path B becomes available and §6 should be revisited.

### Out of scope

- Non-Python AgentCore runtimes; the variable names are Python-distro specific, the topology is not.
- AgentCore Gateway, Memory, and Identity telemetry: Runtime spans only.
- Other hosts (Lambda, ECS, EKS, self-hosted). Both patterns transfer; the variables do not.
- Live tailing or streaming fetch; `fetch` is request/response.
- Production sampling strategy and cost modelling.
- Session stitching across invocations, consistent with [ADR-002](002-episode-schema.md).

## Verification

- The image builds and runs with a byte-identical `CMD`; `git diff` on the Dockerfile is empty across the whole change, on both paths.
- **Path A, no collector:** one invocation, then `axda trace fetch --from cloudwatch --session …` yields a trace containing the root `invoke_agent` span and every child, and `axda evaluate` gates on it.
- The same trace fetched via Path A and captured via Path B produces byte-identical Episodes modulo `meta.adapter`.
- The same session fetched from the per-agent span destination and from `aws/spans` produces byte-identical Episodes and reports, modulo nothing: same adapter, same plan hash. This is what makes the destination a `--log-group` argument rather than a second adapter.
- `fetch` against a log group with no matching spans names `--log-group` in its error, so the per-agent destination is discoverable from the failure alone.
- `--log-stream` on a per-agent group returns the same span set as the unscoped query over that group, and scans less — checked against a stream confirmed to carry spans, since an empty one returns nothing and would pass a naive version of this check.
- With content capture on, a Path A Episode reports `coverage.has_message_content = true` and runs the content clauses. **Amended:** this holds only once the content records are joined; against the span log group alone the same trace reports `false`, which is the bug this amendment fixes rather than an acceptable outcome.
- Fetching one trace by id recovers the span tree from the span log group and the message bodies from the agent's content stream, with no flag naming either the content group or the stream: both come off the spans' own resource attributes.
- A span set fetched with `--no-content`, and the same set fetched with content and then evaluated, differ in exactly the content clauses: `content.no_pii` and friends SKIP in the first and run in the second, with identical verdicts everywhere else.
- Reordering the content records does not change the Episode: the merge is order-independent, so reports stay byte-identical across fetches.
- A content record whose `traceId` disagrees with the span it would attach to is not merged, and contributes no turn.
- With capture off, the same contract reports those clauses `skipped` and exits `0`: the degradation is visible, not silent.
- The report of a content-joined Episode contains no prompt text under the default `--evidence=masked`: findings carry `trace_id`, `span_id`, and `path` only.
- `cloudwatch-spans/v1` does **not** set `coverage.degraded` on a healthy trace; it does set it on a span truncated at the log event size limit.
- A short invocation (<500ms) exports all spans including the final assistant turn: span counts compared at `OTEL_BSP_SCHEDULE_DELAY=200` versus the 5s default.
- Concurrent invocations: no fetched trace contains spans from another trace id.
- `fetch` against a still-exporting trace waits for the settle window; with `--wait` too short it reports a non-stable span set rather than writing a partial trace.
- `axda evaluate --gate` on an `adapter: xray/v1` Episode is refused, and the report carries the forensics banner.
- Path B only: spans in CloudWatch contain no `gen_ai.*.messages`; spans in the evaluation sink do.
- Path B only: killing the gateway raises the span-delivery alarm on the CloudWatch exporter.

## References

- [ADR-001](001-agent-admission-control.md): trace collection is out of scope; the agent stays ignorant; on-ramp beats ideal
- [ADR-002](002-episode-schema.md): versioned adapters, Episode boundaries, coverage flags, opt-in content
- [ADR-003](003-contract-lowering.md): how missing coverage becomes `skipped`
- [CloudWatch Transaction Search](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch-Transaction-Search.html): `aws/spans`, semantic-convention storage, W3C trace ids
- [Ingesting spans for complete visibility](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch-Transaction-Search-ingesting-spans.html): 100% ingestion versus indexing percentage
- [Add observability to your AgentCore resources](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/observability-configure.html): environment variables, `DISABLE_ADOT_OBSERVABILITY`, and the per-agent span destination (`UNIFIED_TRACES_DESTINATION_ENABLED`, ADOT `>=0.18.0`)
- [View observability data for your AgentCore agents](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/observability-view.html): the `spans` stream, `[runtime-logs]`, and `otel-rt-logs` log group layout
- [AgentCore observability telemetry](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/observability-telemetry.html): ADOT requirement, Transaction Search prerequisite
- [X-Ray segment documents](https://docs.aws.amazon.com/xray/latest/devguide/xray-api-segmentdocuments.html): metadata handling and the 64 kB cap behind the `xray/v1` restriction
