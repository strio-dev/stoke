# Generic host tasks and actual Nix checks

These examples require explicit caller trust and already-provisioned native
Linux tools. They install nothing and do not modify host security settings.
Mixed host/container workflows require Coopr `0.6.0` alongside Stoke. Host-only
workflows use the runner's provisioned tools.

## Captured files, fresh tests and native gates

From the Stoke checkout:

```sh
run_dir=$(mktemp -d)
stoke run -f examples/host/stoke.yaml --context . --allow-host \
  --output "$run_dir/output"
```

On a native ARM64 runner, add `--input native-platform=linux/arm64`.
`prepare` captures its declared output, both `verify` matrix variants execute
freshly against that exact data, and `package` waits for all verification gates.
Each task receives a separate workspace. Host-only execution needs no Coopr
service. No reusable host-result cache is enabled for these tests.

For a reviewable GitHub Actions plan with visible native stages/matrix variants:

```sh
stoke ci generate -f examples/host/stoke.yaml --context . \
  --output .github/workflows/host.generated.yml \
  --plan-output .stoke/host-plan.json
```

The generated workflow requires an explicit result repository because stages
exchange data. Supply an authorized OCI repository from the handwritten caller;
missing exact results fail instead of rebuilding the producer. Cache and result
repositories serve different purposes. Generation alone publishes nothing.

## Discover Nix checks from Nix

The flake is the source of truth for systems and check names. From this checkout:

```sh
python3 scripts/nix-workflow.py --list
python3 scripts/nix-workflow.py --output .stoke/nix-host.yaml
python3 scripts/nix-workflow.py --check .stoke/nix-host.yaml
```

Create `.stoke` beforehand if it does not exist. The generator performs Nix
expression evaluation, not check builds. JSON is emitted as valid workflow YAML;
no Python YAML dependency is needed. Existing handwritten files or modified
generated bodies are not overwritten. `--system` selects a supported native
runner system; it does not create that runner or enable emulation.

The emitted host matrix requests caller-attested `nix-build` capability, uses
one active variant, and passes `HOME` and `NIX_CONFIG` explicitly so provisioned
Nix configuration/caches can be used. Additional required configuration such as
certificate variables can be declared with repeated `--inherit-env NAME`.
Credentials do not belong in checked-in YAML or reports.

Run the actual checks locally with the provisioned Nix installation:

```sh
run_dir=$(mktemp -d)
stoke run -f .stoke/nix-host.yaml --context . --allow-host \
  --host-capability nix-build --output "$run_dir/output"
```

No Nix-specific executor is involved. Each selected check is an ordinary generic
host task variant running `scripts/run-nix-check.py`. The helper invokes Nix with
`--max-jobs 1 --cores 2`, retains native Nix sandbox/cache behavior, and captures
`reports/nix.json`, result JSON and raw internal JSON stderr. The input snapshot
contains explicit Stoke source/check paths under `source/`, not runtime
directories, `.git`, a producer's whole Nix store or undeclared project files.
`reports` and the private temporary directory remain outside that flake input;
the helper rejects configurations that would copy its own runtime/log files as
flake source.

Host task `executed` means the host command ran. The report independently marks
the Nix target as `built`, `substituted`, `already-valid`, `unknown` or `failed`.
Only observed successful execution of the exact check builder is reported as
fresh. Reusing a Nix check derivation is a successful cached check result, not a
fresh test run. Unexpected activity schemas remain unknown. The generic runner
does not parse this Nix-specific report.

Captured workspace reports are available after successful host execution. A
failed check keeps its normal Stoke logs and failed status; failed workspace
outputs are not exported to downstream tasks.

Nix must already be configured with the intended store, binary caches, trusted
keys and supported build capabilities. Capability attestation is not a probe or
an installer. This example does not run VM/KVM checks on Stoke's behalf; a flake
that declares such checks needs an appropriately provisioned runner and an
explicit qualification decision. Native ARM and restricted runners need their
own execution evidence.

## Prepare hosted execution; do not push or dispatch

Generate the Nix workflow's native matrix after reviewing local discovery:

```sh
stoke ci generate -f .stoke/nix-host.yaml --context . \
  --output .github/workflows/nix-host.generated.yml \
  --plan-output .stoke/nix-host-plan.json
```

The handwritten caller must explicitly grant host authority:

```yaml
jobs:
  nix-checks:
    uses: ./.github/workflows/nix-host.generated.yml
    with:
      allow_host: true
      host_capabilities: '["nix-build"]'
```

Host tasks cannot grant either input to themselves. Provision Nix, Python and
Stoke on the native runner before the generated workflow is called. The caller
retains its triggers, additional jobs, credentials and approvals. Host-only Nix
checks need no Coopr image runtime. Provision Coopr `0.6.0` for mixed image
workflows.

For the captured-file pipeline, use the generated `host.generated.yml`, explicitly
set `allow_host: true`, and supply its required `results_repository`. Prepare
registry authentication and any package-write permission in the caller only
after authorization. A separate cache repository can be recorded through normal
`--cache-from`/`--cache-to` options when a workflow has a justified explicit
host-cache contract; the fresh examples here do not skip commands using that
cache. Exact tested producer outputs keep their run/attempt/variant identities
independently of disposable cache hits.

