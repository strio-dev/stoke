# Runtime and security

## Explicit host execution

Generic `host` tasks require caller `--allow-host` authority. Imported workflows
and generated plans cannot grant it. Commands execute as the runner user with
that user's filesystem, network and credential access. Input snapshots provide
declared data flow, not isolation. Provision and trust the runner and tools;
use fresh execution for tests and uncaptured effects. Opt-in result reuse needs
a justified complete input/output contract. See [host execution](host.md) for
native platform requirements, cache identity, captured-file limits and
process-group cancellation boundaries.

Treat a workflow and every imported template as executable code with the authority of the account running Stoke. Digest pinning identifies content; it does not establish that the content is trustworthy.

## Native execution

Stoke starts a workflow-scoped Coopr service on a mode-0600 Unix socket inside its private mode-0700 directory. Coopr owns native workers, runtime helper bootstrap and execution cleanup. Stoke neither embeds that bootstrap nor uses an API server inside its own process.

Socket access conveys the service account's authority; it is not per-request authentication or a privilege broker. Do not install either executable setuid, grant file capabilities, proxy the socket to untrusted users or expose it through restricted privileged launchers that rely on later application checks. Rootless execution is not a blanket guarantee against unsafe workloads or kernel/container-runtime defects. There is no automatic privileged fallback.

Cancellation waits for backend acknowledgement; `cleanup_failed` or forced termination must remain visible as incomplete cleanup, not be described as a successful cancel. Workload, lifecycle and protocol tests provide bounded evidence rather than full security proof. See [engine lifecycle](coopr-api.md).

## Host requirements

Linux execution requires usable user namespaces, subordinate UID/GID mappings, mapping helpers, a compatible container storage driver, image policy and an OCI runtime for commands. Nested container and Kubernetes policies can prohibit required operations. `--storage-driver vfs` can help when overlay storage is unavailable; it cannot bypass a host namespace or mount prohibition.

`--runtime` selects the OCI runtime. `--signature-policy` selects the native containers/image policy file. Use a policy appropriate to your registry and trust requirements; a policy accepting an image is distinct from trusting workflow code.

## Target architecture and execution support

A task's `platform` selects container image/build semantics, not a CI machine.
Running a foreign architecture still needs a compatible execution mechanism
already configured by the runner. Coopr uses its existing Buildah/runtime path
and runner-provided binfmt/QEMU support; Stoke installs no emulator, changes no
host registration and supplies no direct QEMU backend.

Rootless namespace, ownership, mounts and storage requirements still apply.
An enabled ARM64 handler on an x86_64 runner does not establish native ARM-host
qualification or support for another runner. Missing execution support must
remain a failure rather than a successful metadata-only architecture change.
See [container platforms](platforms.md).

## Credentials and CI

Use `--authfile` for registry authentication and protect the file as a credential. Do not bake credentials into workflow YAML, images, exported root filesystems or cache inputs. `--plain-http` enables HTTP for every resolved workflow reference, including imports. Native image/component/cache registry verification is controlled separately by `--tls-verify`; disabling it is intended for isolated development registries. Normal registry access uses TLS.

`--cache-to` explicitly writes intermediate execution results to its selected
repository. Give cache write credentials only to runs trusted to publish build
inputs for later consumers. Intermediate results can contain files removed from
the final image. Cache descriptors and payload digests are validated for
integrity, but those checks do not authenticate the cache publisher or establish
that its build results are trustworthy. Use separate cache repositories or
access policies for different trust domains. See [registry caching](caching.md#share-results-through-an-oci-registry).

Run untrusted pull-request workflows in isolated disposable accounts or workers without release credentials. An imported workflow can run arbitrary container commands and read the context you provide. Restrict that context to the files needed by the build. Strio access is not required, and no hosted authorization service is provided by this implementation.

## Mounted inputs and host access

Read-only content mounts are the default. Context directories are snapshotted
inputs, while `type: host` explicitly exposes a live caller-owned directory.
Relative live paths and named-context directories resolve against the caller
context selected by `--context`, not an imported package directory. A workflow
or trusted template can request paths outside that context through a live mount;
review these requests with the same authority as its commands.

An explicit writable live mount can change host files accessible to the mapped
container UID/GID and runner account. It is not a transactional output: failure
or cancellation cannot undo writes. The owner controls permissions, concurrent
access, retention and cleanup. Live mounts bypass result caching, including
read-only mounts whose contents can change during execution. No daemon sockets
are mounted by default.

Writable context snapshots and writable image/stage overlays have different
semantics: neither promises writeback to its source. OCI artifact views are
always read-only and remain private materializations until execution cleanup.
Artifact integrity checks identify bytes; they do not establish publisher trust.
Cache volumes are writable acceleration shared according to native policy and
must not carry required outputs or data between mutually untrusted users. See
[mount lifetime and access](mounts.md).

Mount requests still depend on host namespace, ownership, SELinux and storage
policy. They do not provision another architecture, bypass a denied mount or
establish support for arbitrary nested-container runners. Inline environments
remain container images; this feature adds no general host shell or host-Nix
executor.

## Native definitions and publication

Native Coopr tasks parse caller- or component-selected files with the same account's authority. Stoke constrains definition paths to existing regular files inside the selected canonical root, including symlink resolution. Packaged files are an explicit bounded allowlist, validated against OCI payload paths, sizes, digests and modes before materialization. Loading them creates separate private temporary trees rather than merging them into the caller checkout. These are input and ownership rules, not a security boundary against code running as that account. Coopr retains native definition/component semantics; packaged bytes still require the same trust decision as workflow code.

A YAML `publish` task is an explicit write to its declared registry or local destinations. Use `needs` for required test gates and select an explicit build/test target when delivery is not wanted: a run without targets selects all root tasks, including publication. Independent publication tasks can overlap with other ready work. A failure in an unrelated branch cannot undo a transfer that already completed; express every required release gate through dependency edges. Give release credentials only to trusted runs. Build caches do not suppress a selected publication request, and multiple destinations can partially complete before failure. Preserve per-destination reports and handle retries deliberately.
