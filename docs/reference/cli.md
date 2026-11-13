# CLI reference

Run `stoke --help` for command help. Flags precede positional targets or references. The default workflow file is `stoke.yaml`; ordinary workflow commands accept `oci://registry/repository@sha256:…` through `-f`. CI generation currently requires a local checkout-relative workflow selector.

## Inspect a workflow

```sh
stoke check [-f FILE] [--context DIR] [--input NAME=VALUE ...]
stoke plan [-f FILE] [--input NAME=VALUE ...] [TASK ...]
```

`check` validates and resolves imports without executing task commands or parsing native Coopr syntax. It loads and validates declared component assets, checks native/environment definition files in their caller/component root, and checks availability of named-context/live host directories. Caller context defaults to the local YAML directory or current directory for an OCI root; use `--context` to override that application input independently of packaged files. `plan` prints selected logical task groups and expanded jobs, including native/environment task kind, mount counts, requested target platforms, variant identifiers, matrix values, prerequisites and cache policy, without checking caller definition files or local mount paths. Optional packaged assets are still fetched and validated while resolving their owning definitions. Without targets, all root workflow tasks are selected, including publication tasks; imported templates run only when selected through a dependency. Registry access may be needed while resolving imports.

`check`, `plan` and `run` accept repeated `--input NAME=VALUE` flags for the root workflow's declared string inputs. Flags precede positional targets. The first `=` separates the name from the value; later `=` characters belong to the literal value. Quote shell-sensitive values, for example `--input 'message=a=b'`. Explicit empty values use `--input note=`. Duplicate CLI input names are rejected. Unknown input names and missing required inputs fail before execution. Optional inputs use their declared defaults, or empty strings when no default exists. Input values are ordinary build configuration, not a secret channel. See [reusable inputs](../guides/reusable-inputs.md).

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
| `--coopr-binary PATH` | Compatible Coopr executable; defaults to `STOKE_COOPR_BINARY`, then `coopr` on PATH. Requires local API v1, execute and capabilities required by selected native tasks; registry cache options also require `registry-cache`, task/step mounts require `run-mounts`, explicit native/environment build targets require `build-platform`, and export steps require `selected-file-exports`. |
| `--runtime PATH` | OCI runtime; defaults to `crun`. |
| `--network MODE` | Explicit container RUN network default, requiring `run-network`; native RUN choices still apply. Does not change host-step networking or registry access. |
| `--storage-driver NAME` | Native storage driver; defaults to `vfs`. |
| `--authfile FILE` | Registry credential file. |
| `--signature-policy FILE` | Native containers/image signature policy. |
| `--tls-verify=BOOL` | Override native image/component/cache registry TLS verification; use `--tls-verify=false` only for isolated development registries. Omitted values retain Coopr defaults. |
| `--plain-http` | Use HTTP for all resolved OCI workflow references; use only with isolated development registries. |

Host-only execution requires no Coopr executable. Host tasks default to fresh
execution and export only declared captured files. Explicitly reusable host
results use a separate bounded OCI file format through the same directional
cache repository options. Host cache lookup/authentication/integrity errors
fail clearly; a missing key executes the command. See [host steps](../guides/host.md).

Registry cache reads and writes are independent. Supplying only `--cache-from`
leaves that repository read-only; supplying only `--cache-to` writes newly
produced results without querying that repository. Use both for read/write reuse,
and repeat either option for multiple repositories. The local `--cache-dir`
continues to operate alongside them. For example:

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

Default run state is retained under `$XDG_STATE_HOME/stoke/runs`, falling back to `~/.local/state/stoke/runs`, with a unique run directory. Each successful image job exports `outputs/JOB/image` as an OCI layout. Without export steps, it also exports `outputs/JOB/rootfs` by default. A task with explicit `export` steps defaults to those selected files instead; `outputs.filesystem: true` additionally requests the full rootfs, and `false` always disables it. Each export snapshots its ordered-step filesystem into a caller-workspace destination, including declared reports attempted before ordinary failed-RUN cleanup. Component jobs instead export `outputs/JOB/component.oci.tar`, an OCI archive file with no flattened filesystem. Publication tasks record transfer results rather than another build export. With `--output`, that fresh directory replaces `outputs`; selected workspace destinations, native/component stores and `run.json` keep their existing locations. Run stores and output directories must remain outside the input context. Existing outputs are not overwritten. A failure prevents dependent work and, according to matrix failure policy, stops admission and cancels active workflow work. Stoke waits for cleanup and retains evidence; failed runs are not successful artifacts.

