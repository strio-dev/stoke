# Define once. Run where you work.

<div class="strio-intro" markdown>
Stoke puts reusable container build and test workflows behind one command. Run the same YAML definition on your machine or inside a GitHub Actions, Jenkins, GitLab CI or Tekton job, with Coopr supplying execution and build caching.
</div>

[Set up development](contributing.md){ .md-button .md-button--primary }
[CLI reference](reference/cli.md){ .md-button .strio-container-link }
{ .strio-actions }

Your CI platform handles triggers, checkout, job placement, credentials and approvals. Invoke `stoke run` within its job or step to compose builds, tests and artifacts. Stoke's task dependencies describe the work inside that invocation; the CI platform keeps its own job graph. See [running in existing CI](guides/ci.md).

## Start with a local workflow

[Get started](getting-started.md) to build Stoke, install Coopr and prepare rootless execution. Then follow the [first-workflow tutorial](tutorials/first-workflow.md) to build, test and package a text artifact.

## Reuse and move definitions

Publish shared task definitions as OCI artifacts, import them by digest and carry their declared dependencies with Bale. [Declared inputs](guides/reusable-inputs.md) and `with` arguments let each invocation configure its own task dependency scope. Stoke includes Bale metadata integration; ordinary workflows do not require a Bale executable. Execution starts an explicitly selected API-capable Coopr process. See [engine selection](guides/coopr-api.md) and [shared workflows](guides/shared-workflows.md).

## Know the first implementation

[Independent tasks](guides/parallel-tasks.md) and [matrix variants](guides/matrices.md) can overlap on Linux within a workflow-wide job limit. Tasks start after all prerequisites succeed. Coopr owns native execution and build caching; Stoke owns workflow dependencies and task composition. [Local and OCI registry cache reuse](guides/caching.md) can warm later local or CI runs, while test-result caching needs an explicit policy for external inputs. Registry cache options require Coopr’s `registry-cache` capability. Services and secrets are not available yet.

Combine [native Coopr image/component builds](guides/native-coopr.md) with literal test tasks, declared filesystem exports and publication gates. [Optional packaged files](guides/packaged-components.md) let templates carry their own definitions and helpers while the caller supplies application context separately.
