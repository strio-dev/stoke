# Visible CI jobs with exact OCI results

Stoke generates visible GitHub Actions jobs for the workflow's stages and native
matrix rows for their variants. GitHub owns scheduling and success/failure gates.
Coopr still owns container execution and its existing input-keyed registry cache.
Exact cross-job results travel through OCI, without CI artifact uploads/downloads
or success receipts.

| Object | Purpose | Missing object |
| --- | --- | --- |
| Coopr registry cache | Reuse work for matching tracked inputs | Coopr can execute a cache miss inside the producer job. |
| Immutable result | Preserve this producer execution's output | Consumer fails; it never rebuilds a replacement after tests. |
| Immutable input bindings | Identify the producer executions selected by a job | Missing/conflicting native metadata fails before execution. |
| Native CI `needs` | Require prerequisite jobs to succeed | GitHub blocks dependent work; stored objects do not assert success. |

## Generate and review

```sh
stoke ci generate -f examples/ci/stoke.yaml --context . \
  --cache-from registry.example.com/team/stoke-cache \
  --cache-to registry.example.com/team/stoke-cache \
  --results-repository registry.example.com/team/stoke-results package
```

Replace the example repositories with destinations you control. Cache reads and
writes remain independent and repeatable. Results have a separate purpose and
retention policy; a separate repository is recommended. Credentials are supplied
by the runner and are never written into the plan or generated workflow.

Use `--network none` during generation to record an explicit Coopr RUN network
default. It participates in the plan identity and every generated invocation;
`ci run` rejects a different override. This leaves explicit native RUN networking
and registry traffic separate. Empty selection preserves existing engine
defaults; host-task networking is unaffected. The runner must provide the
negotiated `run-network` capability.

Generation writes `.stoke/ci-plan.json` and the separately owned reusable
`.github/workflows/stoke.generated.yml`. It does not merge or rewrite handwritten
CI jobs. Edited generated content is rejected rather than silently overwritten.
Expected fingerprints in commands reject stale plan/YAML pairs. These checks
establish configuration consistency, not publisher authentication.

The current plan format is version 2. Version 1 plans are rejected. Preserve an
older generated pair before removing those two generated files and regenerating;
keep handwritten workflow files in place. Stoke does not silently migrate or
overwrite an unsupported plan.

Check regeneration with identical options:

```sh
stoke ci generate --check -f examples/ci/stoke.yaml --context . \
  --cache-from registry.example.com/team/stoke-cache \
  --cache-to registry.example.com/team/stoke-cache \
  --results-repository registry.example.com/team/stoke-results package
```

Inputs are recorded configuration, not a secret channel. Changes to ordinary
source bytes remain Coopr cache inputs; the CI plan does not freeze a checkout.

## Supply the repository from handwritten CI

A plan with cross-job data and no generation-time result repository emits a
**required** reusable-workflow input. This keeps the example reviewable without
inventing an authorized registry destination:

```yaml
name: Stoke checks
on:
  workflow_dispatch:
    inputs:
      results_repository:
        description: Authorized OCI repository for exact Stoke results
        required: true
        type: string
permissions:
  contents: read
jobs:
  stoke:
    uses: ./.github/workflows/stoke.generated.yml
    with:
      results_repository: ${{ inputs.results_repository }}
```

The caller owns permissions, credentials, triggers and surrounding jobs. Generated
workflows inherit permissions; they do not silently grant registry write access
or disable a caller's grant. If using `GITHUB_TOKEN` with GHCR, explicitly grant
the necessary package permission in the caller and provision registry
credentials. Other registries use their own provisioned credentials.

Runners must already provide Stoke and the selected tasks' tools and native
platform. Container tasks also require compatible Coopr and rootless/runtime
capabilities; host-only plans do not start Coopr. `--runner` selects labels; it
does not provision software, architectures or QEMU. Install Coopr `0.6.0` on the
runner and use `STOKE_COOPR_BINARY` or `--coopr-binary` to select it explicitly. Generic [host steps](host.md) require the
handwritten caller's `allow_host: true` input and explicit capability
attestations. The earlier Nix-specific executor draft is not used.

