# Pass inputs to reusable components

Declare a component's configurable values once and bind them explicitly to task environment values or native Coopr arguments. A caller selects an imported task with `uses` and supplies its arguments through `with`. Each invocation gets its own dependency scope, so calling the same component twice can build with different values.

## Declare and use strings

This reusable definition builds a small text artifact and verifies it:

```yaml
version: 1
inputs:
  version:
    type: string
    required: true
  mode:
    default: release
tasks:
  build:
    image: docker.io/library/busybox:1.37
    env:
      VERSION: {input: version}
      MODE: {input: mode}
    steps:
      - run: [sh, -eu, -c, 'mkdir -p /out; printf "%s\n%s\n" "$VERSION" "$MODE" > /out/build.txt']
  verify:
    from: build
    env:
      VERSION: {input: version}
      MODE: {input: mode}
    cache: false
    steps:
      - run: [sh, -eu, -c, 'test "$(sed -n 1p /out/build.txt)" = "$VERSION"; test "$(sed -n 2p /out/build.txt)" = "$MODE"']
```

The complete example is checked in under `examples/reusable-inputs`. `version` is required; `mode` defaults to `release`. Optional inputs without a default become empty strings. A required declaration cannot also have a default, and an explicitly supplied empty string counts as a supplied value. Unknown input names fail resolution.

The only input type is `string`, which is also the default when `type` is omitted. Quote values such as `"1.2"` or `"true"` when their intended type is text. YAML numbers, booleans, nulls, lists and objects are not input values. Input names follow the same naming rules as tasks and imports.

An `{input: NAME}` binding supplies one entire value. It does not interpolate text into commands, image selectors, filenames or other YAML fields. In this example, Stoke binds `VERSION` and `MODE`, then the explicitly selected shell reads those environment variables. Quoting shell expansions keeps each value together.

## Run the definition locally

Root definitions receive inputs from repeated `--input NAME=VALUE` flags:

```sh
stoke check -f examples/reusable-inputs/stoke.yaml --input version=1.2.3
stoke plan -f examples/reusable-inputs/stoke.yaml --input version=1.2.3 verify
stoke run -f examples/reusable-inputs/stoke.yaml --input version=1.2.3 --input mode=debug verify
```

Flags precede positional task names. The CLI splits at the first `=`; later `=` characters remain part of the value. Use `--input note=` for an explicit empty value when `note` is declared. Quote values according to your shell's rules. `check` and `plan` resolve inputs without executing task commands; running requires the compatible Coopr engine and [rootless prerequisites](../getting-started.md#prepare-rootless-execution). This example also needs registry access for BusyBox and a suitable image trust policy.

Input values are ordinary build configuration. They can enter image configuration, execution records and caches; the input mechanism is not a secrets channel.

## Call the component twice

Publish the definition with the [shared-workflow procedure](shared-workflows.md#publish-a-definition), then copy its actual returned digest into a caller:

```yaml
version: 1
imports:
  checks: registry.example.com/team/checks@sha256:REPLACE_WITH_PUBLISHED_DIGEST
tasks:
  debug:
    uses: checks/verify
    with:
      version: "1.2.3"
      mode: debug
  release:
    uses: checks/verify
    with:
      version: "1.2.3"
```

The placeholder deliberately is not a valid reference. Imports require the real 64-character lowercase hexadecimal digest. `release` receives the component's `release` default; `debug` receives `debug`. Both calls get their own `build` prerequisite and bind all tasks in that scope to their invocation values. Defaults are resolved for each invocation, and the fetched definition remains unchanged.

`with` accepts strings or explicit caller input bindings. It is available only alongside `uses`. Inputs belong to the imported component document and apply to the selected task and its prerequisites; they are not inherited implicitly from similarly named caller inputs. Unknown arguments and missing required values fail before task execution.

Local `env`, `workdir`, `cache` and `outputs` overrides retain their existing task-kind restrictions. A local `strategy` replaces the selected template's entire strategy. Caller-added `needs` still points to caller tasks. All imported data edges, including `from`, `copy.from`, native argument task sources and publication sources, follow the invocation's dependency scope.

## Forward inputs through another component

A wrapper declares its own interface and explicitly forwards values:

```yaml
version: 1
inputs:
  version:
    required: true
  mode:
    default: release
imports:
  checks: registry.example.com/team/checks@sha256:REPLACE_WITH_PUBLISHED_DIGEST
tasks:
  verify:
    uses: checks/verify
    with:
      version: {input: version}
      mode: {input: mode}
```

Replace the digest placeholder before using or publishing this wrapper. Forwarding reads the containing component's inputs, then passes concrete strings to the next invocation. Nested component defaults apply only when that caller omits the corresponding argument. Package-owned files retain their original owner through wrappers; input scoping does not copy assets into the caller's application context.

## Bind native Coopr arguments

A native task binds an input through `coopr.args`:

```yaml
version: 1
inputs:
  version:
    required: true
assets:
  - image.coopr
tasks:
  build:
    coopr:
      files: [image.coopr]
      files-from: component
      target: release
      args:
        VERSION: {input: version}
```

The complete native definition is `examples/reusable-inputs/image.coopr`; `native.yaml` contains this task. Stoke passes the resolved string through Coopr's normal argument interface. Coopr owns native argument scope, expansion and caching. Native tasks still cannot set task `env`, `workdir` or `steps`; use the argument names supported by their native definition. Existing task-output, matrix and registry bindings remain available alongside input bindings on different arguments.

## Publication, matrices and caching

`stoke publish` stores the reusable definition's declarations, bindings and optional assets. It does not require a value for a declared required input or publish the current caller's bound values. `stoke pull` preserves the interface. Supply `--input` when checking, planning or running a retrieved or remote root.

Inputs are resolved before task matrices expand. A caller can supply a complete replacement `strategy` for the selected template, while Coopr arguments may separately use `{matrix: KEY}`. `with` does not accept matrix bindings, and inputs do not generate axes or interpolate matrix values.

Bound environment values and native arguments become ordinary Coopr execution inputs. Changed effective values can affect cache reuse. Different invocation names do not create cache shards, and an unused input does not add a Stoke cache key. Coopr owns identity and reuse; Stoke does not add a workflow-result cache. See [caching](caching.md#component-inputs).

Bale discovers declared imports, task images and typed native `{ref: REFERENCE}` arguments. Strings passed through inputs, defaults or `with` are not registry dependency declarations and are not automatically relocated. An argument has one binding kind, so `{input: NAME}` cannot also act as a typed `ref`. Keep portable registry dependencies explicitly declared and use Bale recipe additions for dependencies outside the discoverable graph. See [shared workflows and relocation](shared-workflows.md#relocation).