Ready jobs run automatically, with an internal bound of four active jobs across the workflow. Each task's `strategy.max-parallel` adds a per-matrix limit. Ready dependents may start while unrelated branches remain active. The internal bound controls execution admission, not CPU or memory reservation. See [parallel tasks](../guides/parallel-tasks.md).

A run owns its Coopr Unix-socket service. Signals cancel requests through Coopr's API and await cleanup acknowledgement; normal shutdown closes the parent-lifetime pipe and drains the service. See [engine selection](../guides/coopr-api.md). Component asset directories remain available until engine cleanup is confirmed, then loader-owned temporary trees are removed. If Coopr reports `cleanup_failed`, Stoke retains those trees, including any native worker staging beneath them. Persistent outputs, cache and run directories remain caller-owned. A filesystem export and OCI export are separate promotions, not a two-output transaction.

Inline environments lower to native image producers followed by ordinary consumers. Their generated `stoke-environment-` producer names appear in the report; equivalent resolved inputs, policies and prerequisite gates share a producer within that run. See [task environments](../guides/environments.md).

Task logs and available cache counters go to stderr. When a run directory has been established, stdout contains one JSON report on success or execution failure, and the same report is saved as `run.json` in that directory. Fields include `status`, `directory`, `output` and a `tasks` array in stable plan order, independent of completion order. Every selected job is included; never-started jobs have status `skipped` and a reason. Logs from overlapping jobs can interleave. Jobs record their name/status, logical task, kind (`image`, `component` or `publish`), matrix values, actual result `platform` and applicable identities/paths/metadata. The actual platform can differ from a native build's requested default when a native FROM stage selects a different target; a `from` consumer inherits and validates the actual result. Literal jobs include available portable `cache` counters; native definition/component jobs omit unavailable counters. Publication jobs retain `destinations`, including partial successes and errors on a failed task. A nonzero `progressDropped` records engine-reported omitted progress observations, including on failed tasks; it does not change task success or imply a complete timing trace. Failures include an error. A non-matrix failure or fail-fast variant failure stops admission and cancels active workflow jobs. A matrix with `strategy.fail-fast: false` finishes its active and queued siblings before its failed group stops the workflow; another failed group or external cancellation can interrupt them. All prerequisite variants must succeed before dependent tasks can start. Cleanup uncertainty remains a failure rather than an ordinary successful cancellation. Configuration errors before a run directory is established only produce an error on stderr.

