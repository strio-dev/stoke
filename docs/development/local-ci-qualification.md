# Qualify local workflow cache reuse

`TestCIRealWorkflowRegistryWithCoopr` runs the actual
`examples/ci/stoke.yaml` through ordinary local invocations. It performs an
initial build, fresh tests and packaging, a return run from an independent
checkout/store using registry cache, and a separate fresh-timestamp build probe.
Coopr owns container execution and cache reuse. The fixture owns its local
registry, source copies, native stores and evidence.

This test does not invoke generated stage exports or simulate a CI provider.
Qualify those commands separately using the
[stage qualification checklist](registry-portability.md).
Historical generated-provider results apply only to their recorded source and
tools; they are not assertions made by the current fixture.

## Run the fixture

Use an already provisioned rootless Linux host with adequate disk space and
source-matched Stoke and Coopr executables. No runner is provisioned by this
test. From the Stoke checkout:

```sh
STOKE_BINARY=/absolute/path/to/reviewed/stoke \
STOKE_COOPR_BINARY=/absolute/path/to/reviewed/coopr \
STOKE_SMOKE_BUSYBOX=/absolute/path/to/static/busybox \
STOKE_REAL_CI_SOURCE_DIR="$PWD" \
STOKE_CACHE_CI_EVIDENCE_DIR=/absolute/disk-backed/evidence \
  nix develop --no-update-lock-file path:. -c \
  go test -count=1 -timeout=75m -v ./internal/artifact \
    -run '^TestCIRealWorkflowRegistryWithCoopr$'
```

The fixture needs ordinary access to the authored image/module registries.
It writes only to its disposable local cache registry. Its inner context is
bounded to 60 minutes; the outer Go timeout leaves time for cleanup.

| Optional environment | Meaning |
| --- | --- |
| `STOKE_REAL_CI_STORAGE_DRIVER` | Explicit `overlay` or `vfs` for runtime calls and scoped fixture cleanup. |
| `STOKE_REAL_CI_NETWORK` | Explicit Coopr RUN network setting passed to every runtime phase through normal CLI validation. Empty retains the product default. |
| `STOKE_REAL_CI_PRUNE_NATIVE` | `1` prunes only successful phases' newly owned native stores after output assertions. Requires an explicit driver and non-root single-user execution. Sources, exports, registry cache and evidence remain. |
| `STOKE_REAL_CI_USERS` | Optional JSON map naming already provisioned disposable guest users for `local`, `ci` and `return`. Requires a guest root coordinator. |
| `STOKE_REAL_CI_SCRATCH_TMP` | Absolute short temporary-directory parent required with multiple users. Each user receives a private UID directory. |

The optional guest mode selects explicit kernel identities for runtime children
and records actual `id -u` probes. It does not create guest users, mapping ranges
or delegated services. Review the helper's preconditions before using it.
A single-user invocation preserves the calling user's execution identity.

## Assertions and evidence

The fixture checks:

- The actual producer binary, both mounted check observations and packaged
  binary have matching hashes. The mounted binary validates the real workflow
  and rejects a deliberately invalid workflow.
- Test/vet tasks execute fresh, with zero Coopr result hits. The source tests
  use `go test -count=1`.
- The equivalent-input return run restores the authored Go build RUN itself.
  COPY hits or matching binary bytes alone do not satisfy this assertion.
- A complete checkout with changed timestamps records its own cache
  observation. It does not substitute for the equivalent-input assertion.

Source-copy manifests preserve regular-file bytes and record any fixture-owned
metadata normalization. The fixture compares bytes of its selected actual
inputs and keeps image sources digest-pinned. Undeclared time/network inputs
and arbitrary metadata changes are not promised equivalent.

Each phase retains stdout/stderr, a JSON report and its persisted `run.json`.
The shared runner compares emitted and persisted reports. The fixture also
retains source manifests, execution identities and `real-workflow-proof.json`.
Inspect every phase; failure in the final return or probe fails the test.

These are local cache and output assertions. They do not establish immutable
per-attempt bindings, provider success gating, generated-stage execution,
hosted retry behavior, default Pasta networking, cross-host reuse or native ARM
execution. See [hosted qualification](hosted-ci-qualification.md) for separately
recorded historical runs.
