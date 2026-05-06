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
#   5. Always includes the latest changelog entry (curated prose plus
#      the auto-generated bullet groups) in the message body, so
#      subscribers don't need to click through.
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

assert_not_contains() {
  if [[ "$3" != *"$2"* ]]; then echo "  ✓ $1"; bump_score pass
  else echo "  ✗ $1"; echo "    expected NOT to contain: $2"; bump_score fail
  fi
}

# Lay out a scratch repo with two tags so notify-discord.sh's
# `git describe --tags --abbrev=0 "${VERSION}^"` finds the predecessor.
# Also writes a stub changelog.mdx so the script has somewhere to read
# prose/bullets from.
make_repo() {
  local tmp=$1 prev=$2 version=$3
  cd "$tmp"
  mkdir -p apps/website/src/content/docs
  cat > apps/website/src/content/docs/changelog.mdx <<EOF
## ${version} - 2026-04-30

### Fixes
- placeholder ([#1](https://example/1))

---
EOF
  git init --quiet --initial-branch=main
  git config user.email "test@example.com"
  git config user.name "Test"
  git add -A
  git -c commit.gpgsign=false commit --quiet -m "first"
  git tag "$prev"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "second"
  git tag "$version"
}

# Replace the changelog.mdx in a scratch repo with custom content so
# we can pin the prose/bullet split logic. Keeps the predecessor tags
# intact.
write_changelog() {
  local tmp=$1
  cat > "$tmp/apps/website/src/content/docs/changelog.mdx"
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

    payload=$(run_notify "$tmp" "$version")
    actual=$(echo "$payload" | jq -r '.allowed_mentions.roles | join(",")')
    assert_eq "$prev → $version" "$expected" "$actual"
  )
done

# ── Test: first-ever release (no predecessor tag) pings patch only ──

echo ""
echo "First-ever release:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  cd "$tmp"
  mkdir -p apps/website/src/content/docs
  cat > apps/website/src/content/docs/changelog.mdx <<'EOF'
## v0.1.0 - 2026-04-30

### Features
- initial ([#1](https://example/1))

---
EOF
  git init --quiet --initial-branch=main
  git config user.email "test@example.com"; git config user.name "Test"
  git add -A
  git -c commit.gpgsign=false commit --quiet -m "first"
  git tag v0.1.0

  roles=$(run_notify "$tmp" v0.1.0 | jq -r '.allowed_mentions.roles | join(",")')
  assert_eq "no prior tag → patch only" "PA" "$roles"
)

# ── Test: payload sets flags=4 (SUPPRESS_EMBEDS) ──

echo ""
echo "Embed suppression:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp" v1.5.3 v1.5.4

  flags=$(run_notify "$tmp" v1.5.4 | jq -r '.flags')
  assert_eq "flags=4" "4" "$flags"
)

# ── Test: bullets always reach Discord, with or without curated prose ──
#
# Subscribers should never have to click through just to find out if a
# release is relevant; echoing the auto-generated bullets answers that
# inline. With curated prose, the prose sits above the bullets so
# readers get framing plus detail.

echo ""
echo "Bullets always included (no prose):"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp" v1.5.3 v1.5.4

  # Latest entry has no prose paragraph; only bullets.
  write_changelog "$tmp" <<'EOF'
## v1.5.4 - 2026-04-30

### Fixes
- **(daemon)** stop leaking goroutines on shutdown ([#42](https://example/42))

---
## v1.5.3 - 2026-04-29

---
EOF

  content=$(run_notify "$tmp" v1.5.4 | jq -r '.content')
  assert_contains "bullet text reaches Discord"  "stop leaking goroutines" "$content"
  assert_contains "changelog link still present" "gmux.app/changelog"      "$content"
  assert_not_contains "no triple-newline gap"    $'\n\n\n'                 "$content"
)

# ── Test: prose and bullets both reach Discord when prose is present ──
#
# Curated prose sets the framing; bullets fill in the detail. We send
# both so the message stands on its own.

echo ""
echo "Bullets always included (with prose):"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp" v1.5.3 v1.5.4

  write_changelog "$tmp" <<'EOF'
## v1.5.4 - 2026-04-30

Hand-written highlight paragraph.

### Fixes
- **(daemon)** stop leaking goroutines on shutdown ([#42](https://example/42))

---
## v1.5.3 - 2026-04-29

---
EOF

  content=$(run_notify "$tmp" v1.5.4 | jq -r '.content')
  assert_contains "prose reaches Discord" "Hand-written highlight"  "$content"
  assert_contains "bullets reach Discord" "stop leaking goroutines" "$content"
)

# ── Test: a literal `---` inside prose survives the prose/bullet split ──
#
# The split keys off `### ` (the first group heading), not on `---`.
# So horizontal rules in user prose don't truncate the Discord summary.

echo ""
echo "Horizontal rule in prose survives:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp" v1.5.3 v1.5.4

  write_changelog "$tmp" <<'EOF'
## v1.5.4 - 2026-04-30

Theme A summary.

---

Theme B summary.

### Fixes
- something ([#1](https://example/1))

---
## v1.5.3 - 2026-04-29

---
EOF

  content=$(run_notify "$tmp" v1.5.4 | jq -r '.content')
  assert_contains "first half of prose survives"  "Theme A summary" "$content"
  assert_contains "second half of prose survives" "Theme B summary" "$content"
  assert_contains "bullets reach Discord"         "something"       "$content"
)

# ── Test: trailing `---` separator does not leak into the summary ──
#
# Regression: an earlier release.yml awk extracted everything up to the
# next `## v` heading without stripping the entry's trailing `---`,
# which surfaced as a horizontal rule in the rendered output.
# `extract-release-notes.sh` (used by both this script and release.yml)
# now strips it; this pins that contract from the Discord side too.

echo ""
echo "Trailing entry separator stripped:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp" v1.5.3 v1.5.4

  write_changelog "$tmp" <<'EOF'
## v1.5.4 - 2026-04-30

### Fixes
- placeholder ([#1](https://example/1))

---
## v1.5.3 - 2026-04-29

---
EOF

  content=$(run_notify "$tmp" v1.5.4 | jq -r '.content')
  # The line right above the changelog footer must not be a bare `---`
  # (which Discord renders as an `<hr>` between the bullets and the
  # changelog link). Use a regex on the raw content to guard against
  # both LF and CRLF line endings.
  if [[ "$content" =~ $'\n---\n' ]] || [[ "$content" =~ $'\n---\r\n' ]]; then
    echo "  ✗ trailing --- leaked into Discord summary"
    bump_score fail
  else
    echo "  ✓ trailing --- not in Discord summary"
    bump_score pass
  fi
)

# ── Test: long summaries truncate at a paragraph break, not in the middle ──

echo ""
echo "Truncation:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp" v1.5.3 v1.5.4

  {
    echo "## v1.5.4 - 2026-04-30"
    echo ""
    for _ in {1..30}; do echo -n "Lorem ipsum dolor sit amet consectetur adipiscing elit. "; done
    echo; echo
    for _ in {1..30}; do echo -n "Second paragraph filler text appears here. "; done
    echo
    echo ""
    echo "### Fixes"
    echo "- placeholder ([#1](https://example/1))"
    echo ""
    echo "---"
  } > "$tmp/apps/website/src/content/docs/changelog.mdx"

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
