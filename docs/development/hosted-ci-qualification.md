# Hosted CI qualification

Hosted qualification uses prebuilt tools images, native GitHub jobs and per-job
token authentication. Ordinary `.github/workflows/ci.yml` runs flake checks.
Generator checks, local fixtures and short tests remain separate from actual
hosted execution and GHCR behavior.

## Verified hosted results

The full workflows completed successfully on both native architectures:

| Architecture | Frozen application checkout | Workflow run |
| --- | --- | --- |
| ARM64 | `655367d4fb9278760b6e03af3c23be307ebb332e` | [43119419077](https://github.com/strio-dev/stoke/actions/runs/43119419077) |
| AMD64 | `ff72aaf40ebd5d099878621ee96f6fe96e247d15` | [43119897878](https://github.com/strio-dev/stoke/actions/runs/43119897878) |

These runs used Coopr `9e0866d7760def6b1358f7034540d6c243cb8017` and these
private `ghcr.io/strio-dev/stoke-qualification-tools` image manifests:

| Architecture | Tools image digest |
| --- | --- |
| AMD64 | `sha256:2810897b2a4acb94f7b8b76fabc039ff9467816751be95d77b2e758936acda27` |
| ARM64 | `sha256:7798424beb2fa42954c6d6c9fd94d0ea6e3cd24cb828dc2b5ec4f9c62dcf74a0` |

Build, environment, fresh test/vet matrix, package and native final gates passed.
The checks actually executed `go test -count=1 -mod=readonly -short ./...` and
`go vet -mod=readonly ./...`. Their standalone binary hashes matched the exact
producer, as did the cold package's binary hash. Each job used independent
native/cache/output stores and exact GHCR result/InputSet bindings.

| Architecture | Canonical build output | Package output |
| --- | --- | --- |
| AMD64 | `sha256:c6e081670afedd5f4962d50608259e5611362f3f4c5eaadccbb415868e4ed778` | `sha256:37bb0b61a65b2196b507a3140dc0bb84e8197fa8c7eb9004d82b22f7cc2bd183` |
| ARM64 | `sha256:eda469715488418328cb49938a35fda3b600219e589053413c64d0cd250e66e4` | `sha256:4be75ad4acac870a19fbb8b3775607e017fab126b412795a1c335d1c89908c0f` |

Two concurrent AMD64 warm invocations retained distinct scopes and native
outputs; the ARM64 warm invocation also retained its own scope. Fresh checks
passed against each invocation's exact producer. Warm package COPY and RUN
checkpoints hit the registry cache and restored the cold package digest, so their
hash-printing RUNs did not execute again. The Go-build RUN missed in all three
warm invocations; those runs do not establish Go-build reuse.

The separate public negative fixture in
[run 43124022353](https://github.com/strio-dev/stoke/actions/runs/43124022353)
executed both variants freshly. The pass variant printed `STOKE_GATE_PASSED`;
the fail variant printed `STOKE_EXPECTED_TEST_FAILURE` and exited 23. Package
was skipped and the native final gate received `false`. This is the expected
negative outcome. Auth files, job containers and networks were cleaned up on
both success and failure; registry objects were retained.

## Pending Go-build reuse probe

Coopr [PR #24](https://github.com/strio-dev/coopr/pull/24), at
`e88a2107ba9ad133ee7e7b9318dd436dcd8f2ac4`, repairs shared-prefix metadata
association between independent cache producers. The fix has local regression
evidence. Matched image preparation
[run 43130605718](https://github.com/strio-dev/stoke/actions/runs/43130605718),
from Stoke `483e8e939bfd4d4df3ec18a43f36626106591395`, passed on both architectures:

| Architecture | Matched tools image digest |
| --- | --- |
| AMD64 | `sha256:10e9bd7f43f1a54c87277416ca0fcc9b78a2a6fdda250552f9285c658e4c5df7` |
| ARM64 | `sha256:03acbebd4417e0bd0eec87fccb7720b3a4dd9342e94c1a5a6ae7c4c5a9a1b99d` |

Both image receipts confirm UID/GID1001 and the native system. These image source
pins differ from the original application checkouts; changing the tools must not
change the input bytes. The final hosted reuse results remain pending.

The final `cache-reuse` matrix reserves two containers, one per native
architecture. It runs only `stoke run ... build` with `--cache-from` and
read-only package access. Each job checks out the complete original application
commit from the table above and verifies the checkout before and after execution.
Auth files, engine state, caches, logs and outputs stay outside that context.

Success requires a positive `Using cache` receipt for the actual Go-build RUN,
absence of a standalone `STOKE_BUILD_EXECUTED` marker, and a passed build report
whose `tasks[].digest` matches the original canonical build output above.
A COPY hit, echoed command or identical binary bytes alone is insufficient.
This probe does not rerun the complete DAG with the new engine. No final hosted
Go-build cache hit is claimed until both results are recorded.

## Native caller and batch bounds

The generator owns these five plan/workflow pairs:

| Case | Plan | Generated workflow |
| --- | --- | --- |
| Cold | `.stoke/hosted-cold.json` | `.github/workflows/hosted-cold.generated.yml` |
| Warm | `.stoke/hosted-warm.json` | `.github/workflows/hosted-warm.generated.yml` |
| Failure | `.stoke/hosted-failure.json` | `.github/workflows/hosted-failure.generated.yml` |
| ARM cold | `.stoke/hosted-arm-cold.json` | `.github/workflows/hosted-arm-cold.generated.yml` |
| ARM warm | `.stoke/hosted-arm-warm.json` | `.github/workflows/hosted-arm-warm.generated.yml` |

The reusable `.github/workflows/stoke-dogfood.yml` caller runs a cold invocation
followed by two concurrent AMD64 warm invocations, an ARM cold/warm path, or the
separate expected-failure fixture. Each invocation owns its scope. Native
producer, matrix and package dependencies remain visible; the final status gate
uses native conditions on the host without a container or tool provisioning.
The generic example and host-workflow pairs remain separate.

Manual dispatch through `.github/workflows/stoke-runtime.yml` is restricted to
`strio-dev/stoke`'s `initial` branch. `prepare-images` selects image preparation;
`roundtrip`, `arm-roundtrip` and `failure` select the dogfood caller;
`cache-reuse` selects the final build-only matrix. Unknown direct reusable-call
strings select no jobs. Use `[skip ci]` on qualification branch updates to avoid
the unrelated push-triggered runtime matrix.

The ceiling remains **54 container-job starts: 52 charged, two remaining**.
The final matrix reserves those two starts; no additional dispatch or selective
rerun is authorized. Keep at most two concurrent workflows and matrix parallelism
two. Qualification runtime jobs and both native image-preparation jobs have
45-minute limits; these do not bound queued time. The earlier drain time and
absolute deadline are superseded. Preserve actual counts, report omitted cases,
and stop on unexpected failure.

## Generate and validate callers

Validate the workflows from the repository root:

```sh
stoke check -f examples/ci/stoke.yaml --context .
stoke check -f examples/ci/failure.yaml --context .
```

Run `stoke ci generate --check` for every pair using its reviewed image digest,
native runner (`ubuntu-24.04` or `ubuntu-24.04-arm`), 45-minute timeout, approved
container options and `--github-token-auth`. Keep `--context .`: omitting it
selects the workflow directory and misses repository-root inputs. Regeneration
is checked locally; no duplicate hosted validator executes the workflow.
See the [tools-container generator](../guides/ci-generation.md#opt-in-tools-containers).

Cache directions are literal plan configuration. Cold exports without
`--cache-from`; warm and failure include cache import. Exact-result storage is
configured separately. Coopr's `instruction-`, `package-` and `graph-v2-` tags
are distinct from Stoke's `sci-`, `scip-` and `stoke-host-v1-` tags, so the
authorized repository can hold both. The final reuse probe imports only.
Keep exact references throughout the retry window; this batch performs no
registry deletion. Internal handoff uses OCI references and native job outputs
without CI artifact upload/download or a successful-status object.

## Container profile and authentication

Runtime qualification uses UID1001, VFS and exactly these container options:

```text
--security-opt seccomp=unconfined --security-opt apparmor=buildah --security-opt systempaths=unconfined --device=/dev/net/tun:/dev/net/tun:rw
```

The profile uses the runner's existing `buildah` AppArmor policy and preserves
private RUN networking. It adds no privileged mode, capabilities, FUSE device,
host networking or host policy changes. GitHub automatically mounts its Docker
socket and runner workspace into the job container; that exposure remains part
of the qualified profile.

Image preparation uses two normal hosted jobs, the existing recipe and explicit
source pins. It disables the installer's KVM setup and optional runner trust
grant, and publishes distinct `tools-ARCH-SOURCE-RUN-ATTEMPT` tags only in the
existing private tools package. Preparation establishes packaging; runtime
qualification requires its own receipts.

The package `stoke-qualification-tools`, ID `16790989`, has been reverified
private. Check its identity, visibility and caller access before writes; missing
or public metadata is not permission to create a package. No new package or
multiarch index is part of this batch.

`--github-token-auth` authenticates each generated execution job independently
through Coopr login and a private auth file. It supplies result transport,
registry-cache access and credentials for the private tools-image pull.
Execution/preparation jobs use `contents: read` and `packages: write`; the final
reuse probe uses `contents: read` and `packages: read`. The host final gate
requires no package authentication. The caller owns permissions and package
access; the generator grants neither. These are short-lived job tokens, with
no PAT or persistent scope change. Keep credentials out of plans, URLs, command
arguments and logs, and remove auth files during unconditional cleanup.
See [GHCR authentication](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry#authenticating-to-the-container-registry).

## Earlier failure evidence

Earlier failures remain evidence for the repairs and profile:

- [43084980426](https://github.com/strio-dev/stoke/actions/runs/43084980426)
  failed when Pasta could not open `/dev/net/tun`; the approved TUN mapping
  enabled subsequent online RUNs.
- [43089151406](https://github.com/strio-dev/stoke/actions/runs/43089151406)
  reached Go execution but failed cache publication with
  `MANIFEST_BLOB_UNKNOWN`.
  [43101790380](https://github.com/strio-dev/stoke/actions/runs/43101790380)
  reached the old cache metadata replay bound.
- [43114525943](https://github.com/strio-dev/stoke/actions/runs/43114525943)
  failed Stoke's result-envelope publication after its ARM Go build.
  [43114866113](https://github.com/strio-dev/stoke/actions/runs/43114866113)
  was cancelled under the stop-on-unexpected-failure rule. These attempts predate
  the standard OCI result/InputSet index repair.

## Remaining qualification boundaries

Normal native matrix handoff is established by the completed full workflows.
Selective consumer-only or partial-matrix reruns, omitted-job output retention
across attempts, secret masking and producer-retry lineage conflicts remain
unqualified on GitHub. Generated unique output names and local fixtures do not
establish those provider retry behaviors. See [matrix outputs](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idoutputs)
and [reruns](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/re-run-workflows-and-jobs).

Missing exact-result handling remains a local-fixture proof. The hosted batch
did not remove results to test a missing consumer input, and did not qualify a
retry-retention window or GitHub package-version retention.

## GHCR cleanup qualification boundary

Stoke's generic `ci cleanup` verifies scope/type and requests deletion of one OCI
metadata index by digest. It does not discover safe candidates, enforce retry
windows, map GitHub package versions or garbage-collect shared blobs.
[OCI Distribution](https://github.com/opencontainers/distribution-spec/blob/v1.1.1/spec.md#content-management)
permits registries to disable deletion; deleting a manifest can invalidate all
tags referencing it. Local deletion fixtures do not establish GHCR behavior.
No remote deletion, package-administration or GC operation is prepared or
authorized. Future deletion, tag/version cleanup, restoration or blob collection
requires separate targets and authorization while preserving retained attempts
and shared content.