## Opt-in tools containers

Use `--job-container-image REGISTRY/REPOSITORY@sha256:DIGEST` to put invocation
setup and task execution jobs in the same tools image. This supplies Stoke and
compatible Coopr without installing them in job steps. The final status gate
uses GitHub's native prerequisite results and the runner's shell; it needs no
tools container, checkout or registry credentials. The image reference must be
digest-pinned and literal; generation neither builds nor publishes it. The default remains ordinary runner jobs.

`--job-container-options '...'` passes explicitly selected Docker options to
GitHub's `jobs.<job>.container.options`. It requires an image. Both values are
recorded in the plan fingerprint and must be repeated for `--check`. No user ID,
privileged mode, capabilities, mounts, devices or security relaxation is added
implicitly. GitHub's restrictions on job-container options still apply, including
its unsupported `--network` and `--entrypoint` options. Keep credentials out of
these recorded arguments.

For private GHCR images and GHCR result/cache repositories, explicitly add
`--github-token-auth`. Every GHCR job-container pull then uses GitHub's native
container credentials. Execution jobs use the provisioned Coopr's `login`
command with the token on stdin, keep its standard auth file in a fresh private
runner-temp directory, and remove that file after execution. Coopr login is an
additional prerequisite for an opted-in host-only workflow. Token permissions
remain in the handwritten caller. Grant package access only to trusted jobs
and the intended test repositories. The option records the authentication mode
in the plan without recording credential values.

The generator does not select or qualify a container security profile:

- The runner must support Linux Docker job containers and the image's native
  architecture. Pull access must exist **before** steps start. Use an accessible
  image, provisioned pull credentials, or the explicit GHCR authentication
  option above. The caller grants token permissions. Runtime registry access
  requires authentication inside each execution job too.
- The image process must be able to write the runner's checkout, temp, GitHub
  HOME and existing file-command files such as `GITHUB_OUTPUT`. Match the actual
  runner ownership when building the image; changing only Docker's `--user` does
  not update its account, subordinate mappings or writable runtime directories.
- Host tasks run inside the job container and still require the caller's
  `allow_host` and capability attestations. Their declared tools must be in that
  image. This does not provide host Nix, binfmt/QEMU or arbitrary project tools.
- Nested Coopr OCI execution requires a separately qualified outer security
  profile. The default profile remains unqualified for nested execution. The
  [hosted qualification record](../development/hosted-ci-qualification.md)
  identifies the explicitly approved profile and the behavior actually tested;
  the generator does not enable that profile implicitly.

### Build a development tools image

`lib.mkContainer` reuses the selected Coopr flake's container/runtime packaging
and locked nix2container builder, then adds current Stoke, Bash, ordinary action
tools and upstream nix-ld for the runner's unmodified Node binary. There is no
Nix daemon, CI runner or Node distribution bundled in the image. VFS storage
avoids a FUSE device requirement, and `BUILDAH_ISOLATION=rootless` is explicit;
the base image's chroot default is not inherited.

The tools image rebuilds the selected Coopr package's existing Shadow helpers
with libcap headers. This enables upstream's capability-aware setuid path for
`newuidmap` and `newgidmap`; it does not change their root-owned `4755` image
permissions or install file capabilities. The build checks that both helpers
contain that path. Host permissions and container security options are unchanged.

Select the Coopr release source and the measured runner UID/GID. This example
builds from the Stoke checkout and the immutable source of Coopr `0.6.0`.

```sh
nix build --impure --max-jobs 1 --cores 2 --out-link result-tools-image --expr '
  let
    stoke = builtins.getFlake ("path:" + toString ./.);
    coopr = builtins.getFlake "github:strio-dev/coopr/35871e4b01e60657b45fd17e04ac1db119f839e6";
  in stoke.lib.mkContainer {
    cooprFlake = coopr;
    system = "x86_64-linux";
    uid = 1001;
    gid = 1001;
  }'
```

UID/GID 1001 is an example, not a claim about a particular hosted runner. The
result is a nix2container image descriptor with its existing copy helpers. The
caller selects the source for the engine, base image and nix2container. A
registry digest can be used for generation only after separate authorized
publication. The job image must already be available when GitHub creates the
job container.

