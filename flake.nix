{
  description = "Stoke portable workflows on Coopr";
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    flake-parts.inputs.nixpkgs-lib.follows = "nixpkgs";
    strio-theme.url = "github:strio-dev/strio-zensical-theme";
    strio-theme.inputs.nixpkgs.follows = "nixpkgs";
    strio-theme.inputs.flake-parts.follows = "flake-parts";
  };
  outputs =
    inputs:
    inputs.flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      # Source-parameterized development packaging; no production Coopr pin.
      flake.lib.mkContainer =
        {
          cooprFlake,
          system,
          uid,
          gid ? uid,
        }:
        let
          pkgs = import inputs.nixpkgs { inherit system; };
        in
        pkgs.callPackage ./nix/container.nix {
          inherit uid gid;
          stoke = inputs.self.packages.${system}.stoke;
          coopr = cooprFlake.packages.${system}.coopr;
          baseImage = cooprFlake.packages.${system}.container;
          nix2container = (import cooprFlake.inputs.nix2container { inherit pkgs; }).nix2container;
        };
      perSystem =
        {
          config,
          pkgs,
          system,
          ...
        }:
        let
          go = pkgs.callPackage ./nix/go.nix { };
          stoke = pkgs.callPackage ./nix/package.nix { inherit go; };
          docs = pkgs.callPackage ./nix/docs.nix {
            inherit system;
            inherit (inputs) strio-theme;
          };
          quality = stoke.overrideAttrs {
            pname = "stoke-quality";
            nativeBuildInputs = stoke.nativeBuildInputs ++ [
              go
              pkgs.golangci-lint
            ];
            buildPhase = "true";
            doCheck = true;
            checkPhase = ''
              # Honor the runner's declared Nix core budget for direct Go checks.
              export GOMAXPROCS="$NIX_BUILD_CORES"
              export GOFLAGS="-p=$NIX_BUILD_CORES"
              export GOMEMLIMIT=2GiB
              export GOCACHE="$TMPDIR/go-cache"
              export GOLANGCI_LINT_CACHE="$TMPDIR/lint-cache"
              go test -short ./...
              go vet ./...
              golangci-lint run
            '';
            installPhase = "touch $out";
            postInstall = "";
          };
        in
        {
          packages = {
            default = stoke;
            inherit stoke docs;
          };
          apps.default = {
            type = "app";
            program = "${stoke}/bin/stoke";
          };
          apps.docs-preview = inputs.strio-theme.lib.mkPreview {
            inherit system;
            sourcePaths = [
              "docs"
              "zensical.toml"
            ];
          };
          checks = {
            build = stoke;
            inherit quality docs;
            helpers =
              pkgs.runCommand "stoke-helpers"
                {
                  nativeBuildInputs = [ pkgs.python3 ];
                  src = pkgs.lib.fileset.toSource {
                    root = ./.;
                    fileset = pkgs.lib.fileset.unions [
                      ./scripts/nix-workflow.py
                      ./scripts/run-nix-check.py
                      ./scripts/test-nix-workflow.py
                      ./scripts/cache-performance.py
                      ./scripts/tests/test_cache_performance.py
                      ./scripts/check-ghcr-qualification.py
                      ./scripts/tests/test_ghcr_qualification.py
                    ];
                  };
                }
                ''
                  python3 -B "$src/scripts/test-nix-workflow.py"
                  python3 -B "$src/scripts/tests/test_cache_performance.py"
                  python3 -B "$src/scripts/tests/test_ghcr_qualification.py"
                  touch "$out"
                '';
            workflows = pkgs.runCommand "stoke-workflows" { nativeBuildInputs = [ pkgs.actionlint ]; } ''
              actionlint -config-file ${./.github/actionlint.yaml} ${./.github/workflows}/*.yml
              touch "$out"
            '';
            format =
              pkgs.runCommand "stoke-format"
                {
                  nativeBuildInputs = [
                    go
                    pkgs.nixfmt
                    pkgs.just
                  ];
                  src = pkgs.lib.cleanSource ./.;
                }
                ''
                  cp -r "$src" source
                  chmod -R u+w source
                  cd source
                  test -z "$(gofmt -l cmd internal)"
                  nixfmt --check flake.nix nix/*.nix
                  just --fmt --check
                  touch "$out"
                '';
          };
          formatter = pkgs.nixfmt-tree;
          devShells.default = pkgs.mkShell {
            inputsFrom = [ stoke ];
            packages = [
              go
              pkgs.golangci-lint
              pkgs.nixfmt
              pkgs.just
              pkgs.crun
              pkgs.netavark
              pkgs.aardvark-dns
              pkgs.passt
              pkgs.slirp4netns
              pkgs.pkgsStatic.busybox
              pkgs.python3
              inputs.strio-theme.packages.${system}.zensical
            ];
            GOTOOLCHAIN = "local";
            STOKE_SMOKE_BUSYBOX = "${pkgs.pkgsStatic.busybox}/bin/busybox";
          };
        };
    };
}
