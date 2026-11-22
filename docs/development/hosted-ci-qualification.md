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

## Go-build reuse with the repaired engine

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
change the input bytes.

The final `cache-reuse` matrix in
[run 43132580221](https://github.com/strio-dev/stoke/actions/runs/43132580221),
at workflow commit `f75b4848618b1f3f4096b0c4004b73bc7bc1ca00`, passed on both
architectures. It ran only `stoke run ... build` with `--cache-from` and read-only
package access. Each job checked out the complete original application commit
and verified its HEAD and clean tracked, untracked and ignored file state before
and after execution. Auth files, engine state, caches, logs and outputs stayed
outside that context. The application trees were:

| Architecture | Original application tree | Reuse job |
| --- | --- | --- |
| ARM64 | `6b6bae372eedef0ff9e94d42659631e69fe2396e` | [130900063936](https://github.com/strio-dev/stoke/actions/runs/43132580221/job/130900063936) |
| AMD64 | `ef3cfabb61ddce15016e3072d092a6515fd3ee78` | [130900064246](https://github.com/strio-dev/stoke/actions/runs/43132580221/job/130900064246) |

Both actual Go-build RUNs emitted `Using cache`, with no standalone
`STOKE_BUILD_EXECUTED` marker. Both reports passed with exactly one image build
on the expected platform; their `tasks[].digest` values matched the original
canonical build outputs above. Credential, container and network cleanup passed.
A COPY hit, echoed command or identical binary bytes alone was insufficient.
This establishes registry reuse by the repaired engine against the original
input bytes; it does not rerun the complete DAG with that engine.

## Historical qualification

The run links above identify the tested workflows and tools images.
Those runs used the earlier exact-result exchange. Current generated stages
invoke `stoke run --ci` and restore external image dependencies from Coopr's
ordinary cache. The historical results do not qualify this current path.

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

In those recorded workflows, the former `--github-token-auth` generator option
authenticated execution jobs through Coopr login and a private auth file.
Those jobs used it for result exchange, registry cache access and private
tools-image pulls. Current stage templates leave authentication to the native
caller.
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

The recorded hosted batch
did not remove results to test a missing consumer input, and did not qualify a
retry-retention window or GitHub package-version retention.
