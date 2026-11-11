# Use a Coopr definition as a task environment

Use `environment` when ordinary Stoke steps need an image built from a Coopr
definition. Coopr builds the environment image; Stoke runs the task from its
native image identity without publishing an intermediate image to a registry.
Coopr retains parsing, dependency planning, execution and caching.

## Run the repository example

The checked-in `examples/environments/stoke.yaml` uses
`examples/environments/environment.coopr` to build a Go test environment. Its
primary caller context is mounted read-only at `/workspace`, and two locked
cache volumes hold Go build/module caches. The environment definition supplies
the working directory and disables CGO; the ordinary step runs Stoke's Go tests.
The task sets `cache: false` and invokes `go test -count=1`, so neither Coopr's
execution-result cache nor Go's test-result cache suppresses that test command.
Go compilation and module caches can still accelerate it.
The task retains its OCI output and run record without exporting an additional
flattened copy of the Go toolchain filesystem.

From the repository root, with a compatible engine selected:

```sh
stoke check --context . -f examples/environments/stoke.yaml
stoke plan -f examples/environments/stoke.yaml quality
stoke run --context . -f examples/environments/stoke.yaml quality
```

The example needs the rootless runtime setup, initial access to the Go container
image and module dependencies, and directories writable by the mapped account
for its caches. It must also work with empty caches. It does not require host Go
or host-Nix execution.

Execution-result reuse and Go cache-volume reuse are separate. A successful
cached test result records an earlier execution, not a new test run. Use
`cache: false` on the task or `--no-cache` for the invocation when the tests must
run again; the disposable Go caches can still accelerate that execution.

## Select the definition and target

This task fragment assumes an available `ci.coopr` definition with an image
target named `tools`:

```yaml
test:
  environment:
    files: [ci.coopr]
    target: tools
    args:
      MODE: release
  env:
    TEST_MODE: strict
  workdir: /workspace
  steps:
    - run: [check-project]
```

The fragment describes the fields; it is not runnable without that definition
and its `check-project` program. `environment` accepts the native build fields
`files`, `files-from`, `target` and `args`; `kind` may be omitted or `image`.
Component artifacts are callable build artifacts, not runnable task environments,
and `kind: component` is rejected.

Files are ordered, distinct regular files relative to the caller context by
default. `files-from: component` selects the owning component's declared assets.
Stoke checks containment and availability; Coopr parses and executes the files.
See [native builds](native-coopr.md) for file rules and native argument scope.

Arguments use the same string, `{input: NAME}`, `{from: TASK}`, `{matrix: KEY}`
and digest-pinned `{ref: REFERENCE}` bindings as `coopr.args`. Input and matrix
bindings resolve before execution; task bindings add dependency edges. These
bindings do not interpolate arbitrary YAML text.

## Inheritance and precedence

The environment image supplies its filesystem and image configuration. Task
`env` entries override matching inherited environment values, and task `workdir`
overrides the inherited working directory. Omitted values keep the image's
configuration. RUN steps use literal argv; request an explicit shell when shell
interpretation is needed.

A task selects exactly one of `environment`, `image`, `from`, `coopr`, `publish`
or `uses`. Combining selectors is an error. `environment` can be combined with
ordinary `steps`, `env`, `workdir`, `contexts`, `mounts`, `needs`, `strategy`,
`cache` and image `outputs` settings. Native mounts inside the definition remain
Coopr's responsibility; task and step mounts apply to the ordinary RUN steps.

## Keep the two-task form

The explicit build-and-inherit form remains supported:

```yaml
tools:
  coopr:
    files: [ci.coopr]
    target: tools
    args:
      MODE: release
test:
  from: tools
  env:
    TEST_MODE: strict
  workdir: /workspace
  steps:
    - run: [check-project]
```

This is the same field example as above, with the environment build named
explicitly. Use it when other tasks also need the producer's image or when an
explicit producer name makes the workflow easier to inspect.

Stoke lowers inline environments to native producer tasks after input/matrix
binding and definition-file resolution. Equivalent resolved definitions,
arguments, contexts, cache policy and prerequisite gates share a producer within
the run. Different gates remain separate so sharing cannot bypass a required
test or introduce a dependency cycle. A task-level `platform` also participates
in producer identity; its consumer must match the actual resulting image
platform. See [container platforms](platforms.md). Coopr decides whether the
underlying build instructions reuse cached work.

Generated producers have names beginning `stoke-environment-` and appear in the
run report. They use the existing native build output and `from` path; this is
not a second build solver or a registry publication. Environment images and
consumer images share the run's native store lifetime. Successful requested
exports remain caller-owned; cancellation drains the same Coopr cleanup path as
other tasks.

## Runtime and release requirements

Use Coopr `0.6.0` on PATH, or select it through `STOKE_COOPR_BINARY` or
`--coopr-binary`. An environment with [mounts](mounts.md) additionally requires
`run-mounts`.

The host must support Coopr's rootless container execution, input access and
selected native storage/runtime configuration. An inline environment does not
install a runner, allocate a machine or make another architecture executable.
It does not introduce host shell or host-Nix execution. See
[runtime and security](security.md) and [cache policy](caching.md).
