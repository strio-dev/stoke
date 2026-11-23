# Select the Coopr execution service

Stoke delegates container execution to a Coopr executable through HTTP over a private Unix socket. A workflow run starts and owns the service; you do not need to install a persistent daemon or start a shared service manually.

## Select a compatible executable

Stoke requires Coopr API version `1`. Generation and runtime use the same
capability calculation for selected tasks, imported values and execution options;
an unsupported capability fails before execution.

| Selected behavior | Required capability |
| --- | --- |
| Literal image tasks | `execute` |
| Native image definitions / component definitions | `build` / `component`, respectively |
| Publication | `publish`, plus `publish-oci-layout` for exact imported images |
| Named contexts or captured host inputs | `named-contexts` |
| Task or step mounts | `run-mounts` |
| Explicit native build platform | `build-platform` |
| Selected-file exports | `selected-file-exports` with ordered-state capture |
| Fresh CI gates without unused image output | `optional-oci-output` |
| CI data prerequisites restored from cache instead of executed in the selected stage | `cache-require-hit` |
| Registry cache reads or writes | `registry-cache` |

Component imports also require `component-load` and `component-load-name`.
Requested network modes require the corresponding Coopr networking capabilities.
A cache miss for one of these CI prerequisites fails the run instead of
re-executing the prerequisite.

Select a Coopr executable that matches the SDK revision used to build Stoke
(the revision in `go.mod` for a source build). Stoke checks API version and
required capabilities at startup; a version label alone does not establish
compatibility. For packaged installations, keep `coopr` with its `libexec`
directory and verify the archive against its published `SHA256SUMS`.

Select an executable for local workflows.

```sh
export STOKE_COOPR_BINARY=/absolute/path/to/compatible/coopr
stoke run test
```

Or override the selection for one run:

```sh
stoke run --coopr-binary /absolute/path/to/compatible/coopr test
```

The flag takes precedence over `STOKE_COOPR_BINARY`; otherwise Stoke looks for `coopr` on PATH. It checks API version `1` and the capabilities required by the selected execution path. Literal tasks and image imports require `execute`; native builds, components and publication require their respective capabilities above. A missing binary, unsupported service command or incompatible API fails the run. Stoke does not fall back to an embedded backend or reinterpret normal Coopr CLI output.

