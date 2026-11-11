# Contributing

Run `just integration` on a Linux host configured for rootless execution. It checks literal and native component/image tasks, cold/warm local-cache restoration, forced execution, source invalidation, fresh tests and failed-test gates. It also checks native matrices, optional filesystem exports, exact image/component identities in gated local publication, preservation of existing destinations and cancellation. The local static BusyBox fixture needs no registry image pull or external publication. The `examples/native-coopr` walkthrough uses a registry BusyBox image and has its own network requirements. Runtime acceptance is separate from `just check`, which does not assume the host permits namespaces.

Run `just dag-integration` for independent native/literal overlap, dependency joins, bounded admission, concurrent matrix variants, isolated variant outputs and warm-cache restoration. Separate four-task and matrix cancellation cases check active work, queued tasks, downstream gates and descendant cleanup. Its HTTP rendezvous uses a temporary per-process Pasta host-loopback mapping. Set `STOKE_SMOKE_HOST` when another explicitly selected rootless backend needs a different reachable host address. Inspect the retained reports and request traces; this does not establish support for every host or storage driver.

Select Coopr `0.6.0` with `STOKE_COOPR_BINARY=/absolute/path/to/coopr` for Stoke runtime suites.

Coopr owns direct API protocol, native literal/progress and CLI service lifetime
acceptance. From the corresponding Coopr source checkout, run
`just api-service-acceptance /absolute/path/to/coopr` on a configured rootless
Linux host. This explicit-binary target uses Coopr's public client and pinned
static BusyBox/CGO development fixtures; it does not build the executable or
establish that it matches the checkout. Supply a source-matched binary. Direct
Go invocations skip the CLI lifetime case without `COOPR_TEST_SERVICE_BINARY`,
and native cases skip with `-short`, without CGO or without BusyBox; those skips
are not qualification. See Coopr's contributing instructions for provisioning
and exact test selectors.

Set `TMPDIR` to an executable scratch filesystem with space for native stores.
All retained Python runtime fixtures use Python's standard temporary-directory
selection and preserve their evidence.

The combined literal/native fixture explicitly uses `network=none`. The DAG fixture covers networked workflow and matrix scheduling. These suites do not replace the separate Pasta HTTP/DNS/isolation qualification. Stoke cancellation coverage includes DAG active/queued workload drain and native frontend cancellation; Coopr owns direct API service lifetime acceptance.

`just ci-integration` invokes the Go registry/generated-job/exact-result tests
directly. Set `STOKE_COOPR_BINARY`, `STOKE_SMOKE_BUSYBOX` and
`STOKE_CACHE_CI_EVIDENCE_DIR` to provisioned executables and a disk-backed evidence
parent outside the checkout. The tests own disposable local registries and
independent stores. The recipe is an entry point to those tests, not additional
qualification or a hosted Actions/GHCR run.

`just registry-cache-integration` uses the same provisioned binaries and evidence
parent. Its two owning tests cover directional registry access, fresh independent
stores, mirror seeding, content invalidation, offline local reuse and export
failures. The original three-task case selects VFS and requires a non-root user;
its producer store is pruned through the scoped fixture helper before deletion.
Shared helpers and wrapper recipes are not additional independent test evidence.

The actual Go workflow fixture (`TestCIRealWorkflowRegistryWithCoopr`) also accepts
`STOKE_REAL_CI_NETWORK` as an explicit invocation setting, validated by the normal
CLI. For example, `slirp4netns` is recorded in the generated plan and used by the
initial local run, CI jobs, local return and fresh-checkout probe. An empty value
keeps the existing default; there is no backend fallback. The selected value is
retained in `execution-configuration.json` and `real-workflow-proof.json`. A run
with slirp4netns does not qualify Pasta or change the authored workflow.

For bounded scratch usage, `STOKE_REAL_CI_PRUNE_NATIVE=1` opts this fixture into
Coopr's scoped `image prune --all --force` after each successful phase's output
assertions and publication. It requires an explicit storage driver and a non-root
single-user coordinator. Only native images and cache mounts in new stores beneath
that invocation's fresh evidence directory are eligible; failed phases and prior
stores remain untouched. OCI/rootfs exports, registry cache, sources and reports
remain available. Configuration/proof JSON records the mode, and each prune keeps
its command, result and output in phase-specific evidence files. This optional
fixture policy does not change Stoke's normal native-store retention.
The prune command uses its own storage configuration, excluding inherited image
stores, storage overrides and driver options without changing host configuration.

