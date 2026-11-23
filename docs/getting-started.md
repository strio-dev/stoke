# Get started

Stoke runs on Linux AMD64 and ARM64.

## Install Stoke

```sh
curl -fsSL https://stoke.strio.dev/install.sh | sh
stoke version
```

The installer checks the release archive's checksum and installs to
`/usr/local`, using `sudo` when needed. To install an exact
[release tag](https://github.com/strio-dev/stoke/releases) into a writable
prefix, substitute the tag for `TAG`.

```sh
curl -fsSL https://stoke.strio.dev/install.sh | STOKE_INSTALL_PREFIX="$HOME/.local" sh -s -- TAG
export PATH="$HOME/.local/bin:$PATH"
```

## Prepare rootless execution

Install [Coopr](https://coopr.strio.dev/getting-started/) to run container
workflows. The first workflow needs a Coopr executable that supports
[selected-file exports](reference/workflow.md#outputs). Stoke finds Coopr on
PATH. See [engine selection](guides/coopr-api.md) for executable overrides.

Use a regular Linux account with user namespaces enabled, subordinate UID and GID allocations in `/etc/subuid` and `/etc/subgid`, and working `newuidmap` and `newgidmap` helpers. Container execution also needs an OCI runtime such as `crun`, an appropriate containers/image signature policy and usable containers/storage support.

These requirements also apply when Stoke runs inside a container or CI job. See [security and runtime](guides/security.md).

## Run your first workflow

Follow the [first-workflow tutorial](tutorials/first-workflow.md) to build,
test and export one text file and rerun the workflow in the same workspace.

For existing Coopr definitions, follow [native image/component builds](guides/native-coopr.md). They can feed normal `from`/`copy.from` tasks and explicit publication tasks in the same workflow. A run with no targets selects every root task, including declared publication; choose a build/test target when delivery is not wanted.

Flags precede target names. The default definition path is `stoke.yaml`. For local definitions the context defaults to the directory containing the YAML file; for an OCI root it defaults to the current directory. Use `--context` to select another caller directory. Reusable templates can separately carry an explicit `assets` allowlist; select those files with component scope. See [packaged components](guides/packaged-components.md).
