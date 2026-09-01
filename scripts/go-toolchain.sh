#!/usr/bin/env bash
# Print the repository's exact Go toolchain after checking duplicated module pins.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GO_TOOLCHAIN="$(awk '$1 == "toolchain" { print $2; exit }' "$ROOT/go.work")"
if [[ ! "$GO_TOOLCHAIN" =~ ^go[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "error: go.work must declare an exact 'toolchain goX.Y.Z' version" >&2
  exit 1
fi

for module in cli/gmux services/gmuxd; do
  module_toolchain="$(awk '$1 == "toolchain" { print $2; exit }' "$ROOT/$module/go.mod")"
  if [[ "$module_toolchain" != "$GO_TOOLCHAIN" ]]; then
    echo "error: $module/go.mod toolchain '${module_toolchain:-<missing>}' does not match go.work toolchain '$GO_TOOLCHAIN'" >&2
    exit 1
  fi
done

printf '%s\n' "$GO_TOOLCHAIN"
