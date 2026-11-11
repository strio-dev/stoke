# How Stoke works

Stoke is a portable workflow and build tool invoked from a local shell or a step in an existing CI job. GitHub Actions, Jenkins, GitLab CI or Tekton decides when that job runs and which agent or machine runs it. The same `stoke run` command composes container builds, tests and artifacts inside the selected environment.

Stoke loads a YAML workflow, resolves reusable OCI task templates, validates dependencies and selects the requested tasks. It schedules ready tasks through Coopr's versioned local execution API, after every prerequisite logical task has passed. Independent tasks and matrix variants can overlap within the invocation's internal bound; a matrix also has its own variant limit. Plans and final reports retain stable topological order while actual completion order can vary.

## Responsibilities

| Layer | Owns |
| --- | --- |
| Host CI platform | Events and triggers, source checkout, CI job graph, agent/machine allocation, credentials and approvals, job status and retained CI artifacts. |
| Stoke | Workflow/task semantics, selection, prerequisite ordering, template composition and input binding, invocation-scoped dependencies, optional package-file ownership, CLI and run records. |
| Coopr | Typed container operations, native definition/component parsing and planning, execution, storage and caching. |
| Bale | OCI dependency declarations, bundle discovery and transfer, relocation destination records. |

The CI platform's graph orders jobs across its agents or machines. Stoke's workflow graph orders work inside one invocation; it does not allocate CI agents or translate its tasks into provider jobs. That workflow graph is also separate from the component/build graph inside Coopr and from any future Strio graph relating published component versions to downstream projects. `needs` expresses workflow ordering even when no filesystem data crosses that edge. `from`, `copy.from`, stage mounts, native/environment argument task bindings and publication sources add data dependencies as well.

## Ready-task scheduling

Stoke automatically schedules ready tasks under an internal limit of four active jobs across the whole workflow. Each matrix variant, native build and publication request consumes one slot. Matrix `strategy.max-parallel` adds a per-task bound. A dependent task becomes eligible as soon as its own prerequisites pass, even while an unrelated branch is still running. Dependency gates wait for every variant of each prerequisite task.

A non-matrix failure or fail-fast matrix failure stops admission, cancels active work and waits for cleanup. A matrix with `fail-fast: false` finishes its remaining variants before its failed group stops the workflow; another failure or external cancellation can still interrupt it. Final reports include selected jobs that never started as `skipped`. See [parallel tasks](../guides/parallel-tasks.md) for limits and failure semantics.

## Reusable invocation scopes

A reusable component declares inputs at document level. Each `uses` invocation supplies strings through `with`; Stoke checks required values and defaults, then binds the selected task and its prerequisites in an independent scope. Nested invocations explicitly forward values with `{input: NAME}`. Caller-added prerequisites retain their caller scope, and component-owned files retain their package owner. The same OCI package can therefore serve multiple calls with different inputs without mutating its definition or merging its assets into caller context.

Root invocations receive inputs from `--input NAME=VALUE`. Definition publication preserves declarations, bindings and assets; it does not publish a bound invocation. Inputs are not a general expression language or a source of implicit Bale dependencies. Bound environment values and native arguments flow into Coopr's normal cache semantics. See [reusable inputs](../guides/reusable-inputs.md).

## Image environments and mounted inputs

An inline `environment` selects a Coopr image definition for ordinary task
steps. After argument/matrix binding, Stoke lowers it to a native image producer
and a `from` consumer. Producers with equivalent resolved inputs, policies and
gates can be shared within the run. This reuses the existing build/dependency
path rather than adding a solver. Consumer `env` and `workdir` override matching
inherited image configuration. Components remain distinct from runnable images.

Task and step mounts become Coopr RUN mounts. Contexts are content snapshots;
explicit host mounts are live caller-owned directories. Images/stages supply
filesystem views, OCI artifacts supply verified raw files, and cache volumes
provide disposable acceleration. Coopr owns materialization, access enforcement,
cache identity, native sharing and cleanup. Live binds bypass result reuse;
context writes and image-overlay writes do not imply host or image writeback.
See [environments](../guides/environments.md) and [mounts](../guides/mounts.md).

