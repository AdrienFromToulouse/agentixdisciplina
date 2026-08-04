# Contributing to axda

`axda` is an out-of-band evaluator for AI agents: it reads a recorded OpenTelemetry trace, checks it against a declarative contract, and emits verdicts with span-anchored evidence. It is pre-alpha. The contract surface, report schema, and Episode model follow the ADRs and are meant to be stable; everything else moves — which makes contributions welcome and architectural drift not.

## Where the docs are

| Document | What it covers |
|---|---|
| [README.md](README.md) | Overview, quickstarts (local trace and AWS Bedrock AgentCore), what works today |
| [docs/guide.md](docs/guide.md) | The user guide: full CLI reference, contract format, clause reference, report schema |
| [docs/adrs/summary.md](docs/adrs/summary.md) | **Read this first.** The architecture, and the six invariants a change may not quietly undo |
| [CLAUDE.md](CLAUDE.md) | Working rules for AI-assisted contributions; the same rules apply to humans |

## Build and test

Go **1.26** or newer (a hard floor in `go.mod`), no CGO, single static binary:

```bash
go build -o axda ./cmd/axda      # build
go test ./...                    # full test suite
go test ./internal/contract/ -run TestInvariant -v   # one test
go vet ./...                     # no other linter is configured; gofmt is expected
```

There is no CI: run those locally before pushing. Then exercise the binary against the checked-in example:

```bash
./axda explain  --contract examples/support-agent/contract.yaml
./axda evaluate --contract examples/support-agent/contract.yaml \
                --trace examples/support-agent/testdata/violating.json
```

Exit codes are part of the tool's contract and asserted in tests: `0` pass, `1` blocking violation, `2` contract or trace error.

LLM judge clauses need `ANTHROPIC_API_KEY`; without credentials they report `SKIP`, so the test suite does not require a key.

## Repo layout

| Path | What lives there |
|---|---|
| `cmd/axda` | CLI (cobra): `evaluate`, `explain`, `trace fetch` |
| `internal/adapter` | Trace decoders: OTLP/JSON, CloudWatch span records |
| `internal/episode` | The normalized unit of evaluation; the only thing evaluators ever see |
| `internal/contract` | Contract parsing and compilation; the closed clause registry (`registry.go`) |
| `internal/engine/rego` | Embedded OPA: quantified queries over the tool log (built-ins in `policy/*.rego`) |
| `internal/engine/cue` | Value constraints under unification: invariants, `tool.args_match` |
| `internal/engine/judge` | LLM judge and the LLM claim extractor; always advisory |
| `internal/detect` | Builtin regex/Luhn detection (`content.no_pii`, `content.deny_patterns`) |
| `internal/evaluate` | Runs the plan: coverage checks, panic isolation, score and gate |
| `internal/report`, `internal/verdict` | Report rendering and verdict semantics (`Blocks()`) |
| `internal/fetch` | CloudWatch trace acquisition |
| `internal/extract` | Structural claim extraction and the verbatim gate |
| `examples/support-agent` | The worked example: contract plus three recorded traces |
| `docs/` | User guide and ADRs |

## The rules your change must not break

These are the six invariants from [docs/adrs/summary.md](docs/adrs/summary.md), and the tests assert them directly. A change that touches one needs an ADR amendment, not just code.

1. **The agent stays ignorant.** No SDK, no callback, no middleware, no import. The only coupling is the trace.
2. **`skipped` is never `passed`, and neither is `errored`.** A check that could not run, or that broke, must never read as a check that succeeded.
3. **Determinism is a property of the whole input closure.** Only deterministic verdicts block; anything downstream of an LLM is advisory. Practical consequence: any new evaluator output must be explicitly sorted (OPA iteration order is unstable), and ten runs over one trace must produce byte-identical reports.
4. **No finding without a span.** Every finding carries `trace_id`/`span_id`/`path`.
5. **Contracts are compiled, never interpreted.** Clause names resolve against the closed registry; an unknown name is a compile error, never a prompt. Fail at load, not mid-run: new validation belongs at load time.
6. **Evidence is masked by default.** A report must not contain the PII it reports on.

## Common contribution shapes

**A new clause kind.** Register it in `internal/contract/registry.go` (grounding and judge kinds live in `grounding.go`). Pick the engine by question shape: Rego for quantified queries over the event log (allowlists, ordering), CUE for value constraints under unification, `metric` for budgets, judge for anything probabilistic (always advisory). Declare `Requires` coverage flags so missing instrumentation skips instead of passing, reject malformed params at load, and add tests. Update both clause tables in the same PR: the guide's clause reference and the README's "What works today".

**An ADR or an amendment.** ADRs follow a fixed house structure — copy an existing one in `docs/adrs/` rather than inventing a format, and add it to `summary.md`. Amendments to shipped decisions go in the summary's Amendments section.

**Implementing something from "Not built yet".** The README and guide both track the gap between the ADRs and the binary. Keep those sections accurate in the same PR, whether you close the gap or decide to defer it.

## PR hygiene

- `gofmt`, `go vet ./...`, and `go test ./...` clean.
- **Never commit real identifiers.** AWS account, agent, session, and trace ids in docs, tests, and examples are placeholders, always.
- Do not commit the built `axda` binary or the `.axda/` cache directory (both are gitignored).
- Documentation that quotes binary behavior — sample outputs, flag defaults, sizes — must match a real run of the binary, not memory.
