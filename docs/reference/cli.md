# CLI reference

Run `stoke --help` for command help. Flags precede positional targets or references. The default workflow file is `stoke.yaml`; ordinary workflow commands accept `oci://registry/repository@sha256:…` through `-f`. CI generation currently requires a local checkout-relative workflow selector.

## Log in to a registry

```sh
stoke registry login registry.example.com
stoke registry login --username USER --password-stdin --authfile FILE registry.example.com
```

Give the registry host and optional port. Stoke verifies TLS and prompts for
missing credentials. The password prompt hides input. Supply a password or
token on stdin for automation.

Login uses the shared Podman/Coopr credential store. The destination follows
`--authfile`, then `REGISTRY_AUTH_FILE`, then `$DOCKER_CONFIG/config.json` if
`DOCKER_CONFIG` is set, then the native default. On Linux the native default
is normally `$XDG_RUNTIME_DIR/containers/auth.json`. Login can create the file.
For Docker's global `credsStore`, use `docker login` or select a separate
native auth file. Stoke can write through a configured per-registry helper.

## Inspect a workflow

```sh
stoke check [-f FILE] [--context DIR] [--input NAME=VALUE ...]
stoke plan [-f FILE] [--input NAME=VALUE ...] [TASK ...]
```

`check` validates and resolves imports without executing task commands or parsing native Coopr syntax. It loads and validates declared component assets, checks native/environment definition files in their caller/component root, and checks availability of named-context/live host directories. Caller context defaults to the local YAML directory or current directory for an OCI root; use `--context` to override that application input independently of packaged files. `plan` prints selected logical task groups and expanded jobs, including native/environment task kind, mount counts, requested target platforms, variant identifiers, matrix values, prerequisites and cache policy, without checking caller definition files or local mount paths. Optional packaged assets are still fetched and validated while resolving their owning definitions. Without targets, all root workflow tasks are selected, including publication tasks; imported templates run only when selected through a dependency. Registry access may be needed while resolving imports.

`check`, `plan` and `run` accept repeated `--input NAME=VALUE` flags for the root workflow's declared string inputs. Flags precede positional targets. The first `=` separates the name from the value; later `=` characters belong to the literal value. Quote shell-sensitive values, for example `--input 'message=a=b'`. Explicit empty values use `--input note=`. Duplicate CLI input names are rejected. Unknown input names and missing required inputs fail before execution. Optional inputs use their declared defaults, or empty strings when no default exists. Input values are ordinary build configuration, not a secret channel. See [reusable inputs](../guides/reusable-inputs.md).

In the human plan, `needs` lists explicit success gates and `data-from` lists
consumed task outputs. Data dependencies must succeed too. A task can consume
`build` while separately waiting for `test`, as in [the first workflow](../tutorials/first-workflow.md).

Local workflow paths must resolve to regular files, including through symlinks,
and contain at most 1 MiB. Directories, FIFOs and devices are rejected before
parsing.

## Execute

```sh
stoke run [-f FILE] [OPTIONS] [TASK ...]
```

