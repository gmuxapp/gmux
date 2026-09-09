#!/usr/bin/env bash
# Build gmuxd and gmux release binaries.
# Usage: ./scripts/build.sh [--skip-frontend] [--print-plan]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
WEB_EMBED="$ROOT/services/gmuxd/cmd/gmuxd/web"

# ── Version stamp ──
#
# Real releases come from GoReleaser, which never runs this script and passes
# the tag through -ldflags itself. Everything this script builds is a source
# build, and stamping every one of them "dev" makes `gmux --version` and
# /v1/health useless for the one question they get asked on a local install:
# *which tree is this binary from?* So derive a cheap, diagnostic stamp from
# the VCS: dev+<short hash>[-dirty], falling back to plain "dev" outside a
# checkout. Everything that treats "dev" as "not a release" (the daemon
# replacement policy, the update checker) recognises the dev+ prefix too.
#
# Exported, not just passed to `go build`: vite bakes VERSION into the bundle
# as __GMUX_VERSION__, and a bundle whose version disagrees with the daemon's
# makes the tab think it is permanently stale (see version-watch.ts).
dev_version() {
  local hash
  # git first, and not by accident: it is the only read-only source that can
  # also report *dirtiness*. This repo is colocated (.git and .jj), and jj keeps
  # git HEAD current, so the git answer is the same commit with a usable marker.
  # The `-e $ROOT/.git` test matters: inside a secondary jj workspace (a grove)
  # there is no .git of its own, and git would happily answer with the *parent*
  # repository's HEAD, which is a different tree.
  #
  # --no-optional-locks is what makes "read-only" true rather than nearly true:
  # `git status` otherwise refreshes the stat cache, which *writes* .git/index
  # and takes index.lock while doing it. A build must not touch the repository
  # it is reading, and must not contend with an editor or a jj snapshot for the
  # index lock.
  if [ -e "$ROOT/.git" ] && command -v git >/dev/null 2>&1 \
    && hash="$(git --no-optional-locks -C "$ROOT" rev-parse --short=12 HEAD 2>/dev/null)" \
    && [ -n "$hash" ]; then
    if [ -n "$(git --no-optional-locks -C "$ROOT" status --porcelain 2>/dev/null)" ]; then
      echo "dev+$hash-dirty"
    else
      echo "dev+$hash"
    fi
    return
  fi
  # jj-only checkout (e.g. a `jj workspace`/grove with no .git of its own).
  # --ignore-working-copy keeps the build read-only: it must not take the repo
  # lock or write a snapshot. The cost is that the stamp names the tree as of
  # the last jj command and can never say -dirty, so it is marked approximate
  # with a trailing '~' rather than claiming precision it does not have.
  if command -v jj >/dev/null 2>&1 \
    && hash="$(jj --repository "$ROOT" --ignore-working-copy log --no-graph --color never \
         -r @ -T 'commit_id.short(12)' 2>/dev/null)" \
    && [ -n "$hash" ]; then
    echo "dev+$hash~"
    return
  fi
  echo "dev"
}

# Mirror of buildversion.IsDev (packages/buildversion): a build "came from a
# working tree" iff it is "dev" or "dev+<hash>[-dirty]". The Go side is the
# source of truth — anything that must not be mistaken for a release (the
# update checker, the daemon replacement policy) asks it — and this shell copy
# exists only so the build never hands a source binary a release identity.
is_dev_version() {
  case "$1" in
    dev | dev+*) return 0 ;;
    *) return 1 ;;
  esac
}

skip_frontend=false
print_plan=false
for arg in "$@"; do
  case "$arg" in
    --skip-frontend) skip_frontend=true ;;
    # Print the computed stamp and whether the frontend would be rebuilt, then
    # exit without building anything. The seam scripts/build_test.sh drives.
    --print-plan) print_plan=true ;;
  esac
done

# The stamp the currently embedded bundle was built with. A --skip-frontend
# build reuses it: the daemon serves that bundle, and version-watch turns any
# bundle/daemon disagreement into a forced full page load on the next in-app
# navigation (one lost SPA state per tab, per daemon-only rebuild).
#
# The reuse is *guarded*, because the cache is not always adoptable. A single
# `VERSION=v9.9.9 ./scripts/build.sh` leaves a release string behind, and
# reusing it would stamp every later daemon-only build v9.9.9: no longer
# buildversion.IsDev, so the update checker goes online and the daemon/CLI
# replacement policies treat a source binary as a release. So the stamp is
# adopted only when it is a dev-class build; and when it disagrees with the
# version this build stamps anyway, the frontend is rebuilt instead of leaving
# bundle and daemon at different versions.
EMBED_STAMP="$BIN/.web-version"
if [ "$skip_frontend" = true ] && [ -s "$EMBED_STAMP" ]; then
  embedded_stamp="$(cat "$EMBED_STAMP")"
  want_version="${VERSION:-$(dev_version)}"
  if [ -z "${VERSION:-}" ] && is_dev_version "$embedded_stamp"; then
    VERSION="$embedded_stamp"
  elif [ "$embedded_stamp" != "$want_version" ]; then
    echo "→ Embedded bundle is stamped $embedded_stamp, this build stamps $want_version: rebuilding the frontend"
    skip_frontend=false
  fi
fi

VERSION="${VERSION:-$(dev_version)}"
export VERSION

if [ "$print_plan" = true ]; then
  echo "version=$VERSION"
  echo "frontend=$([ "$skip_frontend" = true ] && echo skip || echo build)"
  exit 0
fi

# A toolchain directive is a preferred minimum under GOTOOLCHAIN=auto; a newer
# ambient Go can still win. Builds must use the repository's exact toolchain.
GO_TOOLCHAIN="$("$ROOT/scripts/go-toolchain.sh")"
export GOTOOLCHAIN="$GO_TOOLCHAIN"
if ! GO_VERSION="$(go version 2>&1)"; then
  echo "error: required Go toolchain $GO_TOOLCHAIN is unavailable." >&2
  echo "The go command could not find or download it:" >&2
  echo "$GO_VERSION" >&2
  exit 1
fi
echo "→ Using $GO_VERSION"
echo "→ Stamping version $VERSION"

mkdir -p "$BIN"

# ── Frontend ──

if [ "$skip_frontend" = false ]; then
  echo "→ Building frontend…"
  pnpm -C "$ROOT/apps/gmux-web" exec vite build

  # Copy dist into the go:embed directory
  rm -rf "$WEB_EMBED/assets" "$WEB_EMBED/favicon.svg" "$WEB_EMBED/manifest.json"
  cp -r "$ROOT/apps/gmux-web/dist/"* "$WEB_EMBED/"
  # Record what the embedded bundle reports as its version, so a later
  # --skip-frontend build can stamp the daemon to match it. Kept next to the
  # binaries, not inside the embed dir (which is embedded with `all:`).
  printf '%s' "$VERSION" > "$EMBED_STAMP"
  echo "  Embedded $(du -sh "$WEB_EMBED" | cut -f1) of frontend assets"
fi

# ── Go binaries ──

LDFLAGS_COMMON="-s -w -X main.version=$VERSION"
export CGO_ENABLED=0

echo "→ Building gmuxd…"
(cd "$ROOT/services/gmuxd" && go build -ldflags "$LDFLAGS_COMMON" -o "$BIN/gmuxd" ./cmd/gmuxd)

echo "→ Building gmux…"
(cd "$ROOT/cli/gmux" && go build -ldflags "$LDFLAGS_COMMON" -o "$BIN/gmux" ./cmd/gmux)

echo ""
ls -lh "$BIN/gmuxd" "$BIN/gmux"
echo "✓ Build complete"
