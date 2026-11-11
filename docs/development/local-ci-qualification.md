# Qualify local and generated CI execution

The local dogfood follows the real Go workflow through an initial local run,
separate generated build/environment/test-matrix/package jobs, and a final
ordinary local run. Coopr executes the containers and owns registry-cache reuse.
The test registry and every native store belong to this qualification run.

This procedure is development QA on an already provisioned rootless machine.
An optional disposable VM separates guest users and stores; Stoke does not
provision runners. This container-workflow qualification
does not exercise generic [host tasks](../guides/host.md) or Nix flake checks as
workflow stages. Use Coopr `0.6.0` for this local procedure. Hosted execution
remains a separate prepared test. See
[CI generation](../guides/ci-generation.md) and
[hosted qualification](hosted-ci-qualification.md).

**Qualification snapshot, 2026-11-07:** the full actual-checkout fixture passed
in 22m47s on a Linux host with rootless VFS and explicit slirp4netns. It completed the
initial local run, five visible generated CI execution jobs, the final ordinary
local run and the separate fresh-timestamp checkout probe. The equivalent-input
CI/local-return Go build RUNs restored from registry cache; test/vet steps ran
fresh, and the tested and packaged producer binary hashes matched. The timestamp
probe also restored the build RUN in this case; that is not a guarantee for
arbitrary metadata changes.

This proof used one host and UID with independent checkout paths, native stores
and local caches. Native CI admission was exercised by the local provider loop,
not GitHub Actions. Distinct-host/user, native ARM, hosted/GHCR and Pasta
qualification remain separate. Earlier `ENOSPC`, setup and failed-test attempts
are retained as historical failures; they are not substituted for this result.

## Fixture contract

`internal/artifact/ci_real_workflow_runtime_test.go` drives the actual
`examples/ci/stoke.yaml`, including its native Go build and fresh test/vet matrix.
The example prints the SHA256 of `/built/stoke` inside each check. Its existing
steps, gates, mounts and cache policy remain in effect.

`internal/artifact/ci_real_workflow_helpers_test.go` supplies the optional user
switching, complete-checkout manifests and binary verification. The shared
command factory in `ci_registry_runtime_test.go` and
`ci_generated_registry_runtime_test.go` preserves ordinary fixture behavior when
these options are absent.

| Environment | Meaning |
| --- | --- |
| `STOKE_BINARY` | Absolute reviewed Stoke development executable. |
| `STOKE_COOPR_BINARY` | Absolute Coopr `0.6.0` executable with its complete package. |
| `STOKE_SMOKE_BUSYBOX` | Absolute static BusyBox required by the shared registry fixture. The Stoke development shell supplies this. |
| `STOKE_REAL_CI_SOURCE_DIR` | Complete actual checkout to copy; not a reduced compilation fixture. |
| `STOKE_CACHE_CI_EVIDENCE_DIR` | Dedicated absolute evidence parent on adequate disk-backed storage. |
| `STOKE_REAL_CI_STORAGE_DRIVER` | Optional explicit `overlay` or `vfs` for runtime CLI calls and scoped fixture cleanup. It does not change product defaults. |
| `STOKE_REAL_CI_NETWORK` | Optional explicit Coopr network selection, also recorded in the generated plan; no automatic backend fallback. |
| `STOKE_REAL_CI_PRUNE_NATIVE` | Optional `1` to retire successful phases' new native stores after assertions/publication. Requires an explicit driver and non-root single-user execution; exports, registry cache and evidence remain. |
| `STOKE_REAL_CI_USERS` | Optional JSON map naming three existing guest users: `local`, `ci`, `return`. Requires a disposable guest root coordinator. |
| `STOKE_REAL_CI_SCRATCH_TMP` | Absolute short temporary-directory parent required with multiple users; each user gets a private UID subdirectory. |

Without `STOKE_REAL_CI_USERS`, all commands retain the invoking user's identity.
On an already provisioned rootless machine with adequate storage, run from the
Stoke checkout:

