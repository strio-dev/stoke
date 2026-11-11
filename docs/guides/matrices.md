# Run task matrices

A matrix runs one build task definition for several combinations of values. Stoke expands each combination into a separate job. Literal-operation tasks receive matrix environment variables; native Coopr tasks bind selected values explicitly as arguments. Variants and independent logical tasks can overlap. The scheduler's internal four-job bound and each matrix's `strategy.max-parallel` limit active work together.

## Define combinations

The checked-in `examples/matrix/stoke.yaml` uses two axes:

```yaml
version: 1
tasks:
  test:
    image: docker.io/library/busybox:1.37
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
    steps:
      - run: [sh, -eu, -c, 'printf "%s %s\n" "$STOKE_MATRIX_MODE" "$STOKE_MATRIX_TOOLCHAIN"']
  complete:
    image: scratch
    needs: [test]
```

The two axes produce four combinations. `exclude` removes `release` with `next`; `include` adds `debug` with `experimental`, leaving four variants. `complete` waits until every selected `test` variant succeeds.

```sh
stoke check -f examples/matrix/stoke.yaml
stoke plan -f examples/matrix/stoke.yaml complete
stoke run -f examples/matrix/stoke.yaml complete
```

These commands require the same [rootless runtime setup](../getting-started.md) as other Stoke workflows. The example uses a single image on the current host; it does not provision machines or demonstrate execution on other architectures.

## Pass values to commands

Axis `mode` becomes `STOKE_MATRIX_MODE`; axis `toolchain` becomes `STOKE_MATRIX_TOOLCHAIN`. Strings, integers and booleans are accepted and normalized to text. Axis names follow environment-variable naming rules; names that differ only by case are rejected because they would produce the same variable.

Values are literal. Stoke does not substitute `${{ matrix.mode }}` expressions, change image strings or forward matrix values into arbitrary YAML fields. The explicit `platform: {matrix: KEY}` binding selects a container target for each variant; it still requires a compatible native runner or runner-provided execution support. See [container platforms](platforms.md). Use an explicit shell to expand the injected variables inside a command. `STOKE_MATRIX_` environment names are reserved for matrix tasks and cannot be set explicitly in `env` (case-insensitively). Injected values replace matching environment entries inherited from the container image; each variant receives its own copied task environment. A key present in another combination but absent from this one is injected as an empty string, clearing inherited values for that matrix variable.

## Control overlap and failure

`strategy.max-parallel` defaults to `4` and limits active variants in that logical task. Set it to `1` for serial variants within that task. Explicit values must be integers from `1` to `256`; `0` is rejected. The current scheduler also admits at most four jobs across the whole workflow. A variant needs a free slot under both bounds. Setting one matrix to `max-parallel: 1` serializes that matrix's variants while independent ready tasks may still overlap. See [parallel tasks](parallel-tasks.md).

`strategy.fail-fast` defaults to `true`: a failing variant stops workflow admission and cancels active work, including sibling variants and independent tasks. With `false`, Stoke lets that matrix's active and queued siblings finish before its failed logical task stops the workflow. Unrelated tasks may continue during that interval, while dependents of the failed matrix remain blocked. Another failed logical task or external cancellation can still cancel those siblings. In both modes the workflow exits unsuccessfully; disabling fail-fast never turns a failed variant into a pass.

Cancellation from a signal cancels the whole run and waits for active cleanup. Reports include every selected job in plan order; variants and downstream jobs that never start are `skipped` with a reason. Each variant has separate output paths; variants share the run's native store roots and Coopr-coordinated portable cache. [Cache policy](caching.md) remains per task and therefore applies to all its variants.

## Use dependencies and outputs

`needs: [test]` is an all-variant success gate. It does not copy artifacts or choose one variant's image. A `from: test`, `copy.from: test`, native argument `{from: test}` or `publish.from: test` edge is rejected when `test` expands to multiple variants: choosing a producer implicitly would be ambiguous. A producer with exactly one expanded variant can supply a compatible image/component normally. Publication tasks cannot themselves declare a matrix.

Variant names are stable: `TASK--matrix-` followed by 16 hexadecimal characters derived from the normalized combination. Plans and run reports retain the logical task name and a `matrix` object so you can identify a variant without decoding its name. Use the recorded OCI and filesystem paths to collect artifacts. There is no automatic artifact fan-in or variant selector syntax yet.

## Share a matrix template

OCI publication retains the declarative, unexpanded matrix. A caller using an imported task inherits its strategy. If the caller supplies `strategy`, it replaces the whole template strategy, including concurrency and fail-fast settings; axis lists are not merged. Sibling uses keep independent copies.

Matrix values are explicit execution inputs, not YAML string substitutions. Bale dependencies continue to declare the workflow's static images and direct imports. Stoke does not discover registry references hidden in native arguments or definition files, and matrix expansion does not add dependency declarations. See [shared workflows](shared-workflows.md).

## Build native Coopr variants

```yaml
version: 1
tasks:
  image:
    coopr:
      files: [image.coopr]
      args:
        MODE: {matrix: mode}
        VERSION: "1.2"
    strategy:
      matrix:
        mode: [debug, release]
    outputs: {filesystem: false}
  complete:
    image: scratch
    needs: [image]
```

Each native build receives its `MODE` argument and constant `VERSION`. The Coopr definition must declare and use those arguments according to Coopr's frontend rules. Stoke does not inject `STOKE_MATRIX_MODE` into native builds or turn arbitrary fields into expressions. A binding key must occur in the matrix axes or includes; an include-only key missing from a particular variant resolves to an empty string.

Each variant exports a separate OCI artifact. Native frontend cache counters are unavailable in current reports and are omitted; available literal-program counters retain their normal meaning. Coopr owns reuse for the native arguments, definitions and build context. See [native builds](native-coopr.md).

## Build named Nix outputs

`examples/matrix/nix.yaml` illustrates a matrix over named flake outputs. It runs a command equivalent to:

```sh
nix build --extra-experimental-features 'nix-command flakes' \
  --no-link ".#$STOKE_MATRIX_OUTPUT"
```

Replace the placeholder image with a reviewed Nix-enabled image, provide the actual flake and output names, and qualify its writable store, networking, sandbox policy and rootless behavior before use. The example has not been execution-tested against a Nix container; matrix scheduling tests do not prove nested Nix execution works. Both example output names target `x86_64-linux`; matrix labels do not provision another architecture, enable emulation or add a host-execution mode.

The current limit is 256 combinations per matrix before exclusions, at most 256 resulting variants after includes, and 1,024 expanded jobs in a selected workflow. Empty matrices or an expansion with no remaining variants are errors. See [the YAML reference](../reference/workflow.md#strategy-and-matrix) for inclusion rules and limits.
