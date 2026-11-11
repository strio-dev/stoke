# Development tools image. The caller selects Coopr and the runner identity;
# this recipe does not choose a production engine pin or a runtime security policy.
{
  lib,
  pkgs,
  stoke,
  coopr,
  baseImage,
  nix2container,
  uid,
  gid ? uid,
}:
assert lib.assertMsg (
  builtins.isInt uid && uid >= 1 && uid < 65535 && builtins.isInt gid && gid >= 1 && gid < 65535
) "the tools image requires explicit non-root uid/gid values below 65535";
let
  # Match Coopr's subordinate-map layout while excluding the selected outer ID.
  subordinate =
    id:
    lib.optionalString (id > 1) "stoke:1:${toString (id - 1)}\n"
    + "stoke:${toString (id + 1)}:${toString (65536 - id)}\n";
  root = pkgs.runCommand "stoke-container-config" { } ''
    mkdir -p "$out"/{etc/containers,home/stoke/.local/share,home/stoke/.cache,run/stoke}
    printf 'root:x:0:0::/root:/bin/false\nstoke:x:${toString uid}:${toString gid}::/home/stoke:/bin/bash\n' > "$out/etc/passwd"
    printf 'root:x:0:\nstoke:x:${toString gid}:\n' > "$out/etc/group"
    printf '%s' ${lib.escapeShellArg (subordinate uid)} > "$out/etc/subuid"
    printf '%s' ${lib.escapeShellArg (subordinate gid)} > "$out/etc/subgid"
    # VFS avoids requiring a FUSE device merely to use the tools image.
    printf '[storage]\ndriver = "vfs"\n' > "$out/etc/containers/storage.conf"
    # Upstream nix-ld supports the runner's externally mounted Node binary.
    interpreter=$(cat ${pkgs.nix-ld}/nix-support/ldpath)
    mkdir -p "$out$(dirname "$interpreter")"
    ln -s ${pkgs.nix-ld}/libexec/nix-ld "$out$interpreter"
  '';
  tools = pkgs.buildEnv {
    name = "stoke-container-tools";
    paths = [
      stoke
      coopr
      pkgs.bash
      pkgs.coreutils
      pkgs.gnutar
      pkgs.gzip
      pkgs.gnugrep
    ];
    pathsToLink = [
      "/bin"
      "/share/licenses/stoke"
    ];
  };
in
nix2container.buildImage {
  name = "stoke-tools";
  tag = "dev";
  fromImage = baseImage;
  fromImageEnv = true;
  copyToRoot = [
    root
    tools
  ];
  perms = [
    {
      path = root;
      regex = "^${root}/home/stoke(/.*)?$";
      mode = "0755";
      inherit uid gid;
    }
    {
      path = root;
      regex = "^${root}/run/stoke$";
      mode = "0700";
      inherit uid gid;
    }
  ];
  config = {
    User = "${toString uid}:${toString gid}";
    WorkingDir = "/work";
    Entrypoint = [ "/bin/bash" ];
    Env = [
      # Preserve the base image's setuid mapping helpers ahead of ordinary tools.
      "PATH=/usr/bin:/bin"
      "STOKE_COOPR_BINARY=${coopr}/bin/coopr"
      "HOME=/home/stoke"
      "USER=stoke"
      "XDG_DATA_HOME=/home/stoke/.local/share"
      "XDG_CACHE_HOME=/home/stoke/.cache"
      "XDG_RUNTIME_DIR=/run/stoke"
      "BUILDAH_ISOLATION=rootless"
      "NIX_LD=${pkgs.stdenv.cc.bintools.dynamicLinker}"
      "NIX_LD_LIBRARY_PATH=${lib.makeLibraryPath [ pkgs.stdenv.cc.cc.lib ]}"
    ];
  };
}
