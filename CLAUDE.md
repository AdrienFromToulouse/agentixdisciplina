# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`axda` (AgentixDisciplina) is a Go CLI that evaluates recorded AI-agent traces against declarative contracts — "Agent Admission Control." It reads an OpenTelemetry trace, checks it against a `contract.yaml`, and emits verdicts with span-anchored evidence. The agent under evaluation never knows it exists; the only coupling is the trace.

## Commands

```bash
go build -o axda ./cmd/axda        # build the binary
go test ./...                      # full test suite
go test ./internal/contract/ -run TestInvariant -v   # single test
go vet ./...                       # no other linter is configured; gofmt is expected

# exercise the binary against the checked-in example
./axda explain  --contract examples/support-agent/contract.yaml
./axda evaluate --contract examples/support-agent/contract.yaml \
                --trace examples/support-agent/testdata/violating.json
```

Exit codes are part of the tool's contract (asserted in tests): `0` pass, `1` blocking violation, `2` contract/trace error. Subcommands: `evaluate`, `explain`, `trace fetch` (CloudWatch). LLM judge clauses need `ANTHROPIC_API_KEY` and are cached in `.axda/judge-cache.json`; without credentials they SKIP.

## Architecture

The architecture is settled in `docs/adrs/` — read `docs/adrs/summary.md` first. It lists six invariants later work may not quietly undo (agent stays ignorant; skipped/errored is never passed; determinism is a property of the whole input closure; no finding without a span; contracts are compiled, never interpreted; evidence is masked by default). Changes that touch those need an ADR amendment, not just code. ADRs follow a fixed house structure — copy an existing one rather than inventing a format.

### Evaluation pipeline

```
trace (OTLP / CloudWatch / raw) ──internal/adapter──► Episode (internal/episode)
contract.yaml ──internal/contract──► Plan (clauses resolved against a closed registry)
Plan × Episode ──internal/evaluate──► Report (internal/report, internal/verdict)
```

- **`internal/episode`** — the normalized unit of evaluation: turns, tool calls, claims, metrics, and `Coverage` flags. Evaluators only ever see an Episode, never a raw trace.
- **`internal/contract`** — parses and *compiles* the contract. `registry.go` holds the closed clause registry: an unknown clause name is a compile error, never interpreted (this is the decision the system rests on, ADR-003 §1). `values.go` binds invariant operands (`spec.values`) with explicit, required cardinality; an undeclared identifier fails at load. `contract.go` produces the `Plan` that `axda explain` prints, content-hashed into every report.
- **`internal/engine/`** — one package per evaluator backend, split by question shape: `rego` (embedded OPA; quantified queries over the tool log — allowlists, ordering; built-in policies live in `rego/policy/*.rego`), `cue` (value constraints under unification — invariants, `tool.args_match` schema checks), `judge` (LLM, always advisory). Regex/Luhn detection (`content.no_pii`) is builtin Go in `internal/detect`, no engine.
- **`internal/evaluate`** — runs the plan: coverage check (`requires ⊄ coverage` → SKIP, never PASS), panic isolation (a broken evaluator is ERRORED, never a pass), score and gate computation. The gate is the violation list; the score only summarizes.

### Rules the code enforces everywhere (and tests assert directly)

- **SKIP ≠ PASS.** A clause whose inputs are absent (missing coverage flag, unbound value with no default) reports SKIPPED with a remediation hint. Never let a check vacuously pass.
- **Determinism → byte-identical reports.** Only deterministic verdicts can block; anything downstream of an LLM is advisory (`Blocks()` in `internal/verdict` enforces this — even `blocking: true` on a judge cannot gate). Rego result sets are explicitly sorted (`engine/rego/rego.go`) because OPA iteration order is unstable; keep any new evaluator output sorted.
- **No finding without a span.** Every `Finding` carries `trace_id`/`span_id`/`path`.
- **Fail at load, not mid-run.** Unknown clause names, undeclared/malformed invariant expressions, missing `cardinality`, non-namespaced custom clauses, and non-compiling Rego are all compile errors, before any trace is read. New validation belongs at load time.
- **Evidence is masked by default.** A report must not contain the PII it reports on (`--evidence full` is the opt-in).

### Extension points

Custom clauses are contract-declared Rego, compiled and namespace-checked at load (`acme.*`; bare namespaces like `tool.` are reserved so built-ins cannot be shadowed). WASM plugins, OCI bundles, and the inline gate are specified in ADRs 004–006 but not built; the README's "Not built yet" section tracks the gap between ADRs and binary — keep it accurate when implementing or deferring features.
