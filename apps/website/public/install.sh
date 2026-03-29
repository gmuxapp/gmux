#!/bin/sh
# gmux installer - https://gmux.app
# Usage: curl -sSfL https://gmux.app/install.sh | sh
#
# Environment variables:
#   GMUX_INSTALL_DIR  - where to put binaries (default: ~/.local/bin)
#   GMUX_VERSION      - specific version to install (default: latest)

set -eu

REPO="gmuxapp/gmux"
INSTALL_DIR="${GMUX_INSTALL_DIR:-$HOME/.local/bin}"

err() { echo "error: $1" >&2; exit 1; }

need() {
  command -v "$1" > /dev/null 2>&1 || err "need '$1' (command not found)"
}

detect_os() {
  case "$(uname -s)" in
    Linux*)  echo linux  ;;
    Darwin*) echo darwin ;;
    *)       err "unsupported OS: $(uname -s)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *)             err "unsupported architecture: $(uname -m)" ;;
  esac
}

sha256() {
  if command -v sha256sum > /dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum > /dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    err "need 'sha256sum' or 'shasum'"
  fi
}

resolve_version() {
  if [ -n "${GMUX_VERSION:-}" ]; then echo "$GMUX_VERSION"; return; fi
  v="$(curl -sSfL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"//;s/".*//')"
  [ -n "$v" ] || err "could not determine latest version"
  echo "$v"
}

install_desktop_entry() {
  data_home="${XDG_DATA_HOME:-$HOME/.local/share}"
  apps_dir="${data_home}/applications"
  icon_dir="${data_home}/icons/hicolor/scalable/apps"

  mkdir -p "$apps_dir" "$icon_dir"

  # Icon (terminal prompt SVG)
  cat > "${icon_dir}/gmux.svg" << 'ICON'
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">
  <rect width="32" height="32" rx="6" fill="#0f141a"/>
  <polyline points="8,10 16,16 8,22" fill="none" stroke="#49b8b8" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/>
  <line x1="18" y1="22" x2="25" y2="22" stroke="#49b8b8" stroke-width="2.4" stroke-linecap="round"/>
</svg>
ICON

  # Desktop entry
  cat > "${apps_dir}/gmux.desktop" << DESKTOP
[Desktop Entry]
Name=gmux
Comment=Terminal session manager
GenericName=Terminal Manager
Exec=${INSTALL_DIR}/gmux
Icon=gmux
Terminal=false
Type=Application
Categories=Development;TerminalEmulator;
Keywords=terminal;tmux;session;
StartupNotify=false
DESKTOP

  # Update desktop database if available (not critical)
  if command -v update-desktop-database > /dev/null 2>&1; then
    update-desktop-database "$apps_dir" 2>/dev/null || true
  fi

  echo "Installed desktop entry and icon"
}

main() {
  need curl; need uname

  os="$(detect_os)"
  arch="$(detect_arch)"
  version="$(resolve_version)"

  # Darwin archives are .zip, Linux are .tar.gz
  case "$os" in
    darwin) ext=zip    ; need unzip ;;
    *)      ext=tar.gz ; need tar   ;;
  esac

  archive="gmux_${version#v}_${os}_${arch}.${ext}"
  base="https://github.com/${REPO}/releases/download/${version}"

  tmpdir="$(mktemp -d)"
  trap 'rm -rf "$tmpdir"' EXIT

  echo "Installing gmux ${version} (${os}/${arch})..."
  curl -sSfL -o "${tmpdir}/${archive}" "${base}/${archive}"

  # Verify checksum
  curl -sSfL -o "${tmpdir}/checksums.txt" "${base}/checksums.txt"
  expected="$(grep -F "${archive}" "${tmpdir}/checksums.txt" | head -1 | awk '{print $1}')"
  [ -n "$expected" ] || err "archive not found in checksums.txt"
  actual="$(sha256 "${tmpdir}/${archive}")"
  [ "$expected" = "$actual" ] || err "checksum mismatch: expected ${expected}, got ${actual}"

  # Extract
  case "$ext" in
    zip)    unzip -qo "${tmpdir}/${archive}" -d "${tmpdir}" ;;
    tar.gz) tar -xzf "${tmpdir}/${archive}" -C "${tmpdir}"  ;;
  esac

  # Install
  mkdir -p "$INSTALL_DIR"
  install -m 755 "${tmpdir}/gmux"  "${INSTALL_DIR}/gmux"
  install -m 755 "${tmpdir}/gmuxd" "${INSTALL_DIR}/gmuxd"

  echo "Installed gmux and gmuxd to ${INSTALL_DIR}"

  # Install .desktop entry and icon on Linux
  if [ "$os" = "linux" ]; then
    install_desktop_entry
  fi

  # If gmuxd was already running, restart it
  # Running sessions are not affected, they reconnect automatically.
  if curl -sSf http://localhost:8790/v1/health > /dev/null 2>&1; then
    "${INSTALL_DIR}/gmuxd" start 2>/dev/null || true
    echo "Restarted gmuxd (running sessions are safe)"
  fi

  case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *) echo "Note: add ${INSTALL_DIR} to your PATH" ;;
  esac
}

main "$@"
