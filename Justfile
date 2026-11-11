set default-list

build:
    nix build --no-update-lock-file path:.#stoke --out-link result-stoke

test:
    nix develop --no-update-lock-file path:. -c go test -short ./...

vet:
    nix develop --no-update-lock-file path:. -c go vet ./...

lint:
    nix develop --no-update-lock-file path:. -c golangci-lint run

fmt:
    nix develop --no-update-lock-file path:. -c gofmt -w cmd internal
    nix develop --no-update-lock-file path:. -c nixfmt flake.nix nix/*.nix
    nix develop --no-update-lock-file path:. -c just --fmt

check:
    nix flake check --no-update-lock-file path:.

# Literal/native execution, local cache, input changes, gates and cancellation.
integration: build
    nix develop --no-update-lock-file path:. -c python3 scripts/native-smoke.py

# Prove task/matrix overlap, dependency gates, cache isolation and cancellation.
dag-integration: build
    nix develop --no-update-lock-file path:. -c python3 scripts/dag-smoke.py

# Actual packaged workflow and nested Coopr components after offline relocation.
oci-integration: build
    STOKE_BINARY="{{ justfile_directory() }}/result-stoke/bin/stoke" nix develop --no-update-lock-file path:. -c go test -count=1 -timeout=8m -v ./internal/artifact -run '^TestPackagedWorkflowWithCooprBale$'

# Reusable OCI input declarations, invocation isolation and Coopr cache reuse.
input-integration: build
    STOKE_BINARY="{{ justfile_directory() }}/result-stoke/bin/stoke" nix develop --no-update-lock-file path:. -c go test -count=1 -timeout=6m -v ./internal/artifact -run '^TestReusableInputsWithCoopr$'

# Registry cache export, fresh-consumer restore, directional access and failures.
registry-cache-integration: build
    STOKE_BINARY="{{ justfile_directory() }}/result-stoke/bin/stoke" nix develop --no-update-lock-file path:. -c go test -count=1 -timeout=20m -v ./internal/artifact -run '^Test(RegistryCacheWithCoopr|CIRegistryCacheRoundTripWithCoopr)$'

# Real RUN mount sources, side effects, native environments and cleanup.
mount-integration: build
    STOKE_BINARY="{{ justfile_directory() }}/result-stoke/bin/stoke" nix develop --no-update-lock-file path:. -c go test -count=1 -timeout=12m -v ./internal/artifact -run '^TestMountsAndEnvironmentsWithCoopr$'

# AMD64/ARM64 execution through the runner's existing Buildah/QEMU support.
platform-integration: build
    STOKE_BINARY="{{ justfile_directory() }}/result-stoke/bin/stoke" nix develop --no-update-lock-file path:. -c go test -count=1 -timeout=12m -v ./internal/artifact -run '^TestPlatformsWithCoopr$'

# Verify checked-in GitHub Actions generation without changing handwritten CI.
ci-check: build
    result-stoke/bin/stoke ci generate --check -f examples/ci/stoke.yaml --context . package

# Registry-only local/CI/local reuse, input invalidation and generated job gates.
ci-integration: build
    STOKE_BINARY="{{ justfile_directory() }}/result-stoke/bin/stoke" nix develop --no-update-lock-file path:. -c go test -count=1 -timeout=15m -v ./internal/artifact -run '^TestCI(Registry|Generated|Exact)'

docs-build:
    nix build --no-update-lock-file path:.#docs --out-link result-docs

docs-check: docs-build

docs-serve:
    nix run --no-update-lock-file path:.#docs-preview
