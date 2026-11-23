# Build, test and export a file

Turn a text file into an uppercase artifact, test it and export just that file.

## Set up the workspace

Complete [getting started](../getting-started.md). The workflow pulls BusyBox
from Docker Hub. Create a workspace with the input file.

```sh
workspace="$(mktemp -d)"
cd "$workspace"
printf '%s\n' 'hello from stoke' > message.txt
```

Save this workflow as `stoke.yaml`.

```yaml
version: 1

tasks:
  build:
    image: docker.io/library/busybox:1.37
    workdir: /work
    steps:
      - copy:
          sources: [message.txt]
          destination: /work/message.txt
      - run: |
          mkdir -p /out
          tr a-z A-Z < message.txt > /out/message.txt

  test:
    from: build
    cache: false
    steps:
      - run: |
          test "$(cat /out/message.txt)" = "HELLO FROM STOKE"

  artifact:
    from: build
    needs: [test]
    steps:
      - export:
          source: /out/message.txt
          destination: dist/message.txt
```

`run: |` executes a POSIX shell script with `sh -eu`: command failures and unset variables fail the step, subject to the shell's normal rules. The image must supply `sh`.

Both `test` and `artifact` use `from: build` to consume the build's completed image. The artifact's separate `needs: [test]` waits for test success; it does not supply data. A failed test prevents the artifact task from running. `cache: false` keeps the test fresh. The export step copies only `/out/message.txt` to `dist/message.txt` in the caller workspace; it needs neither a scratch image nor a full filesystem export.

## Inspect the plan

```sh
stoke check
stoke plan artifact
```

The selected order is `build`, `test`, `artifact`. The plan distinguishes the artifact's `needs: test` success gate from its `data-from: build` input. Tasks outside this closure are not executed. Without targets, Stoke selects every root workflow task.

## Run and inspect the result

```sh
stoke run artifact
cat dist/message.txt
```

The last command prints `HELLO FROM STOKE` followed by a newline. The run also retains its normal image outputs and run report in the reported run directory. No flattened rootfs is requested. `--output` can relocate that retained directory; it does not relocate the selected file, whose destination is relative to `--context`.

## Run again

Run the same target again in the same workspace.

```sh
stoke run artifact
```

A local rerun updates matching files and merges directories, preserving
unrelated entries. Use a separate workspace to keep an earlier export. A
custom `--output` or `--events` path must still be fresh. A missing export source fails the task; a failed test leaves the gated artifact unexported. Exporting reports from a failed command is covered in [selected-file exports](../reference/workflow.md#outputs).

Coopr can reuse cached build instructions and selected outputs; the test still runs because its cache is disabled. Cache counters describe portable cache decisions, not all native storage reuse. Read [caching](../guides/caching.md) before deciding whether to cache real tests.
