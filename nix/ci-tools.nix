let
  stoke = builtins.getFlake ("path:" + builtins.getEnv "GITHUB_WORKSPACE");
  cooprBase = builtins.getFlake "github:strio-dev/coopr/8a9a12c29263965309b10943faf417197b39698f";
  system = builtins.getEnv "TOOLS_SYSTEM";
  tracePath = builtins.getEnv "COOPR_TRACE_PATCH";
  tracePatch = builtins.toFile "coopr-perf-trace.patch" (
    builtins.readFile (builtins.getEnv "COOPR_TRACE_PATCH")
  );
  coopr =
    if tracePath == "" then
      cooprBase
    else
      cooprBase
      // {
        packages = cooprBase.packages // {
          ${system} = cooprBase.packages.${system} // {
            coopr = cooprBase.packages.${system}.coopr.overrideAttrs (old: {
              postPatch = (old.postPatch or "") + ''
                patch --batch -p1 --fuzz=0 < ${tracePatch}
              '';
            });
          };
        };
      };
  pkgs = import stoke.inputs.nixpkgs { inherit system; };
  backend = import coopr.inputs.nix2container { inherit pkgs; };
  image = stoke.lib.mkContainer {
    cooprFlake = coopr;
    inherit system;
    uid = 1001;
    gid = 1001;
  };
in
if builtins.getEnv "BUILD_BENCHMARK_REFERENCE" == "true" then
  import (stoke.outPath + "/benchmarks/buildkit-reference") {
    stokeFlake = stoke;
    cooprFlake = coopr;
    inherit system;
  }
else
  {
    inherit image;
    copyTo = image.copyTo;
    skopeo = backend.skopeo-nix2container;
  }
