# Run in existing CI

Invoke the same `stoke run` command locally and as a step inside a GitHub Actions, Jenkins, GitLab CI or Tekton job. Stoke carries the reusable build, test and artifact workflow; the CI platform runs that command in its chosen environment.

For separate GitHub jobs and matrices derived from your Stoke dependencies, use
[CI configuration generation](ci-generation.md). It generates a separate reusable
workflow and plan while preserving your handwritten jobs. The examples below
remain the direct, single-job invocation path.

Keep triggers, checkout, agent/machine selection, credentials, approvals, status reporting and artifact retention in the CI configuration. In the direct invocation below, Stoke resolves its own workflow dependencies and delegates container builds and caching to Coopr within the selected job. These are separate graphs: a Stoke matrix expands work inside that invocation, while a CI matrix can allocate separate jobs or machines. The optional generator maps those dependencies to provider jobs; the platform still owns their scheduling. Neither path adds a Stoke CI worker or control plane.

The examples assume `stoke` is already installed on the runner and the project has a `stoke.yaml` task named `test`. Provision Stoke from your reviewed source/Nix build; no public download URL is assumed. Configure rootless UID/GID mappings, mapping helpers, runtime, storage and signature policy before running a job. A generic hosted runner is not automatically a verified rootless environment.

## GitHub Actions

Use this job in an existing workflow on an appropriately provisioned runner:

```yaml
jobs:
  test:
    runs-on: self-hosted
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          persist-credentials: false
      - name: Check workflow
        run: stoke check
      - name: Run tests
        run: stoke run test
```

The checkout action uses the same immutable pin as this repository's CI template; review updates according to your repository policy. Workflow code from pull requests must not gain access to release credentials through runner provisioning.

## GitLab CI

Choose a runner with Stoke and the native prerequisites already available:

```yaml
stoke-test:
  tags: [stoke-rootless]
  script:
    - stoke check
    - stoke run test
```

The tag is an example runner selector, not a GitLab-provided execution environment. Configure it for your installation.

## Jenkins

On a provisioned agent:

```groovy
pipeline {
  agent { label 'stoke-rootless' }
  stages {
    stage('Test') {
      steps {
        checkout scm
        sh 'stoke check'
        sh 'stoke run test'
      }
    }
  }
}
```

## Tekton

Call Stoke from a step in a Tekton `Task`. Bind a workspace containing the
checkout through your existing `PipelineRun` or `TaskRun`:

```yaml
apiVersion: tekton.dev/v1
kind: Task
metadata:
  name: stoke-test
spec:
  workspaces:
    - name: source
  steps:
    - name: test
      image: registry.example.com/team/stoke-rootless:reviewed
      workingDir: $(workspaces.source.path)
      script: |
        #!/bin/sh
        set -eu
        stoke check
        stoke run test
```

