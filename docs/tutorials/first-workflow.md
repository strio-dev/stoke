# Build, test and export an artifact

Run the checked-in example at `examples/first-workflow/stoke.yaml`. It turns a text file into an uppercase artifact, verifies the result and packages only that file in a scratch image.

## Before you start

Complete [getting started](../getting-started.md). Run commands from the repository root with `stoke` on your PATH, or substitute `/tmp/stoke`. You need registry access for `docker.io/library/busybox:1.37` and a working rootless execution environment.

## Read the workflow

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
      - run: [sh, -eu, -c, 'mkdir -p /out; tr a-z A-Z < message.txt > /out/message.txt']
  test:
    from: build
    cache: false
    steps:
      - run: [sh, -eu, -c, 'test "$(cat /out/message.txt)" = "HELLO FROM STOKE"']
  artifact:
    image: scratch
    needs: [test]
    steps:
      - copy:
          from: build
          sources: [/out/message.txt]
          destination: /message.txt
```

`from: build` starts the test from the build's completed image. The artifact copies from the build but also declares `needs: [test]`, so a failed test prevents the artifact task from running. `cache: false` keeps the test command from being replaced by a cached result.

## Inspect the plan

```sh
stoke check -f examples/first-workflow/stoke.yaml
stoke plan -f examples/first-workflow/stoke.yaml artifact
```

The selected order is `build`, `test`, `artifact`. Tasks outside this closure are not executed. Without targets, Stoke selects every root workflow task.

## Run and inspect the result

```sh
stoke run -f examples/first-workflow/stoke.yaml --context examples/first-workflow artifact
```

The command reports the retained run directory. Each successful task has an OCI image layout and a flattened filesystem export; `run.json` records execution results. The artifact task's filesystem contains `/message.txt` with `HELLO FROM STOKE` followed by a newline.

For a known location, choose a fresh path outside the input context:

```sh
stoke run -f examples/first-workflow/stoke.yaml \
  --context examples/first-workflow --output /tmp/stoke-first-run artifact
```

The destination must not already exist. Keep execution stores and cache outside the context to avoid copying generated state back into later builds.

## Run again

Repeat the command without `--output`, or choose a different fresh output directory. Coopr can reuse cached build instructions; the test still runs because its cache is disabled. Cache counters describe portable cache decisions, not all native storage reuse. Read [caching](../guides/caching.md) before deciding whether to cache real tests.