The earlier tools image built from Coopr PR22 source passed local unprivileged
tests of raw Node, packaged tools, control-job commands and writes to runner-style
paths owned by UID 1001. Writes to UID 1000-owned paths failed with `EACCES`.
The privileged Docker VM proof used a separate prototype. A later offline probe
of the release-based image failed UID/GID map writes even with explicitly relaxed
seccomp, AppArmor and system-path restrictions. Static inspection identified
missing capability-aware helper code. With the packaging correction, native
AMD64 and ARM64 hosted jobs completed networked Go builds under the explicitly
approved profile. The ARM run then failed exact-result publication and the AMD
run was cancelled. This establishes nested execution on those runners, not a
successful build/test/package handoff or cache restoration. See the
[qualification record](../development/hosted-ci-qualification.md) for exact images,
runs and remaining coverage.

## Visible stages, matrices and environments

Build, test and package remain separate native jobs by default. Matrix output
keys are generated uniquely for each concrete variant; one shared matrix output
would lose values because completion order is unspecified. Existing `fail-fast`
and `max-parallel` policy maps to native matrices where work is split.

A shared inline Coopr environment is an actual producer in local execution. When
its consumers span CI jobs or matrix variants, Stoke exposes a separate visible
environment producer job. Consumers import that exact image. Single-consumer
environments stay inside their consumer job. Producer identity uses the existing
file/context/platform/cache/gate lowering rules, including distinct prerequisite
gates; it does not implement another component planner.

A small setup job creates a unique invocation scope. This separates two calls of
the same reusable workflow within one GitHub run. An aggregate job requires all
generated jobs to succeed using native `needs.<job>.result == 'success'`
conditions. It fails for failed, cancelled or skipped prerequisites without
starting Stoke or a tools container. Pure workflow gates are native CI dependencies and
are not executed again merely to recover another job's data.

## Exact selection and retry behavior

The common scope records provider/server, repository, run, source revision,
invocation nonce and plan fingerprint. Each result also records its concrete
producer, attempt, execution nonce, kind, platform, canonical OCI descriptor and
selected input bindings. Local storage IDs are never result identities.

The registry stores actual image/component output and immutable input sets.
Native job outputs carry small references to those input sets. They contain no
success field. CI wire format v2 uses standard OCI indexes: a result index
references its typed metadata artifact, canonical output manifest and optional
input-set index. An input-set index references its typed metadata artifact and
bound result indexes. Only actual file blobs, such as an original component
archive, are artifact layers. The canonical output bytes and digest do not
change. There is no legacy-envelope reader. Already-present nondistributable
image layers are transferred as bounded payload bytes too; URLs are not followed.
A gate-only job can forward its input bindings **before it runs**;
that object therefore cannot be a success receipt. Native `needs` remains the
only success authority.

A join rejects two execution-envelope references for the same producer, even if
payload bytes happen to match. For example, a newly rerun build B cannot be
combined with old tests that consumed build A. Rerun the producer and its
dependent gates deliberately. There is no automatic producer replay, cache
reconstruction or mutable `latest` lookup at the consumer.

A consumer-only retry may reuse an older producer when its exact reference is
explicitly supplied through native metadata, belongs to the same invocation,
source and plan, and has no future attempt. Missing, masked or conflicting output
metadata fails clearly. GitHub's documentation does not guarantee old
`needs.outputs` preservation for every partial/matrix rerun; hosted qualification
remains separate. The safe recovery is to rerun the producer and dependent gates
or the whole invocation, rather than guess which registry object was tested.

Execution-only CI image inputs pass the exact digest-pinned registry source to
Coopr. Coopr checks its image-source policy and metadata, then pulls layers only
when needed.
A consumer cache hit can avoid the producer image while still restoring the
consumer's requested outputs. A miss or fresh check must fetch its required
inputs; unavailable content fails without replaying the producer. Coopr's native
registry TLS and signature-policy settings apply to these image sources.

