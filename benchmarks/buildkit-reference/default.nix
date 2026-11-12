# Benchmark-only addition to the existing development tools image.
{
  stokeFlake,
  cooprFlake,
  system,
}:
let
  pkgs = import stokeFlake.inputs.nixpkgs { inherit system; };
  lib = pkgs.lib;
  go = pkgs.callPackage (stokeFlake.outPath + "/nix/go.nix") { };
  reference = (pkgs.buildGoModule.override { inherit go; }) {
    pname = "stoke-buildkit-reference";
    version = "benchmark";
    src = lib.fileset.toSource {
      root = ./reference;
      fileset = lib.fileset.unions [
        ./reference/main.go
        ./reference/worker.go
        ./reference/workload.go
        ./reference/registry.go
        ./reference/observe.go
        ./reference/proof.go
        ./reference/go.mod
        ./reference/go.sum
      ];
    };
    vendorHash = "sha256-lFpdpKv2zbq+HorijkbcdUNRV4JCihjxE7MtDJzrWs0=";
    subPackages = [ "." ];
    env = {
      CGO_ENABLED = "0";
      GOMEMLIMIT = "2GiB";
    };
    preBuild = ''
      export GOMAXPROCS="$NIX_BUILD_CORES"
    '';
    doCheck = false; # Runtime qualification is separate from image preparation.
    postInstall = ''
      mv "$out/bin/stoke-buildkit-reference" "$out/bin/buildkit-reference"
    '';
  };
  wrapper = pkgs.writeShellApplication {
    name = "benchmark-rootless";
    runtimeInputs = [
      pkgs.rootlesskit
      pkgs.slirp4netns
      pkgs.coreutils
      pkgs.python3
      pkgs.bash
    ];
    text = ''
      # Keep the image's verified setuid mapping helpers first.
      export PATH="/usr/bin:/bin:${
        lib.makeBinPath [
          pkgs.rootlesskit
          pkgs.slirp4netns
          pkgs.coreutils
          pkgs.python3
          pkgs.bash
        ]
      }"
      ${builtins.readFile ./rootless.sh}
    '';
  };
  base = stokeFlake.lib.mkContainer {
    inherit cooprFlake system;
    uid = 1001;
    gid = 1001;
  };
  image = base.override (args: {
    stoke = pkgs.symlinkJoin {
      name = "stoke-benchmark-tools";
      paths = [
        args.stoke
        reference
        wrapper
        pkgs.rootlesskit
        pkgs.slirp4netns
      ];
    };
  });
  backend = import cooprFlake.inputs.nix2container { inherit pkgs; };
in
{
  inherit image reference wrapper;
  copyTo = image.copyTo;
  skopeo = backend.skopeo-nix2container;
}