## Target platforms

Task `platform` selects a container target, independently of host-CI runner
allocation. Literal, declared-input and explicit matrix bindings resolve before
submission. Ordinary programs use Coopr's platform-aware execution; native
builds receive a default while preserving explicit native FROM choices. Inline
environment identity includes the resolved target.

Stored images retain the producer's actual reported platform. A `from` consumer
inherits it when unspecified and rejects a conflicting explicit target. Reports
record actual output platforms; plans show requested targets. Coopr owns source
selection, platform-aware cache identity and RUN execution. Existing runner
binfmt/QEMU configuration may enable a foreign target, but Stoke adds no
emulator backend or host setup. See [container platforms](../guides/platforms.md).

## Owned Coopr process

For workflows with container tasks, Stoke starts one API-capable Coopr process per workflow run and uses HTTP over a private Unix socket. It negotiates API version `1` and the capabilities required by the selected tasks; there is no embedded-backend fallback. Coopr's native dependencies and helper bootstrap remain in its own executable. Stoke uses Coopr's public `api` and `client` packages for wire types, negotiation, framing and cancellation; its adapters own workflow lowering and service supervision, without importing the native engine.

Literal-operation tasks become typed program requests. `coopr` tasks submit native definition image/component builds, and `publish` tasks submit explicit artifact transfers after their gates pass. Independent tasks and matrix variants use concurrent requests with shared run store roots and Coopr-coordinated cache. Dependent tasks consume successful prerequisite images/components in those stores without publishing intermediates. A parent-lifetime pipe scopes the service to its owner; shutdown cancels and drains active work. See [engine selection](../guides/coopr-api.md).

Registry cache sources and destinations are invocation settings supplied through
`--cache-from` and `--cache-to`. The API forwards them to Coopr's existing cache
backend for literal and native image/component builds, and requires the engine's
`registry-cache` capability when requested. Local cache operation remains in
place. Coopr owns entry identity, content validation, restoration and writes;
Stoke adds no second execution-cache format. These repositories are
optional accelerators, separate from workflow imports, Bale dependency records
and final artifact publication. Generated CI separately publishes immutable OCI results to preserve the exact producer output across jobs; those results are required data, not cache-hit or success evidence. See [caching](../guides/caching.md) and [visible CI results](../guides/ci-generation.md).

Coopr `0.6.0` provides the service and the SDK revision used by Stoke. The [engine guide](../guides/coopr-api.md) records the release source and executable selection.

The YAML schema exposes these build and publication operations without choosing a new Stoke grammar for native definitions: Coopr owns its definition language. `stoke publish` publishes reusable YAML definitions; a YAML `publish` task transfers built images/components. Optional `assets` travel with their owning workflow artifact. Caller context remains separate: literal COPY selects package files with `scope: component`, native definitions use `files-from: component`, and native COPY addresses the reserved `stoke-assets` context. See [packaged component files](../guides/packaged-components.md).

## First implementation boundary

Available: strict YAML, declared string inputs with explicit environment/native-argument bindings, invocation-scoped reusable tasks, dependency validation and selection, bounded parallel ready-task and matrix scheduling, literal argv/environment/workdir, native definition/component builds and argument bindings, inline image-definition environments, task/step context/host/cache/image/stage/artifact mounts, caller/component and task-output copying, OCI templates with optional files, typed registry inputs, declared filesystem export control, gated artifact publication, Coopr local/OCI registry cache, and Bale metadata integration for declared workflow inputs.

Services, secrets, expression interpolation, portable synchronization of cache-volume contents, Strio reporting and a final language extension model are not available. CI triggers, runner/agent provisioning and scheduling across machines belong to the host CI platform. Strata and a shared content store are not dependencies.

Matrix dependencies wait for every producer variant; data edges require an unambiguous single producer. Variants share the run's native store roots while keeping separate output paths; Coopr coordinates concurrent cache access. Matrix axes supply literal environment values to literal tasks and explicit argument bindings to native tasks. An explicit `platform: {matrix: KEY}` binding selects a container target, but does not select a different machine or provide foreign-architecture execution support. See [task matrices](../guides/matrices.md).
