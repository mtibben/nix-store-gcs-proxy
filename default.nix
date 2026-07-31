{
  pkgs ? import <nixpkgs> { },
  version ? "dev",
}:

pkgs.buildGoModule {
  pname = "nix-store-gcs-proxy";
  inherit version;
  src = ./.;
  vendorHash = "sha256-RTaTYe/sBcpK3v6eMEc9DnpGdy+bRZP+lrkl12q0P8I=";
  ldflags = [ "-X main.version=${version}" ];

  nativeCheckInputs = [ pkgs.golangci-lint ];
  checkPhase = ''
    runHook preCheck

    export GOLANGCI_LINT_CACHE="$TMPDIR/golangci-lint-cache"
    go test ./...
    golangci-lint run

    runHook postCheck
  '';
}
