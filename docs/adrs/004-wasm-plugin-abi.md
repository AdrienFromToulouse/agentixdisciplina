# ADR-004: WASM Plugin ABI `axda/plugin/v1`

**Status:** Proposed
**Date:** 2026-08-02
**Authors:** Adrien
**References:** [ADR-001](001-agent-admission-control.md), [ADR-002](002-episode-schema.md), [ADR-003](003-contract-lowering.md), [ADR-006](006-oci-distribution.md), [wazero](https://wazero.io/)

## Context

[ADR-001](001-agent-admission-control.md) chose WASM over gRPC subprocesses for third-party evaluators, for one reason above the others: `--policy github.com/some-org/evals` means running code authored by strangers against a production trace that contains everything the agent said and every argument it passed to every tool. A default-deny sandbox is the only thing standing between a shared policy ecosystem and a trace-exfiltration channel.

That decision leaves the mechanism unspecified, and the mechanism is where sandboxes usually fail. Three things need pinning down:

- **The boundary.** What can a plugin reach, how does it ask, and what does the user have to agree to? A capability model that is granted implicitly is not a capability model.
- **Determinism.** [ADR-001](001-agent-admission-control.md) promises byte-identical reports and [ADR-002 §4](002-episode-schema.md) establishes that a verdict is deterministic only if all its inputs were. A WASM module is not automatically deterministic: WASI hands out clocks, randomness, and filesystem access, and Go's runtime calls `random_get` before `main` even starts.
- **Failure.** A plugin that traps, hangs, or returns garbage must never produce a passing clause.

## Decision

### 1. wazero on a restricted WASI Preview 1, not the component model

`axda` embeds [wazero](https://wazero.io/) (pure Go, no CGO, no external runtime) which preserves the single-static-binary and cross-compile properties from [ADR-001](001-agent-admission-control.md).

The guest targets **WASI Preview 1**, not the component model. The component model's interface story is genuinely better and would remove most of §3's hand-rolled memory protocol, but the toolchain is not there for the languages plugin authors will actually use: Go reaches `wasip1` through first-class support in the standard toolchain, and TinyGo and Rust both have mature `wasm32-wasip1` targets. Choosing the better ABI at the cost of "you cannot build this with the Go toolchain" would leave the extension path theoretical. Revisit when Go targets WASI 0.2 directly.

The guest does **not** get stock WASI. See §4.

### 2. Guest exports

```wat
axda_abi_version()               -> i32   ;; 1
axda_manifest()                  -> i64   ;; ptr<<32 | len
axda_alloc(size: i32)            -> i32   ;; ptr
axda_free(ptr: i32, size: i32)
axda_evaluate(ptr: i32, len: i32) -> i64  ;; ptr<<32 | len
```

Call sequence per evaluation: host reads `axda_abi_version()` and refuses on major mismatch → host calls `axda_alloc` for the request size, writes a protobuf `EvaluateRequest` into guest memory, calls `axda_evaluate`, unpacks the returned pointer/length, reads the `VerdictSet`, calls `axda_free`.

Wire format is protobuf, not JSON: the Episode is the largest thing crossing the boundary and gets copied on every call, so encoding cost is not incidental. Field numbers are permanent and never reused, which is what lets `episode/v1` grow additively without an ABI bump.

```proto
message EvaluateRequest {
  Episode episode      = 1;   // ADR-002
  string  clause_name  = 2;   // which registered clause is being evaluated
  bytes   params       = 3;   // JSON, validated against the manifest schema
  string  episode_schema = 4; // "episode/v1"
}

message VerdictSet { repeated Verdict verdicts = 1; }

message Verdict {
  string   clause    = 1;
  Status   status    = 2;   // PASS | FAIL | SKIPPED
  Severity severity  = 3;
  string   message   = 4;
  repeated Evidence evidence = 5;  // must reference spans present in the episode
}
```

`ERRORED` is deliberately absent from the guest's `Status` enum. A plugin cannot declare itself errored; only the host assigns that state, and only for the conditions in §7. This keeps "errored" meaning "the host could not trust this result" rather than something a plugin can self-report or, worse, fail to self-report.

### 3. Self-description via manifest

`axda_manifest()` returns a protobuf describing the clause kinds the plugin provides. These register into the [ADR-003 §7](003-contract-lowering.md) namespace:

```proto
message Manifest {
  string abi             = 1;  // "axda/plugin/v1"
  string episode_schema  = 2;  // "episode/v1"
  repeated ClauseKind clauses = 3;
}

message ClauseKind {
  string name             = 1;  // must be namespaced: "acme.verify_kyc"
  bytes  params_schema    = 2;  // JSON Schema
  repeated string requires = 3; // Coverage flags (ADR-002)
  bool   prefix_decidable  = 4; // ADR-005
}
```

The manifest declares `requires` but **not** `class`. Verdict class is computed by the host from the plugin's granted capabilities (§5), because a plugin asserting its own trustworthiness is not evidence of anything.

### 4. A deterministic WASI shim, not the real one

Stock `wasi_snapshot_preview1` grants a wall clock, a real entropy source, and file descriptors. `axda` instantiates a replacement:

| WASI call | Behaviour |
|---|---|
| `clock_time_get` (realtime) | returns `episode.meta.started_at`, fixed for the whole evaluation |
| `clock_time_get` (monotonic) | returns a counter incremented per call (ordered, reproducible, not wall-clock) |
| `random_get` | fills from ChaCha8 seeded with `episode_id` |
| `fd_write` (1, 2) | captured into the plugin log, never the process's stdout |
| `fd_*` (all other) | `ENOTCAPABLE` |
| `environ_get`, `args_get` | empty |
| `sock_*`, `path_*` | `ENOTCAPABLE` |

`random_get` returns a seeded stream rather than an error because Go's `wasip1` runtime calls it during startup to seed map and hash iteration; returning `ENOSYS` would break every Go-authored plugin before `main`. Seeding from `episode_id` keeps a plugin that uses randomness reproducible for a given episode (which is the property that actually matters) without pretending randomness is unavailable.

The result: a plugin holding no capabilities is deterministic by construction. It cannot observe anything that varies between two runs over the same episode.

### 5. Capabilities are declared, granted, trusted, and reflected in verdict class

Privileged operations are host functions in a module named `axda_host`. **Ungranted functions are not linked at all**, so a module importing one fails to instantiate (at bundle load, before a trace is read) rather than trapping halfway through an evaluation. Failing at load makes over-reach a setup error with a clear message instead of a mysterious mid-run failure.

| Host function | Capability | Effect on class |
|---|---|---|
| `log(ptr, len)` | none: always available | deterministic |
| `read_file(ptr, len) -> i64` | `read_file` | **deterministic** |
| `judge(ptr, len) -> i64` | `judge` | **probabilistic** |
| `http_fetch(ptr, len) -> i64` | `http` | **probabilistic** |

`read_file` preserves determinism because it is chrooted to the bundle directory and the bundle is content-addressed in `axda.lock` ([ADR-001 §7](001-agent-admission-control.md)): the bytes a plugin can read are pinned by the same hash that pins the plugin itself. `judge` and `http` reach outside that hash, so verdicts from a plugin holding either are forced to `probabilistic` and cannot block the build, whatever the plugin or the contract claims.

This is [ADR-002 §4](002-episode-schema.md)'s rule applied to a second surface: determinism is a property of the whole input closure, and the host is the only party positioned to judge it.

`judge` exists so an LLM-judging plugin calls back into the host's configured provider (with the host's credentials, rate limits, and caching) rather than opening its own socket. A plugin that wants to talk to a model does not need `http`, and asking for `http` when `judge` would do is a signal reviewers can act on.

Grants live in bundle metadata and require explicit user acceptance:

```yaml
# axda.yaml
plugins:
  - path: plugins/kyc.wasm
    capabilities: [read_file]
  - path: plugins/vendor-judge.wasm
    capabilities: [judge]
```

```
$ axda evaluate --policy github.com/acme/evals@v2.0.0
  bundle github.com/acme/evals@v2.0.0 requests capabilities:
    plugins/vendor-judge.wasm   judge   (verdicts will be advisory)
  accept with: axda policy trust github.com/acme/evals@v2.0.0
```

`axda policy trust` records the accepted capability set against the bundle digest. A later version that adds a capability re-prompts; a version that only changes policy code does not. This is the npm-install-scripts problem, and the answer is the same one Terraform reached for provider checksums: pin the artifact, surface the delta, make escalation an event.

### 6. Resource limits and instance lifecycle

- **Memory:** capped at 64 MiB (1024 pages), configurable per bundle.
- **Time:** wall-clock deadline, default 2s per evaluation, enforced through wazero's `WithCloseOnContextDone`.
- **Instances:** the module is compiled once per bundle and **instantiated fresh for every evaluation**. No state survives between episodes, which removes both cross-episode data leakage and the class of nondeterminism where verdict *n* depends on evaluation *n−1*.

wazero has no instruction-level fuel metering (unlike wasmtime), so an infinite loop is caught by the wall-clock deadline rather than a deterministic instruction budget. That is a genuine determinism wrinkle: a plugin near the limit could time out on a loaded machine and complete on an idle one. It is acceptable only because the failure direction is safe: a timeout is `errored` (§7), and `errored` on a blocking clause fails the build. A plugin cannot get a *pass* out of being slow. The report records the deadline and the elapsed time so the flakiness is diagnosable rather than mysterious.

### 7. Every failure mode resolves to `errored`, never `passed`

| Condition | Outcome |
|---|---|
| ABI major version mismatch | bundle load fails, exit `2` |
| imports an ungranted host function | bundle load fails, exit `2` |
| manifest declares a non-namespaced or reserved clause name | bundle load fails, exit `2` |
| trap (unreachable, OOB access, alloc failure) | clause `errored` |
| exceeds the deadline | clause `errored` |
| returns malformed protobuf | clause `errored` |
| returns evidence referencing a span not in the episode | clause `errored` |

`errored` on a blocking clause exits `1`, exactly like a violation. A broken evaluator is a failed check, not an absent one: the same reasoning that makes `skipped ≠ passed` in [ADR-003 §5](003-contract-lowering.md), applied to plugins that break rather than plugins that lack data.

The evidence-validation row matters more than it looks: without it a plugin could fabricate a `span_id` and produce a violation pointing at nothing, which would erode the one property [ADR-001](001-agent-admission-control.md) says every finding must have.

### 8. Toolchain and authoring

| Language | Build |
|---|---|
| Go 1.24+ | `GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared` with `//go:wasmexport` |
| TinyGo | `tinygo build -target=wasip1 -buildmode=c-shared` |
| Rust | `cargo build --target wasm32-wasip1` with `#[no_mangle] extern "C"` |

`axda plugin scaffold --lang go` generates a working evaluator with the alloc/free/evaluate boilerplate, generated Episode bindings, and a test harness. `axda plugin verify plugin.wasm` checks exports, ABI version, manifest validity, and (the useful one) that declared capabilities match actual imports in both directions: importing an undeclared function is an error, and declaring a capability the module never imports is a warning about over-privilege.

## Architecture Overview

```
  bundle load                        per evaluation
  ───────────                        ──────────────
  read axda.yaml                     instantiate fresh module
      │                                    │
      ├─ capability grants                 ├─ alloc + write EvaluateRequest (proto)
      ├─ check trust record ──► prompt     ├─ call axda_evaluate
      │                                    ├─ read VerdictSet
      v                                    ├─ validate evidence spans
  wazero.CompileModule                     └─ close instance (no state carries over)
      │
      ├─ link deterministic WASI shim   clock=episode start · rand=seed(episode_id)
      │                                 fd_write→log · everything else ENOTCAPABLE
      │
      └─ link axda_host: ONLY granted functions
                │
      ┌─────────┴──────────┬─────────────┬──────────────┐
    log()              read_file()     judge()      http_fetch()
    always             bundle-chroot   host LLM     network
      │                     │              │             │
      └── deterministic ────┘              └─ probabilistic ─┘
                │                                  │
             blocking                           advisory

  trap · deadline · malformed output · bogus span  ──►  ERRORED (never PASSED)
```

## Consequences

### Benefits

- A hostile bundle cannot exfiltrate the trace it is evaluating: with no capability grant there is no clock to time-channel with, no socket, and no filesystem.
- Over-reach fails at load with a legible message, not mid-run with a trap.
- Capability-derived verdict class means a plugin cannot obtain blocking authority by claiming to be deterministic; the host decides from what it actually granted.
- Fresh instantiation per evaluation makes cross-episode leakage and order-dependent verdicts structurally impossible rather than merely discouraged.
- Every failure path lands on `errored`, so no plugin bug can be mistaken for a passing check.
- `judge` as a host capability keeps model credentials, caching, and rate limiting in one place instead of scattered across vendor plugins.
- Pure-Go runtime keeps the single-binary, no-CGO, cross-compile properties intact.

### Trade-offs

- **The authoring barrier is real.** WASI p1 with a hand-rolled memory protocol is meaningfully harder than "read JSON from stdin". `scaffold` and `verify` reduce it; they do not remove it. This is the accepted price of running strangers' code, and it is why built-ins cover the common cases.
- **No fuel metering.** wazero's lack of instruction budgeting means the timeout is wall-clock and therefore machine-dependent. Fail-safe, but a source of CI flakiness that a wasmtime-based design would not have. Switching runtimes would cost CGO or a non-Go dependency, which is a worse trade.
- **Full Episode copy per evaluation.** Every plugin gets the whole episode serialized into its memory; ten plugins means ten copies. No shared memory, no lazy access. Fine at current trace sizes, a problem at very large ones.
- **A deterministic clock will surprise someone.** A plugin measuring its own elapsed time gets a counter, not milliseconds. Documented, still a footgun.
- **Capability granularity is coarse.** `http` is all-or-nothing (no per-host allowlist in v1) so a plugin needing one endpoint gets the whole network. Mitigated by class forcing (it cannot block the build anyway) and by `judge` covering the main legitimate use.
- **WASI p1 is the deprecated preview.** Choosing it buys toolchain reach today and a migration later.

### Out of scope

- WASI 0.2 / the component model: revisit when the Go toolchain targets it directly.
- Per-host or per-URL scoping of the `http` capability.
- Plugin-to-plugin composition or plugins that invoke other clauses.
- Shared or streaming Episode access instead of a full copy.
- Plugin-supplied adapters or claim extractors. [ADR-002 §4](002-episode-schema.md) allows `extractor: plugin` in the schema, but the extractor ABI is deferred.
- A plugin registry or discovery service; distribution is [ADR-006](006-oci-distribution.md)'s problem.

## Verification

- A plugin built with each supported toolchain loads, evaluates a fixture episode, and returns the expected verdicts.
- A plugin importing `http_fetch` with no `http` grant fails at bundle load with exit `2`, and no trace is read.
- A plugin granted `http` produces verdicts with `class: probabilistic`, and a contract marking its clause `blocking: true` still does not fail the build.
- A plugin granted only `read_file` produces `deterministic` verdicts and can block.
- Determinism: a plugin calling `clock_time_get` and `random_get` produces byte-identical verdicts across 10 runs over the same episode.
- A plugin that writes to a global on evaluation *n* observes it zeroed on *n+1*.
- An infinite-loop plugin is killed at the deadline, its clause reports `errored`, and a blocking clause in that state exits `1`.
- A plugin returning evidence with a fabricated `span_id` reports `errored`, not a violation.
- `axda plugin verify` flags a module declaring `http` that never imports `http_fetch`.
- Adding a capability to a trusted bundle re-prompts; changing only policy code does not.

## References

- [ADR-001](001-agent-admission-control.md): chose WASM, and why the sandbox is load-bearing
- [ADR-002](002-episode-schema.md): Episode is the wire type; provenance determines verdict class
- [ADR-003](003-contract-lowering.md): plugin clause kinds register into the namespaced registry
- [ADR-006](006-oci-distribution.md): plugin layers and signature verification
- [wazero](https://wazero.io/): pure-Go WebAssembly runtime
- [Go WASI support](https://go.dev/blog/wasi): `GOOS=wasip1`, `//go:wasmexport`, reactor builds
