# Run explicitly trusted host steps

A `host` task runs literal command arguments as the runner's user, using tools
that are already installed. It is useful for native tools such as Nix, signing
clients or platform checks that need the provisioned host environment. It is
not a container or a hermetic sandbox. A command has that user's filesystem,
network and credential authority.

The caller must pass `--allow-host` to `stoke run` or `stoke ci run`. A workflow
or imported component can request host execution but cannot grant this
permission. Planning or generating a host task does not authorize executing it.
Review imported host commands and the files/tools they can access before opting
in. Stoke installs nothing and changes no host security settings.

## Declare a task

```yaml
version: 1
tasks:
  inspect:
    platform: linux/amd64
    host:
      tools: [python3]
      inputs:
        - path: README.md
      outputs: [reports]
    cache: false
    steps:
      - run:
          - python3
          - -c
          - |
            from pathlib import Path
            Path("reports").mkdir()
            Path("reports/size.txt").write_text(str(Path("README.md").stat().st_size))
```

Run it with an explicit caller context and a fresh output path outside that
context:

```sh
run_dir=$(mktemp -d)
stoke run -f host.yaml --context . --allow-host --output "$run_dir/output"
```

The task receives a private workspace containing only its declared input
snapshots. Relative `workdir` selects a directory inside that workspace; the
default is its root. `run` is argv, with no implicit shell or interpolation.
Every step executable must be listed in `tools`. An explicit shell is an
ordinary provisioned tool, so shell syntax requires an explicit shell command.
A step's optional `stdout: reports/tool.json` additionally captures its standard
output to a fresh workspace file while retaining normal logs.

The selected `platform` is required and must equal the runner's native
`linux/amd64` or `linux/arm64`. Existing explicit matrix/input platform bindings
are supported. A host task cannot use container emulation to satisfy a foreign
platform, select a machine, or provision an OS. Unsupported platforms and
missing tools fail clearly before commands execute. Host-only runs do not
start Coopr; mixed workflows retain their ordinary Coopr execution path.

## Environment and capabilities

The environment starts with the runner's `PATH`, `LANG=C.UTF-8` and `TZ=UTC`.
Stoke provides private `HOME` and `TMPDIR` directories unless you explicitly
supply them. Task `env` entries and literal matrix environment values follow the
ordinary binding rules. `host.inherit-env` declares additional caller
variables to pass; `STOKE_*` names are reserved. For example, a fresh Nix task
may deliberately inherit `HOME` and `NIX_CONFIG` to use provisioned Nix
configuration, credentials and caches. These are live runner dependencies.

`host.capabilities` requests named runner capabilities beyond tool discovery.
The caller attests to each using repeated `--host-capability NAME` flags, or
`--host-capabilities-json '["NAME"]'`. An attestation does not enable a kernel
feature, install a tool, check credentials, or prove sandbox support. Provision
and verify the named capability before attesting to it. An import cannot add
caller attestations.

## Inputs, outputs and data dependencies

Each `host.inputs` entry selects `path` from the caller context, or from a prior
host task's captured output using `from`. `destination` optionally places that
snapshot at a different workspace-relative path. Paths are literal normalized
relative paths, not glob patterns; `..`, absolute paths and overlapping input
destinations are rejected. Neither the launch directory nor an imported
component's entire package is captured implicitly.

```yaml
host:
  tools: [python3]
  inputs:
    - from: prepare
      path: out
      destination: tested
  outputs: [reports]
```

This `from` adds a real data edge. A plain `needs: [prepare]` adds only a success
gate and does not copy its files. Matrix producers with several variants need
an unambiguous concrete output selection; Stoke does not guess which variant
produced the intended data. Imported dependencies retain their own invocation
scope.

Snapshots and captured outputs support regular files and directories, including
executable permission bits. Symlinks, hard-linked files, devices and special
permission bits are rejected; this is not a full filesystem backup format.
Ownership, ACLs, extended attributes and arbitrary filesystem metadata are not
represented. Input snapshots and restored captures normalize file and directory
modification times to the archive epoch, including implicit parents and the
workspace root. Tools receive those normalized times, not original checkout
times. Declare original-time information in a regular input file when needed;
ctime, inode numbers and live filesystem state are outside the reusable content
contract. A Nix `result` symlink is therefore not a captured closure: capture
regular reports or a complete exported OCI layout instead of an unresolved link
into the producer's store.

Only paths listed in `host.outputs` are captured, with their ordinary parent
directories. Each capture is bounded to **512 MiB including tar headers**,
65,536 entries and an 8 MiB inventory. Capture is streamed; archive traversal,
links, mismatched contents, unsupported metadata and corrupted digests fail.
Restoration verifies the exact immutable artifact and installs it into fresh
storage without replacing an existing path. A later container `copy.from` can
consume captured host files through Coopr's named-context input machinery; a
host result is not a runnable image or a Coopr component.

## Caching requires a complete contract

Host tasks default to fresh execution. Leave `cache: false` for tests that must
run, commands with uncaptured effects, mutable external state or any case where
you cannot establish complete input identity. Declared paths alone do not prove
that a host command is hermetic.

Explicit `cache: true` requires `host.effects: captured`, nonempty declared
inputs and outputs, and explicit `host.toolchain` paths. This is the author's
correctness assertion that the reusable result depends only on the declarations
and that required effects are captured. Stoke does not prove the assertion or
prevent arbitrary host reads/writes. A wrong assertion can produce a wrong
cache hit; use fresh execution when the contract is not justified.