Publication downloads the verified original OCI layout, retaining its canonical
digest even if native execution uses another compressed representation.
It requires Coopr's explicit
`publish-oci-layout` capability and transfers the selected original manifest;
an older engine fails instead of rebuilding or re-exporting the imported image.
Components retain the original OCI archive bytes, auxiliary
roots and Bale binding metadata; their verified component store handles
publication. Execution verification is not permission to publish a different
immutable output silently.

Stoke removes downloaded exact-input payloads after execution cleanup is
confirmed. On `cleanup_failed`, it preserves those local files and imported
component assets for the unconfirmed execution. This local lifetime is separate
from registry retention and garbage collection below.

Read-only jobs can reuse an existing input set when it already represents the
join. Combining different input sets may require a small metadata write; export
jobs require result write access. Missing write permission fails explicitly.

## No registry and explicit co-location

Independent jobs need no result repository. Cross-job data requires one at runtime;
missing configuration fails before executing a consumer. To keep a data-connected
component local deliberately, generate with an explicit selection:

```sh
stoke ci generate --co-locate build -f examples/ci/stoke.yaml --context . package
```

This groups that task's data-connected component, including necessary shared
environments and intervening gates. A selected matrix stays internal. It can
remove registry handoffs for that component, while other stages remain visible.
The generator never selects this policy silently because a registry is absent.

Ordinary `stoke run` keeps its complete local workflow and once-per-run scheduler.
`ci run` executes one planned job/unit and relies on provider gates. Directly
invoking a projected job is not a substitute for the fully gated local workflow.
GitHub can let unrelated jobs finish after another job fails; this preserves
dependency success gates, rather than local workflow-wide cancellation timing.

## Retention, access and cleanup

Digest addressing provides immutable content identity, not publisher trust or
permanent availability. Trust result-repository writers. Keep cache/result write
credentials away from untrusted workflows. Scope fields and nonces prevent
accidental mixing; they are not authorization credentials or attestations.

Results and payload retention tags must survive the supported retry window.
Retention tags include scope/producer identity and full object digest, are never
used for result selection, and require no registry compare-and-swap. Generic
registries provide no universal TTL or garbage-collection guarantee.

After stopping writers and waiting out the retry window, explicit cleanup can
remove a selected metadata object:

```sh
stoke ci cleanup --results-repository registry.example.com/team/stoke-results \
  --scope "$INVOCATION_SCOPE_JSON" --result "$IMMUTABLE_METADATA_REFERENCE"
```

Cleanup verifies scope and metadata type. It deletes the selected metadata
index by digest, including every tag that points to that same index; it is not
tag-only removal. It does not delete shared image
manifests, payload blobs or cache entries. Registry operators must expire payload
retention tags and run their registry's supported garbage collection separately.
Registries that disable deletion return a clear error. There is no automatic
namespace-wide cleanup. If retained exact content disappears, a later consumer
fails; it does not replace it with a newly built result.

Use the **whole invocation and all eligible attempts** as the retention boundary.
A later attempt can still reference an earlier attempt's result/InputSet ancestry.
Writer quiescence alone does not make that ancestry disposable. Keep the complete
referenced closure until the invocation is terminal and its supported retry
window has closed; preserve unknown references. The host CI or an operator must
provide those facts and an explicit object inventory. Stoke does not implement
an age scanner, retry-window clock, writer lease or automatic janitor.

Do not derive expiry from OCI `created`: metadata uses the Unix epoch for
deterministic encoding. Payload retention anchors are keyed by scope, producer
and payload digest, so identical bytes from different attempts can share one
anchor. Removing an attempt's metadata is not permission to remove that anchor
or its payload. Metadata-only cleanup need not reclaim significant storage.

Treat cancellation or a lost HTTP response as an unknown deletion outcome. The
registry may have committed the DELETE already; reconcile the exact reference
before proceeding. Scope/type checks constrain the requested object, not remote
transaction rollback or registry garbage collection.

