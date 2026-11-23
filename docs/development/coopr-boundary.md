# Stoke to Coopr execution boundary

Stoke uses a workflow-scoped Coopr process serving a versioned HTTP API over a private Unix socket. The process boundary keeps container execution and caching in Coopr while Stoke owns workflow orchestration.

## Process and protocol

Stoke resolves `--coopr-binary`, then `STOKE_COOPR_BINARY`, then `coopr` on PATH. A run creates a private mode-0700 temporary directory and launches:

```text
coopr system service --socket ABSOLUTE_SOCKET --parent-fd INHERITED_PIPE_FD
```

The service creates a mode-0600 socket. Stoke requires API version `1` and the capabilities needed by the selected plan before submitting work. Independent HTTP requests carry concurrent matrix variants; there is no shared host service, TCP listener or required background daemon. Normal Coopr CLI commands remain daemonless, and its Go execution library remains available for other consumers.

Registry cache requests additionally require `registry-cache`. The wire cache
options carry independent read and write repository lists to Coopr's existing
cache backend for literal, native image and native component execution. Their
absence leaves local-cache behavior unchanged; requesting unsupported registry
caching fails rather than silently dropping the repositories.

Stoke imports Coopr's public `api` and `client` packages. Its workflow adapter
constructs requests and projects results; the public client owns HTTP framing,
stream validation and cancellation reconciliation. Native dependencies and
bootstrap stay in the Coopr executable. Stoke does not import Coopr's root
execution package or API server internals. There is one maintained execution
path, with no fallback to embedding, private worker messages or scraped CLI text.

## Lifetime and ownership

Each workflow run owns its service process, socket directory and parent-lifetime pipe. EOF on the inherited pipe or service termination cancels active requests and drains their backend cleanup. Stoke requests cancellation and waits for acknowledgement when an execution is interrupted or its response is uncertain; forced termination is reported as unconfirmed cleanup, not successful cancellation.

Caller-owned store roots, cache, inputs and successful outputs remain caller-owned. Native image IDs are usable only within the matching store scope. Matrix variants share the workflow's store roots and Coopr-coordinated cache while retaining separate outputs. Coopr owns execution, frontend planning, cache identity and invalidation; Stoke owns workflow dependencies, matrix scheduling and task policy.

The socket provides same-account access to the service's authority. It is not an authorization broker, sandbox against the account itself or proof that all rootless workloads are safe. Do not expose the service through setuid, file capabilities, a restricted privileged launcher or a network proxy relying on application checks to constrain native helper authority.

## Event and log correlation

The Coopr SDK owns stream framing, operation progress and raw stdout/stderr.
Stoke observes lifecycle facts at scheduler admission/completion and final report
ownership, writing optional [CDEvents](../guides/events.md) without another
scheduler or report model. The SDK progress callback supplies operation timing,
but is not a task lifecycle or a correlated log record. Raw concurrent logs
remain raw; no existing OTel logging backend is being replaced.

