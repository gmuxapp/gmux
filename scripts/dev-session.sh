#!/usr/bin/env bash
# Source this to get a gmux-dev command that launches sessions
# against this worktree's dev stack (started by dev-server.sh).
#
# Usage:
#   source scripts/dev-session.sh
#   gmux-dev bash

_GMUX_DEV_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ "$(basename "$(dirname "$_GMUX_DEV_ROOT")")" == ".grove" ]]; then
  _GMUX_DEV_INSTANCE="$(basename "$_GMUX_DEV_ROOT")"
  _GMUX_DEV_HASH=$(printf '%s' "$_GMUX_DEV_ROOT" | cksum | awk '{print $1}')
  _GMUX_DEV_PORT=$((_GMUX_DEV_HASH % 100 + 8800))
  _GMUX_DEV_SOCKET_DIR="/tmp/gmux-dev-$_GMUX_DEV_INSTANCE"
  _GMUX_DEV_STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/gmux-dev-$_GMUX_DEV_INSTANCE"
else
  _GMUX_DEV_INSTANCE="gmux-dev"
  _GMUX_DEV_PORT=8791
  _GMUX_DEV_SOCKET_DIR="/tmp/gmux-dev-sessions"
  _GMUX_DEV_STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/gmux-dev"
fi

gmux-dev() {
  GMUX_SOCKET_DIR="$_GMUX_DEV_SOCKET_DIR" \
  XDG_STATE_HOME="$_GMUX_DEV_STATE_DIR/state" \
  PI_CODING_AGENT_DIR="$_GMUX_DEV_STATE_DIR/pi-agent" \
  "$_GMUX_DEV_ROOT/bin/gmux-dev" "$@"
}

echo "gmux-dev ($_GMUX_DEV_INSTANCE :$_GMUX_DEV_PORT) → gmux-dev <cmd>"