GHCR support must first be qualified against this existing OCI operation.
Local fixtures exercise exact digest targeting, scope/type checks, alias removal,
failure/cancellation behavior and shared-content preservation; they do not
establish GHCR deletion support, permissions or storage reclamation. GitHub's
documented package-version REST deletion is a separate administrative operation,
not a required fallback. No administrative adapter is implemented or selected.
Consider one only if real compatibility evidence and a product requirement
justify it. See the [hosted qualification boundary](../development/hosted-ci-qualification.md#ghcr-cleanup-qualification-boundary)
for official references and the held live-test requirements. No GHCR version
mapping, tag-only deletion, TTL or shared-blob garbage collection is qualified.

## Input and platform limits

Exact result transport freezes produced outputs, not unrelated mutable base
tags, native ARG strings, RUN network responses, time, random values, secrets or
other undeclared inputs. Pin external references for consistent resolution and
keep source/configuration stable during a run. The existing native-store
selection stickiness is not a distributed run-wide resolution lock.

COPY/ADD measured digests exclude timestamps. Primary RUN contexts, local named
contexts and aggregate package context keys still include tar metadata; fresh
checkout timestamps can therefore cause conservative misses. Effective ownership
and modes can also be observable inputs. The generator preserves these Coopr
rules. Dropping fields only from cache keys while workloads still observe them
would not establish equivalent execution inputs.

Native metadata is size-bounded, uses separate per-variant environment slots and
fails rather than truncates. The initial generator bounds total input environment
size; runners with unusually low process limits need separate qualification.
Exact input metadata has graph, document and encoded-byte limits; wrong platforms
and malformed metadata fail before consumer execution. Remote image expansion
uses Coopr's native image validation and resource policy. Local OCI imports retain
their plain/gzip validation and 32 GiB expanded-byte limit; components and captured
host files retain bounded full-payload validation. Full native AMD64 and ARM64 workflows,
including fresh test/vet jobs, package gates and exact GHCR result handoff,
passed with the [documented images and container profile](../development/hosted-ci-qualification.md#verified-hosted-results).
Other runner configurations and hosted retry behavior remain separately
unqualified; COPY-only ARM cache tests are not ARM execution.

## Reproduce qualification

On a provisioned rootless development machine, select the reviewed engine and a
disk-backed evidence directory outside the checkout:

```sh
STOKE_COOPR_BINARY=/absolute/path/to/reviewed/coopr \
STOKE_CACHE_CI_EVIDENCE_DIR=/absolute/path/to/external/evidence \
  just ci-integration
```

The fixtures use process-owned local registries, independent native/cache/HOME
state, exact result bindings and actual generated CLI selections. They do not
dispatch a hosted workflow or publish to an external registry. Developer reports
and final files remain separate optional CI artifacts.

The heavier real Go dogfood runs the unchanged authored example against a
complete actual-checkout snapshot, including fresh test/vet jobs:

```sh
STOKE_BINARY="$PWD/result-stoke/bin/stoke" \
STOKE_COOPR_BINARY=/absolute/path/to/reviewed/coopr \
STOKE_CACHE_CI_EVIDENCE_DIR=/absolute/path/to/external/evidence \
STOKE_REAL_CI_SOURCE_DIR="$PWD" \
  nix develop --no-update-lock-file path:. -c \
  go test -count=1 -timeout=30m -v ./internal/artifact \
    -run '^TestCIRealWorkflowRegistryWithCoopr$'
```

Image/module download access is required for that test. Preserve native and
provider qualification gaps when reading its result: independent stores on one
host do not prove every machine/storage/credential combination or hosted retry
behavior. See [local qualification](../development/local-ci-qualification.md)
for the recorded snapshot and procedure.

An earlier large Go run reached all five CI execution jobs before its final
local return failed with VFS `ENOSPC`. A later run with adequate scratch passed
the full roundtrip. Rootless VFS copies can require substantial filesystem
metadata and temporary storage. Check filesystem metadata, per-user quotas and
free space when provisioning the runner.

References: [GitHub output/matrix rules](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idoutputs),
[run/attempt contexts](https://docs.github.com/en/actions/reference/workflows-and-actions/contexts),
[rerun API](https://docs.github.com/en/rest/actions/workflow-runs#re-run-a-job-from-a-workflow-run),
[OCI Distribution v1.1.1](https://github.com/opencontainers/distribution-spec/blob/v1.1.1/spec.md).
