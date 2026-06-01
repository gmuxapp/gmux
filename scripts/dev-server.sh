#!/usr/bin/env bash
# Start the dev stack: vite + gmuxd + watchexec.
#
# The main checkout uses fixed, well-known values (port 22226, hostname
# gmux-dev) so tailscale auth state is preserved across restarts.
#
# Grove worktrees (.grove/<name>) get isolated ports, socket dirs,
# state dirs, and tailscale hostnames derived from the worktree name,
# so multiple instances run simultaneously without collisions.
#
# Usage: ./scripts/dev-server.sh
#
# Usage: ./scripts/dev-server.sh

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
  DEV_PORT=22226
  DEV_VITE_PORT=5173
  DEV_SOCKET_DIR="/tmp/gmux-dev-sessions"
  DEV_STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/gmux-dev"
  DEV_TS_HOSTNAME="gmux-dev"
fi

# ── Tear down any previous instance on these ports ──
#
# Kill stale Vite and gmuxd processes that would block port reuse.
# `gmuxd-dev start` handles stopping a running daemon via its socket, but
# Vite has no such mechanism — if a previous dev-server.sh left a Vite
# process behind (e.g. after Ctrl+C didn't propagate), it holds DEV_VITE_PORT
# and the new Vite falls back to a different port, breaking the daemon proxy.

echo "→ Tearing down any previous $INSTANCE_NAME instance..."

# Stop gmuxd via its socket (graceful; no-op if not running).
if [[ -S "${XDG_STATE_HOME:-$HOME/.local/state}/$INSTANCE_NAME/state/gmux/gmuxd.sock" ]]; then
  "$DEV_BIN_DIR/gmuxd-dev" stop 2>/dev/null || true
fi

# Kill any process listening on our ports.
_kill_port() {
  local port="$1"
  local pid
  pid=$(lsof -ti :"$port" 2>/dev/null) || return 0
  echo "  killing pid $pid (port $port)"
  kill "$pid" 2>/dev/null || true
  # Wait up to 3 s for the port to free.
  local i
  for i in 1 2 3; do
    sleep 1
    lsof -ti :"$port" >/dev/null 2>&1 || return 0
  done
  kill -9 "$pid" 2>/dev/null || true
}
_kill_port "$DEV_VITE_PORT"
_kill_port "$DEV_PORT"

# Clear the Vite dep-optimisation cache so a config change always takes effect.
rm -rf "$ROOT/apps/gmux-web/node_modules/.vite"

# ── Prepare directories and config ──

mkdir -p "$DEV_SOCKET_DIR" "$DEV_STATE_DIR/config/gmux" "$DEV_STATE_DIR/state" "$DEV_STATE_DIR/pi-agent"

cat > "$DEV_STATE_DIR/config/gmux/host.toml" << EOF
port = $DEV_PORT

[tailscale]
enabled = true
hostname = "$DEV_TS_HOSTNAME"
EOF

# Seed projects.json so dev sessions launched in the workspace are reachable
# in the web UI. Without a project whose match rules cover the session's cwd,
# navigateToSession() can't compute a URL and the session never appears as
# openable from the home page or sidebar — it just lands in an unreachable
# "discovered" bucket. (e2e/global-setup.ts seeds an equivalent file for the
# same reason.)
#
# WORKSPACE_DIR is the agent workspace that contains this repo; the path rule
# is non-exact, so every session under it (the gmux repo, other projects,
# scratch dirs) is matched into one navigable "workspace" project.
#
# Seed projects.json when absent; if the file already exists but is missing
# the workspace entry (e.g. seeded by e2e setup or an earlier run), add it.
# Never remove or rewrite entries gmuxd has added (e.g. per-project sessions lists).
if [[ ! -f "$GMUX_STATE_DIR/projects.json" ]]; then
  cat > "$GMUX_STATE_DIR/projects.json" << EOF
{
  "version": 2,
  "items": [
    { "slug": "home", "match": [{ "path": "~", "exact": true }] },
    { "slug": "workspace", "match": [{ "path": "$WORKSPACE_DIR" }] }
  ]
}
EOF
else
  # Upsert: add workspace entry if it is missing.
  python3 - "$GMUX_STATE_DIR/projects.json" "$WORKSPACE_DIR" << 'PYEOF'
import json, sys
path, workspace_dir = sys.argv[1], sys.argv[2]
with open(path) as f:
    data = json.load(f)
slugs = {item['slug'] for item in data.get('items', [])}
if 'workspace' not in slugs:
    data['items'].append({'slug': 'workspace', 'match': [{'path': workspace_dir}]})
    with open(path, 'w') as f:
        json.dump(data, f, indent=2)
    print(f'→ Added workspace project pointing at {workspace_dir}')
PYEOF
fi

# ── Shared env ──

export GMUX_SOCKET_DIR="$DEV_SOCKET_DIR"
export GMUX_CONFIG_DIR="$DEV_STATE_DIR/config/gmux"
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
    GMUX_SOCKET_DIR=$DEV_SOCKET_DIR \
    GMUX_CONFIG_DIR='$DEV_STATE_DIR/config/gmux' \
    XDG_CONFIG_HOME='$DEV_STATE_DIR/config' \
    XDG_STATE_HOME='$DEV_STATE_DIR/state' \
    GMUXD_DEV_PROXY='http://localhost:$DEV_VITE_PORT' \
    PI_CODING_AGENT_DIR='$DEV_STATE_DIR/pi-agent' \
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
  echo "  Launch a session: bin/gmux-dev <cmd>"
echo "══════════════════════════════════════════════════════"

wait
