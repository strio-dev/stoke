# Prepare a hosted CI qualification

The prepared manual test exercises container tasks without granting
host-execution authority. Hosted qualification requires an authorized push and
dispatch, plus provisioned runners and test registry access.

The caller is `.github/workflows/stoke-dogfood.yml`. It preserves the separate
validation and reusable-workflow jobs, requires explicit registry destinations
and reviewed executable hashes, and installs nothing. Existing
`.github/workflows/ci.yml` is unchanged. A local registry proof does not establish
GHCR behavior, hosted runner capabilities or GitHub retry-output retention.

GitHub's manual-run interface requires the workflow on the default branch.
While `main` contains only a stub README, an already registered workflow can be
dispatched through the API against `initial`. Unregistered, dispatch-only files
cannot bootstrap themselves. Enable publication or container security options
only after approval for their concrete targets.

## Private tools-image transport

The registered `ci.yml` has an explicit `registry_qualification` dispatch input,
disabled by default. When enabled on `strio-dev/stoke`'s `initial` branch, ordinary
checks must pass before the reusable `stoke-registry-qualification.yml` starts.
It builds the AMD64 tools image, publishes only to
`ghcr.io/strio-dev/stoke-qualification-tools`, and fetches the emitted immutable
manifest digest on a fresh hosted job. It never loads or starts the image.

Before the first publication, an authorized organization-wide inventory must
confirm the approved name is unused. The workflow's explicit bootstrap check
allows a 404; that repository-scoped response alone does not prove absence.
GitHub documents new container packages as private by default. The post-write
check requires private visibility and association with `strio-dev/stoke` before
emitting the digest or admitting the read-only consumer. Unexpected metadata
stops the run; it changes no package permissions or visibility and deletes nothing.

The writer uses its short-lived job token with `packages: write`; the independent
consumer has `packages: read` and its own temporary auth file. These permissions
apply to the whole job. Trusted source, fixed destinations and dispatch/ref guards
constrain the qualification; GitHub does not scope the token to a repository-name
allowlist. No CLI credential is passed into a workflow. Package access inheritance
is separate from visibility; GitHub warns that linking a private package to a
public repository can also permit fork access.

For an authorized run:

```sh
gh workflow run ci.yml --repo strio-dev/stoke --ref initial \
  -f registry_qualification=true
```

Successful publication/readback proves private registry transport and static
image configuration, not nested Coopr execution, generated job-container startup
or Coopr registry-cache reuse. Cache/results packages are not written by this
tools-only qualifier. Their host/container workflow gates remain separate. The
current image and hosted AppArmor execution restrictions remain in force.

