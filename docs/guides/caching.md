# Reuse execution results

Coopr owns container build and instruction caching: cache identity, invalidation, storage and reuse. Stoke passes cache settings to Coopr and reports counters when that execution frontend supplies them. The workflow layer selects which tasks run and whether each build task may reuse cached execution results.

The sections below describe container tasks. Generic host tasks default to fresh
execution and use the separate, explicit captured-file contract described under
[host result reuse](#host-result-reuse).

## Local cache

Container caching is enabled by default. The portable local cache persists across runs under `$XDG_CACHE_HOME/stoke`, or `~/.cache/stoke` when the XDG variable is unset. Select another location with `--cache-dir`:

```sh
stoke run --cache-dir /path/to/stoke-cache build
```

Run outputs and native execution stores are separate from the portable cache. Each workflow run uses its own store roots. Literal-program cache statistics report portable instruction-cache hits, misses, stored entries, skipped entries and errors; they do not measure all native Buildah reuse. Stored entries count successful artifact writes per destination, including updates after warm reuse. Native Coopr definition/component builds currently omit the `cache` report field because their counters are unavailable; absence does not mean zero activity. A warm run may still perform work to resolve inputs, restore states and export outputs.

The native acceptance fixture reuses its RUN layers across fresh run stores, while image configuration/history timestamps can change its whole-image digest. Layer reuse and reproducible image metadata are separate properties. Stoke does not normalize native output timestamps. It does normalize the metadata of materialized component-input files and directories so extracting an unchanged package to a new temporary path does not introduce extraction-time timestamps into named-context inputs.

## Measure warm-cache cost

A cache hit can skip execution while still requiring input hashing, content
verification, native image restoration and output export. Registry reads can
also seed the writable local cache. Compare complete run time and transferred
bytes alongside execution reuse; a hit count alone does not measure the work
saved.

The development [cache-performance method](../development/cache-performance.md)
uses fixed cold, local-warm, registry-warm and changed-source workloads. It keeps
correctness checks and resource limits fixed, and separates fewer registry bytes
from an elapsed-time improvement. Its offline packaging workloads do not predict
every project's build or CI performance.

## Share results through an OCI registry

Use Coopr's existing registry cache to warm a later run on another machine. The
same Stoke workflow can run locally and inside your existing CI job; the cache
repository carries eligible execution results between them.

For example, run locally and write results to a repository you control:

```sh
stoke run --cache-to registry.example.com/team/stoke-cache test
```

Then let a CI job with a fresh local cache read that repository:

```sh
stoke run --cache-from registry.example.com/team/stoke-cache test
```

Replace the example repository with your actual cache repository. The first
command explicitly writes to it. Provision a rebuilt Coopr executable supporting
the `registry-cache` API capability through `STOKE_COOPR_BINARY` or
`--coopr-binary`; older API-capable development engines may lack that capability.
Coopr `0.6.0` provides this capability. See [engine selection](coopr-api.md).

Released Coopr `0.6.0` advertises `registry-cache`, but its cache publication
fails strict GHCR descriptor checks. The repaired OCI-index transport and
shared-prefix reuse fix are in [Coopr PR #24](https://github.com/strio-dev/coopr/pull/24).
See [hosted qualification](../development/hosted-ci-qualification.md) for the
tested immutable engine revision, images and actual cache-reuse evidence.

`--cache-from` reads and `--cache-to` writes. Each option is repeatable, and they
are independent: a read repository is never updated unless also selected for
writes. Trusted runs can read and update the same repository:

```sh
stoke run \
  --cache-from registry.example.com/team/stoke-cache \
  --cache-to registry.example.com/team/stoke-cache \
  test
```

Stoke accepts an explicit registry and repository, without a tag or digest, such
as `ghcr.io/example/stoke-cache` or `localhost:5000/team/stoke-cache`. Short names,
transport prefixes, `oci://` references and BuildKit-style `type=registry,...`
options are rejected. Coopr chooses cache entry identities within the repository.
The cache format belongs to Coopr and is not interchangeable with BuildKit or
Dagger cache formats.

The local cache selected by `--cache-dir` remains enabled alongside registry
sources and destinations. A registry hit can seed the writable local cache for
later local reuse. Matching inputs,
platforms and compatible execution settings are still required. A cache hit can
require source resolution, restoration and export work; warming a cache does not
guarantee an offline run or a hit for every eligible instruction.

Registry caching applies to literal-operation tasks and native Coopr image and
component builds. It carries eligible instruction snapshots, component outputs
and package results. It does not synchronize RUN cache-mount contents or create
a Stoke workflow pass/fail cache. YAML publication tasks always perform their
selected delivery and do not read or write an execution cache.

### Credentials and transport

Use `--authfile` or the native registry configuration for credentials, with read
access for consumers and write access only for runs allowed to update the cache.
The CI platform supplies those credentials; they do not belong in workflow YAML.
Registry caches use Coopr's native authentication, certificate and TLS settings.
`--tls-verify=false` is intended only for isolated development registries.
`--plain-http` controls OCI workflow definitions and imports; it does not switch
native registry-cache transport to HTTP.

Trust cache repositories as build inputs. Cache entries are checked for content
integrity, but a matching digest does not establish trust in the publisher.
Intermediate results may contain files that were removed from a final image.
See [runtime and security](security.md).

### Misses and failures

| Condition | Behavior |
| --- | --- |
| Cache entry is missing | Coopr treats it as a miss and executes the work. |
| An unavailable or corrupt entry is encountered during lookup | Coopr warns and rebuilds; available counters record the error. |
| Proven invalid imported bytes are found while preparing a cache graph | Coopr warns, records the error and skips that whole graph's publication; its previous tag stays unchanged and it is not counted as stored. |
| Repository configuration or cache initialization is invalid | The build fails; invalid options are not silently ignored. |
| An explicitly selected cache export or cache-seeding write fails | The build fails and retains the error. |

The skipped graph is not replaced with a partial alternative set. Other complete
graphs can still publish. Missing retained payloads during complete export,
stream-close failures, cancellation, native export and staging I/O remain
errors. Local graph writes verify existing payloads and generated metadata;
corrupt destination content fails before retagging, without automatic repair or
garbage collection. Cache warnings can therefore mean a successful task did not
refresh its portable cache.

Coopr `0.6.0` embeds BuildKit's generic dependency
solver and cache manager, while Buildah still executes operations. It retains
multiple complete results for a logical instruction graph, so a forced build of
a shared COPY prefix can preserve an earlier downstream result. Portable OCI
artifacts carry those dependencies, result metadata and complete images. No
BuildKit daemon is required.

Cache reuse remains an optional accelerator, and artifacts remain the source
of truth.

## Disable result reuse

Disable caching for a task whose result must be obtained again:

```yaml
version: 1
tasks:
  test:
    image: docker.io/library/busybox:1.37
    cache: false
    steps:
      - run: [sh, -eu, -c, 'test -f /etc/passwd']
```

Or disable caching for every selected task in one invocation:

```sh
stoke run --no-cache test
```

These controls force fresh execution. Coopr may import existing portable graph
records to retain historical alternatives when updating a cache; those results
cannot satisfy forced execution. It may store newly executed results in the
local cache and explicitly selected `--cache-to` repositories; `Stored` can be
nonzero while `Hits` is zero.

Generated CI jobs omit image export and portable-cache writes for fresh
`cache: false` gates when no task consumes their image and no full filesystem
output is requested. Selected file exports still run, including failed-test
reports. Data-producing tasks retain their outputs and cache-refresh behavior.
This optimization requires the engine's `optional-oci-output` capability.

A cached successful test is evidence of an earlier execution, not evidence that an external service is healthy now. Set `cache: false` for tests that depend on network state, time, mutable remote data or other conditions outside tracked build inputs. Pin dependencies and make relevant inputs explicit before relying on reusable test results.

## Mount inputs and side effects

Coopr includes resolved image/artifact identities, selected paths and relevant
mount options in execution inputs. Context mounts use Coopr snapshots and
content hashing. A writable primary context modifies a disposable build-scoped copy,
and its RUN must execute because an image-cache hit cannot replay the mutation.
Writable image/stage mounts instead use per-RUN overlays with no source
writeback. Artifact views are immutable.

Explicit live host-directory mounts bypass result reuse even when read-only;
their content is not a stable snapshot. Writable live effects are not restored
from cache and are not rolled back on cancellation or failure. Use tracked
context inputs when reusable execution results are wanted.

Cache volumes are separate from execution-result entries. Stoke selects
`<cache-dir>/mounts` as their persistent parent; Coopr/Buildah owns the native
IDs, storage and `shared`/`locked` sharing. `private` is rejected by Stoke because
the current backend does not support it. Volume contents are not hashed
wholesale or transferred by `--cache-from`/`--cache-to`; commands must work with
empty volumes and produce required artifacts outside them. A cached RUN need
not execute to warm its cache volume. See [mount inputs and caches](mounts.md).

Inline [task environments](environments.md) reuse the native build-and-inherit
path. Stoke can share an equivalent environment producer with matching inputs
and gates within a run; Coopr remains responsible for actual build-cache reuse.

## Platform identity

A task's resolved container platform is part of Coopr's execution/build inputs.
Different targets must resolve compatible source content and cannot reuse an
image merely by changing its architecture label. Inline environment-producer
identity includes the target as well as resolved files, arguments, contexts,
policy and prerequisite gates.

A `from` consumer inherits the producer's actual reported image platform and
rejects a conflicting explicit target. A native build default can differ from
its actual final image when native FROM stages make explicit choices. Use the
actual `platform` in run records when inspecting outputs. A cache hit does not
prove a fresh test or native execution on the runner's architecture. See
[container platforms](platforms.md).

## Component inputs

Stoke resolves declared inputs and `with` values before submitting work to Coopr. Bound environment values and native arguments become ordinary concrete execution inputs. Changing a value used by a task can therefore change Coopr's cache identity. An unused input does not add a separate Stoke cache key, and distinct invocation names do not create separate cache shards. Coopr decides whether identical effective work can reuse an earlier result.

Each reusable invocation owns its prerequisite graph, so different argument values cannot overwrite a sibling invocation's task definitions. That scoping controls workflow semantics; it does not introduce another cache implementation. See [reusable inputs](reusable-inputs.md).

## Boundaries

Stoke uses Coopr's native cache identity and invalidation behavior. It does not add a workflow-level pass/fail cache. A prerequisite shared by ordinary local tasks executes once per run, independently of whether its internal build instructions hit the cache. Reusable component invocations each own their prerequisite scope. Changing scheduling order is not itself an application input.

Matrix variants share the selected build task cache policy. Literal-task environment values and explicitly bound native Coopr arguments form part of their execution inputs. Variants use the same portable cache directory, coordinated by Coopr's native cache locking. Stoke does not add cache shards or a workflow pass/fail cache. Packaged files form a named Coopr input context; changed asset bytes or executable mode can change that context identity. Coopr decides reuse, and this does not promise per-file or Dagger-equivalent cache behavior.

Publication tasks always execute when selected after their prerequisites pass. They cannot set `cache`; a cached build or test does not imply its image was delivered to a destination. Publication results retain per-destination status and can describe partial failure. See [native builds and publication](native-coopr.md).

Independent ready tasks and matrix variants within one run share that cache under Coopr's coordination. Automatic scheduling does not create cache shards or a separate result cache. See [parallel tasks](parallel-tasks.md).

Only one Stoke run may write a selected portable cache directory at a time. A concurrent run using the same directory fails with a cache-in-use error; choose separate local directories when running independent Stoke processes concurrently. Those runs can select the same registry repository, where Coopr manages cache entries by their keys.

Cache repository options belong to the invocation. They do not change the workflow YAML, become workflow imports or add Bale dependency declarations. A registry cache is separate from publishing reusable Stoke components or final images.

Cache volumes are exposed through task/step mounts. Stoke provides no container-cache pruning command or distributed cache service. Coopr's native store maintenance is separate; consult its storage guide before selecting a store for pruning. Generated CI's [exact-result cleanup](ci-generation.md#retention-access-and-cleanup) has a separate retention contract. Do not share writable cache directories between mutually untrusted users. Keep caches outside the host context. Losing a cache should cost execution time, not remove the source of truth for an artifact.

## Host result reuse

Host commands execute by default, including fresh tests and uncaptured external
effects. Opting into `cache: true` requires declared inputs, captured outputs,
complete toolchain inputs and `host.effects: captured`. That is an explicit
author contract, not proof that a host command is hermetic. Read the
[host execution guide](host.md#caching-requires-a-complete-contract) before
enabling it.

Verified host captures use `<cache-dir>/host` and can use the same explicit
`--cache-from`/`--cache-to` repositories through a distinct host-file OCI kind.
They do not change Coopr's image cache or add a workflow pass/fail cache. The key
includes effective environment, provisioned tools, native platform/user identity,
execution location and declared content; different runner identities can
conservatively miss. A hit restores the exact captured files and seeds explicit
cache destinations. `--no-cache` forces commands to execute.

Host registry transport uses `--authfile` and TLS by default; `--plain-http`
explicitly enables HTTP for that transport. Trust repository writers as build
input publishers. Digest and input-key validation establish integrity and
binding, not that a trusted command actually produced the bytes. A missing key
executes the task, while authentication, transport and malformed-entry failures
are reported. No automatic host-cache pruning is provided.

Disposable host-cache lookup is separate from exact CI result exchange. A
missing exact producer result fails rather than replaying a fresh producer
after its tests. Only authorized registry writers should publish either kind;
native CI success gates still control downstream admission.