Export steps require the [pinned Coopr PR25 candidate](../guides/coopr-api.md#select-a-compatible-executable)
and its `selected-file-exports` capability. Jobs retain `exports` receipts in step
order, including reports captured on failed tasks; task status still controls
success gates.

Report files are replaced atomically. A persistence error fails the command and
marks its returned report failed; the saved report can be absent or retain an
earlier complete version. Use the exit status and returned error when deciding
whether a run succeeded.

## Generate and execute CI jobs

```sh
stoke ci plan [-f FILE] [--context DIR] [OPTIONS] [TASK ...]
stoke ci generate [-f FILE] [--context DIR] [OPTIONS] [TASK ...]
stoke ci scope --plan-id ID [--github-output FILE]
stoke ci run --plan FILE --job ID --scope JSON --attempt N [OPTIONS]
stoke ci verify-results --results JSON
stoke ci cleanup --results-repository REPOSITORY --scope JSON --result REFERENCE
```

`ci plan` resolves the same inputs, imports, selected dependency closure and
matrix variants as local execution. Plan version two preserves visible authored
stages and native matrix rows by default. Shared inline environments spanning
jobs become visible producer jobs. `--co-locate` is an explicit choice.
Local workflow/context selectors stay inside the checkout; OCI workflow imports
remain supported. Recorded input values must not contain secrets.

`ci generate` writes a separately owned reusable workflow and plan. It never
merges handwritten jobs. Generated ownership/fingerprint checks reject edited
files and stale pairs. Both output paths must stay inside the repository without
symlink traversal. Native CI owns success gates and credentials.

| Generation option | Meaning |
| --- | --- |
| `-f FILE` | Checkout-relative workflow; defaults to `stoke.yaml`. |
| `--context DIR` | Checkout-relative caller context; defaults to the workflow directory. |
| `--input NAME=VALUE` | Declared workflow input, repeatable. |
| `--runner LABEL` | Existing runner label, repeatable; replaces `self-hosted`, `linux`. |
| `--job-container-image REF` | Opt-in digest-pinned tools image for every generated job, including setup/result checks. |
| `--job-container-options TEXT` | Explicit Docker job-container options; requires an image and adds no security options implicitly. |
| `--github-token-auth` | Opt in to GHCR image pulls and per-job registry login with the caller-granted GitHub token. Requires provisioned Coopr for login and caller-owned package permissions. |
| `--timeout MINUTES` | Native job timeout, 1–360; defaults to 45. |
| `--cache-from REPOSITORY`, `--cache-to REPOSITORY` | Independent Coopr registry-cache directions, repeatable. |
| `--network MODE` | Record the container RUN default in the plan and generated jobs. |
| `--results-repository REPOSITORY` | Exact OCI result repository, independent of caches. If omitted when cross-job data exists, generation emits a required reusable-call input. |
| `--co-locate TASK` | Explicitly group that data-connected component, including shared environments and necessary intervening gates; repeatable. |
| `--output FILE`, `--plan-output FILE` | Generated workflow and JSON plan destinations. |
| `--check` | Compare both generated files without writing; fail on drift. |
| `--authfile FILE`, `--plain-http` | Import registry access; never recorded as credentials. HTTP is for isolated test registries. |

`ci scope` reads provisioned `GITHUB_SERVER_URL`, `GITHUB_REPOSITORY`,
`GITHUB_RUN_ID` and `GITHUB_SHA`, creates a random invocation nonce and emits
scope JSON. `--github-output` also appends the native `scope` output. Scope fields
and nonces establish consistency and uniqueness, not authorization.

`ci run` executes one planned job or explicitly grouped unit. It fetches exact
immutable result bindings, rejects conflicts/missing data and imports selected
payloads into Coopr's ordinary stores. It never rebuilds an external producer on
a cache miss. Ordinary `stoke run` retains the full locally gated workflow.
The JSON report adds `ci` input-binding metadata; it is an optional report, not a
success receipt or a required CI artifact. Gate-only bindings may be emitted
before the gate executes. Native `needs` remains the success authority.

| CI execution option | Meaning |
| --- | --- |
| `--plan FILE`, `--plan-id ID` | Plan path and expected fingerprint embedded by generation. |
| `--job ID` | Required concrete planned job/unit ID. |
| `--scope JSON`, `--attempt N` | Native invocation scope and positive current attempt. Explicit prior-attempt results may be reused when valid; no registry attempt scanning. |
| `--needs-env` | Read only expected per-variant native environment slots generated by Stoke. |
| `--needs JSON` | Explicit native `needs` object for local/provider integration; mutually exclusive with `--needs-env`. |
| `--github-output FILE` | Append this variant's bounded unique native metadata output to an existing regular file; symlinks are rejected. |
| `--results-repository REPOSITORY` | Runtime result repository; required for cross-job data if not configured in the plan. |
| `--output DIR`, `--cache-dir DIR` | Fresh outputs and local cache/staging outside the checkout; generated jobs use `$RUNNER_TEMP`. |
| `--coopr-binary PATH` | Reviewed executable; defaults to `STOKE_COOPR_BINARY`, then `coopr` on PATH. |
| `--cache-from REPOSITORY`, `--cache-to REPOSITORY` | Independent invocation cache directions, repeatable. |
| `--no-cache` | Force this job's work fresh; does not authorize producer replay. |
| `--runtime PATH`, `--storage-driver NAME` | Native execution defaults: `crun`, `vfs`. |
| `--network MODE` | Must match the generated plan when supplied; defaults to that plan's recorded selection. |
| `--authfile FILE`, `--signature-policy FILE`, `--tls-verify BOOL`, `--plain-http` | Registry policy; TLS false/HTTP are isolated-test options. HTTP applies to workflow/result transport; native/cache TLS uses the Coopr option. |

Missing or omitted native output metadata fails with a producer-and-dependent-gates
rerun instruction. GitHub partial-rerun output preservation remains a hosted
qualification gap. A result envelope contains exact output and input identities,
never pass/fail; an input-set reference cannot replace a native gate.

`ci verify-results` accepts the native compact status array or a `needs` result
object. Empty/malformed values and every status except `success` fail, including
skipped and cancelled jobs. `ci cleanup` deletes only one scope-validated metadata
root. Call it after stopping writers and waiting out the retry window; the
command does not establish those conditions. Digest deletion also
removes every tag alias for that metadata index. It does not delete shared
payload manifests/blobs/cache entries; registry retention and deletion support
remain operator-owned. See [CI generation](../guides/ci-generation.md) for trust,
retention, import bounds, explicit co-location and provider/platform limitations.

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

`stoke`, `stoke help`, `stoke -h` and `stoke --help` print help. `stoke version` and `stoke --version` print the build version, which is `dev` in a development checkout. Errors are reported on stderr with a nonzero exit status; task failures do not become successful runs.

Exit status 0 indicates success or help, 1 indicates validation/resolution/execution/I/O failure, and 2 indicates an unknown command, invalid flags or missing required arguments.
