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
  vendorHash = "sha256-vkK2TZJDUh30ZZHvoYt9eIOk9vasginbSrpBY+BtMKY=";
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
