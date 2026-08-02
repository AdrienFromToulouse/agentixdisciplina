# ADR-006: OCI Bundle Distribution and Signing

**Status:** Proposed
**Date:** 2026-08-02
**Authors:** Adrien
**References:** [ADR-001](001-agent-admission-control.md), [ADR-003](003-contract-lowering.md), [ADR-004](004-wasm-plugin-abi.md), [ORAS](https://oras.land/docs/concepts/artifact/), [Sigstore](https://docs.sigstore.dev/cosign/signing/other_types/)

## Context

[ADR-001 §7](001-agent-admission-control.md) chose git resolution for v1 on adoption grounds: it reuses existing SSH keys and tokens, so private bundles work with no new infrastructure and nothing stands between someone and their first run. That reasoning still holds and git is not being replaced.

But git has three properties that become disqualifying once bundles are enforcing policy in a regulated or air-gapped deployment:

- **Tags are mutable.** `@v1.2.0` can be force-pushed to point at different content. `axda.lock` catches this on re-resolution, but the artifact itself carries no immutable identity.
- **There is no signing story.** Git commit signatures attest that someone pushed a commit, not that a reviewed bundle was published. There is no standard way to say "only accept bundles published by our security team's CI".
- **Air-gapped and mirrored environments are painful.** Getting a git dependency into a network-isolated environment means a manual clone-and-copy dance with no integrity checking at the boundary.

Meanwhile [ADR-004 §5](004-wasm-plugin-abi.md) raised the stakes. Bundles can carry WASM plugins that request capabilities, and the trust prompt records acceptance against a bundle digest. The value of that record depends entirely on the digest being a trustworthy, immutable identity for reviewed content. Git gives us a commit SHA — content-addressed, but with no statement about who reviewed it or whether it was ever tested.

Every organisation that would deploy this already runs a container registry with authentication, quotas, replication, garbage collection, and vulnerability scanning. OPA bundles, Helm charts, SBOMs, and model weights already ship as OCI artifacts. Building bespoke distribution infrastructure for policy bundles when that exists would be a mistake.

## Decision

### 1. OCI is a second `Resolver`, not a replacement

```
--policy github.com/company/support-agent-evals@v1.2.0     # git, unchanged
--policy oci://ghcr.io/company/support-agent-evals:v1.2.0  # new
--policy ./local-bundle                                     # file, unchanged
```

Both remain first-class and permanently supported. Git is the on-ramp — clone, edit, push, run. OCI is the production path — signed, immutable, mirrorable. Forcing a registry publish before someone can try a policy they just wrote would be the wrong trade, which is why [ADR-001](001-agent-admission-control.md) sequenced them this way.

The `Resolver` interface from [ADR-001 §7](001-agent-admission-control.md) is unchanged; this is a second implementation behind it.

### 2. Artifact layout

A bundle is an OCI image manifest with a distinct `artifactType`, following the pattern ORAS established and OPA bundles already use:

```
manifest
  artifactType: application/vnd.axda.bundle.v1+json
  config:
    mediaType: application/vnd.axda.bundle.config.v1+json
    (bundle metadata: name, version, episode schema, contract apiVersion,
     clause kinds provided, capability requests)
  layers:
    - application/vnd.axda.bundle.layer.v1.tar+gzip     contract, policy/, schema/, judges/, testdata/
    - application/vnd.axda.plugin.v1+wasm               one layer per plugin
  annotations:
    org.opencontainers.image.source        git URL
    org.opencontainers.image.revision      commit SHA
    org.opencontainers.image.created       build time
    dev.axda.episode.schema                episode/v1
    dev.axda.contract.apiversion           axda.dev/v1
    dev.axda.capabilities                  judge,read_file
    dev.axda.clauses                       acme.verify_kyc,acme.pci_scope
```

Two layout choices carry weight:

**Plugins are separate layers with their own media type.** A registry then deduplicates a plugin shared across bundles, garbage-collects it independently, and — the operationally useful part — a security team can enumerate every `application/vnd.axda.plugin.v1+wasm` blob in the registry to inventory the WASM running against their traces. Tarring plugins into the bundle layer would make them invisible.

**Capabilities and clause names are annotations.** Annotations are readable from the manifest without pulling any layer, so `axda policy inspect oci://…` can report "this bundle wants the `judge` capability" over a HEAD-sized request, before anything is downloaded. [ADR-004](004-wasm-plugin-abi.md)'s consent prompt is a lot more meaningful when the user can inspect before fetching rather than after.

The `git` resolver produces the same in-memory bundle from the same directory layout, so nothing downstream of resolution knows which resolver ran.

### 3. Signing with Sigstore, keyless by default

Bundles are signed with cosign. Signatures are themselves OCI artifacts referencing the subject digest, discoverable through the referrers API — no separate signature infrastructure.

Keyless (OIDC, ephemeral certificate, transparency log) is the default because the alternative is a long-lived private key that has to be stored, rotated, and revoked, and the realistic outcome of asking a team to do that is an unsigned bundle. Keyless binds the signature to a workload identity — a specific GitHub Actions workflow in a specific repository — which is a stronger and more useful claim than "someone had the key".

Verification is policy, expressed in the consuming repo:

```yaml
# .axda/trust.yaml
policies:
  - pattern: oci://ghcr.io/company/*
    require_signature: true
    certificate_identity: https://github.com/company/agent-evals/.github/workflows/release.yml@refs/heads/main
    certificate_oidc_issuer: https://token.actions.githubusercontent.com

  - pattern: oci://ghcr.io/vendor/*
    require_signature: true
    certificate_identity_regexp: ^https://github\.com/vendor/.+
    certificate_oidc_issuer: https://token.actions.githubusercontent.com
```

`require_signature: true` on a pattern makes an unsigned or mismatched bundle a **resolution failure** — exit `2`, before any trace is read. Verification failures are never warnings. A warning on a signature check is a check that gets ignored.

Key-based signing is supported for environments without OIDC.

### 4. Digests are mandatory in the lockfile; tags are only ever an input

Tags resolve **once**. `axda.lock` records the digest, and every subsequent resolution fetches by digest:

```yaml
policies:
  - source: oci://ghcr.io/company/support-agent-evals
    version: v1.2.0
    digest: sha256:9f2c4e...
    signature:
      verified: true
      identity: https://github.com/company/agent-evals/.github/workflows/release.yml@refs/heads/main
      issuer: https://token.actions.githubusercontent.com
    capabilities: [judge]
```

`--frozen` fails if the resolved digest differs from the lock, so a re-tagged `v1.2.0` is a hard error rather than a silent policy change. The recorded signature identity is part of the lock, which means an attacker who compromises a *different* CI workflow in the same organisation and publishes a validly-signed bundle still fails verification — the identity moved.

### 5. Signature, digest, and capability consent are one trust decision

[ADR-004 §5](004-wasm-plugin-abi.md) records capability acceptance against a bundle digest. This ADR makes that record stronger by binding three facts together:

```
axda policy trust  ⇒  (digest, signer identity, capability set)
```

- Because capabilities are in the signed config and annotations, a capability escalation changes the digest, which invalidates consent and re-prompts.
- Because the signer identity is in the lock, a bundle signed by a different identity re-prompts even at the same version.
- Because plugins are separate signed layers, swapping a plugin changes the manifest digest even if the policy text is byte-identical.

Capability escalation therefore cannot ride along with a routine version bump. That is the specific attack the OCI move buys protection against, and it is the reason this ADR matters more than "git works fine".

### 6. Publishing runs the bundle's own tests first

```
$ axda bundle push oci://ghcr.io/company/support-agent-evals:v1.2.0
  running axda test … 14 clauses, 28 fixtures … ok
  building artifact … 2 layers (bundle 41 KiB, kyc.wasm 210 KiB)
  pushing … sha256:9f2c4e…
  signing (keyless, github-actions) … ok
  attaching SLSA provenance … ok
```

`axda test` runs before the push and a failure aborts it. `--skip-tests` exists and prints a warning that lands in the CI log.

[ADR-001 §7](001-agent-admission-control.md) required bundles to ship golden traces on the grounds that a policy never exercised against a failing trace is how you get a green CI check that verifies nothing. That requirement is only enforceable at a choke point, and publishing is the choke point. A bundle in a registry is a bundle other teams will trust; making "it passed its own fixtures" a precondition of publication is the cheapest available guarantee.

SLSA provenance is attached as a referring artifact, so a consumer can trace a bundle back to the commit and workflow that produced it.

### 7. Air-gapped transfer

```
axda bundle save oci://ghcr.io/company/evals:v1.2.0 -o evals-v1.2.0.tar   # includes signature
axda bundle load evals-v1.2.0.tar                                          # verifies on load
axda bundle copy oci://ghcr.io/... oci://internal.registry/...             # mirror, digest preserved
```

The tarball is an OCI layout directory, so it is also consumable by `oras`, `crane`, and `skopeo`. Digests and signatures survive the round trip; `load` verifies against `.axda/trust.yaml` exactly as a network pull does.

## Architecture Overview

```
  authoring                    publishing                     consuming
  ─────────                    ──────────                     ─────────
  bundle dir                   axda bundle push               axda evaluate --policy oci://…
    contract.yaml                   │                              │
    policy/*.rego                   ├─ axda test  ──fail──► abort  ├─ HEAD manifest
    schema/*.cue                    │                              │    └─ annotations:
    judges/*.md                     ├─ build manifest              │       capabilities, clauses
    plugins/*.wasm                  │    artifactType              │         (inspect before pull)
    testdata/*.json                 │    + bundle layer            │
        │                           │    + one layer per plugin    ├─ verify signature
        │                           │                              │    identity + issuer
        └── git push ──► git        ├─ push ──► registry ◄─────────┤    unsigned ⇒ exit 2
            (ADR-001 §7,            │                              │
             still supported)       ├─ cosign sign (keyless)       ├─ pin digest ⇒ axda.lock
                                    │    └─ referrers API          │
                                    │                              ├─ trust check (ADR-004)
                                    └─ attach SLSA provenance      │    (digest, identity, caps)
                                                                   │    delta ⇒ re-prompt
                                        air-gap:                   │
                                        save → tar → load          └─ evaluate
```

## Consequences

### Benefits

- Bundles get immutable, content-addressed identity, so `axda.lock` pins an artifact rather than a mutable pointer.
- Signature verification gives a real answer to "who published this policy", and keyless binds it to a workflow rather than to whoever holds a key.
- Capability escalation cannot hide inside a version bump — it changes the digest and re-prompts.
- Registries bring auth, replication, quotas, GC, and scanning that we would otherwise have to build or do without.
- Separate plugin layers make WASM inventory a registry query, and let a security team see what is running.
- Annotations make capability inspection possible before download, which is what makes consent informed.
- Publishing runs the bundle's own fixtures, so untested policy cannot enter the shared distribution channel.
- Air-gapped and mirrored deployment become standard registry operations.

### Trade-offs

- **A second resolver is a second code path** for auth, caching, error handling, and offline behaviour, and both must stay working. Accepted: git's zero-friction on-ramp is worth more than the maintenance cost of keeping it.
- **Publishing friction is real.** `git push` versus build-push-sign is a meaningful difference in iteration speed, and it is exactly why git remains supported rather than deprecated.
- **Sigstore is an external dependency** — a transparency log and an OIDC issuer that can be down or unreachable. Key-based signing is the fallback; offline verification of previously-verified digests works from the lock.
- **Keyless signing needs OIDC**, which excludes some CI systems and most laptops. Those environments use keys and inherit key management.
- **Registries are not designed for tiny artifacts.** A 40 KiB bundle in infrastructure built for multi-gigabyte images works, but pull latency is dominated by round trips rather than bytes.
- **`--skip-tests` exists**, so the §6 guarantee is a strong default rather than an invariant. Removing it would push people to hand-craft artifacts with `oras`, which is worse.

### Out of scope

- A public bundle registry, index, or discovery service. This ADR specifies a format and a protocol, not a marketplace.
- Bundle-to-bundle dependencies. Bundles are self-contained; composition is out of scope in [ADR-003](003-contract-lowering.md) too.
- Encrypted bundles or confidential policy content — a policy readable by the tool is readable by its operator.
- Vulnerability scanning of WASM plugin layers; the media type makes it possible, the scanners do not exist.
- Automatic bundle updates or a renovate-style bot.
- Delta or partial-layer pulls.

## Verification

- Round trip: `axda bundle push` then `axda evaluate --policy oci://…` produces a report byte-identical to the same bundle resolved from `file://`.
- Resolving the same bundle by git and by OCI produces identical plan hashes.
- An unsigned bundle under a `require_signature: true` pattern exits `2` and reads no trace.
- A bundle signed by an identity not matching `certificate_identity` exits `2`, with the expected and actual identities both in the error.
- Re-tagging `v1.2.0` to different content and re-running with `--frozen` exits `2` on the digest mismatch.
- Adding a capability and republishing re-prompts for trust; republishing with only policy-text changes does not.
- Changing a plugin binary without touching policy text changes the manifest digest.
- `axda policy inspect oci://…` reports the capability set without pulling any layer (asserted by registry request count).
- `axda bundle push` on a bundle with a failing fixture aborts before pushing anything.
- `save` → transfer → `load` preserves digest and signature, and `load` rejects a tarball whose signature does not verify.
- A pushed artifact is pullable with `oras pull` and inspectable with `crane manifest`.

## References

- [ADR-001](001-agent-admission-control.md) — the `Resolver` interface, git resolution, `axda.lock`, the testdata requirement
- [ADR-003](003-contract-lowering.md) — clause namespacing carried in annotations
- [ADR-004](004-wasm-plugin-abi.md) — capability grants and the trust record this ADR binds to a digest and identity
- [ORAS / OCI artifacts](https://oras.land/docs/concepts/artifact/) — `artifactType`, config blob, arbitrary layers
- [Cosign: signing other types](https://docs.sigstore.dev/cosign/signing/other_types/) — signing non-image OCI artifacts
- [OPA bundles as OCI images](https://github.com/open-policy-agent/opa/issues/1413) — prior art for policy-as-OCI media types
- [SLSA provenance](https://slsa.dev/) — build attestation attached via the referrers API
