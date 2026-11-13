# Build with native Coopr definitions

Use a `coopr` task when a build already lives in Coopr definition files or produces a reusable Coopr component. Stoke orders the workflow, supplies explicit arguments and retains results; Coopr parses its native definitions and owns their component/build graph, execution and cache behavior. This does not make Coopr's native grammar a second Stoke workflow language.

Examples with export steps require the [pinned Coopr candidate](coopr-api.md#select-a-compatible-executable).
Coopr `0.6.0` remains usable for existing native tasks without export steps.
If the executable is outside PATH, select it through `STOKE_COOPR_BINARY`.

```sh
export STOKE_COOPR_BINARY=/absolute/path/to/coopr
```

See [engine selection](coopr-api.md) for the matching Nix source build and
[rootless requirements](../getting-started.md#prepare-rootless-execution) for host setup.

## Build, test and collect an image

The checked-in `examples/native-coopr/stoke.yaml` combines a native component, a native image, literal tests and a copied artifact. Its native build has this shape:

```yaml
version: 1
tasks:
  component:
    coopr:
      kind: component
      files: [component.coopr]
      target: installed
  build:
    coopr:
      files: [image.coopr]
      target: release
      args:
        COMPONENT: {from: component}
    outputs: {filesystem: false}
  test:
    from: build
    cache: false
    steps:
      - run: [sh, -eu, -c, 'test "$(cat /output.txt)" = "HELLO FROM NATIVE COOPR"']
  artifact:
    image: scratch
    needs: [test]
    steps:
      - copy: {from: build, sources: [/output.txt], destination: /output.txt}
      - export: {source: /output.txt, destination: artifacts/native/output.txt}
```

Use the checked-in files together, with the YAML directory as their default context:

```sh
stoke check -f examples/native-coopr/stoke.yaml
stoke plan -f examples/native-coopr/stoke.yaml artifact
stoke run -f examples/native-coopr/stoke.yaml artifact
```

The example's native image uses `docker.io/library/busybox:1.37`, so running it requires registry access and a suitable image trust policy. It is distinct from the acceptance suite's local static-BusyBox fixture. Pin a reviewed base digest for reproducibility.

The component builds first because its argument binding adds a workflow dependency. The `test` task starts from the built native image. The final task copies from that original image only after the test succeeds. `needs` is a success gate; it does not change a task's image or copy its files.

Task-level `platform` can select the native build default. It is a sibling of
`coopr`, not a native build argument or a field inside `coopr`. Explicit platform
choices in native FROM stages remain effective, and run reports retain the
actual output platform. Explicit targets require the engine's `build-platform`
capability. See [container platforms](platforms.md).

## Bind native arguments

Every `coopr.args` value is a literal string or one explicit input, task-output, matrix or registry binding:

```yaml
args:
  VERSION: {input: version}
  BASE: {from: build}
  TOOLS: {from: component}
  MODE: {matrix: mode}
```

This is a field reference, not a complete task: the input declaration, named sources and matrix key must exist in its workflow. Quote numeric and boolean literals when they are native argument strings. Stoke does not evaluate expressions or substitute values into filenames, targets or arbitrary YAML text.

An image task binding supplies a Coopr image source: `containers-storage:IMAGE_ID` for a local result or a digest-pinned registry reference for an exact CI input. A component binding supplies its native component digest reference. Treat these values as source references, not stable literal identifiers. The native definition decides where an argument is consumed—for example, as a FROM source or component reference. Stoke passes values to Coopr's normal argument interface instead of generating native source text. Coopr's normal argument scope still applies: the example redeclares its global `COMPONENT` argument inside the image stage before using it in a component call.

`{input: version}` supplies the declared component input after required/default resolution. A reusable caller passes it with `with`; a local or remote root receives it from `--input version=VALUE`. See [reusable inputs](reusable-inputs.md).

`{matrix: mode}` supplies that variant's normalized `mode` value. The key must be declared in the task's axes or includes; if absent from a particular variant, its value is empty. Native tasks receive no implicit `STOKE_MATRIX_` environment variables. See [native matrix builds](matrices.md#build-native-coopr-variants).

Every task-output argument requires one producer variant. A multi-variant producer can be used as a `needs` gate, but Stoke does not guess which image/component to bind. Publication tasks do not produce build values. A typed `{ref: REGISTRY_REFERENCE}` instead names an explicit registry artifact pinned with `@sha256`; Stoke declares it for Bale and applies verified relocation before calling Coopr. See [OCI component inputs](packaged-components.md#consume-an-oci-coopr-component).

Native `coopr` tasks cannot set `env`, `workdir` or `steps`. Use native arguments according to the Coopr definition, select an inline [task environment](environments.md), or add a separate task using `from: build` for literal operations. Components are callable Coopr build artifacts; they cannot be used directly as container bases or `copy.from` sources.

## Choose exported outputs

Every successful image build exports an OCI image layout. Export steps require
the candidate's `selected-file-exports` capability and ordered state capture.
To export a file from a native build, use an ordinary consuming task and place
`export` in its ordered steps:

```yaml
artifact:
  from: build
  steps:
    - export:
        source: /output.txt
        destination: artifacts/native/output.txt
```

The source is inside that task's filesystem at the export operation; the destination is relative to the caller workspace, including inside a native CI job. Later RUNs cannot change bytes already captured by an earlier export. Imported templates retain the caller destination. Cached prefixes restore their selected files without rerunning instructions. A declared export following an ordinary failed RUN is attempted before cleanup while preserving failure and dependent success gates. Destinations must be fresh, nonoverlapping workspace paths; globs, traversal, reserved control paths, escaping or dangling symlinks and special files are rejected. See [output validation](../reference/workflow.md#outputs).

On a cache hit, Coopr restores the selected native image before copying those paths; its image layers may still need to be downloaded.

A task with export steps omits the full flattened filesystem by default. Other image tasks retain their default rootfs export. `outputs: {filesystem: true}` explicitly includes the full filesystem; `filesystem: false` omits it in either case. Native `coopr` tasks still cannot mix `steps` into their definitions: use `from` or an inline native environment for ordinary export steps. A component exports an OCI archive file and cannot export a flattened filesystem.

| Task output | Recorded path/identity |
| --- | --- |
| Image OCI layout | `outputs/JOB/image`, plus `imageID` and exported `digest`. |
| Exported files/directories | Each export step's `destination` in the caller workspace. |
| Flattened image filesystem | `outputs/JOB/rootfs`, enabled by default without export steps or explicitly with `filesystem: true`. |
| Component OCI archive | `outputs/JOB/component.oci.tar`, plus native `reference` and exported `digest`. |

With `--output`, its fresh directory replaces the `outputs` prefix. Use the paths in the JSON run report rather than inferring matrix job names. OCI and filesystem exports are separate promotions, so a failure can leave partial evidence. A component archive is a file, not an image-layout directory.

The exported digest identifies the OCI artifact bytes. Native frontend metadata can retain a different pre-export manifest identity. Native definition/component builds omit cache counters when unavailable; absence is not a report of zero reuse. Literal tasks report their available portable-cache counters. See [caching](caching.md).

The same `stoke run --cache-from` and `--cache-to` repository options apply to
native image and component builds. They forward to Coopr's existing registry
cache alongside the local cache; they do not publish the task's final artifact or
synchronize cache-mount data. They require `registry-cache`;
see [registry cache reuse](caching.md#share-results-through-an-oci-registry)
for read/write configuration and failure behavior.

## Publish after tests

The example includes an explicit publication task:

```yaml
publish:
  needs: [test]
  publish:
    from: build
    destinations: [oci:/tmp/stoke-native-example-image]
```

Run it deliberately with a fresh destination:

```sh
stoke run -f examples/native-coopr/stoke.yaml publish
```

Its implicit `from` dependency builds the source image, and `needs: [test]` requires the test to pass before Coopr transfers it. The example writes a local OCI layout; replace destinations with appropriate reviewed Coopr registry/local transfer references when needed. The destination `/tmp/stoke-native-example-image` must not exist. Repeating that example requires choosing another fresh path or deliberately managing the earlier caller-owned output.

A component task can be published by naming it in `publish.from`. Every publication source must have exactly one variant. Destinations remain literal outputs, not relocated workflow inputs. Publication has no matrix, `cache`, `env`, `workdir`, `steps` or `outputs` fields, and always executes when selected—even when its build prerequisites reuse cached work.

Multiple destinations are not atomic. `run.json` preserves each destination's status and error, including successfully delivered destinations when a later transfer fails. Treat these records as partial delivery evidence and do not report the whole task as successful after an error.

`stoke publish` is a separate command for publishing reusable **workflow definitions**. A YAML `publish` task transfers built images/components. Running a workflow without targets selects all its root tasks, including any publication tasks; choose an explicit build/test target when you want only that closure.

## Context, shared definitions and Bale

Native `files` are ordered paths relative to the caller context by default. Set `files-from: component` to select definitions from the owning component's optional `assets` allowlist. Files must resolve to distinct existing regular files inside the selected root; escaping paths and symlink escapes are rejected before execution. Coopr owns native parsing and semantics.

Selecting packaged definitions does not replace the build's caller context. Native COPY reads caller files by default and can read declared helpers with `from="stoke-assets"`. Reserve that context name in asset-using definitions. When generating a Coopr component, packaging-time contexts are not automatically saved as invocation overrides; capture required payloads through Coopr's existing PACKAGE semantics.

Local and imported templates retain their own component files. A local YAML root defaults caller context to its directory; an OCI root defaults to the current directory. Use `--context` for the application source, independently of the package. See [packaged component files](packaged-components.md).

Native definitions and components retain their authored RUN mounts. A task can provide additional named local inputs through `contexts`; ordinary Stoke task/step mounts belong to literal steps, including steps on an inline environment. See [mount inputs and caches](mounts.md).

Bale carries declared imports, static task images, image/artifact mount inputs and typed native or environment `ref` arguments. Stoke applies their verified destination mappings; Coopr handles nested component delivery metadata. Hidden KDL or ordinary string references are not automatically discovered or rewritten, and publication destinations remain outputs. See [shared workflows and Bale](shared-workflows.md).