| Option | Meaning |
| --- | --- |
| `--input NAME=VALUE` | Supply a declared root string input; repeat for multiple inputs. |
| `--allow-host` | Explicitly trust selected host tasks with the runner user's authority. Defaults to denied, including for imported tasks. |
| `--host-capability NAME` | Attest a provisioned host capability; repeatable. Installs or enables nothing. |
| `--host-capabilities-json JSON` | Strict JSON string-array transport for caller capability attestations in CI. |
| `--context DIR` | Caller build context and base for relative named-context/live host directories; defaults to the local YAML directory, or the current directory for an OCI root. |
| `--output DIR` | Fresh run output directory; must not exist. |
| `--cache-dir DIR` | Local portable result-cache root; `DIR/mounts` supplies the persistent native cache-volume parent. |
| `--cache-from REGISTRY/REPOSITORY` | Read Coopr execution results from an OCI registry repository; repeatable. Requires an explicit registry, without a tag or digest. |
| `--cache-to REGISTRY/REPOSITORY` | Write Coopr execution results to an OCI registry repository; repeatable and an explicit registry write. |
| `--no-cache` | Bypass execution-result reads for all selected tasks; newly executed results can still be stored locally or in selected cache destinations. |
| `--coopr-binary PATH` | Compatible Coopr executable; defaults to `STOKE_COOPR_BINARY`, then `coopr` on PATH. Requires local API v1 and the [capabilities needed by selected tasks](../guides/coopr-api.md#select-a-compatible-executable). |
| `--events FILE` | Optional validated [CDEvents JSONL](../guides/events.md) in a fresh private file outside execution inputs and runtime storage. Also supported by direct `stoke run --ci`. |
| `--runtime PATH` | OCI runtime; defaults to `crun`. |
| `--network MODE` | Explicit container RUN network default, requiring `run-network`; native RUN choices still apply. Does not change host-step networking or registry access. |
| `--storage-driver NAME` | Native storage driver; defaults to `vfs`. |
| `--authfile FILE` | Registry credential file. Overrides `REGISTRY_AUTH_FILE` and `DOCKER_CONFIG`. See [credential lookup](../guides/security.md#credentials-and-ci). |
| `--signature-policy FILE` | Native containers/image signature policy. |
| `--tls-verify=BOOL` | Override native image/component/cache registry TLS verification; use `--tls-verify=false` only for isolated development registries. Omitted values retain Coopr defaults. |
| `--plain-http` | Use HTTP for Stoke workflow artifacts, imports and host-file caches. For isolated test registries only. |

Host-only execution requires no Coopr executable. Host tasks default to fresh
execution and export only declared captured files. Explicitly reusable host
results use a separate bounded OCI file format through the same directional
cache repository options. Host cache lookup/authentication/integrity errors
fail clearly; a missing key executes the command. See [host steps](../guides/host.md).

Registry directions resolve independently: CLI flags override `STOKE_CACHE_FROM`
and `STOKE_CACHE_TO`, which override root workflow `cache-from` and `cache-to`.
The variables use comma-separated repositories without spaces; set-empty clears,
while unset falls back. `--cache-from=` or `--cache-to=` explicitly clears that
direction. An unspecified direction retains its lower-priority default, so use
`--cache-to=` to guarantee a read-only registry-cache invocation. Local cache
storage remains separate. See [caching](../guides/caching.md).

```sh
stoke run \
  --cache-from registry.example.com/team/stoke-cache \
  --cache-to registry.example.com/team/stoke-cache \
  test
```

Replace the repository with one you control and configure its credentials through
`--authfile` or native registry configuration. Tags, digests, short names,
transport prefixes and BuildKit-style cache options are rejected. Registry cache
options apply to literal and native image/component builds; YAML publication
tasks ignore execution caches and always perform their selected transfer. The
cache format and identity belong to Coopr. Missing entries rebuild; lookup errors
warn and rebuild, while invalid configuration and explicit export/seeding write
failures fail the build. See [registry cache reuse](../guides/caching.md#share-results-through-an-oci-registry).

Default run state is retained under `$XDG_STATE_HOME/stoke/runs`, falling back to `~/.local/state/stoke/runs`, with a unique run directory. Each successful image job exports `outputs/JOB/image` as an OCI layout. Full flattened filesystem output at `outputs/JOB/rootfs` is disabled by default; `outputs.filesystem: true` enables it, and `false` keeps it disabled. Each export snapshots its ordered-step filesystem into a caller-workspace destination, including declared reports attempted before ordinary failed-RUN cleanup. Component jobs instead export `outputs/JOB/component.oci.tar`, an OCI archive file with no flattened filesystem. Publication tasks record transfer results rather than another build export. With `--output`, that fresh directory replaces `outputs`; selected workspace destinations, native/component stores and `run.json` keep their existing locations. Run stores and output directories must remain outside the input context. OCI and rootfs destinations must be fresh. Local reruns update matching selected files and merge directories in the workspace. A failure prevents dependent work and, according to matrix failure policy, stops admission and cancels active workflow work. Stoke waits for cleanup and retains evidence; failed runs are not successful artifacts.

Ready jobs run automatically, with an internal bound of four active jobs across the workflow. Each task's `strategy.max-parallel` adds a per-matrix limit. Ready dependents may start while unrelated branches remain active. The internal bound controls execution admission, not CPU or memory reservation. See [parallel tasks](../guides/parallel-tasks.md).

A run owns its Coopr Unix-socket service. Signals cancel requests through Coopr's API and await cleanup acknowledgement; normal shutdown closes the parent-lifetime pipe and drains the service. See [engine selection](../guides/coopr-api.md). Component asset directories remain available until engine cleanup is confirmed, then loader-owned temporary trees are removed. If Coopr reports `cleanup_failed`, Stoke retains those trees, including any native worker staging beneath them. Persistent outputs, cache and run directories remain caller-owned. A filesystem export and OCI export are separate promotions, not a two-output transaction.

Inline environments lower to native image producers followed by ordinary consumers. Their generated `stoke-environment-` producer names appear in the report; equivalent resolved inputs, policies and prerequisite gates share a producer within that run. See [task environments](../guides/environments.md).

Task logs and available cache counters go to stderr. When a run directory has been established, stdout contains one JSON report on success or execution failure, and the same report is saved as `run.json` in that directory. Fields include `status`, `directory`, `output` and a `tasks` array in stable plan order, independent of completion order. Every selected job is included; never-started jobs have status `skipped` and a reason. Overlapping jobs can interleave complete lines, each prefixed with its concrete task/variant name. Incomplete lines flush when the task finishes; long lines are split into bounded chunks. Raw host log files and captured outputs are unchanged. Jobs record their name/status, logical task, kind (`image`, `component` or `publish`), matrix values, actual result `platform` and applicable identities/paths/metadata. The actual platform can differ from a native build's requested default when a native FROM stage selects a different target; a `from` consumer inherits and validates the actual result. Literal jobs include available portable `cache` counters; native definition/component jobs omit unavailable counters. Publication jobs retain `destinations`, including partial successes and errors on a failed task. A nonzero `progressDropped` records engine-reported omitted progress observations, including on failed tasks; it does not change task success or imply a complete timing trace. Failures include an error. A non-matrix failure or fail-fast variant failure stops admission and cancels active workflow jobs. A matrix with `strategy.fail-fast: false` finishes its active and queued siblings before its failed group stops the workflow; another failed group or external cancellation can interrupt them. All prerequisite variants must succeed before dependent tasks can start. Cleanup uncertainty remains a failure rather than an ordinary successful cancellation. Configuration errors before a run directory is established only produce an error on stderr.

Export steps require a [compatible Coopr executable](../guides/coopr-api.md#select-a-compatible-executable)
advertising `selected-file-exports`. Jobs retain `exports` receipts in step
order, including reports captured on failed tasks; task status still controls
success gates.

Report files are replaced atomically. A persistence error fails the command and
marks its returned report failed; the saved report can be absent or retain an
earlier complete version. Use the exit status and returned error when deciding
whether a run succeeded.

## CI stages

```sh
stoke ci plan [-f FILE] [--context DIR] [OPTIONS] [TASK ...]
stoke ci generate [TEMPLATE [STAGE]] [OPTIONS]
stoke ci generate [STAGE] --template-file FILE [OPTIONS]
stoke run --ci [OPTIONS] TASK
```

`ci plan` prints the resolved groups, matrix variants and their native
dependencies. `ci generate` renders standalone stage files. The native caller
owns scheduling, checkout, success gates and credentials. See
[CI stage templates](../guides/ci-generation.md) for built-in, local and OCI
selectors and the root `ci-template` setting.

Generation accepts `-f`, `--context`, repeatable `--input` and
`--co-locate TASK`. Explicit `--network`, `--cache-from` and `--cache-to`
are included in the emitted command when set. Generation reads no ambient
cache overrides. Use `--check` to detect stale generated files without writing.
`--authfile` uses the normal registry credential lookup. `--plain-http`
is for isolated template or workflow registry fixtures.

`run --ci` uses the same workflow inputs and execution options as `run`.
It requires explicit task selections and fails if external data prerequisites
cannot be restored from the normal Coopr cache. The caller handles gates that
only require success.

In CI mode, a literal image job with `cache: false` and no requested rootfs
does not export an OCI layout or digest if the resolved workflow uses it only
as a success gate. This applies even when the gate is explicitly selected.
An in-store image ID is not an exported OCI result.

## Publish and retrieve definitions

```sh
stoke publish [-f FILE] [--authfile FILE] [--plain-http] REGISTRY/REPOSITORY:TAG
stoke pull [--output FILE | --directory DIR] [--authfile FILE] [--plain-http] REFERENCE
```

`publish` validates the reusable definition, preserving its input declarations and bindings without requiring values for required inputs. `pull` preserves those declarations too. `--input` is an invocation option for `check`, `plan` and `run`, not a definition-publication option.

`publish` validates the workflow, collects its optional `assets` relative to the YAML directory, uploads YAML/file layers, attaches dependency metadata and prints the canonical reference. It requires write access to the selected registry. `pull --directory DIR` writes `stoke.yaml` and declared files into a fresh directory. For YAML-only artifacts, stdout and `--output FILE` remain available. A package containing assets requires `--directory` so files are not silently discarded; `--output` and `--directory` are mutually exclusive. Pulling does not execute tasks or overwrite caller outputs. Registry dependency metadata and relocation behavior are covered in [shared workflows](../guides/shared-workflows.md).

This command publishes workflow definitions only. Built image/component delivery is selected through a YAML `publish` task, for example `stoke run release`; see [native tasks](../guides/native-coopr.md#publish-after-tests). Workflow publication carries YAML and explicitly declared component files, not the caller's application source. Retrieved YAML applies verified destination mappings to imports, images and typed native registry references; file content and ordinary argument strings remain unchanged. See [packaged components](../guides/packaged-components.md).

`check` and `plan` also accept `--authfile` and `--plain-http` when resolving OCI definitions. Root remote references may use tags; imports inside a workflow must be digest-pinned for stable reuse.

## Help and version

`stoke`, `stoke help`, `stoke -h` and `stoke --help` print help. `stoke help run` also opens `stoke run --help`. `stoke version` and `stoke --version` print the build version, which is `dev` in a development checkout. Errors are reported on stderr with a nonzero exit status; task failures do not become successful runs.

Exit status 0 indicates success or help, 1 indicates validation/resolution/execution/I/O failure, and 2 indicates an unknown command, invalid flags or missing required arguments.

## Diagnose a failed run

Human failure summaries name failed tasks and the retained `run.json` path on
stderr. In GitHub Actions the same text is appended to the native step summary
when its file is available. Stdout remains the single JSON result. GitLab and
other CI jobs receive the same terminal diagnostics and nonzero exit status.

After fixing the cause, rerun the original terminal targets with the same inputs
and execution options; retrying only the failed prerequisite can omit its gates
or downstream work. Fresh tasks and side effects can execute again. Explicit
`--output` and `--events` destinations must be fresh. A local retry can update
earlier selected exports. Move them aside to keep the earlier contents. A
new `--output` does not relocate selected exports.

For generated CI, use provider-native retries preserving the original invocation
native success gates. Retry the workflow after changing its inputs.