The opt-in [tools-container generator](../guides/ci-generation.md#opt-in-tools-containers)
is now available. It covers every generated job, but does not rewrite this
handwritten caller's host-provisioned validation job. A container-based hosted
qualification must review that caller too, select an already available image,
and verify its runner identity, pull credentials and nested OCI security profile.
No new image is published and no hosted container profile is qualified here.

## Before an authorized push

Choose an authorized cache repository and a separate result repository. Keep
result objects through the supported retry window; do not apply disposable-cache
retention to them. The generator currently records cache directions as literal
configuration. It does not accept them as reusable-workflow inputs. Generate the
pair locally with the chosen cache before reviewing the proposed files:

```sh
stoke ci generate -f examples/ci/stoke.yaml --context . \
  --cache-from registry.example.com/team/stoke-qualification-cache \
  --cache-to registry.example.com/team/stoke-qualification-cache package
stoke ci generate --check -f examples/ci/stoke.yaml --context . \
  --cache-from registry.example.com/team/stoke-qualification-cache \
  --cache-to registry.example.com/team/stoke-qualification-cache package
```

Do not pass `--results-repository` during this generation: this caller supplies
that required reusable-workflow input at dispatch. The checked-in example has no
cache destination, so the prepared caller deliberately fails until this local
configuration step is completed. Do not invent a destination to make it pass.

Provision every eligible `self-hosted, linux` runner with the reviewed Stoke and
Coopr `0.6.0` packages, a supported OCI runtime/rootless setup,
Python 3, and measured scratch capacity. Scope each native store and run directory
to its job. Check filesystem metadata and user quotas as well as free data space.
No installer, storage-driver change or runner provisioning is performed by Stoke.

Record candidate source archives, package closures and executable SHA256 values.
At dispatch, supply those hashes and the two selected repositories. The validation
job checks its selected entrypoints (`STOKE_COOPR_BINARY`, otherwise `coopr`),
versions and plan configuration, then runs read-only regeneration verification.
A wrapper's hash identifies that entrypoint, not every referenced file; provision
the complete reviewed immutable package. These per-run candidate checks are not
a production engine pin or fleet attestation. The other generated jobs can run on
different machines: all must have the same reviewed provisioning, and their
engine/version/store evidence must be inspected separately.

Stoke performs its ordinary API/capability negotiation in execution jobs. Missing
capabilities fail rather than selecting another backend. The caller adds no
bespoke daemon, API probe, cache engine or success-receipt transport.

## Authentication and permissions

The prepared caller grants only `contents: read`; it neither logs in to a
registry nor grants package deletion. Provision short-lived/test-scoped registry
credentials through the runner's normal credential configuration, with only the
access each stage needs. Do not place credentials in plan inputs, registry URLs,
command arguments or diagnostic logs. Native output references and repositories
are configuration, not a secret channel. Masked outputs must fail clearly.
By default, provision Docker-compatible credentials on every selected runner.
For GHCR, `ci generate --github-token-auth` instead uses each execution job's
token through Coopr login and passes the private auth file to result transport
and Coopr cache access. It also supplies native credentials for GHCR tools-image
pulls before steps start. The selected Coopr executable must be provisioned for
login, including for an opted-in host-only workflow.

If a future authorized GHCR run uses `GITHUB_TOKEN`, the caller must explicitly
add the required package publication permission and arrange package access and
per-job authentication. An authentication step in `validate-generated` does not
provision other jobs' runners. No such login propagation is implemented here.
GitHub documents workflow-token publication for associated packages and separate
access rules for private packages. See [GHCR authentication](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry#authenticating-to-the-container-registry).

## Hosted cases to record

| Case | Required evidence |
| --- | --- |
| Cold run | Visible build, environment, test/vet matrix and package jobs; native `needs` gates; exact producer/input-set references; fresh checks actually execute. |
| Warm run | Independent stores and matching inputs restore build cache; tests remain fresh; package uses the exact tested producer. |
| Failed test | The native matrix/group fails and blocks package; existing result objects do not assert success. |
| Consumer-only retry | Reuse an explicitly supplied prior-attempt reference in the same scope, or fail clearly if GitHub omits it. |
| Partial matrix retry | Preserve every unique variant output; missing/masked metadata fails without scanning registry tags. |
| Producer rerun | New execution bindings conflict with old tests, even for identical bytes; deliberately rerun dependent gates. |
| Concurrent invocations | Separate invocation scopes; no mutable latest/winner lookup; no output-key collisions. |
| Missing result | A cache miss may execute a producer before gates; a missing exact consumer result must not replay it after tests. |
| Retention | Retained references survive retries; explicitly expired owned metadata cleanup leaves shared payloads and unrelated scopes usable. |

GitHub combines matrix outputs and warns that duplicate names can be overwritten
by the last finishing variant; generated output names are unique. This remains a
hosted qualification item, not merely a YAML-lint assertion. See [matrix job
outputs](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idoutputs).
GitHub documents that omitted jobs retain their previous-attempt outputs for
newly run jobs. Record actual variant output availability, secret masking and
lineage validation in the hosted test. Missing inputs still require a clear
failure and an explicit producer/dependent-gates rerun. See
[reruns](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/re-run-workflows-and-jobs).

Developer reports may be exported separately after review. No CI artifact
upload/download or successful-status object is required for internal data flow.
Do not dispatch this matrix or add automatic push/PR triggers as part of local
qualification.

## GHCR cleanup qualification boundary

Stoke's generic `ci cleanup` verifies scope/type and requests deletion of one OCI
metadata manifest by digest. It does not map digests to GitHub package version
IDs, call GitHub's package-management API, enumerate safe candidates, remove tags
selectively, enforce TTLs or garbage-collect shared blobs. Do not infer any of
those capabilities from local registry tests.

The [OCI Distribution specification](https://github.com/opencontainers/distribution-spec/blob/v1.1.1/spec.md#content-management)
permits registries to disable deletion. Deleting a manifest by digest makes all
tags pointing to that manifest unavailable; it is distinct from tag-only deletion
and blob deletion. Stoke rejects subject-bearing CI metadata, so its ordinary
ORAS manifest deletion does not perform fallback referrers-index maintenance.

Qualification starts with the existing OCI operation: inspect its implementation,
exercise disposable local fixtures, and compare official provider documentation.
Those checks do not prove the GHCR endpoint supports DELETE. Live qualification
requires an explicitly authorized disposable GHCR object and must record exact
responses, alias effects and retained/shared-content reachability. No remote
deletion, publication, package administration or additional credential scope is
authorized by preparing this test.

The reviewed GitHub documentation does not settle GHCR's OCI manifest-DELETE
behavior. That is unqualified support, not proof of incompatibility. No GitHub
administrative adapter is selected or required on that evidence. Investigate
one only after demonstrated need and a clear product requirement; never switch
silently from the OCI operation to broader package administration.

GitHub's [package-version deletion API](https://docs.github.com/en/rest/packages/packages#delete-package-version-for-an-organization)
addresses a numeric `package_version_id` under the owner/package and documents
`read:packages` plus `delete:packages` for classic personal tokens. The general
delete/restore guide requires package-admin access; the organization REST
endpoint additionally specifies organization-admin authority. Verify the exact
target and credential requirements before an authorized test. That is a
different operation from OCI manifest deletion; a
version can have multiple container tags. A future adapter must resolve and
review the exact version and all tags/references, preserve retained attempts and
shared content, revalidate after stopping writers, and limit each explicit batch.

GitHub documents deletion/restoration with a workflow token as a preview requiring
package-admin access. Its 30-day restoration condition and namespace-availability
condition are recovery rules, not a live retry-retention guarantee. See [GitHub's
delete/restore rules](https://docs.github.com/en/packages/learn-github-packages/deleting-and-restoring-a-package).

No GHCR manifest-delete compatibility, version mapping, untag-only operation,
automatic retention, restore behavior, blob GC or package permission has been
exercised by this local goal. No GHCR cleanup job is prepared or authorized here.
