# Qualify registry transport across isolated guest users

The separate `Stoke native runtime` workflow runs existing mount and local
registry suites on native GitHub-hosted x86_64 and ARM runners. Each job uses
the checksum-pinned Coopr `0.6.0` archive, a source-built Stoke package and its
own loopback registry. It installs no mapping helper or security-policy
workaround. Job results establish support only for the runner and package
actually tested. Cross-machine shared-registry reuse and GHCR remain separate
from these per-host tests.

`nix/registry-portability.nix` is an optional local acceptance fixture. Import it
with already selected local `pkgs`, `stoke` and `coopr` derivations; it adds no
Coopr production pin or flake input. Coopr must expose its current `buildImage`
Nix interface. For example, write a local expression outside either checkout:

```nix
let
  engine = builtins.getFlake "github:strio-dev/coopr/35871e4b01e60657b45fd17e04ac1db119f839e6";
  workflow = builtins.getFlake "path:/absolute/stoke";
  system = "x86_64-linux";
  pkgs = import engine.inputs.nixpkgs { inherit system; };
in import /absolute/stoke/nix/registry-portability.nix {
  inherit pkgs;
  coopr = engine.packages.${system}.coopr;
  stoke = workflow.packages.${system}.stoke;
}
```

Run the expression with the normal sandboxed Nix builder and a resource guard:

```sh
nix --max-jobs 1 --cores 2 build --impure --file /absolute/portability-test.nix \
  --out-link /absolute/new-evidence/portability-result
```

Main owns execution and qualification. A checked-in fixture is not a passing
receipt. It requires readable/writable KVM and enough memory/storage for two
simultaneous guests. It requests one vCPU and 2,304 MiB per guest, separate
8 GiB writable disks and a fifteen-minute test deadline. Image preparation has
its own Nix costs; the outer resource guard must also bound it. No host security
settings, users, container stores or existing user data are changed.

The producer is UID 1000 and the consumer UID 2000, with distinct homes, native
stores and guest kernels. Both use disk-backed guest Nix stores. Test-script
reference injection is disabled, and the Nix input/tool is provisioned only on
the producer. The consumer asserts that the producer-only store path and base
layout are absent from its host filesystem before and after image consumption.
Nix builds the input closure; normal Coopr runs outside Nix to compose images.

The fixture generates and regenerates visible build/derive/test-matrix/package
jobs, preserving a handwritten workflow unchanged. It checks rendered native
prerequisite edges and unique variant metadata slots, then invokes the actual
`ci run` commands across guests. The driver runs the matrix rows serially to
bound resources; it checks generated matrix concurrency configuration without
claiming concurrent hosted scheduling. Small native
output metadata passes through the test driver. Exact payloads and cache graphs
travel through the disposable guest registry; there are no CI artifact transfers,
success receipts, shared result directories or shared host Nix store.

It checks a particular random producer output, fresh tests, cross-user cache
reuse, prior-attempt consumer retry with empty caches, canonical tested-image
publication to the local registry, native failed-gate refusal and deliberate
loss of exact input metadata. Missing tested data must fail without rebuilding
the producer or running tests against replacement bytes. The loss injection is
not retention automation and does not establish when production retries expire.

The test result contains normal NixOS VM logs and a final copied `summary.json`;
the producer retains `/home/ci/evidence/summary.json`, per-job reports and logs during the test. The
test driver may inspect them for verification, but consumers do not use those
files as transport. The fixture manually coordinates projected job commands;
it does not execute GitHub Actions or prove provider partial-rerun metadata
preservation. Separate guest kernels on one physical host are not another
physical-machine, native ARM, restricted-runner or live GHCR qualification.
