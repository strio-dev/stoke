# Prepare a hosted CI qualification

Hosted qualification uses the prebuilt tools image, generated GitHub jobs,
native dependencies and per-job token authentication. Ordinary
`.github/workflows/ci.yml` runs flake checks only. Generator checks and short
tests establish local validation; they do not establish hosted cache reuse,
exact cross-job results or GitHub retry-output retention.

## Proven AMD64 boundary

[Run 43072582292](https://github.com/strio-dev/stoke/actions/runs/43072582292),
at commit `fc7be2d0b73ab9a1c2ec5facb93bcaa2a43eade6`, passed the narrow
`stoke-runtime.yml` container task on `ubuntu-24.04`. It pulled the private AMD64
tools image, ran as UID1001 with VFS, copied the packaged static Stoke binary
into a scratch image, executed its version command with task networking disabled
and cache reuse disabled, and verified exported binary bytes against the input.

The tools package was verified private on 2026-11-11. The existing AMD64 manifest
used by that run is:

```text
ghcr.io/strio-dev/stoke-qualification-tools@sha256:3d6da25e0dcdf669517843c4654b50e6bc3a688145e265523edf46bc8c99b6d7
```

Its exact container options were:

```text
--security-opt seccomp=unconfined --security-opt apparmor=buildah --security-opt systempaths=unconfined
```

That run proves this task under this profile. It does not qualify the real Go
workflow, GHCR cache/result writes, warm restoration, native job handoff,
retries, concurrent invocations or ARM. The profile uses the runner's existing
`buildah` AppArmor policy; it adds no privileged mode, capabilities, FUSE device
or host provisioning. GitHub still supplies its normal job-container mounts,
including the Docker socket and runner workspace.

## Dormant container-first preparation

The preparation uses three generator-owned plan/workflow pairs:

| Case | Plan | Generated workflow |
| --- | --- | --- |
| Cold | `.stoke/hosted-cold.json` | `.github/workflows/hosted-cold.generated.yml` |
| Warm | `.stoke/hosted-warm.json` | `.github/workflows/hosted-warm.generated.yml` |
| Failure | `.stoke/hosted-failure.json` | `.github/workflows/hosted-failure.generated.yml` |

The reusable native caller, `.github/workflows/stoke-dogfood.yml`, prepares a
`roundtrip` path with one cold invocation followed by two concurrent warm
invocations, and a `failure` path using a separate tiny public expected-failing
fixture. The real workflow's test/vet matrix remains fresh. Each invocation
gets its own scope, and each job gets independent native/cache/output stores.
The generic example and host-workflow pairs remain separate.

The `dogfood` entry job in the registered `.github/workflows/stoke-runtime.yml`
is held behind a hard-false condition. Batch execution and registry writes remain
disabled, and `packages: read` remains in place in both callers until a concrete
batch is approved. Preparing or validating these files does not authorize a
dispatch, rerun, package write, image publication or security-policy change.

Use the existing [tools-container generator](../guides/ci-generation.md#opt-in-tools-containers)
with `--runner ubuntu-24.04`, the digest and exact options above, and
`--github-token-auth`. These settings cover generated setup, execution and
final-gate jobs. The caller's validation job uses the same image with default
container security options and does not execute nested tasks. Reuse the AMD64
digest; `stoke-tools-prepare.yml` remains build-only. There is no ARM digest or
ARM plan pair yet. Native ARM image building and publication into the same
private tools package require authorization before preparing those pairs.

Validate workflow files from the repository root with their actual context:

```sh
stoke check -f examples/ci/stoke.yaml --context .
stoke check -f examples/ci/failure.yaml --context .
```

The caller contains the complete `stoke ci generate --check` commands for all
three pairs, also with `--context .`. Omitting that option selects the workflow
directory and does not validate the real example's repository-root inputs.

Cache directions are literal plan configuration. Configure cold cache export,
warm cache import and exact-result storage explicitly. They may share one
authorized private OCI repository: Coopr's `instruction-`, `package-` and
`graph-` tags differ from Stoke's `sci-`, `scip-` and `stoke-host-v1-` tags.
Keep exact-result objects through the retry window; disposable-cache retention
must not remove them. The prepared batch performs no deletion.

## Authentication and permissions

For an authorized GHCR batch, `--github-token-auth` uses each execution job's
short-lived token through Coopr login and passes its private auth file to result
transport and Coopr cache access. It also supplies credentials for the private
tools-image pull before steps start. Authentication in validation alone does not
authenticate other jobs.

The caller owns package access and any approved `packages: write` permission;
the generator grants neither access nor authorization to publish. Package
visibility remains a GitHub setting, independent of image labels and token
scopes. Keep credentials out of plans, URLs, command arguments and logs. Missing
or masked native outputs must fail clearly. See [GHCR authentication](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry#authenticating-to-the-container-registry).

## Hosted cases to record

| Case | Required evidence |
| --- | --- |
| Cold run | Visible build, environment, test/vet matrix and package jobs; native `needs` gates; exact producer/input-set references; fresh checks actually execute. |
| Warm run | Independent stores and matching inputs restore build cache; tests remain fresh; package uses the exact tested producer. |
| Failed test | The native matrix/group fails and blocks package; existing result objects do not assert success. |
| Consumer-only retry | Reuse an explicitly supplied prior-attempt reference in the same scope, or fail clearly if GitHub omits it. |
| Partial matrix retry | Preserve every unique variant output; missing/masked metadata fails without scanning registry tags. |
| Producer rerun | New execution bindings conflict with old tests, even for identical bytes; deliberately rerun dependent gates. |
| Concurrent invocations | Separate invocation scopes; no mutable latest/winner lookup; no output-key collisions. |
| Missing result | A cache miss may execute a producer before gates; a missing exact consumer result must not replay it after tests. |
| Retention | Selected exact references remain readable throughout retries; no objects are deleted by this batch. |

These are acceptance requirements, not results of generator checks or short
tests. The dormant cold/warm/failure paths do not by themselves qualify selective
reruns, missing-result handling, producer-rerun conflicts or ARM execution.

GitHub combines matrix outputs and warns that duplicate names can be overwritten
by the last finishing variant; generated output names are unique. This remains a
hosted qualification item, not merely a YAML-lint assertion. See [matrix job
outputs](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idoutputs).
GitHub documents that omitted jobs retain their previous-attempt outputs for
newly run jobs. Record actual variant output availability, secret masking and
lineage validation in the hosted test. Missing inputs still require a clear
failure and an explicit producer/dependent-gates rerun. See
[reruns](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/re-run-workflows-and-jobs).

Developer reports may be exported separately after review. No CI artifact
upload/download or successful-status object is required for internal data flow.
Dispatches and reruns remain held until their complete batch is approved.

## GHCR cleanup qualification boundary

Stoke's generic `ci cleanup` verifies scope/type and requests deletion of one OCI
metadata manifest by digest. It does not discover safe candidates, enforce a
retry window, map GitHub package versions or garbage-collect shared blobs.
The [OCI specification](https://github.com/opencontainers/distribution-spec/blob/v1.1.1/spec.md#content-management)
permits registries to disable deletion; deleting a manifest can invalidate every
tag referencing it. Local fixtures do not establish GHCR's behavior.

No GHCR cleanup job, remote deletion or package-administration operation is
prepared or authorized. Manifest deletion, version deletion, tag-only cleanup,
restoration and blob collection remain separate unqualified behaviors. Any
future cleanup requires its own reviewed targets and authorization while
preserving retained attempts and shared content.
