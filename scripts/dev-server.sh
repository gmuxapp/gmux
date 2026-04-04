#!/usr/bin/env bash
# Start the dev stack: vite + gmuxd + watchexec.
#
# The main checkout uses fixed, well-known values (port 8791, hostname
# gmux-dev) so tailscale auth state is preserved across restarts.
#
# Grove worktrees (.grove/<name>) get isolated ports, socket dirs,
# state dirs, and tailscale hostnames derived from the worktree name,
# so multiple instances run simultaneously without collisions.
#
# Usage: ./scripts/dev-server.sh
#
# Then from another terminal:
#   source scripts/dev-session.sh && gmux-dev <cmd>

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEV_BIN_DIR="$ROOT/bin"

# ── Instance identity ──
#
# Grove worktrees live at .grove/<name> under the main repo. Detect
# this by checking if the parent directory is named ".grove".

if [[ "$(basename "$(dirname "$ROOT")")" == ".grove" ]]; then
  # Grove worktree: derive isolated values from the worktree name.
  INSTANCE_NAME="$(basename "$ROOT")"
  INSTANCE_HASH=$(printf '%s' "$ROOT" | cksum | awk '{print $1}')
  DEV_PORT=$((8800 + INSTANCE_HASH % 100))
  DEV_VITE_PORT=$((DEV_PORT + 100))
  DEV_SOCKET_DIR="/tmp/gmux-dev-$INSTANCE_NAME"
  DEV_STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/gmux-dev-$INSTANCE_NAME"
  DEV_TS_HOSTNAME="gmux-dev-$INSTANCE_NAME"
else
  # Main checkout: fixed values, preserves existing tailscale auth.
  INSTANCE_NAME="gmux-dev"
  DEV_PORT=8791
  DEV_VITE_PORT=5173
  DEV_SOCKET_DIR="/tmp/gmux-dev-sessions"
  DEV_STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/gmux-dev"
  DEV_TS_HOSTNAME="gmux-dev"
fi

# ── Prepare directories and config ──

mkdir -p "$DEV_SOCKET_DIR" "$DEV_STATE_DIR/config/gmux" "$DEV_STATE_DIR/state" "$DEV_STATE_DIR/pi-agent"

cat > "$DEV_STATE_DIR/config/gmux/host.toml" << EOF
port = $DEV_PORT

[tailscale]
enabled = true
hostname = "$DEV_TS_HOSTNAME"
EOF

# ── Shared env ──

export GMUX_SOCKET_DIR="$DEV_SOCKET_DIR"
export XDG_CONFIG_HOME="$DEV_STATE_DIR/config"
export XDG_STATE_HOME="$DEV_STATE_DIR/state"
export GMUXD_DEV_PROXY="http://localhost:$DEV_VITE_PORT"
export PI_CODING_AGENT_DIR="$DEV_STATE_DIR/pi-agent"

# ── Install deps + build ──

echo "→ Installing node dependencies..."
(cd "$ROOT" && pnpm install --frozen-lockfile)

echo "→ Building Go binaries..."
(cd "$ROOT/cli/gmux" && go build -o "$DEV_BIN_DIR/gmux-dev" ./cmd/gmux)
(cd "$ROOT/services/gmuxd" && go build -o "$DEV_BIN_DIR/gmuxd-dev" ./cmd/gmuxd)

# ── Cleanup ──

PIDS=()
cleanup() {
  echo ""
  echo "Shutting down dev environment ($INSTANCE_NAME)..."
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
"$DEV_BIN_DIR/gmuxd-dev" start &
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
    (cd '$ROOT/cli/gmux' && go build -o '$DEV_BIN_DIR/gmux-dev' ./cmd/gmux) &&
    (cd '$ROOT/services/gmuxd' && go build -o '$DEV_BIN_DIR/gmuxd-dev' ./cmd/gmuxd) &&
    echo '→ Restarting gmuxd-dev ($INSTANCE_NAME)...' &&
    GMUX_SOCKET_DIR=$DEV_SOCKET_DIR \\
    XDG_CONFIG_HOME='$DEV_STATE_DIR/config' \\
    XDG_STATE_HOME='$DEV_STATE_DIR/state' \\
    GMUXD_DEV_PROXY='http://localhost:$DEV_VITE_PORT' \\
    PI_CODING_AGENT_DIR='$DEV_STATE_DIR/pi-agent' \\
    '$DEV_BIN_DIR/gmuxd-dev' start
  " &
PIDS+=($!)

echo ""
echo "══════════════════════════════════════════════════════"
echo "  gmux dev: $INSTANCE_NAME"
echo ""
echo "  Local:     http://localhost:$DEV_PORT"
echo "  Tailscale: https://$DEV_TS_HOSTNAME.<tailnet>"
echo "  Sockets:   $DEV_SOCKET_DIR"
echo ""
echo "  Launch dev sessions:"
echo "    source scripts/dev-session.sh && gmux-dev <cmd>"
echo "══════════════════════════════════════════════════════"

wait