A future opt-in log observer can reuse those execution boundaries and the
[published CI/CD attributes](https://opentelemetry.io/docs/specs/semconv/registry/attributes/cicd/)
(currently release candidate):
`cicd.pipeline.run.id`, `cicd.pipeline.name`, `cicd.pipeline.task.run.id` and
`cicd.pipeline.task.name`. Use the actual pipeline identity when observed and
the concrete task subject for task-run identity; an isolated CI unit's chain ID
does not identify the full host pipeline. The CI attempt remains a project
attribute until the standard defines one. TraceId/SpanId belong only to genuine
enabled tracing context, as described by the
[OTel log model](https://opentelemetry.io/docs/specs/otel/logs/data-model/#trace-context-fields).
CDEvents occurrence IDs are not trace IDs. `RunID` and `TaskRunID(job.Name)` on
the same observer supply invocation and concrete task identities; CI scope,
unit and actual attempt remain separately configured before admission. Future
task-log writers can reuse these values before concurrent streams meet the
shared writer, without parsing logs or maintaining another lifecycle. With
`--events` disabled, no observer or correlated log output is created today.

Lifecycle identities and CDEvents emission must remain independent of tracing
and its sampling decision. Future tracing would carry task-local context through
the existing execution call and writer boundary, rather than a shared mutable
current span. Neither disabled tracing nor an unsampled span suppresses events.
This is a correlation contract, with no OTel runtime dependency or exporter.

For a future CloudEvents transport, the official SDK's
[`AsCloudEvent`](https://github.com/cdevents/sdk-go/blob/v0.5.1/pkg/api/bindings.go)
converts the existing occurrence without creating another lifecycle event.
The [distributed tracing extension](https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/extensions/distributed-tracing.md)
places real `traceparent` and optional `tracestate` on that CloudEvent envelope.
Use the CloudEvents SDK's existing extension support when context is available;
do not place these transport attributes into CDEvents custom data or derive them
from workflow IDs. Current JSONL contains bare CDEvents and is unchanged.

## Product surface and release gate

Stoke's YAML exposes literal operations, native `coopr` image/component tasks and explicit `publish` tasks. Argument bindings pass single-producer image/component identities or selected matrix values without generating native definition text. Definition selection can use caller context or the owning component's optional file snapshot; Coopr owns grammar and parsing. A local named `stoke-assets` context exposes only that package's declared files, while primary context remains caller-owned. The API carries local context bindings and an explicit typed COPY context selector; it does not upload or merge arbitrary host directories. Build tasks retain OCI/native images. Full filesystem output is opt-in with `outputs.filesystem: true`; explicit export steps independently use negotiated `selected-file-exports`. Publication runs after its data dependency and `needs` gates, is not cached, and preserves partial destination statuses. The separate `stoke publish` command publishes reusable YAML definitions.

The planner and runtime share one capability calculation for selected jobs and
their actual execution options. The [runtime guide](../guides/coopr-api.md)
defines API compatibility, required capabilities and the matching source build.

Workflow artifacts have one YAML layer and optional declared file layers. YAML-only packages remain valid. Imported templates retain their own file snapshots, including across `uses` and matrix expansion. Loader-owned temporary trees are removed on load failure or after confirmed engine cleanup. Unconfirmed cleanup retains borrowed inputs and staging for investigation. No shared persistent content store is introduced.

Bale declarations cover imports, images and typed native `ref` arguments; verified delivery mappings are applied before execution. Coopr retains nested component delivery resolution. Stoke does not parse hidden KDL/ordinary-string registry references or rewrite native source. Publication destinations are outputs, never input declarations. Packaging-time named contexts are not automatically captured as invocation overrides in generated Coopr components; use native PACKAGE capture when required.

Resolved cache directions (root YAML, environment or CLI) are execution settings
and do not enter those Bale declarations. For image/component results, Coopr
owns the OCI cache representation, keys, content checks,
restoration and writes. Registry caches and final artifact/workflow publication
remain separate operations. Native registry credentials and TLS settings cover
cache access. Stoke's separate ORAS workflow and host-cache
transports use `--plain-http`; host-cache transport does not inherit Coopr's
`registries.conf` mirror/remap behavior.

See [the runtime guide](../guides/coopr-api.md) for use and [architecture](../concepts/architecture.md) for layer responsibilities. Qualification must cover literal preservation, native image/component builds, distinct output formats, gated publication, cold/warm caching, concurrent variants, cancellation and service lifetime. Read the current validation records for actual results; schema tests alone do not establish full runtime or security qualification.

## Historical evidence from 0.5.0

The original boundary review inspected published commit `c0fe63f05d24bdc5f1dce0b4f104f0dd9b1d497c`. Its public library supplied literal typed execution, while ordinary CLI commands supplied the full definition/component frontend. Neither the private planner nor native worker protocol was a supported public transport API. Generating KDL from literal operations would introduce quoting and frontend-expression changes, so the API retains a separate literal-program request.

Pinned evidence remains useful for understanding the migration trigger; it does not describe the new command:

- [Public Go API](https://github.com/strio-dev/coopr/blob/c0fe63f05d24bdc5f1dce0b4f104f0dd9b1d497c/coopr.go)
- [Library ownership and frontend boundary](https://github.com/strio-dev/coopr/blob/c0fe63f05d24bdc5f1dce0b4f104f0dd9b1d497c/docs/guides/library.md)
- [CLI build entry point](https://github.com/strio-dev/coopr/blob/c0fe63f05d24bdc5f1dce0b4f104f0dd9b1d497c/cmd/coopr/build.go)
- [Native worker lifecycle](https://github.com/strio-dev/coopr/blob/c0fe63f05d24bdc5f1dce0b4f104f0dd9b1d497c/internal/buildah/worker_process.go)
