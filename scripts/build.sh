#!/usr/bin/env bash
# Build gmuxd and gmux release binaries.
# Usage: ./scripts/build.sh [--skip-frontend]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
WEB_EMBED="$ROOT/services/gmuxd/cmd/gmuxd/web"

# A toolchain directive is a preferred minimum under GOTOOLCHAIN=auto; a newer
# ambient Go can still win. Builds must use the repository's exact toolchain.
GO_TOOLCHAIN="$("$ROOT/scripts/go-toolchain.sh")"
export GOTOOLCHAIN="$GO_TOOLCHAIN"
if ! GO_VERSION="$(go version 2>&1)"; then
  echo "error: required Go toolchain $GO_TOOLCHAIN is unavailable." >&2
  echo "The go command could not find or download it:" >&2
  echo "$GO_VERSION" >&2
  exit 1
fi
echo "→ Using $GO_VERSION"

skip_frontend=false
for arg in "$@"; do
  case "$arg" in
    --skip-frontend) skip_frontend=true ;;
  esac
done

mkdir -p "$BIN"

# ── Frontend ──

if [ "$skip_frontend" = false ]; then
  echo "→ Building frontend…"
  pnpm -C "$ROOT/apps/gmux-web" exec vite build

  # Copy dist into the go:embed directory
  rm -rf "$WEB_EMBED/assets" "$WEB_EMBED/favicon.svg" "$WEB_EMBED/manifest.json"
  cp -r "$ROOT/apps/gmux-web/dist/"* "$WEB_EMBED/"
  echo "  Embedded $(du -sh "$WEB_EMBED" | cut -f1) of frontend assets"
fi

# ── Go binaries ──

VERSION="${VERSION:-dev}"
LDFLAGS_COMMON="-s -w -X main.version=$VERSION"
export CGO_ENABLED=0

echo "→ Building gmuxd…"
(cd "$ROOT/services/gmuxd" && go build -ldflags "$LDFLAGS_COMMON" -o "$BIN/gmuxd" ./cmd/gmuxd)

echo "→ Building gmux…"
(cd "$ROOT/cli/gmux" && go build -ldflags "$LDFLAGS_COMMON" -o "$BIN/gmux" ./cmd/gmux)

echo ""
ls -lh "$BIN/gmuxd" "$BIN/gmux"
echo "✓ Build complete"