The key covers command/task configuration, the full effective environment and
absolute execution directory, native platform/kernel/CPU identity, runner
UID/GID/groups/umask, snapshotted input contents/modes, resolved executable bytes
and declared toolchain contents. Declare complete relevant interpreters, libraries,
configuration and auxiliary tools; hashing a version string is insufficient.
Toolchain inputs are explicit absolute regular-file/directory paths, with
unsupported filesystem entries rejected. Inputs and toolchain are checked again
before publishing a reusable result; mutation prevents reuse.

Reusable commands execute in a stable private workspace under the caller's
canonical temporary directory, with a cancellable per-workspace lock. Private
`HOME` and `TMPDIR` live outside the input tree and are included in the key.
Different execution locations, users or platform identities conservatively
miss. Cacheable input and output trees must not overlap; snapshot a source tree
into `source/` and write outputs elsewhere. A crashed workspace is retained and
reported rather than silently deleted on a later invocation. Completed command
workspaces are removed, including owned read-only directories; unexpected mounts
or ownership prevent cleanup and produce a failure.

For example, a provisioned, self-contained static builder with a known closed
input contract could use:

```yaml
platform: linux/amd64
cache: true
host:
  effects: captured
  tools: [/opt/provisioned/static-builder]
  toolchain: [/opt/provisioned/static-builder]
  inputs:
    - path: src
    - path: build-config.json
  outputs: [dist]
steps:
  - run: [/opt/provisioned/static-builder, --input, src, --config, build-config.json, --output, dist]
```

This tool is an illustrative provisioned executable, not an installer shipped by
Stoke. The static-binary assumption matters; a dynamically linked or interpreted
replacement needs its complete dependencies declared as well.

A cache hit restores captured output files; it is not fresh test execution.
Verified hits also seed explicit `--cache-to` destinations. `--no-cache` bypasses
host result reuse while a declared reusable result can still be stored.
Explicit `--cache-from`/`--cache-to`
repositories can transfer reusable host captures independently of Coopr's image
cache. Empty acceleration caches must not change correctness. Cache entries
cannot stand in for uncaptured host mutations or required external effects.

## Cancellation, logs and CI

Host tasks use the ordinary scheduler, prerequisite gates, matrix failure policy,
failure/cancellation records and skipped-task reporting. Standard output and
error remain in task logs and normal terminal output. Each stream is bounded to
16 MiB; write large outputs to declared files instead of stdout. Selected caller
inputs are snapshotted before host commands begin, so tasks do not implicitly
exchange changes made to the original checkout. Cancellation stops and drains
the owned process group; background children in that group do not become
untracked successful work. Deliberately detached sessions and work submitted to
external daemons are outside process-group containment. Host execution remains
trusted runner execution.

CI generation keeps host stages and matrix variants visible as native jobs.
Native `needs` owns success gating. Cross-job captured files use the existing
run/attempt/variant-scoped immutable OCI result exchange. Tests and later
consumers select the exact producer output; a missing result fails rather than
silently rebuilding a different producer after tests. Disposable cache entries
and exact execution results have different purposes. CI artifact uploads or
success receipts are not required as internal transport.

Generated reusable workflows default `allow_host` to `false`. A handwritten
caller must explicitly set `allow_host: true` and, when needed,
`host_capabilities: '["nix-build"]'`. These are caller authority, not permissions
in the imported YAML or generated plan. The runner owns triggers, machines,
credentials, approvals, tools and native architecture. See the checked-in
`examples/host/README.md` for local use and hosted preparation. No push or hosted
run is implied by generation.

## Actual Nix checks as an ordinary host workflow

`scripts/nix-workflow.py` evaluates the flake's `checks` attribute to discover
supported systems and check names. There is no second maintained list of checks.
Discovery rejects import-from-derivation builds during evaluation; flakes
requiring them need an explicitly designed preparation step. It emits a native-system matrix with `max-parallel: 1`, explicit snapshots under
`source/` and fresh generic host execution. Runtime temporary files and captured
reports stay outside that flake source tree. `scripts/run-nix-check.py` runs one selected
check using provisioned Nix with one build job and two cores. Nix retains its
own package/dependency graph, sandbox settings and binary-cache behavior.

A host command marked `executed` does not prove that Nix executed its checker.
The example captures a separate `reports/nix.json` provenance record:

| Realization | Meaning |
| --- | --- |
| `built` | The exact check derivation had a completed, directly observed local builder activity and Nix succeeded. |
| `substituted` | Target outputs were fetched through observed substitution activities; the checker was not freshly executed here. |
| `already-valid` | Required outputs were locally valid before the invocation, with recognized activity protocol and no target build activity. |
| `unknown` | Evidence or activity schema was insufficient; no fresh-builder claim is made. |
| `failed` | Nix returned failure; the host task and its dependents retain ordinary failure gates. |

The parser matches exact derivation/output identities, not unrelated dependency
builds. It uses upstream Nix 2.34.2's
[activity types](https://github.com/NixOS/nix/blob/2.34.2/src/libutil/include/nix/util/logging.hh)
and [JSON logger](https://github.com/NixOS/nix/blob/2.34.2/src/libutil/logging.cc),
with build/substitution fields from the corresponding store implementations.
This is an example-script compatibility boundary, not a Nix executor inside
Stoke. Unsupported or malformed evidence stays unknown. Raw JSON/logs are
retained in successful captures. A failed host command retains its ordinary
task logs and failure report; its workspace outputs are not captured or made
available to downstream tasks. A remote build-hook activity alone remains
unknown: it may have found an already valid output on the remote store. No
sandbox bypass, daemon socket mount, VM,
installer or host configuration change is introduced.