Hosted CI and a registry roundtrip require separate runtime evidence. These
commands prepare reviewable files; they do not authorize a push, registry
publication, release, or hosted GitHub Actions run. See the host guide for
captured-file limits, cache contracts, cancellation and trust boundaries.

## Explicit cache contract with a closed-toolchain fixture

`cache.yaml` retains the three visible stages and two fresh verification
variants, but opts only `prepare` into captured-output reuse. Its producer is
the Go standard-library acceptance fixture at
`internal/artifact/testdata/host-builder/main.go`. That fixture reads the named
regular input of at most 1 MiB, creates the named output, writes exactly the
input bytes with mode `0644`, and prints `HOST_PRODUCER_EXECUTED`. Its application
logic reads no clocks or environment variables and invokes no subprocesses.
It is a test fixture, not an installed Stoke worker or a new runtime dependency.

The example requires a separately provisioned, reviewed **static** build at
`/opt/provisioned/stoke-host-builder`. Its executable and toolchain declarations
name that exact file. The caller also attests the `static-host-builder`
capability; neither the YAML nor the fixture grants authority or provisions
anything. No command in this example installs a binary or writes `/opt`.

A developer can prepare a candidate in a new evidence directory with the
provisioned Go toolchain:

```sh
candidate_directory=/path/to/new/evidence
CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -o "$candidate_directory/stoke-host-builder" \
  ./internal/artifact/testdata/host-builder
sha256sum "$candidate_directory/stoke-host-builder"
```

Review that binary and its source before separate runner provisioning. The
runtime acceptance harness can explicitly select an evidence candidate through
`STOKE_HOST_BUILDER`; the checked-in example deliberately keeps the fixed
provisioned path. A shell plus an unlisted `cat` executable would not be an
equivalent closed-toolchain cache fixture.

After the binary is provisioned, local execution is explicit:

```sh
run_dir=$(mktemp -d)
stoke run -f examples/host/cache.yaml --context . --allow-host \
  --host-capability static-host-builder --output "$run_dir/output"
```

Verification and packaging always execute freshly. They consume `prepare`'s
exact captured bytes, and `package` waits for both native verification variants.
Host cache hits restore captured files; they are not evidence of fresh producer
execution. The example's static producer makes its particular input/effect
contract reviewable; it does not establish that arbitrary host commands are
hermetic or cache-safe.

## Prepared manual hosted roundtrip

`.github/workflows/host-dogfood.yml` is a manual-only caller prepared for later
authorization. It has no push or pull-request trigger. Its intended sequence is:

1. Check reviewed provisioned Stoke/fixture SHA256 values, Nix/Python availability,
   the actual Nix inventory, and all checked-in generated CI pairs.
2. Run the fixture workflow locally on the runner and seed an authorized cache.
3. Call `host-cache.generated.yml`, preserving separate native `prepare`,
   verification matrix and `package` jobs with exact registry-backed data edges.
4. Return to a fresh local cache directory and report the actual hit/miss and
   producer digest.
5. Call `nix-host.generated.yml` with explicit host authority and `nix-build`
   attestation; its Nix check variants remain visible native CI matrix entries.

All eligible runners must already carry the reviewed development binaries and
Docker-compatible registry credentials. `packages: write` permits use of the
GitHub token where appropriate but does not log in or install credentials. The
`stoke-development` label is a provisioning/trust assertion about that pool:
preflight can validate its own machine, not inspect every other worker. The
local warm/return jobs recheck their binaries; generated execution still relies
on the reviewed runner pool. No Coopr executable is needed for these host-only
workflows, and Coopr remains unpinned.

Generate the cache plan with the explicit placeholder for review only:

```sh
stoke ci generate -f examples/host/cache.yaml --context . \
  --runner self-hosted --runner linux --runner x64 --runner stoke-development \
  --cache-from registry.invalid/stoke/host-cache \
  --cache-to registry.invalid/stoke/host-cache \
  --output .github/workflows/host-cache.generated.yml \
  --plan-output .stoke/host-cache-plan.json package

stoke ci generate -f .stoke/nix-host.yaml --context . \
  --runner self-hosted --runner linux --runner x64 --runner stoke-development \
  --output .github/workflows/nix-host.generated.yml \
  --plan-output .stoke/nix-host-plan.json
```

The handwritten caller rejects `registry.invalid` before any registry action.
Before an authorized push, regenerate both cache directions with the selected
authorized cache repository and verify the generated pair. The manual caller's
`cache_repository` input must exactly match that reviewed configuration. Its
separate `results_repository` input selects exact run/attempt/variant results;
disposable cache lookup does not replace native success gates or exact-result
identity. The caller also verifies the existing `.stoke/ci-plan.json` pair and
the fresh-only `.stoke/host-plan.json` pair; generate the latter with the same
runner labels and the earlier `examples/host/stoke.yaml` command.

For a hit across local/CI/local phases, the effective PATH, resolved static
tool path/content, UID/groups/umask, kernel/CPU identity and canonical temporary
execution location must match. Configure those consistently through runner
provisioning, including the caller's `TMPDIR`; reusable workflows do not inherit
the caller job's environment overrides. Different locations or machines may
correctly miss and execute the producer again. The post-local job reports that
outcome without claiming cross-hardware equivalence. Compare its producer digest
with the visible CI producer report; a cache hit alone does not identify which
runner originally wrote an equivalent cache entry.

No hosted run, registry write, commit, push or publication has been performed by
this preparation. The caller must remain undispatched until separately approved.
