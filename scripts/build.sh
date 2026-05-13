#!/usr/bin/env bash
# Build gmuxd and gmux release binaries.
# Usage: ./scripts/build.sh [--skip-frontend]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
WEB_EMBED="$ROOT/services/gmuxd/cmd/gmuxd/web"

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
  (cd "$ROOT/apps/gmux-web" && npx vite build)

  # Copy dist into the go:embed directory
  rm -rf "$WEB_EMBED/assets" "$WEB_EMBED/favicon.svg" "$WEB_EMBED/manifest.json"
  cp -r "$ROOT/apps/gmux-web/dist/"* "$WEB_EMBED/"
  echo "  Embedded $(du -sh "$WEB_EMBED" | cut -f1) of frontend assets"
fi

# ── Go binaries ──

VERSION="${VERSION:-$(git rev-parse --short HEAD 2>/dev/null || echo dev)+$(date -u '+%Y%m%dT%H%M%SZ')}"
LDFLAGS_COMMON="-s -w -X main.version=$VERSION"
export CGO_ENABLED=0

HOST_GOOS=$(go env GOOS)
HOST_GOARCH=$(go env GOARCH)

build_pair() {
  local goos=$1 goarch=$2
  local suffix="${goos}-${goarch}"
  echo "→ Building gmuxd (${suffix})…"
  (cd "$ROOT/services/gmuxd" && GOOS=$goos GOARCH=$goarch go build -ldflags "$LDFLAGS_COMMON" -o "$BIN/gmuxd-${suffix}" ./cmd/gmuxd)
  echo "→ Building gmux (${suffix})…"
  (cd "$ROOT/cli/gmux" && GOOS=$goos GOARCH=$goarch go build -ldflags "$LDFLAGS_COMMON" -o "$BIN/gmux-${suffix}" ./cmd/gmux)
}

# Always build host arch
build_pair "$HOST_GOOS" "$HOST_GOARCH"

# Cross-compile for the other common target (linux↔darwin, arm64 only)
if [ "$HOST_GOOS" = "linux" ] && [ "$HOST_GOARCH" = "arm64" ]; then
  build_pair darwin arm64
elif [ "$HOST_GOOS" = "darwin" ] && [ "$HOST_GOARCH" = "arm64" ]; then
  build_pair linux arm64
fi

# Symlink host binaries to the bare names for local use
ln -sf "gmuxd-${HOST_GOOS}-${HOST_GOARCH}" "$BIN/gmuxd"
ln -sf "gmux-${HOST_GOOS}-${HOST_GOARCH}"  "$BIN/gmux"

echo ""
ls -lh "$BIN/gmuxd-"* "$BIN/gmux-"*
echo "✓ Build complete"
