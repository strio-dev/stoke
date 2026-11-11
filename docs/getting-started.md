# Get started

Build Stoke from this checkout and validate a workflow before running it. No published Stoke binary release is required or assumed.

## Build with Nix

From the repository root:

```sh
nix develop path:.
go build -o /tmp/stoke ./cmd/stoke
/tmp/stoke version
```

The development shell supplies the selected Go toolchain and development tooling. `just build` builds the Nix package; run `./result-stoke/bin/stoke --help` afterward. `nix run path:. -- --help` runs the same packaged command.

Stoke is an independent Go client. Install Coopr `0.6.0` for container execution. The engine executable and the SDK in this checkout share the release source revision. See [engine selection](guides/coopr-api.md) for installation and the matching source build.

Set `STOKE_COOPR_BINARY` if your Coopr executable is outside PATH.

```sh
export STOKE_COOPR_BINARY=/absolute/path/to/compatible/coopr
```

Alternatively pass `--coopr-binary` to `stoke run`. Without either setting, Stoke searches PATH for `coopr`; it validates API version/capabilities and fails if incompatible. See [engine selection and lifecycle](guides/coopr-api.md).

## Prepare rootless execution

Use a regular Linux account with user namespaces enabled, subordinate UID and GID allocations in `/etc/subuid` and `/etc/subgid`, and working `newuidmap` and `newgidmap` helpers. Container execution also needs an OCI runtime such as `crun`, an appropriate containers/image signature policy and usable containers/storage support.

Host policy applies even inside another container or CI runner. A machine with user namespaces disabled or blocked mounts may build Stoke successfully while preventing execution. Stoke does not fall back to privileged execution automatically. See [security and runtime](guides/security.md).

## Validate, inspect, run

```sh
/tmp/stoke check -f examples/first-workflow/stoke.yaml
/tmp/stoke plan -f examples/first-workflow/stoke.yaml artifact
/tmp/stoke run -f examples/first-workflow/stoke.yaml --context examples/first-workflow artifact
```

`check` validates definitions and resolves imports without running task commands. OCI imports require registry access. `plan` prints the selected dependency closure in stable order. `run` automatically schedules ready tasks concurrently and retains outputs and a run report. The current scheduler admits at most four jobs at once across the workflow; each matrix's `strategy.max-parallel` also applies. See [parallel tasks](guides/parallel-tasks.md). Follow [the tutorial](tutorials/first-workflow.md) for the artifact and [the CLI reference](reference/cli.md) for paths and flags.

For existing Coopr definitions, follow [native image/component builds](guides/native-coopr.md). They can feed normal `from`/`copy.from` tasks and explicit publication tasks in the same workflow. A run with no targets selects every root task, including declared publication; choose a build/test target when delivery is not wanted.

Flags precede target names. The default definition path is `stoke.yaml`. For local definitions the context defaults to the directory containing the YAML file; for an OCI root it defaults to the current directory. Use `--context` to select another caller directory. Reusable templates can separately carry an explicit `assets` allowlist; select those files with component scope. See [packaged components](guides/packaged-components.md).
