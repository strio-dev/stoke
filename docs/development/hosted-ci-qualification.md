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

The first online cold build in [run 43084980426](https://github.com/strio-dev/stoke/actions/runs/43084980426)
failed before Go execution because Pasta could not open `/dev/net/tun` in the
outer container. The approved continuation appends only
`--device=/dev/net/tun:/dev/net/tun:rw` to the three options above. It preserves
private RUN networking, without added capabilities or privileged mode. Device
exposure is a separate approval from the earlier offline test.
[Run 43089151406](https://github.com/strio-dev/stoke/actions/runs/43089151406)
then completed Go dependency downloads and the build, before cache publication
failed with GHCR `MANIFEST_BLOB_UNKNOWN`. That establishes online RUN networking,
not cache reuse or a successful workflow. Coopr [PR #24](https://github.com/strio-dev/coopr/pull/24)
repairs the cache publication path. Qualification must use images containing that
fix; the previously published images do not. Do not substitute host networking
on failure.

## Native caller and batch bounds

The preparation uses three AMD64 and two ARM64 generator-owned plan/workflow pairs:

| Case | Plan | Generated workflow |
| --- | --- | --- |
| Cold | `.stoke/hosted-cold.json` | `.github/workflows/hosted-cold.generated.yml` |
| Warm | `.stoke/hosted-warm.json` | `.github/workflows/hosted-warm.generated.yml` |
| Failure | `.stoke/hosted-failure.json` | `.github/workflows/hosted-failure.generated.yml` |
| ARM cold | `.stoke/hosted-arm-cold.json` | `.github/workflows/hosted-arm-cold.generated.yml` |
| ARM warm | `.stoke/hosted-arm-warm.json` | `.github/workflows/hosted-arm-warm.generated.yml` |

The reusable native caller, `.github/workflows/stoke-dogfood.yml`, prepares a
`roundtrip` path with one cold invocation followed by two concurrent warm
invocations, and a `failure` path using a separate tiny public expected-failing
fixture. The real workflow's test/vet matrix remains fresh. Each invocation
gets its own scope, and each job gets independent native/cache/output stores.
The generic example and host-workflow pairs remain separate.

The registered `.github/workflows/stoke-runtime.yml` admits these cases only
through manual dispatch on `strio-dev/stoke`'s `initial` branch. `prepare-images`
selects the separate image-preparation caller; other scenarios go through the
dogfood caller, whose validation rejects unknown names before admitting tasks.
Push branch changes with `[skip ci]` during qualification to avoid starting the
unrelated push-triggered native runtime matrix.

The original batch ceiling remains 54 container-job starts, including eight
already consumed by the two failed paths. Its deadline remains
**2026-11-11 23:03:49 UTC**. The remaining core plan reserves 22 starts for AMD64
cold and two concurrent warm invocations, 14 for ARM cold/warm, and seven for the
intentional failure: at most 43 new starts and 51 cumulative. It retains two
concurrent workflows, matrix parallelism two and 45-minute job limits.

Selective retries are omitted from this reduced plan. Three nominally spare
slots do not safely cover provider rerun fanout. A future rerun requires reserving
its whole original workflow's possible fanout and dropping other cases if needed;
never reset the counter or deadline. The failure case must fail one fresh matrix
variant and skip package. Count actual job starts across every attempt and stop
further admission on unexpected failure or fanout rather than silently retrying.

Use the existing [tools-container generator](../guides/ci-generation.md#opt-in-tools-containers)
with `--runner ubuntu-24.04`, the new candidate digest, the three security
options plus the approved TUN mapping, and `--github-token-auth`. These settings cover generated setup, execution and
final-gate jobs. The caller's validation job uses the same image with default
container security options and does not execute nested tasks. Keep the older
digests only as historical evidence. `prepare-images` builds matching native
AMD64 and ARM64 images with the existing recipe, public Stoke source and Coopr
commit `2f371c53c4f406c705f2311088dfd213968a9c04`. Its two normal hosted jobs
use no relaxed container profile and disable both the installer's KVM setup
and optional runner trust grant. They publish only to the existing private tools
package, under distinct `tools-ARCH-SOURCE-RUN-ATTEMPT` tags. Record each source
and returned manifest digest, then regenerate all five caller pairs before
runtime qualification. Candidate preparation
[run 43098487812](https://github.com/strio-dev/stoke/actions/runs/43098487812)
passed on both native architectures from public Stoke commit
`943bc2f63ca9f1024d714c1524e2d11eb9ea624a`. The candidate image digests are:

| Native architecture | Manifest digest |
| --- | --- |
| AMD64 | `sha256:f5e75f8840b97176502ffd45542026072bddad705bc88c9285b51acc789f67f0` |
| ARM64 | `sha256:05413ec4b4845db9983b059c34785ccb321ad78c8ac04fb9ef891cb097befa96` |

Both reside in the existing private `stoke-qualification-tools` package.
Image publication establishes packaging, not a successful runtime roundtrip.
Historical ARM preparation
[run 43083643695](https://github.com/strio-dev/stoke/actions/runs/43083643695)
published this verified ARM64/UID1001 image from source `6e1e651d2c337ea8b2c346907fa89f88ad85500d`:

```text
ghcr.io/strio-dev/stoke-qualification-tools@sha256:dc7e3f11e097922e8ba5b5f9bb14e9853f5ba0ad12590622d3e1140657aec71d
```

That historical ARM image contains released Coopr, so it must be replaced for
the cache-fix qualification. A runtime device mapping alone requires no image
rebuild; changing the engine source does.
Do not reuse the AMD64 digest as an ARM image or create a new package or multiarch
index. ARM runtime success remains separate from image publication.

Validate workflow files from the repository root with their actual context:

```sh
stoke check -f examples/ci/stoke.yaml --context .
stoke check -f examples/ci/failure.yaml --context .
```

The caller contains the complete `stoke ci generate --check` commands for all
five pairs, also with `--context .`. Omitting that option selects the workflow
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

The execution callers grant `contents: read` and `packages: write`; the
validation job retains `packages: read`. These are short-lived GitHub job tokens,
not new PATs or persistent scope changes. The caller owns package access and
any approved `packages: write` permission;
the generator grants neither access nor authorization to publish. Package
visibility remains a GitHub setting, independent of image labels and token
scopes. Verify that the existing package is accessible and private before writes;
missing or public metadata is not permission to bootstrap a package. Keep
credentials out of plans, URLs, command arguments and logs. Missing
or masked native outputs must fail clearly. See [GHCR authentication](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry#authenticating-to-the-container-registry).

## Hosted cases to record

| Case | Required evidence |
| --- | --- |
| Cold run | Visible build, environment, test/vet matrix and package jobs; native `needs` gates; exact producer/input-set references; fresh checks actually execute. |
| Warm run | Independent stores and matching inputs restore build cache; tests remain fresh; package uses the exact tested producer. |
| Failed test | The native matrix/group fails and blocks package; existing result objects do not assert success. |
| Consumer-only retry | Reuse an explicitly supplied prior-attempt reference in the same scope, or fail clearly if GitHub omits it. Omitted from this reduced batch. |
| Partial matrix retry | Preserve every unique variant output; missing/masked metadata fails without scanning registry tags. Omitted from this reduced batch. |
| Producer rerun | New execution bindings conflict with old tests, even for identical bytes; deliberately rerun dependent gates. Omitted from this continuation's hosted budget. |
| Concurrent invocations | Separate invocation scopes; no mutable latest/winner lookup; no output-key collisions. |
| Missing result | A cache miss may execute a producer before gates; a missing exact consumer result must not replay it after tests. |
| Retention | Selected exact references remain readable throughout retries; no objects are deleted by this batch. |

These are acceptance requirements, not results of generator checks or short
tests. The cold/warm/failure paths do not by themselves qualify selective
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
Dispatches and reruns require approval for their complete bounded batch.

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