Run `just oci-integration` for the OCI chain: optional packaged files, distinct caller input and OCI Coopr components carried through Bale delivery. Its acceptance fixture uses disposable local registries and native rootless execution; it is separate from the hermetic checks and does not authorize an external publication.

Use the pinned Nix environment for Go and documentation tooling:

```sh
nix develop path:.
just
just fmt
just check
```

The repository follows Coopr/Bale's Go CLI layout, Nix flakes, Just recipes and shared Strio Zensical theme. See [template provenance](development/template.md) for the original scaffold and approved artwork sources.

## Checks

`just test`, `just vet` and `just lint` run targeted Go checks. `just check` builds the CLI, runs Go tests/vet/lint, checks Go/Nix/Just formatting, validates workflow files and builds the documentation. Runtime acceptance also needs a host that permits rootless namespaces and native container execution; a successful compiler/unit-test check alone does not prove that host path. Report architecture-specific skips and runtime blockers explicitly.

The example Nix-check provenance helper and cache-performance evaluator have
small standalone Python suites, separate from those five flake checks:

```sh
python3 scripts/test-nix-workflow.py
python3 -m unittest discover -s scripts/tests
```

These tests use supplied evidence and temporary files; they do not run Nix
builders, containers or performance benchmarks.

The orchestration benchmarks use fixed dependency chains, independent tasks,
matrices and reusable-component calls. Run them in the pinned development shell:

```sh
go test ./internal/runner ./internal/source -run '^$' \
  -bench 'Benchmark(Workflow|ReusableInputs)' -benchmem -count=5
```

Keep evaluator source, Go version, concurrency and resource limits fixed when
comparing changes. These fixtures measure planning, loading and scheduling with
local files and fake execution backends; they do not measure container build or
registry-transfer performance.

Use `just docs-build` for the docs and `just docs-serve` for a local preview at `127.0.0.1:8000`. Check whether a preview is already running before starting another. Preserve the existing Direction 03 Continuous Rail artwork, outlined Nimbus Sans wordmark, Inter body typography and approved red accent. No palette comparison page is part of the site.

## Mount and environment qualification

Changes to mounted inputs or inline image environments need both schema/lowering
tests and rootless execution evidence. Check primary/named contexts, explicit
live host access, native cache volumes, image/stage views and OCI artifact files.
Include read-only enforcement, selected paths, digest/content invalidation,
cold/warm reuse, empty caches, concurrent sharing, cancellation and cleanup.
Compare an inline environment with the equivalent explicit `coopr` plus `from`
workflow, including inherited image configuration and prerequisite gates.

`just mount-integration` runs the real-executable mount/environment acceptance
fixture with the selected development engine. It covers the source types and
lifecycle behavior; it is separate from hermetic `just check`. The repository
`examples/environments` workflow additionally exercises ordinary Go tests in an
inline definition-built environment.

Do not equate snapshotted context writes with live host writes or per-RUN image
overlays. Verify that unreplayable side effects cannot disappear behind a cache
hit and that artifacts remain immutable raw file views. Source fixtures must
keep runtime stores and generated exports outside copied input trees.

The mount integration uses a selected Coopr engine advertising `run-mounts`.
Make engine-side fixes in Coopr and qualify them with a matching executable.
Run checks and runtime acceptance from the actual checkout.

See [mounts](guides/mounts.md) and [task environments](guides/environments.md) for
the public syntax and ownership rules. Report runner/architecture limitations
and skipped checks explicitly; passing unit tests do not establish rootless
runtime support.

## Scope and dependencies

Stoke imports Coopr’s public `api` and `client` Go packages. The [engine guide](guides/coopr-api.md) records the SDK revision and matching `0.6.0` executable. Select a source-matched binary for runtime checks. Coopr owns execution and its public API. See [the boundary decision](development/coopr-boundary.md).

Keep workflow semantics separate from container execution. Add tests for graph selection, schema errors, cache policy, artifact resolution and error handling as behavior changes. Documentation examples should match checked-in executable fixtures where possible. Keep planned features explicit and avoid hosted release/install claims until publication is authorized.

Submit changes through a pull request with the checks run and any skipped
runtime qualification recorded for reviewers.