```sh
STOKE_BINARY="$PWD/result-stoke/bin/stoke" \
STOKE_COOPR_BINARY=/absolute/path/to/reviewed/coopr \
STOKE_REAL_CI_SOURCE_DIR="$PWD" \
STOKE_CACHE_CI_EVIDENCE_DIR=/absolute/disk-backed/evidence \
STOKE_REAL_CI_STORAGE_DRIVER=vfs \
STOKE_REAL_CI_NETWORK=slirp4netns \
STOKE_REAL_CI_PRUNE_NATIVE=1 \
TMPDIR=/absolute/disk-backed/tmp GOTMPDIR=/absolute/disk-backed/tmp \
  nix develop --no-update-lock-file path:. -c \
  go test -count=1 -timeout=75m -v ./internal/artifact \
    -run '^TestCIRealWorkflowRegistryWithCoopr$'
```

Choose a supported storage driver and existing private paths. This command does
not create a filesystem, adjust quotas or install a runtime. The fixture's inner
context is bounded to 60 minutes; the outer Go timeout leaves time for cleanup.
A failure remains a failure rather than authorizing looser cache assertions.
Scoped cleanup uses a private storage configuration; it cannot inherit a system
image store. It does not prune prior runs, failed phases or user stores. This
example's explicit slirp4netns choice does not qualify Pasta.

## Isolated scratch and distinct users

The retained VM recipe uses KVM, a 128 GiB logical ext4 disk, 16 GiB RAM and four
virtual CPUs. It selects existing overlay execution explicitly. Native stores,
portable caches, source copies and temporary payloads live under guest
`/scratch`; the guest runtime directory remains `/run/user/UID`.

Only the guest test coordinator runs as root. Runtime Stoke CLI children use
explicit kernel credentials for `local` UID 1000, `ci` UID 1001 and `return` UID
1002. Each has a separate HOME, XDG directories, native store and subordinate-ID
range. The coordinator records an actual `id -u` probe for each runtime command, prepares
its private directories and native-output file, and sets that user's runtime
directory and D-Bus address. Generation and scope creation are orchestration
commands; they do not execute containers.

Provision these users and their delegated, lingering user managers inside the
disposable guest before starting the fixture. The guest's helper search path must
include:

```text
/run/wrappers/bin:/run/current-system/sw/bin:/bin
```

In particular, `/run/wrappers/bin/newuidmap` and `newgidmap` are needed for
subordinate-ID mappings. A root-owned transient systemd service may otherwise
inherit a path containing only the unprivileged utilities. A small single-UID
probe can succeed before the full Go image exercises those mappings; inspect the
actual service environment and helper path as part of preflight.

A regular development invocation does not request credential switching. The
root coordinator mode is restricted to the explicitly configured QA VM; it is
not a product host-execution or privilege interface.

## Preserve the source comparison

Freeze and manifest the complete actual checkout before the run. The VM receives
that archive without a manually filtered test tree. The test makes a complete
copy for the initial local run, each generated job, and the final local return;
regular-file bytes are compared while copying. Preserve the original archive
and its metadata manifest, including any uncommitted work.

The multiple-user baseline intentionally normalizes only its owned copies to a
root-owned, readable, read-only tree. That gives the three runtime users the same
declared source bytes and metadata while their executing identities and native
stores differ. It does not modify the developer checkout. Initial metadata and
the normalization choice are recorded in the source-copy manifests.

A separate complete copy refreshes regular-file and directory timestamps and
runs the native build. Its cache hit or conservative miss is recorded separately;
it never replaces the main roundtrip's strict cache-hit assertion. This is a
physical checkout-copy and timestamp probe, not an observed `actions/checkout`
run or a claim that arbitrary ownership/mode changes are equivalent inputs.
The authored base image references are digest-pinned. Arbitrary mutable image
tags and undeclared network/time inputs remain outside a run-wide resolution
lock. Equivalent-input fixtures preserve source metadata as well as bytes;
whole-context metadata changes can legitimately cause conservative misses.

## Assertions and evidence

