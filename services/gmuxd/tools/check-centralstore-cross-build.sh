#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*}
  arch=${target#*/}
  echo "CGO-free compile: $target"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go test -c \
    -o "$out/centralstore-$os-$arch.test" ./internal/centralstore
done