Selected `steps[].export` operations additionally require
`selected-file-exports`. Stoke checks this before executing tasks. It sends the
ordered checkpoints through the typed program's state selectors in one request;
Coopr owns their execution, cache reuse and capture. See [selected outputs](native-coopr.md#choose-exported-outputs).

When the selected execution reads from or writes to a registry cache, it
requires the `registry-cache` capability.
API capability support does not establish registry compatibility; see the
[current GHCR cache limitations](caching.md#share-results-through-an-oci-registry).
Stoke rejects an unsupported request before executing tasks rather than silently
using only the local cache. The service forwards the configured read/write
repositories to Coopr's existing registry-cache backend for literal programs and
native image/component builds. See [cache transport](caching.md#share-results-through-an-oci-registry).

Exact CI component imports additionally require `component-load` and
`component-load-name`. Each imported archive gets its own name in an
invocation-private component store. The returned `name:tag@digest` retains its
Bale delivery selection; the digest alone can be ambiguous when deliveries
share component bytes. These names must not be reassigned while consumers use
them. Missing capability fails clearly, with no unnamed fallback.

Stoke accepts additive optional API-v1 response fields, while rejecting
ambiguous controls, malformed known payloads and unsupported event kinds.
Documented admission refusals do not cancel another registered execution.
Uncertain admission, stream/writer failure and truncation still require an
independent cleanup acknowledgement. Compatibility is feature-negotiated;
Stoke uses Coopr's public Go `api` and `client` packages for negotiation,
framing, errors and cancellation. Its own adapters lower workflow operations
and supervise the invocation-owned service; they do not duplicate the wire schema.

`--network none` selects Coopr's existing build-wide RUN network default and
requires `run-network`. Empty selection preserves the engine default. Explicit
network choices in native component RUNs still apply; `none` is not a global
offline policy and does not disable registry access. `host` and namespace-path
choices retain native authorization and must be deliberately selected by the
caller. This setting controls container tasks, not generic host commands, and
does not change host configuration or isolation. `stoke ci generate github --network
none` records it in the reviewed plan and generated jobs; CI execution rejects a
different invocation override instead of silently changing declared semantics.

Network-capable rootless RUNs also require a working userspace network helper.
An explicit `--network slirp4netns:enable_ipv6=false` uses Coopr's existing
slirp integration when that helper is provisioned. This is a caller-selected
mode, not an automatic fallback. On the qualified Tempest runner it passed
isolated user/network namespace, controlled HTTP and DNS/outbound TCP checks.
The system Pasta helper on that Fedora runner fails during its sandbox mount
setup; this does not qualify default networking. Changing host security policy,
relabeling a helper or selecting host networking is not an implicit remedy.
A separate stock NixOS guest also passes controlled HTTP/DNS/TCP and helper
cleanup checks for explicit Slirp and Pasta, in serial and isolated-stage native
graphs. That qualification does not fix the Fedora system-helper failure or
establish Internet access on every runner. Other hosts and helper versions need
their own runtime qualification.

Component import names are derived from exact archive bytes, not store paths or
import order. Identical archives keep identical selectors across fresh stores;
distinct delivery archives stay separate. A repacked archive can have a different
selector. Native ARG strings remain observable: a different literal alias or
bare-root value can legitimately change history, environment and cache identity.
Stoke does not rewrite those values merely to claim a cache hit.

`check`, `plan`, workflow-definition `publish` and `pull` do not execute container tasks and do not require this service. Registry access may still be needed to resolve or transfer OCI definitions.

## Workflow-scoped lifetime

Stoke creates a private mode-0700 temporary directory, starts `coopr system service` with an absolute socket path and an inherited parent-lifetime pipe, and connects to its mode-0600 Unix socket. Independent tasks and matrix variants use separate execution requests on that one service, bounded by the scheduler's internal four-job limit and each matrix's `strategy.max-parallel`. The directory is removed after the service exits. If the configured temporary path exceeds the platform Unix-socket limit, only this small private socket directory uses `/tmp`; build scratch continues to use the configured temporary directory.

Selected workflow definitions, native file paths and output ownership are
checked before the service starts. Cancellation also applies while waiting for
API readiness. Once ready, Stoke keeps the service available until canceled
requests acknowledge cleanup and all started jobs have drained.

At normal completion Stoke closes the parent pipe and waits. Pipe EOF or termination causes the service to stop accepting executions, cancel active work and drain backend cleanup. A canceled or uncertain request is canceled by its ID and acknowledged before the client returns; forced shutdown or missing acknowledgement remains an error. Persistent native stores, cache, run reports and outputs are retained according to their existing ownership rules.

If Coopr cannot confirm execution cleanup, its API reports `cleanup_failed`.
Stoke fails the run and preserves loader-owned component assets and downloaded
CI inputs. Native frontend errors identify their retained staging and
materialized input paths. A failed cleanup acknowledgement
requires investigation; service exit alone does not establish worker cleanup.

This is local process supervision, not a privilege boundary. The service uses the caller account's authority and still needs a working rootless/container environment. See [runtime and security](security.md).

## Workflow task dispatch

Stoke supplies optional component files as a separate local named context. Typed COPY can select that context, and native builds use the reserved `stoke-assets` name without replacing the caller context. Only local-directory context bindings are exposed at this boundary.

Stoke translates `image`/`from` task operations into literal programs, `coopr` tasks into native image/component builds, and YAML `publish` tasks into explicit transfers. It validates producer kinds and workflow gates before dispatch; Coopr retains native definition parsing, component resolution and cache ownership. See [native Coopr tasks](native-coopr.md).

The separate `stoke publish` command continues to publish reusable workflow definitions with [OCI/Bale metadata](shared-workflows.md). Built images/components are published only by selected YAML publication tasks. Running without targets selects all root tasks, including declared publication tasks.

Coopr's ordinary CLI remains daemonless; other consumers can continue using its native Go library. Stoke itself needs neither CGO/native engine libraries nor Coopr's same-binary helper bootstrap. Coopr carries those native requirements in its own executable. No API server package is imported into Stoke.
