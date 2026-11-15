# Record CDEvents

`stoke run --events FILE` writes a separate JSON Lines stream of
[CDEvents](https://cdevents.dev/). With Stoke and a compatible Coopr on PATH,
the [first workflow](../tutorials/first-workflow.md) can record events with:

```sh
event_dir=$(mktemp -d)
stoke run -f examples/first-workflow/stoke.yaml \
  --events "$event_dir/events.jsonl" artifact
```

The file must be fresh and outside execution inputs and runtime storage,
including named contexts, component files, live host inputs and declared host
toolchains. Stoke checks the resolved selected inputs before creating it.
The file has mode `0600`; existing files and symlinks are not overwritten.
Schema-validation or file-write errors fail the command. Logs, captured outputs,
the CLI's JSON result and `run.json` keep their existing roles.

## Observed facts

| Observation | Event or data |
| --- | --- |
| Local invocation | `pipelinerun.started` on plan admission; `pipelinerun.finished` after input and engine cleanup |
| Started task or concrete matrix variant | `taskrun.started` and `taskrun.finished` |
| Actual Coopr build call requesting an artifact | `build.started`; `build.finished` requires success and an exact artifact digest |
| Successful publication destination returning an exact digest | `artifact.published`, including a successful destination in a partially failed task |
| Cache counters, matrix values, selected-export receipts and dropped-progress count | Task event `customData` |
| Never-started tasks | Existing run report; local pipeline completion includes their skipped names |

Events use the [released CDEvents 0.5 vocabulary](https://github.com/cdevents/spec/tree/v0.5.0). The official Go SDK and embedded
schemas are version `0.5.1`, which fixes schema defects in `0.5.0`; emitted
`context.specversion` is `0.5.1`. Each event is validated before writing.
These are CDEvents JSON objects with `context` and `subject`, not an additional
CloudEvents transport envelope.

Task names imply no test or deployment outcome. Selected files are export
receipts, not evidence that a package was published. Tags and source digests
are not substituted for missing destination receipts. Registry publications use
OCI PURLs; local artifacts with an observed SHA256 use generic content-addressed
PURLs. No packaging event is inferred without an observed source change.

## Invocation and retry identity

The stream's `context.chainId` scopes one Stoke execution. Task subjects include that
invocation's identity and the concrete task/matrix name. Started and finished
events share a subject ID but have distinct occurrence IDs. Resending retained
lines preserves those IDs; rerunning a command produces new occurrences.

Direct `stoke ci run` accepts the same `--events` flag. Its events include the
validated CI scope hash, execution unit and actual positive attempt in
`customData.ci`. Imported producers are not reported as newly executed tasks.
An isolated CI unit emits no pipeline start or completion. The generated native
aggregate is provider-owned shell code and emits no Stoke pipeline event.

This observer covers `run` and `ci run`. Distribution subcommands and internal
transfers without confirmed receipts are outside this boundary. There are no
inferred download, signing, deletion, test or deployment events. No collector,
backend, network exporter or tracing setup is required. The file contains
workflow/task identities and export paths; review it before sharing it.

Event emission does not depend on trace sampling. The same invocation and task
identities are reserved for future optional log/trace correlation; logs are not
correlated or exported today. Current JSONL has no CloudEvents `traceparent`
extension. See the [correlation contract](../development/coopr-boundary.md#event-and-log-correlation)
for that future transport boundary.
