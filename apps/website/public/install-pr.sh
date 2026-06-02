#!/bin/sh
# gmux PR build installer - https://gmux.app
# Installs pre-release binaries from a PR's CI build artifacts.
#
# Usage: curl -sSfL https://gmux.app/install-pr.sh | sh -s -- 42
#
# Requires: gh (GitHub CLI, authenticated)
#
# Environment variables:
#   GMUX_INSTALL_DIR  - where to put binaries (default: ~/.local/bin)

set -eu

REPO="gmuxapp/gmux"
INSTALL_DIR="${GMUX_INSTALL_DIR:-$HOME/.local/bin}"

err() { echo "error: $1" >&2; exit 1; }
need() { command -v "$1" > /dev/null 2>&1 || err "need '$1' (command not found)"; }

detect_platform() {
  case "$(uname -s)" in
    Linux*)  OS=linux  ;;
    Darwin*) OS=darwin ;;
    *)       err "unsupported OS: $(uname -s)" ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64)  ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *)             err "unsupported architecture: $(uname -m)" ;;
  esac
  case "$OS" in
    darwin) EXT=zip    ;;
    *)      EXT=tar.gz ;;
  esac
}

main() {
  PR="${1:-}"
  [ -n "$PR" ] || err "usage: install-pr.sh PR_NUMBER"

  need gh
  detect_platform

  case "$EXT" in
    zip) need unzip ;;
    *)   need tar   ;;
  esac

  tmpdir="$(mktemp -d)"
  trap 'rm -rf "$tmpdir"' EXIT

  echo "Downloading gmux PR #${PR} build (${OS}/${ARCH})..."
  gh run download --repo "$REPO" --name "gmux-pr${PR}" --dir "$tmpdir"

  archive="$(find "$tmpdir" -name "gmux_*_${OS}_${ARCH}.${EXT}" | head -1)"
  [ -n "$archive" ] || err "no ${OS}/${ARCH} build found in PR #${PR} artifacts"

  case "$EXT" in
    zip)    unzip -qo "$archive" -d "$tmpdir/out" ;;
    tar.gz) mkdir -p "$tmpdir/out"; tar -xzf "$archive" -C "$tmpdir/out" ;;
  esac

  mkdir -p "$INSTALL_DIR"
  install -m 755 "$tmpdir/out/gmux"  "${INSTALL_DIR}/gmux"
  install -m 755 "$tmpdir/out/gmuxd" "${INSTALL_DIR}/gmuxd"
  echo "Installed gmux and gmuxd from PR #${PR} to ${INSTALL_DIR}"

  # If gmuxd was already running, restart it so the new version takes effect.
  # Active sessions survive (the new daemon rediscovers them on startup).
  if curl -sSf http://localhost:8790/v1/health > /dev/null 2>&1; then
    if "${INSTALL_DIR}/gmuxd" start; then
      echo "gmuxd restarted to apply the update."
    else
      echo "Warning: gmuxd restart did not complete successfully; check logs."
    fi
  else
    echo "To start gmux, run: gmux"
  fi
}

main "$@"
