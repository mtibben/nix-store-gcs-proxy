{
  pkgs ? import <nixpkgs> { },
}:

pkgs.buildGoModule {
  pname = "nix-store-gcs-proxy";
  version = "0.1.0";
  src = ./.;
  vendorHash = "sha256-PuSLL8MX/XoTUXO/Y0zvySVmr0J6icdriL2jztN6ZzM=";

  nativeCheckInputs = [ pkgs.golangci-lint ];
  checkPhase = ''
    runHook preCheck

    export GOLANGCI_LINT_CACHE="$TMPDIR/golangci-lint-cache"
    go test ./...
    golangci-lint run

    runHook postCheck
  '';
}
