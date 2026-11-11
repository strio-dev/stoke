# Select a container platform

Task `platform` selects a Linux container target such as `linux/amd64`,
`linux/arm64` or `linux/arm/v7`. Stoke passes the target to Coopr's existing
execution and build paths. It does not choose a CI runner, allocate another
machine or install an emulator.

## Select a target explicitly

Use a literal target on an image task:

```yaml
version: 1
tasks:
  inspect:
    image: docker.io/library/alpine:3.23
    platform: linux/arm64
    steps:
      - run: [uname, -m]
```

This example requires an available image for the target and a runner capable of
executing its commands. A Linux ARM64 runner can execute matching binaries
natively; another architecture needs a compatible runner-provided execution
mechanism. Selecting the target does not establish those prerequisites.

The form is `linux/ARCH` or `linux/ARCH/VARIANT`, with lowercase names. Common
aliases normalize before execution: `linux/x86_64` becomes `linux/amd64` and
`linux/aarch64` becomes `linux/arm64`. Nix system names such as `x86_64-linux`
are not container-platform selectors. A syntactically valid architecture still
needs matching image content and runtime support.

## Bind an input or matrix value

`platform` accepts exactly one literal string, `{input: NAME}` or
`{matrix: KEY}`. This is an explicit binding, not interpolation:

```yaml
version: 1
inputs:
  target:
    default: linux/amd64
tasks:
  inspect:
    image: docker.io/library/alpine:3.23
    platform: {input: target}
    steps:
      - run: [uname, -m]
```

The input follows the normal declared-string rules. Empty or invalid bound
values fail rather than silently selecting the host platform. Supply a target
with the existing `--input target=linux/arm64` invocation option.

A matrix can bind each variant's target:

```yaml
platform: {matrix: target}
strategy:
  matrix:
    target: [linux/amd64, linux/arm64]
```

This is a task-field fragment, not a standalone workflow. The matrix key must
exist and every selected variant must supply a valid nonempty value. Stoke
expands jobs within the current invocation; it does not request an ARM runner
from the host CI. See [task matrices](matrices.md).

Reusable templates can own a platform selector, including a binding to a
declared template input. A `uses` caller can supply that input through `with`,
but cannot override the template by adding `platform` at the callsite.
Publication tasks also reject a platform override: delivery preserves the
built artifact rather than rebuilding or relabeling it.

## Run the repository example

`examples/platforms/stoke.yaml` binds `platform` from a matrix containing
`linux/amd64` and `linux/arm64`. Its inline environment uses
`examples/platforms/environment.coopr` and the Alpine image. Each variant prints
its requested target, runs `uname -m`, then executes it through a child shell.
The task disables execution-result reuse with `cache: false`.

From the repository root, with a compatible development engine selected:

```sh
stoke check --context . -f examples/platforms/stoke.yaml
stoke plan -f examples/platforms/stoke.yaml test
stoke run --context . -f examples/platforms/stoke.yaml test
```

The runner must support both targets and have access to their image content.
`just platform-integration` runs the native acceptance fixture with a selected
compatible Coopr executable. It requires an AMD64 Linux runner with existing
ARM64 QEMU/binfmt support. `STOKE_PLATFORM_EVIDENCE_DIR` can select a short,
private disk-backed parent for retained test evidence.
This example uses a tag for the walkthrough; use a reviewed digest for
reproducible input selection. Native ARM-host execution and foreign execution
through a runner's preconfigured handler are separate qualification cases.

## Defaults and inheritance

| Task form | Platform behavior |
| --- | --- |
| `image` with no `platform` | Coopr's engine-host default. |
| `from` with no `platform` | Inherits the producer's actual reported image platform. |
| `from` with an explicit target | Must match the producer's actual image platform; a conflict fails. |
| Native `coopr` build | Supplies a native build default; explicit platform choices in native FROM stages remain effective. |
| Inline `environment` | Supplies the native environment build target, then constrains the consumer to the resulting image platform. |

An explicit native FROM can produce an image whose actual platform differs
from the task's build default. That actual result is authoritative for a later
`from` consumer. Stoke does not change an existing stored image's architecture
label to make a conflicting consumer request appear to succeed.

An inline environment's target participates in producer identity along with
resolved definitions, arguments, contexts, cache policy and prerequisite gates.
Consumers share a producer only when those inputs match. The explicit
`coopr` build followed by `from` pattern remains supported. Component artifacts
remain distinct from runnable images, regardless of their platform metadata.
See [task environments](environments.md).

## Plans, reports and caches

`stoke plan` shows a task's requested target after input/matrix binding.
The JSON run report records the actual `platform` returned by Coopr for built
images. A requested native build default and an actual final-image platform can
therefore differ legitimately. Use the result field when inspecting artifacts;
do not infer their platform from a matrix name or runner label.

Coopr owns platform-aware source resolution and cache identity. A result for
one target must not be treated as the same image for another target. Cache
reuse does not prove that tests ran again or ran natively: a successful cached
test records an earlier execution. Use `cache: false` or `--no-cache` when a
fresh execution is required. See [cache policy](caching.md).

## Engine and runner requirements

Explicit targets on native `coopr` builds and inline environments require a
development engine advertising `build-platform`. Ordinary image programs use
Coopr's existing program-platform transport. Use Coopr `0.6.0`, selected on PATH
or through `STOKE_COOPR_BINARY` or `--coopr-binary`.

Foreign-architecture RUN uses Coopr/Buildah's existing runtime path and any
QEMU/binfmt setup already supplied by the runner. Stoke bundles no emulator,
invokes no separate QEMU execution backend, changes no host registration and
adds no emulator-installation option. Without compatible execution support,
foreign commands fail; an image's platform metadata alone is not execution
support.

The runner still needs the normal rootless namespaces, mappings, storage and
mount permissions. Emulated Linux ARM64 execution on a configured x86_64
runner is separate from qualification on a native ARM host. Native ARM-host
execution remains unqualified in this milestone; no Windows, macOS or arbitrary
architecture runtime support is implied. See [runtime and security](security.md).