The large test requires all of the following:

- The equivalent-input CI build and final local build report cache restoration
  for the authored Go build RUN itself.
- Generated jobs retain their visible stages and unique native matrix outputs.
  The local provider loop checks rendered `needs`, starts the two check variants
  concurrently and drains them before admitting a dependent group.
- Every CI job carries the same immutable `build` execution binding. Consumers
  do not replay the authored Go producer.
- The canonical producer OCI `/stoke`, both mounted `/built/stoke` observations,
  and package `/dist/stoke` hashes match. Package binaries also execute.
- Test/vet tasks pass with zero Coopr cache hits; `go test -count=1` output proves
  fresh tests executed rather than a stored result being reported as new work.

These are local admission and data-integrity assertions. Actual GitHub scheduling,
masked matrix outputs and partial-rerun metadata availability remain hosted
qualification items. Developer logs and reports are evidence, not internal
success receipts or mandatory CI artifacts.

The fixture retains per-command stdout/stderr, native output files, persisted
`run.json` reports, source manifests, execution identities and
`real-workflow-proof.json`. Inspect the final summary and every phase; a passing
CI section does not turn a failed final local return into a passed roundtrip.

## Reproduce the retained VM case safely

The historical VM case used a reviewed external QA recipe. Its retained files
had the following responsibilities. Prepare and review equivalent files for a
new run.

| File | Responsibility |
| --- | --- |
| `vm-runner.nix` | Pinned Nixpkgs test driver, guest users, ext4 capacity, runtime helpers and bounded test script. |
| `guest-runner.sh` | Explicit guest PATH and fixture environment; records the Go test exit status. |
| `run-vm.py` | Launches the already built driver outside the Nix build sandbox and monitors only its owned scratch disk. |
| `collect-guest-evidence.py` | Collects bounded logs/manifests and explicitly referenced reports without walking native stores. |
| `actual-checkout.tar` / `actual-checkout-manifest.json` | Complete frozen source and recorded byte/metadata identities. |
| `artifact.test` | Precompiled artifact-package test executable, bound to its source and hash. |

Build the Go test executable from the actual checkout with
`go test -c -o /absolute/evidence/artifact.test ./internal/artifact`. Build only the
VM recipe's returned driver with `nix build --impure --file vm-runner.nix`; launching
that driver is a separate local QA operation. Preserve its development package
closures and executable/source hashes before starting it.

For a new collection, use a new owner-only host state directory, set its Btrfs
NOCOW attribute before creating disk files, and create an ownership record naming
the current UID and exact evidence directory. The record uses these fields;
replace the UID and directory for the new collection:

```json
{
  "purpose": "isolated Stoke qualification scratch",
  "evidence": "/absolute/path/to/new/evidence",
  "uid": 1000,
  "maxAllocatedBytes": 48318382080,
  "minHostFreeBytes": 37580963840
}
```

Set both host `XDG_RUNTIME_DIR` and
`TMPDIR` to that private disk-backed state: this driver checks the runtime
directory first, so changing only `TMPDIR` can still leave the disk on tmpfs.
Review and freeze the copied recipe's case-specific paths and inputs before
launch. Do not point a new run at an existing user's VM or overwrite old evidence.

The current supervisor permits at most 45 GiB of allocated backing-file blocks
and requires at least 35 GiB host free data space. The 128 GiB guest disk is
sparse; its logical size is not a host allocation promise. The supervisor checks
those bounds every ten seconds and interrupts only its owned driver if crossed.
If that driver does not drain within 60 seconds, it records a blocker requiring
exact child-identity inspection; it does not broadly kill QEMU processes. These
bounds do not replace filesystem metadata and user-quota checks.

The driver retains machine state. Preserve and verify the curated evidence before
removing only the collection's proven disposable scratch. Full native stores and
portable caches stay on that owned VM disk and are excluded from the collector's
bounded log archive. No host balance, quota, mount, user or storage-driver setting
is changed, and no external registry publication or hosted workflow is run by
this procedure.
