# Measure cache performance

Measure a complete Stoke run before changing cache behavior. A warm build can
skip its RUN instructions and still spend time hashing inputs, reading portable
records, restoring native images, verifying content and exporting outputs.
Coopr owns those operations; Stoke supplies the workflow and cache policy.

The repository's cache-performance collector compares fixed offline packaging
workloads. It does not measure compilation, general CI performance or parity
with another build tool. The recorded measurements used explicitly selected development executables.

## Fixed workloads

`internal/artifact/cache_performance_test.go` drives the selected Stoke and Coopr
executables. Its companion registry fixture records actual HTTP request and
response body bytes without holding its storage lock during network I/O.

Both workloads package frozen Stoke and Coopr source files and baseline
executables. They run a checksum build, an uncached verifier and an artifact
export. The `source-image` workload copies the payload directly into a native
image. The `component-image` workload first captures it with Coopr PACKAGE,
builds a component and consumes that component in the native image. The binaries
are payload files; the workloads do not compile these source trees.

Each workload runs five samples of each scenario:

| Scenario | Starting cache state |
| --- | --- |
| `cold` | Empty application caches. |
| `local-warm` | A copy of the frozen portable local seed. |
| `registry-warm` | A frozen source registry snapshot and an empty local cache. |
| `registry-rw` | The same registry source, with writes to a separate fresh mirror. |
| `changed-source` | The local seed plus an equal-size source edit with its original modification time restored. |

Every invocation has fresh native stores. Baseline and candidate use the same
fixture, seed, Stoke executable, BusyBox, resource limits and run order. The
candidate changes only the selected Coopr executable. Cold describes application
cache state; the collector does not flush the host's OS page cache. Run timed
collections sequentially without competing benchmark or test jobs.

The collector checks exported payload bytes and modes, artifact checksums,
persisted reports and actual build/verifier output. Warm cases must skip the
checksum build; cold and changed-source cases must execute it. The verifier
always runs. Registry sources remain read-only, and the read/write scenario must
write its mirror.

## Prepare a collection

`scripts/cache-performance.py collect` requires the pinned Stoke Nix development
shell with Go `1.27.3` and `STOKE_SMOKE_BUSYBOX`. The host must support the selected
rootless Coopr executable and a user systemd service manager. The launcher applies
an 8 GiB memory limit, no swap, a 400% CPU quota, 256 tasks and a 45-minute runtime
limit, with `GOMAXPROCS=4`. The Go test driver uses CGO disabled; the independently
built Coopr executable retains its native CGO support.

The report's resource fields describe this launcher contract; the Go collector
does not measure cgroup limits itself. Direct test invocations must provision
equivalent limits and retain the external command and limit evidence.

Prepare these inputs before invoking the collector:

- A dedicated absolute work directory named `stoke-cache-performance-*`.
- Frozen source trees at `source/stoke` and `source/coopr` inside that directory.
  Each needs `cmd`, `internal`, `go.mod`, `go.sum`, `LICENSE` and `README.md`.
- A `work.json` containing absolute `baseline_engine` and `baseline_stoke`
  executable paths. These select the immutable payload binaries and seed
  producer, including during candidate collection.
- Enough disk space for retained native stores, cache copies and exported
  payloads. Keep the same storage arrangement for baseline and candidate.
- Unique report paths and fresh per-label run directories. The collector retains
  failures and refuses to overwrite reports or existing run directories.

The `--engine` and `--stoke` arguments select the executables under measurement;
`--root` selects the prepared work directory, and `--output` selects its report.
Use `--label preflight --samples 1` to validate preparation. Then freeze the
collector, evaluator, source, fixture and seed identities before collecting
`baseline` and `candidate` with `--samples 5`. Only baseline or preflight can
initialize seeds; candidate runs must reuse them.

Record frozen identities and resource limits in the experiment's evidence
directory alongside reports, commands and logs. Each experiment needs its own
prepared inputs and frozen contract. Prepare the source snapshots and build the
executables before invoking `collect`.

## Evaluate the reports

After both complete reports exist, run this command from the repository root:

```sh
python3 scripts/cache-performance.py evaluate \
  --baseline /absolute/experiment/baseline.json \
  --candidate /absolute/experiment/candidate.json
```

The evaluator requires five successful samples for every workload/scenario,
matching fixture, seed, Stoke and resource identities, and a rebuilt engine.
It reports each sample and its median. A performance pass requires at least 15%
less median wall time in a warm scenario or at least 25% fewer registry blob
response bytes, with no fixed scenario's median wall time regressing more than
10%. Repeat noisy cases under unchanged settings rather than changing the gate.

Registry byte counts cover successful blob GET response bodies. They exclude
headers, manifests and uploads, so they are not a total network-traffic measure.
Fewer transferred bytes alone do not establish an elapsed-time improvement.
Five samples do not establish a p95 latency result. CPU usage and the largest
waited process's RSS are diagnostics; that RSS is not aggregate worker memory.

The Python comparison does not replace integrity, invalidation, cancellation,
cache-failure or full regression checks. Those checks and an independent
completion audit remain part of performance qualification.

## Attribute the work

Use Coopr's existing `--metadata-file` build option in separate diagnostic runs
to inspect completed operation phases. Native COPY hashing is included in
`prepare`; it is not a separately measured phase. Lookup can include restoration
work, so phase durations overlap and must not be summed. A native-task diagnostic also excludes
Stoke service startup and the workflow's other tasks. Keep it separate from the
fixed end-to-end comparison.

The isolated development engine removes a duplicate read when restoring ordinary
leaf config and layer payloads: ingestion into a fresh private OCI layout
verifies the bytes while copying them. Descriptor conflicts, source restrictions,
corruption, stream-close failures and cancellation still matter. Graphs with
unknown or nested media retain the existing recursive copy path. This changes
no workflow syntax, cache format, public option or cache ownership.
