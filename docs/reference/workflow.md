# Workflow YAML

Version one is a declarative set of named tasks and optional digest-pinned OCI imports. Values are literal or explicit input bindings: Stoke does not interpolate variables or evaluate YAML as an expression language.

## Document

```yaml
version: 1
imports: {}
tasks:
  hello:
    image: docker.io/library/busybox:1.37
    steps:
      - run: [echo, hello]
```

| Field | Meaning |
| --- | --- |
| `version` | Required integer `1`. |
| `imports` | Optional mapping from local alias to OCI workflow reference pinned with `@sha256:` and a 64-character lowercase hexadecimal digest. |
| `inputs` | Optional declarations of string inputs accepted by this component. |
| `assets` | Optional allowlist of component-owned files collected relative to this YAML file. |
| `tasks` | Required nonempty mapping of task names to task definitions. |

Task and import names start with an ASCII letter and contain letters, digits, `_` or `-`. Definitions are limited to 1 MiB and exactly one YAML document. Optional assets are limited separately to 256 files, 8 MiB per file and 32 MiB total. Unknown fields, duplicate keys, anchors, aliases and merge keys are rejected.

## Inputs

```yaml
inputs:
  version:
    type: string
    required: true
  mode:
    default: release
  note: {}
```

Input names follow the task/import naming rules. Each declaration accepts only `type`, `required` and `default`. The only supported type is `string`; omitting `type` selects it. `required` defaults to `false`. A required input must be supplied, including when its value is an explicit empty string; it cannot also declare a default. An omitted optional input uses its default, or an empty string when no default exists. Defaults and supplied values must be YAML strings; quote numeric-looking and boolean-looking text. Null, numeric, boolean, list and object values are rejected. Strings must be valid UTF-8 and cannot contain NUL.

Bindings use exactly `{input: NAME}` in `env`, `coopr.args`, `environment.args` or a nested invocation's `with`. The name must be declared by the containing workflow. A binding supplies the entire value; strings such as `"prefix-${version}"` remain literal. Inputs do not change task names, selectors, paths, matrices or publication destinations. Bind an environment value and use an explicit shell when a command needs to combine text.

A `uses` task passes inputs to its imported component through `with`:

```yaml
uses: tools/build
with:
  version: "1.2.3"
  mode: {input: mode}
```

`with` is available only with `uses`; unknown callee input names and missing required inputs fail resolution. Its values are strings or bindings to the caller's declared inputs. Every invocation gets an independent dependency scope, and its input values apply to the selected template and its imported prerequisites. They do not modify another invocation or the stored OCI definition. Local `needs` still names tasks in the caller's scope.

Use repeated `--input NAME=VALUE` flags on `check`, `plan` and `run` to supply local or remote root inputs. `publish` and `pull` preserve declarations without requiring invocation values. See [reusable inputs](../guides/reusable-inputs.md) for a complete example.

## Component assets

```yaml
assets:
  - native/image.coopr
  - scripts/check.sh
```

Asset entries are canonical relative paths with slash separators. They name regular files, not directories or glob patterns. Absolute/escaping paths, backslashes, duplicate and ancestor-conflicting entries are rejected; `stoke.yaml` and its subtree are reserved. Collection is rooted at the YAML directory and rejects symlink escapes. Only file bytes and executable mode travel with the OCI package; mode and materialization timestamps are normalized. Imported templates keep their original asset owner. See [packaged component files](../guides/packaged-components.md).

## Task

