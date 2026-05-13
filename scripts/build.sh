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

VERSION="${VERSION:-dev}"
LDFLAGS_COMMON="-s -w -X main.version=$VERSION"
export CGO_ENABLED=0

for os in linux darwin; do
  echo "→ Building gmuxd ($os/arm64)…"
  (cd "$ROOT/services/gmuxd" && GOOS=$os GOARCH=arm64 go build -ldflags "$LDFLAGS_COMMON" -o "$BIN/gmuxd-$os-arm64" ./cmd/gmuxd)

  echo "→ Building gmux ($os/arm64)…"
  (cd "$ROOT/cli/gmux" && GOOS=$os GOARCH=arm64 go build -ldflags "$LDFLAGS_COMMON" -o "$BIN/gmux-$os-arm64" ./cmd/gmux)
done

echo ""
ls -lh "$BIN"/gmuxd-* "$BIN"/gmux-*
echo "✓ Build complete"
