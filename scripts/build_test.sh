#!/usr/bin/env bash
# Tests for build.sh's version-stamp plan: which version a build stamps, and
# whether it rebuilds the frontend.
#
# Only that decision is tested, via `--print-plan`, which computes the plan and
# exits before any toolchain or frontend work. The interesting property is not
# the string but its *class*: a source build must stay buildversion.IsDev, or
# the update checker goes online and the daemon/CLI replacement policies treat
# it as a release. The embedded-bundle stamp cache (bin/.web-version) is the
# one path that can hand a source build a foreign identity, so every case here
# is about what the cache is allowed to do.
#
# Each case runs build.sh from a throwaway ROOT with no VCS of its own, so the
# computed stamp is the deterministic fallback "dev" and the test never depends
# on the checkout it runs in.

set -uo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/build.sh"

SCOREBOARD=$(mktemp)
WORK=$(mktemp -d)
trap 'rm -f "$SCOREBOARD"; rm -rf "$WORK"' EXIT
echo "0 0" > "$SCOREBOARD"

bump_score() {
  read -r pass fail < "$SCOREBOARD"
  case "$1" in pass) pass=$((pass + 1)) ;; fail) fail=$((fail + 1)) ;; esac
  echo "$pass $fail" > "$SCOREBOARD"
}

assert_eq() {
  if [[ "$2" == "$3" ]]; then echo "  ✓ $1"; bump_score pass
  else
    echo "  ✗ $1"
    echo "    expected: $(printf %q "$2")"
    echo "    actual:   $(printf %q "$3")"
    bump_score fail
  fi
}

# plan <embedded stamp or empty> [env assignments...] -- prints "<version> <frontend>"
plan() {
  local stamp="$1"; shift
  local root="$WORK/root"
  rm -rf "$root"
  mkdir -p "$root/scripts" "$root/bin"
  cp "$SCRIPT" "$root/scripts/build.sh"
  [ -n "$stamp" ] && printf '%s' "$stamp" > "$root/bin/.web-version"
  local out
  out="$(env "$@" "$root/scripts/build.sh" --print-plan "${PLAN_ARGS[@]}" 2>&1)" || {
    echo "build.sh --print-plan failed: $out" >&2
    return 1
  }
  local version frontend
  version="$(sed -n 's/^version=//p' <<< "$out")"
  frontend="$(sed -n 's/^frontend=//p' <<< "$out")"
  echo "$version $frontend"
}

echo "── full build (no --skip-frontend)"
PLAN_ARGS=()
assert_eq "stamps the computed dev version and builds the frontend" \
  "dev build" "$(plan "" -u VERSION)"
assert_eq "explicit VERSION wins" \
  "v1.2.3 build" "$(plan "" VERSION=v1.2.3)"
assert_eq "a stale release stamp cannot influence a full build" \
  "dev build" "$(plan "v9.9.9" -u VERSION)"

echo "── --skip-frontend reuses the embedded bundle's stamp"
PLAN_ARGS=(--skip-frontend)
assert_eq "adopts a dev-class stamp so bundle and daemon agree" \
  "dev+1a2b3c4d5e6f~ skip" "$(plan "dev+1a2b3c4d5e6f~" -u VERSION)"
assert_eq "adopts a dirty dev stamp too" \
  "dev+1a2b3c4d5e6f-dirty skip" "$(plan "dev+1a2b3c4d5e6f-dirty" -u VERSION)"
assert_eq "adopts a plain dev stamp" \
  "dev skip" "$(plan "dev" -u VERSION)"
assert_eq "no cache: stamps the computed version, nothing to disagree with" \
  "dev skip" "$(plan "" -u VERSION)"

echo "── --skip-frontend never adopts a non-dev stamp (the footgun)"
# What `VERSION=v9.9.9 ./scripts/build.sh` leaves behind. Reusing it would make
# every later daemon-only build claim to be the v9.9.9 release.
assert_eq "release stamp is refused and the frontend is rebuilt instead" \
  "dev build" "$(plan "v9.9.9" -u VERSION)"
assert_eq "a version-shaped-but-not-dev stamp is refused" \
  "dev build" "$(plan "2.1.2-snapshot-abcdef" -u VERSION)"
assert_eq "'development' is not dev-class (prefix match must be dev or dev+)" \
  "dev build" "$(plan "development" -u VERSION)"

echo "── --skip-frontend with an explicit VERSION"
assert_eq "explicit VERSION matching the bundle keeps the skip" \
  "v9.9.9 skip" "$(plan "v9.9.9" VERSION=v9.9.9)"
assert_eq "explicit VERSION disagreeing with the bundle rebuilds the frontend" \
  "v1.0.0 build" "$(plan "v9.9.9" VERSION=v1.0.0)"
assert_eq "explicit VERSION is never overridden by a dev-class stamp" \
  "v1.0.0 build" "$(plan "dev+1a2b3c4d5e6f" VERSION=v1.0.0)"

read -r pass fail < "$SCOREBOARD"
echo ""
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
