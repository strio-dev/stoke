# Mount inputs and caches

Use task `mounts` for inputs needed by every RUN, or step `mounts` for one RUN.
Stoke passes them to Coopr; Coopr resolves inputs, plans dependencies, executes
commands and decides cache reuse. Task and step mounts are combined. A duplicate
target in the same RUN is an error, rather than an override.

Mounts require the `run-mounts` capability provided by Coopr `0.6.0`. See
[engine selection](coopr-api.md) for installing and selecting the executable.

## Choose a source

| Type | Selector | Default access | Changes and lifetime |
| --- | --- | --- | --- |
| `context` | Primary caller context, or `context: NAME` from task `contexts` | Read-only | Coopr snapshots and hashes the input. Writable access uses a disposable execution view; it does not write back to caller files. |
| `host` | `source: DIRECTORY` | Read-only | Explicit live host-directory bind. Writable access changes caller-owned files and has no rollback. |
| `cache` | Optional `id`, optional `sharing` | Writable | Reusable, disposable native cache volume. Its contents are not task outputs or hashed wholesale. |
| `image` | `image: REFERENCE` | Read-only | Filesystem from a resolved container image. Writable access uses a disposable RUN overlay and does not update the source image. |
| `stage` | `from: TASK` | Read-only | Filesystem from an earlier task's completed image. Adds a dependency; writable access is a disposable RUN overlay. |
| `artifact` | Digest-pinned registry `ref` | Read-only only | Verified OCI artifact file view. Coopr owns temporary materialization and cleanup; artifact content cannot be modified. |

`target` is a normalized absolute container path. For content inputs, `source`
selects a path inside the input view; omitting it selects the whole view.
Context sources are relative paths. Selected paths cannot escape their input.
For a host mount, `source` instead names the live directory itself.

Image/stage mounts need runnable-image outputs, not Coopr component artifacts.
A stage source must have exactly one variant. Use `needs` when all variants are
only a success gate; Stoke does not guess which variant supplies mounted files.

The fragments below show mount fields, not standalone workflows. Replace their
paths and registry references with available inputs and place them in a task
with RUN steps.

## Snapshot local inputs

Declare additional local directories on the task:

```yaml
contexts:
  fixtures: ./fixtures
mounts:
  - type: context
    context: fixtures
    source: data
    target: /fixtures
```

Omit `context` to select the primary caller context. `context: stoke-assets`
selects the owning component's declared assets; it is reserved and cannot be
redefined in `contexts`. The name `build` is also reserved.

Relative `contexts` paths and live host `source` directories resolve against
the invocation's caller context: `--context`, the local root workflow's directory
by default, or the current directory for an OCI root. They do not resolve against
Coopr's working directory or an imported package's temporary directory. Absolute
local directory paths are accepted. Unavailable paths and nondirectories fail
before task execution.

Component assets remain a separate immutable input tree. Defining a named
context does not bundle that directory into an OCI Stoke component. A reusable
template can declare mounts and contexts; a `uses` caller cannot replace them.

Set `read-only: false` only when a command needs a writable execution view.
Coopr preserves caller files. Primary-context writes use a build-scoped copy
and can affect later operations in that build. Named contexts materialize as
immutable images; writable mounts use a per-RUN overlay and their changes do
not persist between RUNs. Neither form promises host writeback or a separate exported
artifact. A RUN with writable primary-context side effects must execute because an
image-cache hit cannot restore those mutations.

## Bind a live host directory

```yaml
steps:
  - run: [sh, -eu, -c, 'printf "%s\n" done > /results/status']
    mounts:
      - type: host
        source: ../results
        target: /results
        read-only: false
```

The directory must already exist. Its owner remains responsible for its files,
permissions, sharing and eventual removal. Access uses the container's mapped
UID/GID and the runner account's authority; read-write does not grant access that
the host denies. Concurrent writers need their own application-level
coordination. Cancellation stops execution and drains cleanup, but cannot undo
writes already made.

