{ go_1_27, fetchurl }:
# Keep the pinned Nix environment while taking Go's GO-2026-6809 security fix.
go_1_27.overrideAttrs {
  version = "1.27.3";
  src = fetchurl {
    url = "https://go.dev/dl/go1.27.3.src.tar.gz";
    hash = "sha256-bndJA1zzYPBrJXXRYup535/ajvZxiJ5cN1+OUHd/Jrs=";
  };
}
