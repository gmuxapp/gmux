#!/usr/bin/env bash
# Build and install gmux + gmuxd to ~/.local/bin.
#
# Stops the running gmuxd, replaces the binaries, and restarts it.
# Existing sessions keep running (they use the old binary via open
# file descriptors); new sessions will use the new binary.
#
# Usage: ./scripts/install.sh [--skip-frontend]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
INSTALL_DIR="${GMUX_INSTALL_DIR:-$HOME/.local/bin}"

# ── Build ──

"$ROOT/scripts/build.sh" "$@"

# ── Stop gmuxd ──

echo "→ Stopping gmuxd..."
# Kill any running gmuxd (installed or dev). Multiple instances would
# fight over the socket, so be thorough.
for pid in $(pgrep -x gmuxd 2>/dev/null); do
  kill "$pid" 2>/dev/null || true
done
sleep 1
# Verify none survived.
if pgrep -x gmuxd >/dev/null 2>&1; then
  kill -9 $(pgrep -x gmuxd) 2>/dev/null || true
  sleep 1
fi

# ── Install ──
#
# Remove before copy: the old binary may be held open by running gmux
# processes. Removing the inode lets us write a new file at the same path
# while the old processes continue using the deleted inode.

echo "→ Installing to $INSTALL_DIR..."
mkdir -p "$INSTALL_DIR"

for bin in gmux gmuxd; do
  rm -f "$INSTALL_DIR/$bin"
  cp "$ROOT/bin/$bin" "$INSTALL_DIR/$bin"
done

echo ""
ls -lh "$INSTALL_DIR/gmux" "$INSTALL_DIR/gmuxd"

# ── Restart gmuxd ──

echo "→ Restarting gmuxd..."
if "$INSTALL_DIR/gmuxd" start; then
  echo "✓ gmuxd is running"
else
  echo "⚠ gmuxd failed to start."
  echo "  Check logs: ~/.local/state/gmux/gmuxd.log"
  exit 1
fi
