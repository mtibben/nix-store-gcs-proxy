#!/usr/bin/env nix-shell
#! nix-shell -p go_1_26 git -i bash
mkdir -p out
GOPATH=~/go go build -v -a -ldflags '-extldflags \"-static\"' -o out/nix-store-gcs-proxy-$(git describe --tags)-${GOOS}-${GOARCH} .