| Field | Meaning |
| --- | --- |
| `image` | Container image reference, or `scratch`, for a literal-operation task. |
| `host` | Explicit generic runner execution with declared tools, input snapshots and captured files; requires separate caller authority. See [host tasks](#host-tasks). |
| `platform` | Linux target string or explicit `{matrix: KEY}` / `{input: NAME}` binding; required to match the native runner for host tasks. See [platforms](#platform). |
| `from` | Another task whose completed image supplies this task's starting filesystem and image configuration. Adds a dependency. |
| `coopr` | Native Coopr definition image or component build; see [native builds](#native-coopr-build). |
| `environment` | Coopr image definition used as the starting image for ordinary steps; see [task environments](#task-environment). |
| `publish` | Explicit transfer of a completed image or component; see [publication](#publish-a-built-artifact). |
| `uses` | Imported task template in `alias/task` form. Replaces the local execution selector and steps. |
| `with` | String inputs or caller input bindings for a `uses` invocation. |
| `needs` | Additional prerequisite task names. A dependency alone does not copy files. |
| `env` | Literal-operation task environment values: strings or `{input: NAME}` bindings. Names follow `[A-Za-z_][A-Za-z0-9_]*`. |
| `workdir` | Absolute container working directory, or normalized relative private-workspace directory for a host task. |
| `contexts` | Named local input directories, resolved against the caller context and snapshotted by Coopr. |
| `mounts` | Input/cache mounts applied to every RUN step; step `mounts` add inputs for that RUN. |
| `cache` | Boolean; container builds default to `true`, host tasks to `false`. Host opt-in requires the explicit captured-effect/input/output/toolchain contract below. `false` bypasses result reuse. Publication always executes. |
| `strategy` | Optional matrix axes, variant concurrency and failure policy for build tasks. |
| `outputs` | Optional container filesystem export control. Host captures use `host.outputs` instead. |
| `steps` | Ordered list of `run`, `copy` or `export` operations. May be omitted for a task that only selects an image. |

A task selects exactly one of `image`, `from`, `coopr`, `environment`, `host`, `publish` and `uses`. Imported templates can be overridden with local `needs`, `env`, `workdir`, `cache` and `outputs`, subject to the resolved task kind's restrictions. Local selectors and steps cannot replace a template's execution. A `uses` caller cannot override template mounts or named contexts. A local `strategy` replaces the entire inherited strategy; omitted strategy inherits it. Local environment entries override matching template entries. Imported task dependencies receive the invocation namespace; local `needs` can add local prerequisites in the caller scope. Namespace collisions fail rather than replacing tasks.

Pin image digests for reproducibility. A mutable image tag can resolve differently later even when the YAML is unchanged.

## Platform

Task `platform` is a literal `linux/ARCH[/VARIANT]` string, exactly one
`{matrix: KEY}` binding or exactly one `{input: NAME}` binding. OS, architecture
and variant names must be lowercase. Common aliases normalize before execution,
including `linux/x86_64` to `linux/amd64` and `linux/aarch64` to `linux/arm64`.
Empty values, unsupported forms and unresolved/missing bindings fail rather than
falling back to the host target. Matrix keys must be declared by that task;
input names must be declared by the containing component.

Omitting the field on an image task keeps Coopr's engine-host default. A `from`
consumer instead inherits its producer's actual image platform; an explicit
consumer target must match. On a native `coopr` task, the field is a build
default and does not replace explicit platform choices in native FROM stages.
An inline `environment` passes the target to its producer build and constrains
its consumer against the actual resulting image. Target platform participates
in environment-producer identity.

The field is task-level, not a field inside `coopr` or `environment`.
Publication tasks reject it. A reusable template owns its selector; a `uses`
callsite cannot override it, but can pass a declared input used by that selector.
These bindings do not enable general expression interpolation.

A container target does not select a CI runner or install emulation. Foreign
commands need execution support already configured by the runner. Plans show
requested targets; run records report actual result platforms. See
[container platforms](../guides/platforms.md) for runtime requirements and
inheritance examples.

## Strategy and matrix

```yaml
strategy:
  max-parallel: 2
  fail-fast: false
  matrix:
    mode: [debug, release]
    toolchain: [stable, next]
    exclude:
      - mode: release
        toolchain: next
    include:
      - mode: debug
        toolchain: experimental
```

| Field | Meaning |
| --- | --- |
| `strategy.matrix` | Required when `strategy` is supplied. A mapping of axis names to nonempty scalar lists, plus optional `include` and `exclude` lists. |
| `strategy.max-parallel` | Maximum active variants in this logical task, within the internal four-job workflow bound; defaults to `4`. Positive values up to `256`. |
| `strategy.fail-fast` | Defaults to `true`; a variant failure stops workflow admission and cancels active work. `false` finishes that matrix's siblings before its failed group stops the workflow; external cancellation or another failed group can still interrupt them. |
| `matrix.include` | List of nonempty mappings. Augment compatible surviving base combinations, or append an entry as a standalone combination when it cannot augment one. |
| `matrix.exclude` | List of nonempty mappings. Remove base combinations matching every key/value in an entry before applying includes. Keys must refer to declared axes. |

Axes use environment-variable names (`[A-Za-z_][A-Za-z0-9_]*`). Keys differing only in case are rejected. `include` and `exclude` are reserved matrix keys. Values must be strings, integers or booleans; integers and booleans normalize to decimal/lowercase text. Floats, nulls, nested values and duplicate normalized axis values are rejected.

Axis combinations are the Cartesian product in deterministic order. An exclude entry can match part of a combination. Includes compare with the surviving original combinations: they may add metadata keys without replacing original axis values. An incompatible entry becomes a standalone combination; subsequent includes do not expand earlier standalone entries. Duplicate final combinations collapse. An include-only matrix is supported. A matrix that produces no variants is an error.

For literal-operation tasks, each value is injected as `STOKE_MATRIX_` followed by its uppercased key, replacing matching environment entries inherited from the container image. A matrix task cannot explicitly set `env` names with the reserved `STOKE_MATRIX_` prefix (case-insensitively). Keys absent from a variant but present elsewhere in the matrix are set to empty strings so inherited environment values cannot masquerade as matrix inputs. Native Coopr tasks receive matrix values only through explicit `coopr.args` bindings such as `MODE: {matrix: mode}`; they receive no implicit matrix environment. Inline environments accept the same bindings in `environment.args`, while their ordinary steps receive the literal-task matrix environment. Values do not interpolate YAML or image references. The original axis mapping is retained in the plan and report. Variant identifiers are `TASK--matrix-` plus 16 hexadecimal characters derived from the combination; unchanged combinations retain their identifier when unrelated variants are added.

Limits: 256 Cartesian combinations before exclusions, 256 final variants after includes, and 1,024 expanded jobs across the selected workflow. See [task matrices](../guides/matrices.md) for examples, failure handling and output collection.

## Run

```yaml
steps:
  - run: [sh, -eu, -c, 'printf "%s\n" "$MESSAGE" > /out/message']
```

`run` is a nonempty argv list. Stoke adds no shell; pipes, redirection and `$VARIABLE` expansion require an explicit shell as shown. The container image must contain the selected executable; a host step instead names a provisioned executable declared in `host.tools`. A failed command fails its job. Workflow admission and active-work cancellation follow the task's matrix failure policy; see [parallel tasks](../guides/parallel-tasks.md#failure-and-cancellation).

## Host tasks

```yaml
platform: linux/amd64
host:
  tools: [python3]
  inputs:
    - path: src
    - from: prepare
      path: out
      destination: tested
  outputs: [reports]
  inherit-env: [NIX_CONFIG]
  capabilities: [native-build]
  effects: external
cache: false
steps:
  - run: [python3, src/check.py]
    stdout: reports/check.json
```

`host` is a separate execution selector, available only with explicit caller
`--allow-host`. An import or generated plan cannot grant that authority.
Commands use the runner user's permissions and are non-hermetic unless a
complete external contract is established. Host-only runs require no Coopr
service. See [host execution and trust](../guides/host.md).

| Host field | Meaning |
| --- | --- |
| `tools` | Required distinct executable names or absolute paths. Each step's argv[0] must be declared and available. |
| `inputs` | Explicit snapshots. Each entry requires normalized relative `path`; optional `from` selects a prior host capture and `destination` changes the private-workspace path. |
| `outputs` | Explicit normalized relative paths captured after success. A host result contains files, not an image/component. |
| `inherit-env` | Explicit provisioned caller environment names; reserved `STOKE_*` names are rejected. |
| `capabilities` | Named caller-attested requirements, supplied separately with `--host-capability` or `--host-capabilities-json`; no installation or feature enabling occurs. |
| `effects` | `external` by default; `captured` asserts required reusable effects are captured and declared. |
| `toolchain` | Explicit absolute regular-file/directory inputs needed for optional reusable-result identity. |

Host steps accept literal `run` only. Container `copy`, mounts, contexts and the
container `outputs` policy are rejected. Optional step `stdout` captures to a
fresh relative workspace file while retaining normal logs, and is host-only.
The default host workdir is the private workspace root. Host `platform` must
match native `linux/amd64` or `linux/arm64`; it does not select an emulated
container target or provision a runner.

Input snapshots and captures support regular files/directories, with executable
permission bits. Links, special files and special permission bits are rejected.
This is not ownership/ACL/xattr preservation or an automatic Nix-store closure
export. Capture is limited to 512 MiB including tar overhead, 65,536 entries and
8 MiB inventory metadata. Captured outputs are verified and restored as exact
immutable content into fresh storage.

Container `cache: false` can still allow Coopr to store freshly executed results;
host-result caching is used only with explicit host opt-in.

Host `cache: true` additionally requires `effects: captured` and nonempty inputs,
outputs and toolchain. The key includes task/argv, passed environment, native
platform, captured input contents/modes, resolved executable bytes and toolchain
contents. This declaration is an author assertion, not proof that a host command
has no hidden dependencies or side effects. Fresh tests, mutable undeclared
state and uncaptured effects must leave caching disabled. A hit restores outputs
rather than claiming fresh execution. Generic reports distinguish executed
commands from restored output; an inner tool such as Nix must provide separate
provenance for substitution versus actual checker execution.

## Mounts

A task `mounts` list applies to every RUN; a RUN step's `mounts` list adds mounts
for that step. Mounts cannot be attached to COPY steps or override a target
already supplied by the task. Pure `coopr` tasks keep mounts inside their native
definition; they may declare named `contexts` but cannot add task mounts.

| Field | Meaning |
| --- | --- |
| `type` | Required: `context`, `host`, `cache`, `image`, `stage` or `artifact`. |
| `target` | Required normalized absolute container path. |
| `source` | Selected content path; omitted means the whole view. For `host`, required local directory path. Not available for `cache`. |
| `context` | With `type: context`, a declared named context or reserved `stoke-assets`; omitted selects the primary context. |
| `image` | Required container image reference for `type: image`. |
| `from` | Required earlier image-producing task for `type: stage`; adds an implicit dependency. |
| `ref` | Required explicit registry reference with lowercase `@sha256:` digest for `type: artifact`. |
| `read-only` | Defaults to `true` for content/live inputs and `false` for cache volumes. Artifact mounts require read-only access. |
| `id` | Optional native cache-volume identity; available only for `cache`. |
| `sharing` | For `cache`, `shared` or `locked`; omitted retains the native default. `private` is rejected because the current backend does not support it. |
| `relabel` | For live `host` mounts only, explicitly request `shared` or `private` SELinux relabeling of the selected directory. Omitted leaves caller labels unchanged. |

A source selector cannot be combined with fields belonging to another source
kind. Mount fields reject commas, newlines and NUL. Selected content paths cannot
contain a `..` segment; context paths must be relative. Host directories may be
outside the caller context and resolve relative to that context when not
absolute. `contexts` names follow task naming rules; `build` and `stoke-assets`
are reserved. Each declared directory must be available at execution time.

Context inputs are snapshots, not live host binds. Writable primary contexts use
build-scoped copies; writable image/stage inputs are per-RUN overlays. Neither
promises writeback to its source. Live host binds access caller-owned files and
bypass result caching. Cache volumes are optional acceleration, not outputs.
Artifact views contain raw verified files; archives are not unpacked. See
[mount inputs and caches](../guides/mounts.md) for access, lifetime and cleanup.

## Copy

```yaml
steps:
  - copy:
      sources: [src]
      destination: /work/src
  - copy:
      from: build
      sources: [/out/application]
      destination: /application
```

Without `from`, sources come from the host context supplied to `run`, unless `scope: component` selects the owning component's declared files. `scope: context` explicitly selects the default caller source. A component scope requires declared assets. With `from`, sources come from that task's completed image or captured host files and add an implicit dependency. Use capture-relative paths for host file producers. `scope` and `from` cannot be combined. `sources` must be nonempty; `destination` is an absolute container path. Relative source paths cannot escape their context with `..`. Container image paths can be absolute. Native Coopr copy semantics apply to directories and multiple sources.

## Graph rules

`needs`, `from`, `copy.from`, `host.inputs[].from`, stage-mount `from`, native/environment argument `from` bindings and `publish.from` create prerequisite edges. Unknown references and cycles are errors. Plans use stable topological order. During execution, independent ready tasks and matrix variants can overlap within the internal four-job bound and per-matrix limits. A task becomes ready when all prerequisite logical tasks have passed; it need not wait for unrelated running branches. `needs` waits for every producer variant to succeed; all data-source edges require a producer with exactly one variant. `from` and stage mounts require an image. `copy.from` also accepts captured host files; `host.inputs[].from` selects captured host output. Native/environment argument and publication sources may be images or components, not host captures. Publication tasks cannot supply a build value. A typed registry `ref`, image mount or artifact mount is a Bale input declaration rather than a workflow task edge. Target selection includes the complete dependency closure. Checks validate the whole definition, including tasks outside that closure. Final reports retain plan order and include selected jobs that never started as `skipped`. The workflow-wide bound is internal; `strategy.max-parallel` retains its per-matrix meaning. See [parallel tasks](../guides/parallel-tasks.md).

Shared template resolution is described in [shared workflows](../guides/shared-workflows.md). There is no implicit host environment forwarding, services, secrets mechanism or expression interpolation. Matrix labels do not provision hosts or enable another architecture.

## Native Coopr build

```yaml
coopr:
  files: [image.coopr]
  kind: image
  target: release
  args:
    VERSION: {input: version}
    BASE: {from: build}
    MODE: {matrix: mode}
```

This fragment shows available fields; input declarations, task sources and matrix keys must exist in the containing workflow.

| Field | Meaning |
| --- | --- |
| `files` | Required nonempty ordered list of definition files relative to the selected root. |
| `files-from` | `context` by default; `component` selects the owning component's declared files. |
| `kind` | `image` by default, or `component`. |
| `target` | Optional native Coopr target name. |
| `args` | Mapping of argument names to literal strings or explicit input/task/matrix/registry bindings. Names follow environment-variable naming rules. |

Every argument value is exactly one string, `{input: NAME}`, `{from: TASK}`, `{matrix: KEY}` or `{ref: REFERENCE}`. An input binding resolves a declared component input to a literal native argument before execution. Numeric-looking literal arguments must be quoted. A task binding supplies `containers-storage:IMAGE_ID` for an image or the native component digest reference for a component, within the run's shared stores. A matrix binding requires a key declared by the task's axes or includes; a declared key absent from one variant supplies an empty string. A `ref` binding requires an explicit registry and a lowercase `@sha256:` digest. It adds a direct Bale dependency and receives verified destination substitution before API submission. Ordinary string arguments remain literal and are not dependency discovery. None of these bindings is YAML expression interpolation; Coopr owns subsequent native argument expansion.

Native tasks cannot combine `env`, `workdir` or `steps` with `coopr`. Use arguments supported by the definition, use `environment` for ordinary steps on a definition-built image, or add a separate literal-operation task using `from`. The files must resolve to distinct existing regular files inside the selected canonical root; escaping paths and symlink escapes are rejected. `files-from: component` requires declared assets and preserves their imported ownership. It changes definition selection only: native COPY still reads caller context by default. Use `from="stoke-assets"` for native COPY of declared helpers, and reserve that native context name. Stoke checks file availability while Coopr parses and validates syntax. Packaging-time contexts are not automatically retained as invocation overrides in generated Coopr components; use native PACKAGE capture for embedded payloads.

## Task environment

`environment` uses the same `files`, `files-from`, `target` and `args` fields as a
native build, but only an image can supply the environment (`kind` omitted or
`image`). It is exclusive with every other task selector. Ordinary `steps`,
`env`, `workdir`, `contexts` and `mounts` apply after that image is built. Task
environment values override matching inherited values; an explicit workdir
replaces the inherited workdir.

Stoke lowers environments to native producer tasks and ordinary `from`
consumers after input/matrix binding. Equivalent resolved inputs and prerequisite
gates share the environment producer within the run; Coopr still owns build
execution and reuse. Generated producers appear in run reports. No intermediate
registry push is needed. An explicit `coopr` task followed by `from` remains
valid. See [task environments](../guides/environments.md).

## Outputs

Place an `export` operation inside an ordinary task's ordered steps:

```yaml
steps:
  - run: [sh, -eu, -c, 'mkdir -p /reports; echo first > /reports/result.txt']
  - export:
      source: /reports
      destination: reports
  - run: [sh, -eu, -c, 'echo later > /reports/result.txt']
```

The export snapshots the file or directory at that position. Later RUNs do not change the already captured bytes: this example exports `first`, while the final image contains `later`. A `source` is a canonical absolute container path; a `destination` is a canonical relative caller-workspace path. A directory's contents appear under that exact destination. Paths are literal, without globs or matrix interpolation. Imported tasks retain the caller workspace destination, not their component asset directory.

Export sources cannot be `/`; destinations cannot escape the workspace, overlap one another, replace existing paths, or target the root `.git`, `.stoke` or `stoke.yaml` paths and their subtrees. Use distinct destinations when multiple jobs export files. Files and directories preserve executable modes and relative symlinks that resolve within the selected directory. Dangling or escaping links and special files are rejected.

The workspace parent must be writable for private export staging, and each destination must use that same filesystem. Stoke rejects unsupported layouts before starting tasks and never overwrites an existing destination. The engine must advertise `selected-file-exports`; older engines fail capability preflight.

Cached prefixes restore the corresponding selected files without rerunning cached instructions. When an ordinary RUN fails before a following export, its declared report paths are attempted from that failed filesystem before builder teardown. The task remains failed and dependent success gates remain blocked. A missing source or failed export fails an otherwise successful task; capture errors do not replace the original execution failure. Stoke does not upload these paths to a CI artifact service.

Image tasks export an OCI layout at `JOB/image`. Without export steps, they retain the default flattened filesystem at `JOB/rootfs`. A task with explicit export steps defaults to only those selected files; `outputs: {filesystem: true}` additionally requests the full rootfs, while `filesystem: false` always disables it. These controls do not remove the native image or prevent later `from` and `copy.from` consumers. `--output` relocates the run's OCI/rootfs outputs; export destinations remain relative to the caller workspace. Generated CI jobs omit unused OCI output and portable-cache writes for fresh `cache: false` gates with no full filesystem output; selected file exports and success gates are preserved. This requires the engine's `optional-oci-output` capability.

Native `coopr` tasks cannot mix `steps` into their definitions. Export from an ordinary task using `from: build`, or use an inline native `environment` with ordinary steps. Exports do not address discarded internal stages of a native definition. Component tasks export an OCI archive at `JOB/component.oci.tar` and a native component reference; they cannot export a flattened filesystem or serve as a container base/COPY source. Native definitions, components, data producers and ordinary local runs retain their automatic OCI output. Host tasks retain their separate `host.outputs` contract.

## Publish a built artifact

```yaml
publish:
  from: build
  destinations: [registry.example.com/team/image:tested]
needs: [test]
```

`from` names an image or component producer and adds a dependency. `destinations` is a nonempty list of distinct literal Coopr transfer destinations; it can name registry references or supported local transports such as `oci:/absolute/path/to/layout`. Local output paths must be fresh and separate from context, stores and cache. `needs` adds success gates such as tests before publication.

A publication task always performs its transfer when selected. It cannot set `env`, `workdir`, `steps`, `cache`, `strategy` or `outputs`. Build caches do not suppress publication. Destinations are outputs: they are not declared as Bale input dependencies or rewritten by workflow relocation. Multiple destinations are not an atomic transaction; reports preserve individual destination statuses, including partial completion on failure. See [native builds and publication](../guides/native-coopr.md).
