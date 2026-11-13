{
  lib,
  buildGoModule,
  go,
  makeWrapper,
  crun,
  coreutils,
  gnutar,
  gzip,
  netavark,
  aardvark-dns,
  passt,
  slirp4netns,
}:
(buildGoModule.override { inherit go; }) {
  pname = "stoke";
  version = "dev";
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../cmd
      ../internal
      ../go.mod
      ../go.sum
      ../LICENSE
    ];
  };
  vendorHash = "sha256-gq+Pwi5Eh04yIkMDrKXs4N4Zbu8MAWRA+N0WruWHNSs=";
  subPackages = [ "cmd/stoke" ];
  env.CGO_ENABLED = "0";
  nativeBuildInputs = [ makeWrapper ];
  postInstall = ''
    install -Dm644 LICENSE "$out/share/licenses/stoke/LICENSE"
    wrapProgram "$out/bin/stoke" --prefix PATH : ${
      lib.makeBinPath [
        crun
        coreutils
        gnutar
        gzip
        netavark
        aardvark-dns
        passt
        slirp4netns
      ]
    }
  '';
  meta = {
    license = lib.licenses.mit;
    mainProgram = "stoke";
  };
}
