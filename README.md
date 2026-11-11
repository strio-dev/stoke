# Stoke

Stoke is a portable workflow and build tool. Define container builds, tests and artifact flows in YAML, then invoke the same `stoke run` command locally or inside a GitHub Actions, Jenkins, GitLab CI or Tekton job. The CI platform owns triggers, source checkout, jobs, agents, machines, credentials and approvals; Stoke composes the work performed within that job.

Stoke uses a workflow-scoped Coopr executable for container execution and caching, and OCI artifacts for reusable task definitions. It does not require a Strio service or a persistent execution daemon. This first implementation runs on Linux; ready tasks and matrix variants can overlap within the invocation, with dependencies as success gates. See [running inside existing CI](docs/guides/ci.md).

```sh
nix develop path:.
go build -o /tmp/stoke ./cmd/stoke
/tmp/stoke check -f examples/first-workflow/stoke.yaml
/tmp/stoke plan -f examples/first-workflow/stoke.yaml artifact
/tmp/stoke run -f examples/first-workflow/stoke.yaml --context examples/first-workflow artifact
```

Install Coopr `0.6.0` for container execution. Stoke finds `coopr` on PATH, or you can select it with `STOKE_COOPR_BINARY` or `--coopr-binary`. See [engine selection](docs/guides/coopr-api.md).

Execution requires working rootless user namespaces, subordinate UID/GID mappings, mapping helpers and an OCI runtime. See [getting started](docs/getting-started.md) before running workloads.

- [First workflow](docs/tutorials/first-workflow.md)
- [Workflow YAML](docs/reference/workflow.md)
- [Native Coopr builds and publication](docs/guides/native-coopr.md)
- [Coopr task environments](docs/guides/environments.md)
- [Mount inputs and caches](docs/guides/mounts.md)
- [Parallel tasks](docs/guides/parallel-tasks.md)
- [Task matrices](docs/guides/matrices.md)
- [Container platforms](docs/guides/platforms.md)
- [Caching](docs/guides/caching.md)
- [Package component files](docs/guides/packaged-components.md)
- [Reusable component inputs](docs/guides/reusable-inputs.md)
- [Shared definitions and Bale](docs/guides/shared-workflows.md)
- [CLI reference](docs/reference/cli.md)
- [Contributing](CONTRIBUTING.md)

The supported first slice includes local and OCI workflow definitions with declared string inputs, invocation-scoped reusable tasks and optional declared files, native Coopr image/component builds, inline image-definition task environments, task/step input and cache mounts, task output inheritance/copying, gated image/component publication, Coopr local/OCI registry execution-result caching and workflow publication with Bale dependency metadata for imports, images and typed native registry inputs. Independent tasks and matrix variants run automatically under an internal four-job bound, with all-variant dependency gates. Task `platform` selects a Linux container target through Coopr; native runner allocation and foreign-architecture execution setup remain the host CI's responsibility. Services, secrets and expression interpolation are not implemented. See [architecture](docs/concepts/architecture.md).

Use `stoke run --cache-to REGISTRY/REPOSITORY test` to warm a registry cache and
`--cache-from REGISTRY/REPOSITORY` to reuse it in a later local or CI run. These
options require Coopr’s `registry-cache` API capability. See
[caching](docs/guides/caching.md) for explicit read/write configuration and limits.

Task mounts require Coopr’s `run-mounts` capability. Local contexts are snapshotted inputs; explicit live host binds and disposable cache volumes have separate side-effect and lifetime rules. See [mount inputs and caches](docs/guides/mounts.md).

Changes in this checkout remain uncommitted for review. No hosted Stoke release or installation endpoint is asserted.