The image is a placeholder for your prepared image containing Stoke, a compatible
Coopr executable and its runtime prerequisites. No published Stoke image or
compatible released engine is assumed. Tekton supplies the workspace and runs
the step; the checkout can be produced by an earlier CI task. The `workingDir`
substitution belongs to Tekton, not the Stoke YAML schema. See Tekton's
[Task reference](https://tekton.dev/docs/pipelines/tasks/) and
[workspace reference](https://tekton.dev/docs/pipelines/workspaces/).

This fragment illustrates invocation, not verified Kubernetes rootless support.
The cluster and prepared image must support the user namespaces, mappings,
mounts, storage and networking required by Coopr. Configure those through your
existing platform policy; this example does not change pod privileges or provision
workers.

## Warm a cache before CI

Run the same workflow locally and write eligible Coopr results to an OCI cache
repository:

```sh
stoke run --cache-to registry.example.com/team/stoke-cache test
```

The CI job can then read those results with a fresh local cache and native stores:

```sh
stoke run --cache-from registry.example.com/team/stoke-cache test
```

Replace the placeholder repository with one your machines can access. Configure
read credentials for consumers and write credentials only for trusted runs,
through `--authfile` or the native registry configuration. A trusted CI job can
supply both options to read and update the cache. Both options are repeatable.
The host CI still owns the job, credentials and machine allocation; Stoke passes
cache repositories to Coopr within the invocation.

Use Coopr `0.6.0` for the `registry-cache` API capability. Identical pathnames on two machines do not share a
local cache; the registry repository supplies the common data in this example.
Coopr still matches tracked inputs, platform and execution settings before reuse.
See [registry cache transport](caching.md#share-results-through-an-oci-registry)
for repository syntax, trust, miss and failure behavior.

## Retain outputs and cache

Keep the local cache outside the checkout and under the runner account's control. `--cache-dir` can point at a persistent worker directory and remains active alongside any registry sources/destinations. Cache persistence is optional; it should affect speed rather than correctness. Only one Stoke run can write a selected local directory at a time; independent runs need distinct local directories even when they use the same registry cache repository.

Use a fresh `--output` path outside the checkout when your CI artifact collector needs a known location. Retain `run.json` and the desired task filesystem or OCI output. Do not upload credentials or entire native stores unintentionally. A failed workflow returns a nonzero exit status and blocks dependent task groups. Matrix fail-fast policy controls when a failed matrix stops workflow admission; active work is cancelled and drained when the workflow stops. Never-started selected jobs remain in the report as `skipped`. Configure your CI's failure/evidence retention policy separately.

Selected exports require a Coopr engine advertising `selected-file-exports`.
Run `stoke run -f examples/ci/exports.yaml --context . package`
from the repository root. Its ordered export steps produce `artifacts/stoke`,
`reports/test` and `reports/package`; the test stays fresh while eligible build
and package instructions can reuse Coopr's cache. The optional input
`--input test-outcome=fail` deliberately fails after the real Go tests and leaves
the declared test reports available while blocking packaging. Each invocation
needs fresh destinations. A native CI step can execute the binary or collect
those paths after Stoke returns; use the provider's failure-retention condition
for reports without masking Stoke's nonzero status. Stoke does not upload them
automatically, and generated reusable jobs do not accept caller poststeps.

See [caching](caching.md) for test-result policy and [runtime and security](security.md) for trust boundaries.

Stoke automatically schedules independent ready tasks under an internal bound of four active jobs across the selected workflow, including matrix variants and native/publication requests. Matrix `strategy.max-parallel` adds a per-task cap. This schedules work inside the existing runner; it does not allocate another machine or reserve CPU/memory. See [parallel tasks](parallel-tasks.md).

The checked-in `examples/ci/stoke.yaml` Go workflow keeps its two visible check
variants concurrent while setting `GOMAXPROCS=1`, `GOFLAGS=-p=1` and a
`GOMEMLIMIT=768MiB` target in each Go task. The native build definition declares
the same settings. These are ordinary task inputs used consistently for local
and generated CI runs. The Go memory target is soft; it does not reserve runner
memory or impose a container quota.

The example build and check environment select the same digest-pinned Go image;
the packaging image is pinned too. Updating those dependencies is a reviewable
source change. Other workflows can use mutable tags, whose later resolutions
are different inputs; these example pins do not add a global resolution lock.

For a matrix task, invoke its logical task name once; Stoke schedules its variants within the runner. This does not request additional CI machines or turn an architecture label into cross-architecture execution. See [task matrices](matrices.md).

Native Coopr definition tasks use the same runner and an explicit caller
context. Install Coopr `0.6.0` on that runner. Use the component file scope when
selecting declared helpers and definitions from a template. A separate trusted
CI job can select a YAML publication target after its required gates. Running
without targets selects every root task, including publication. Use
`stoke publish` for reusable workflow YAML and its declared component files.
Use YAML publication tasks to deliver built images. See
[native builds and publication](native-coopr.md).
