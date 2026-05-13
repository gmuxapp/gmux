#!/usr/bin/env bash
# Start the gmux-web dev server inside the Linux sandbox.
#
# The pnpm store was installed on macOS (darwin-arm64), so the esbuild native
# binary is the wrong platform. This script installs the linux-arm64 binary
# once into /tmp and wires it up via ESBUILD_BINARY_PATH.
#
# ── Usage ──────────────────────────────────────────────────────────────────
#
#   # Mock mode (no live gmuxd needed):
#   VITE_MOCK=1 ./scripts/sandbox-dev.sh
#
#   # Live mode — proxy to host gmuxd (requires network policy allow):
#   ./scripts/sandbox-dev.sh
#
#   # Live mode — custom port or host:
#   VITE_DEV_PROXY_PORT=8790 VITE_DEV_PROXY_HOST=127.0.0.1 ./scripts/sandbox-dev.sh
#
#   # Custom server port:
#   PORT=3000 VITE_MOCK=1 ./scripts/sandbox-dev.sh
#
# ── Making the dev server reachable from the host browser ──────────────────
#
#   The server binds to 0.0.0.0 so it's reachable after port publishing.
#   Run this on your HOST machine (not inside the sandbox):
#
#     sbx ports pi-workspace --publish 5174:5174
#
#   Then open http://localhost:5174 in your browser.
#
# ── Live mode network policy (host) ────────────────────────────────────────
#
#   The sandbox proxy target is host.docker.internal:8790 by default.
#   You need to allow outbound access from the sandbox:
#
#     sbx policy allow network "host.docker.internal:8790"
#
# ── Screenshots from inside the sandbox ────────────────────────────────────
#
#   uv run --with playwright python3 -c "
#     from playwright.sync_api import sync_playwright
#     with sync_playwright() as p:
#       b = p.chromium.launch(headless=True)
#       pg = b.new_page(viewport={'width':1280,'height':900})
#       pg.goto('http://127.0.0.1:5174/')
#       pg.wait_for_timeout(3000)
#       pg.screenshot(path='/tmp/screenshot.png', full_page=True)
#       b.close()
#   "
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
WEB_DIR="$REPO_ROOT/apps/gmux-web"

ESBUILD_VERSION="0.25.12"
ESBUILD_INSTALL_DIR="/tmp/esbuild-linux-arm64-${ESBUILD_VERSION}"
ESBUILD_BIN="$ESBUILD_INSTALL_DIR/node_modules/@esbuild/linux-arm64/bin/esbuild"

# Install linux-arm64 esbuild binary once.
if [[ ! -x "$ESBUILD_BIN" ]]; then
  echo "→ Installing @esbuild/linux-arm64@${ESBUILD_VERSION} …"
  mkdir -p "$ESBUILD_INSTALL_DIR"
  npm install --prefix "$ESBUILD_INSTALL_DIR" \
    "@esbuild/linux-arm64@${ESBUILD_VERSION}" \
    --no-save --loglevel=error
fi

export ESBUILD_BINARY_PATH="$ESBUILD_BIN"
export VITE_MOCK="${VITE_MOCK:-0}"

PORT="${PORT:-5174}"

if [[ "${VITE_MOCK}" == "1" ]]; then
  echo "→ Starting Vite dev server in MOCK mode (port=${PORT}) …"
else
  PROXY_HOST="${VITE_DEV_PROXY_HOST:-${IS_SANDBOX:+host.docker.internal}}"
  PROXY_HOST="${PROXY_HOST:-127.0.0.1}"
  PROXY_PORT="${VITE_DEV_PROXY_PORT:-8790}"
  echo "→ Starting Vite dev server, proxy → ${PROXY_HOST}:${PROXY_PORT} (port=${PORT}) …"
  export VITE_DEV_PROXY_HOST="$PROXY_HOST"
  export VITE_DEV_PROXY_PORT="$PROXY_PORT"
fi

cd "$WEB_DIR"
exec node_modules/.bin/vite \
  --config vite.config.sandbox.mjs \
  --port "${PORT}" \
  --host 0.0.0.0 \
  "$@"
