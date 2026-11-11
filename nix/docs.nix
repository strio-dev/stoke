{
  lib,
  strio-theme,
  system,
}:
strio-theme.lib.mkZensicalSite {
  inherit system;
  name = "stoke-docs";
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../docs
      ../zensical.toml
    ];
  };
}
