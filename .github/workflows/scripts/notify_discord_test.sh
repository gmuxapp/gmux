#!/usr/bin/env bash
# Tests for notify-discord.sh.
#
# notify-discord.sh's job is to build a Discord webhook payload that:
#
#   1. Pings every role at or below the bump severity.
#   2. Suppresses the link-preview embed.
#   3. Stays under Discord's 2000-char limit, preferring a paragraph
#      break to a hard byte cut.
#   4. Always carries the changelog link, even when truncated.
#
# Tests use DRY_RUN=1 to capture the payload as JSON and assert on its
# structure (roles list, flags) rather than on the rendered string
# representation, which would be brittle.
set -uo pipefail

REAL_SCRIPT="$(cd "$(dirname "$0")" && pwd)/notify-discord.sh"

if ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: jq is required" >&2
  exit 0
fi

SCOREBOARD=$(mktemp)
trap 'rm -f "$SCOREBOARD"' EXIT
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
    echo "    expected: $2"
    echo "    actual:   $3"
    bump_score fail
  fi
}

assert_le() {
  if (( $3 <= $2 )); then echo "  ✓ $1 ($3 <= $2)"; bump_score pass
  else echo "  ✗ $1 ($3 > $2)"; bump_score fail
  fi
}

assert_contains() {
  if [[ "$3" == *"$2"* ]]; then echo "  ✓ $1"; bump_score pass
  else echo "  ✗ $1"; echo "    expected to contain: $2"; bump_score fail
  fi
}

# Lay out a scratch repo with two tags so notify-discord.sh's
# `git describe --tags --abbrev=0 "${VERSION}^"` finds the predecessor.
make_repo() {
  local tmp=$1 prev=$2 version=$3
  cd "$tmp"
  git init --quiet --initial-branch=main
  git config user.email "test@example.com"
  git config user.name "Test"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "first"
  git tag "$prev"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "second"
  git tag "$version"
}

run_notify() {
  local tmp=$1 version=$2
  (
    cd "$tmp"
    DRY_RUN=1 \
      WEBHOOK_URL="http://fake" \
      VERSION="$version" \
      ROLE_BREAKING="BR" \
      ROLE_FEATURE="FE" \
      ROLE_PATCH="PA" \
      bash "$REAL_SCRIPT"
  )
}

# ── Test: each bump level pings exactly the roles at or below severity ──
#
# Patch must NOT wake up feature/breaking subscribers; major must
# reach everyone. Asserting on .allowed_mentions.roles (the structured
# field Discord uses to decide who to ping) instead of the rendered
# content string avoids breaking on cosmetic text changes.

echo "Bump-level role ping:"
for case in \
  "v1.5.3|v1.5.4|PA" \
  "v1.5.3|v1.6.0|PA,FE" \
  "v1.5.3|v2.0.0|PA,FE,BR"
do
  prev=${case%%|*}; rest=${case#*|}; version=${rest%%|*}; expected=${rest##*|}
  (
    tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
    make_repo "$tmp" "$prev" "$version"
    echo "x" > "$tmp/RELEASE_NOTES.md"

    payload=$(run_notify "$tmp" "$version")
    actual=$(echo "$payload" | jq -r '.allowed_mentions.roles | join(",")')
    assert_eq "$prev → $version" "$expected" "$actual"
  )
done

# ── Test: first-ever release (no predecessor tag) pings patch only ──
#
# `git describe ... "${VERSION}^"` fails when there's no prior tag.
# Without a guard, the empty `prev_major` makes `"X" != ""` true and
# we'd ping every role on the very first release. Patch-only is the
# right default and matches the old `grep -B1` behavior.

echo ""
echo "First-ever release:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  cd "$tmp"
  git init --quiet --initial-branch=main
  git config user.email "test@example.com"; git config user.name "Test"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "first"
  git tag v0.1.0
  echo "x" > RELEASE_NOTES.md

  roles=$(run_notify "$tmp" v0.1.0 | jq -r '.allowed_mentions.roles | join(",")')
  assert_eq "no prior tag → patch only" "PA" "$roles"
)

# ── Test: payload sets flags=4 (SUPPRESS_EMBEDS) ──
#
# Without this Discord generates a preview card for the changelog URL.
# Asserting on the structured field, not on whether the rendered post
# happened to suppress it.

echo ""
echo "Embed suppression:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp" v1.5.3 v1.5.4
  echo "x" > "$tmp/RELEASE_NOTES.md"

  flags=$(run_notify "$tmp" v1.5.4 | jq -r '.flags')
  assert_eq "flags=4" "4" "$flags"
)

# ── Test: long summaries truncate at a paragraph break, not in the middle ──
#
# Constructs a summary with a clear paragraph break under the 2000-char
# limit and a long second paragraph that pushes the total over the
# limit. The cut must land in (or after) paragraph B, leaving paragraph
# A intact, and the changelog link must survive.

echo ""
echo "Truncation:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp" v1.5.3 v1.5.4

  {
    for _ in {1..30}; do echo -n "Lorem ipsum dolor sit amet consectetur adipiscing elit. "; done
    echo; echo
    for _ in {1..30}; do echo -n "Second paragraph filler text appears here. "; done
    echo
    echo "<!-- highlights-end -->"
  } > "$tmp/RELEASE_NOTES.md"

  content=$(run_notify "$tmp" v1.5.4 | jq -r '.content')
  assert_le "under Discord's 2000-char limit" 2000 "${#content}"
  assert_contains "changelog link survives truncation" "gmux.app/changelog" "$content"
  assert_contains "first paragraph not chopped"        "Lorem ipsum dolor"  "$content"
)

# ── Summary ──

read -r pass fail < "$SCOREBOARD"
echo ""
echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]
