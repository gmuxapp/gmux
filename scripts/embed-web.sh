#!/usr/bin/env bash
# Copy the built frontend (apps/gmux-web/dist) into gmuxd's go:embed
# directory and precompress the large assets. Shared by scripts/build.sh
# and the goreleaser `before` hook so a release binary and a dev binary
# embed the same bytes.
#
# The .gz siblings are optional: frontend.go serves one verbatim when the
# client accepts gzip (zero deflate CPU per request, level 9), and falls
# through to the on-the-fly response middleware when it is absent. `gzip -n`
# omits the timestamp so the sibling is reproducible for identical input.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/apps/gmux-web/dist"
WEB_EMBED="$ROOT/services/gmuxd/cmd/gmuxd/web"

[ -f "$DIST/index.html" ] || { echo "embed-web: $DIST/index.html missing; run vite build first" >&2; exit 1; }

rm -rf "$WEB_EMBED/assets" "$WEB_EMBED/favicon.svg" "$WEB_EMBED/manifest.json"
find "$WEB_EMBED" -maxdepth 1 -name '*.gz' -delete
cp -r "$DIST/"* "$WEB_EMBED/"

if command -v gzip >/dev/null 2>&1; then
  find "$WEB_EMBED" \( -name '*.js' -o -name '*.css' -o -name '*.svg' -o -name '*.json' -o -name '*.html' \) \
    -size +1k -exec gzip -9 -k -f -n {} \;
else
  echo "embed-web: gzip not found; skipping precompressed assets (served on the fly instead)" >&2
fi
