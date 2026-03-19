#!/usr/bin/env bash
# Isolated dev environment for gmux.
#
# Completely separate from production: different port, socket dir,
# state dir, tailscale hostname, and config.
#
# What runs:
#   1. vite dev server — HMR for frontend
#   2. gmuxd — API, WebSocket, Tailscale auth; proxies frontend to vite
#   3. watchexec — rebuilds + restarts gmuxd on Go changes
#
# Everything is served through gmuxd on port 8791.
# Local and Tailscale access get the same frontend (vite HMR).
#
# Usage: ./scripts/dev.sh
#
# Launch dev sessions:
#   source scripts/dev-env.sh
#   gmux-dev <cmd>

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

DEV_PORT=8791
DEV_VITE_PORT=5173
DEV_SOCKET_DIR="/tmp/gmux-dev-sessions"
# Persistent state dir so tailscale auth survives reboots (tsnet keeps its
# private key and auth token here). Only session sockets live in /tmp.
DEV_STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/gmux-dev"

# ── Prepare directories and config ──

mkdir -p "$DEV_SOCKET_DIR" "$DEV_STATE_DIR/config/gmux" "$DEV_STATE_DIR/state"

cat > "$DEV_STATE_DIR/config/gmux/config.toml" << EOF
port = $DEV_PORT

[tailscale]
enabled = true
hostname = "gmux-dev"
EOF

# ── Shared env ──

export GMUXD_PORT="$DEV_PORT"
export GMUX_SOCKET_DIR="$DEV_SOCKET_DIR"
export XDG_CONFIG_HOME="$DEV_STATE_DIR/config"
export XDG_STATE_HOME="$DEV_STATE_DIR/state"
export GMUXD_DEV_PROXY="http://localhost:$DEV_VITE_PORT"

# ── Initial build ──

echo "→ Building Go binaries..."
(cd "$ROOT/cli/gmux" && go build -o "$ROOT/bin/gmux-dev" ./cmd/gmux)
(cd "$ROOT/services/gmuxd" && go build -o "$ROOT/bin/gmuxd-dev" ./cmd/gmuxd)

# ── Cleanup ──

PIDS=()
cleanup() {
  echo ""
  echo "Shutting down dev environment..."
  for pid in "${PIDS[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null
}
trap cleanup EXIT INT TERM

# ── Start vite dev server (internal, not exposed directly) ──

echo "→ Starting vite dev server on port $DEV_VITE_PORT..."
(cd "$ROOT/apps/gmux-web" && npx vite --port "$DEV_VITE_PORT") &
PIDS+=($!)

# Give vite a moment to start before gmuxd tries to proxy to it
sleep 1

# ── Start gmuxd (serves everything on one port) ──

echo "→ Starting gmuxd on port $DEV_PORT..."
"$ROOT/bin/gmuxd-dev" start --replace &
PIDS+=($!)

# ── Watch Go files → rebuild + restart gmuxd ──

echo "→ Watching Go files for changes..."
watchexec \
  --watch "$ROOT/services/gmuxd" \
  --watch "$ROOT/cli/gmux" \
  --watch "$ROOT/packages/adapter" \
  -e go \
  --debounce 500 \
  --restart \
  --shell bash \
  -- "
    echo '→ Go files changed, rebuilding...'
    (cd '$ROOT/cli/gmux' && go build -o '$ROOT/bin/gmux-dev' ./cmd/gmux) &&
    (cd '$ROOT/services/gmuxd' && go build -o '$ROOT/bin/gmuxd-dev' ./cmd/gmuxd) &&
    echo '→ Restarting gmuxd-dev...' &&
    GMUXD_PORT=$DEV_PORT \\
    GMUX_SOCKET_DIR=$DEV_SOCKET_DIR \\
    XDG_CONFIG_HOME='$DEV_STATE_DIR/config' \\
    XDG_STATE_HOME='$DEV_STATE_DIR/state' \\
    GMUXD_DEV_PROXY='http://localhost:$DEV_VITE_PORT' \\
    '$ROOT/bin/gmuxd-dev' start --replace
  " &
PIDS+=($!)

echo ""
echo "══════════════════════════════════════════════════════"
echo "  gmux dev environment"
echo ""
echo "  Local:     http://localhost:$DEV_PORT"
echo "  Tailscale: https://gmux-dev.<tailnet>"
echo "  (both serve the same vite HMR frontend)"
echo ""
echo "  Launch dev sessions:"
echo "    source scripts/dev-env.sh"
echo "    gmux-dev <cmd>"
echo "══════════════════════════════════════════════════════"

wait
