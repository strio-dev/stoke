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
stoke ci generate github -f examples/host/stoke.yaml --context . --co-locate prepare
```

Co-location keeps captured files and fresh verification gates in one job.

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
stoke ci generate github -f .stoke/nix-host.yaml --context .
```

In your native workflow, call the generated action with `allow-host: "true"`
and `host-capabilities: '["nix-build"]'`. Host-only Nix checks require Nix,
Python and Stoke on the native runner.

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
