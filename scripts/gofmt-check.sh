#!/usr/bin/env bash
# Fail if any first-party Go file is not gofmt-clean.
#
# `go vet` (the per-module `lint` task) says nothing about formatting, so
# formatting drift only ever surfaced in review — and did: a deleted function
# left two stray blank lines in cli/gmux/cmd/gmux/main.go, plus a dozen
# misaligned comment and struct-tag blocks elsewhere. This costs seconds, so it
# belongs in the fast `checks` job rather than in a nightly.
#
# gofmt's output depends on the toolchain, so use the repository's pinned one
# (CI's setup-go resolves the same version from go.work).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GO_TOOLCHAIN="$("$ROOT/scripts/go-toolchain.sh")"
export GOTOOLCHAIN="$GO_TOOLCHAIN"

# `go fmt` shells out to the selected toolchain's gofmt, which is how a pinned
# toolchain gets honoured: a bare `gofmt` is whatever is on PATH.
GOFMT="$(go env GOROOT)/bin/gofmt"
if [ ! -x "$GOFMT" ]; then
  echo "error: no gofmt in $(go env GOROOT)/bin (toolchain $GO_TOOLCHAIN)" >&2
  exit 1
fi

# third_party/ is vendored upstream code and node_modules/ is not ours.
mapfile -t files < <(cd "$ROOT" && find . -name '*.go' \
  -not -path './node_modules/*' \
  -not -path './third_party/*' \
  -not -path './.git/*' \
  -not -path './.jj/*' \
  -not -path './.grove/*' | sort)

if [ "${#files[@]}" -eq 0 ]; then
  echo "error: found no Go files under $ROOT" >&2
  exit 1
fi

unformatted="$(cd "$ROOT" && "$GOFMT" -l "${files[@]}")"
if [ -n "$unformatted" ]; then
  echo "error: these files are not gofmt-clean (run: gofmt -w <file>)" >&2
  printf '%s\n' "$unformatted" >&2
  exit 1
fi

echo "gofmt: ${#files[@]} files clean ($GO_TOOLCHAIN)"
