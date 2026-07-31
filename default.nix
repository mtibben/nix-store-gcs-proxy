{ pkgs ? import <nixpkgs> { } }:

pkgs.buildGoModule {
  pname = "nix-store-gcs-proxy";
  version = "0.1.0";
  src = ./.;
  vendorHash = "sha256-PuSLL8MX/XoTUXO/Y0zvySVmr0J6icdriL2jztN6ZzM=";
}
