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

main() {
  need curl; need uname

  os="$(detect_os)"
  arch="$(detect_arch)"
  version="$(resolve_version)"

  # Darwin archives are .zip, Linux are .tar.gz (goreleaser config)
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

  case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *) echo "Note: add ${INSTALL_DIR} to your PATH" ;;
  esac
}

main "$@"