On an SELinux host, directory labels must also permit container access. Stoke
leaves caller labels unchanged by default. For a directory you own and intend
to expose, `relabel: shared` explicitly requests native shared container
labeling; `relabel: private` requests a label private to that execution and is
unsuitable for concurrently shared input. Both may change labels on the selected
host directory. These options apply only to live host mounts; they do not
change host-wide SELinux settings or bypass file ownership and permissions.

Live mounts bypass execution-result reuse even when read-only: the content can
change while a command runs. Use a context mount for tracked immutable input.
Stoke does not mount host sockets, a Nix store or daemon sockets automatically.

## Reuse a cache volume

```yaml
mounts:
  - type: cache
    id: compiler-downloads
    sharing: locked
    target: /cache
```

An omitted ID uses Coopr's native scoped default. An explicit ID chooses a native
volume reusable by compatible executions under the same account, ownership and
cache-parent configuration. `shared` permits concurrent use and `locked`
serializes access. The current Buildah backend does not support `private`;
Stoke rejects that value rather than implementing a separate private cache.
Sharing modes do not reserve CI machines or make data portable.

Stoke selects `<cache-dir>/mounts` as the persistent native cache parent, using
the same `--cache-dir` root as its portable result cache. Coopr passes that
parent to its workers and reuses Buildah's native volume storage and locking.
Matching parent, ID and ownership allow reuse across run-specific image stores.
The native on-disk layout is not a Stoke cache format or public path contract.

Cache-volume contents are separate from execution-result entries;
`--cache-from`/`--cache-to` do not transfer them. They persist after a run and
remain caller-owned. Remove unused data only when no run uses that cache parent;
Stoke does not provide a volume-pruning command. Losing the directory must cost
only execution time: commands must succeed with empty volumes and produce
required outputs outside them. A cached RUN may not execute to populate a
volume.

## Mount an image or earlier task

```yaml
mounts:
  - type: image
    image: registry.example.com/team/tools:reviewed
    source: /usr/local/bin
    target: /tools
  - type: stage
    from: build
    source: /out
    target: /built
```

Coopr resolves the image and includes its identity, selected path and mount
options in execution inputs. Pin registry image digests when reproducibility is
required. A stage mount reads the completed native image in this run's store;
no registry push is required. Mounting files does not copy the source image's
environment or working directory into the task.

Explicit writable image/stage mounts change a per-RUN overlay. Their changes
are discarded with that mount and do not update the source image or become
required outputs. Copy wanted files into the task's own filesystem explicitly.

## Mount OCI artifact files

```yaml
mounts:
  - type: artifact
    ref: registry.example.com/team/files@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    source: bundle.tar.gz
    target: /inputs/bundle.tar.gz
```

The reference above is a placeholder, not a downloadable artifact. Use the
artifact's actual digest and the filename in Coopr's verified file view. Omit
`source` to mount that entire view at a directory target. Artifact blob filenames
follow the same rules as native Coopr COPY/ADD inputs. An archive remains a file;
mounting it does not extract its contents.

Coopr reuses native authentication, manifest/blob digest verification and
materialization. Temporary files remain alive until their RUN finishes and are
removed during cleanup. A selected path cannot escape the view, and writable
artifact mounts are rejected. Task image and artifact mount references are Bale
dependencies, so verified relocation mappings apply to them. See
[shared workflows](shared-workflows.md).

## Native definitions and limits

Native `coopr` tasks and Coopr components retain mounts declared inside their
definitions. Task/step `mounts` apply to ordinary Stoke RUN steps, including
steps using an [inline environment](environments.md); they do not replace native
definition mounts. Task `contexts` can supply named inputs to native definitions.

Mounts still require the rootless namespace, storage and runtime support
described in [runtime and security](security.md). Runner policy, ownership,
SELinux labels and storage-driver support can restrict a mount. A supported
source type does not establish support on every Linux runner or architecture.
Host-Nix execution and Nix-to-container composition are separate, unavailable
features. See [caching](caching.md) for result reuse and test-result policy.
