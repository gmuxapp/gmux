#!/usr/bin/env bash
# Tests for version.sh.
#
# The interesting logic lives in git-cliff and cliff.toml; version.sh
# only owns:
#
#   - Loop-prevention guards (skip release commits, exit when no
#     releasable commits)
#   - The bump version that ends up on stdout (single source of truth
#     for the rest of the release pipeline)
#   - Orchestration: reading RELEASE_HIGHLIGHTS.md, writing
#     RELEASE_NOTES.md and changelog.mdx, optional --clear-highlights
#   - Highlights parsing edge cases (HTML comments, the
#     <!-- highlights-end --> sentinel, horizontal-rule survival)
#
# Tests are organised around those behaviours. A small smoke test
# pins that cliff.toml still produces every group heading we expect,
# but we deliberately don't assert section ordering, dates, or other
# template details that would break on cosmetic git-cliff changes.

set -uo pipefail

REAL_SCRIPT="$(cd "$(dirname "$0")" && pwd)/version.sh"
REAL_CLIFF_TOML="$(cd "$(dirname "$0")/../../.." && pwd)/cliff.toml"

if ! command -v git-cliff >/dev/null 2>&1; then
  echo "SKIP: git-cliff is not installed" >&2
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
    echo "    expected: $(printf %q "$2")"
    echo "    actual:   $(printf %q "$3")"
    bump_score fail
  fi
}

assert_contains() {
  if [[ "$3" == *"$2"* ]]; then echo "  ✓ $1"; bump_score pass
  else
    echo "  ✗ $1"
    echo "    expected to contain: $2"
    echo "    actual (first 200 chars): ${3:0:200}"
    bump_score fail
  fi
}

assert_not_contains() {
  if [[ "$3" != *"$2"* ]]; then echo "  ✓ $1"; bump_score pass
  else
    echo "  ✗ $1"
    echo "    expected NOT to contain: $2"
    bump_score fail
  fi
}

# Lay out a temp repo that mirrors the bits of the gmux tree version.sh
# touches: cliff.toml at the root, the changelog under apps/website/...,
# and a stub RELEASE_HIGHLIGHTS.md. Tag v1.0.0 on the initial commit so
# git-cliff's --bump always has a baseline.
make_repo() {
  local tmp="$1"
  mkdir -p "$tmp/.github/workflows/scripts"
  mkdir -p "$tmp/apps/website/src/content/docs"
  cp "$REAL_SCRIPT" "$tmp/.github/workflows/scripts/version.sh"
  cp "$REAL_CLIFF_TOML" "$tmp/cliff.toml"

  cat > "$tmp/apps/website/src/content/docs/changelog.mdx" <<'EOF'
---
title: Changelog
---

## v1.0.0

Initial release.

### Features
- everything ([#1](https://github.com/gmuxapp/gmux/pull/1))

---
EOF

  cat > "$tmp/RELEASE_HIGHLIGHTS.md" <<'EOF'
<!-- stub -->
EOF

  (
    cd "$tmp"
    git init --quiet --initial-branch=main
    git config user.email "test@example.com"
    git config user.name "Test"
    git add -A
    git -c commit.gpgsign=false commit --quiet -m "init"
    git tag v1.0.0
  )
}

# Unset CI so version.sh's "fail loudly in CI when GITHUB_TOKEN missing"
# branch doesn't trip the test runner. Real release runs go through the
# CI=true path.
run_script() {
  local tmp="$1" arg="${2:-}"
  (
    cd "$tmp"
    CI= bash .github/workflows/scripts/version.sh ${arg:+"$arg"} 2>&1
  )
}

# Add an empty commit and return without noise.
commit() {
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "$1"
}

# ── Test: docs / chore alone do not trigger a release ──
#
# `releasable_count` filters to Features / Fixes / Breaking / Security.
# A docs-only push must be a no-op so the workflow doesn't open a PR.

echo "No releasable commits:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "docs: update readme"
  commit "chore: bump deps"

  output=$(run_script "$tmp")
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)

  assert_contains "exits with 'no releasable commits'" "No releasable commits" "$output"
  assert_not_contains "changelog.mdx untouched"        "## v1.0.1"             "$changelog"
)

# ── Test: HEAD = "release: vX.Y.Z" triggers loop-prevention skip ──
#
# Plus the regression that subjects merely *containing* "release/next"
# (e.g. a fix mentioning the branch name) are NOT treated as release
# commits.

echo ""
echo "Release commit detection:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "release: v1.0.1"

  output=$(run_script "$tmp")
  assert_contains "release: vX.Y.Z is recognised" "Release commit, skipping" "$output"
)
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "fix: use fixed release/next branch (#13)"

  version=$(run_script "$tmp" --dry-run | head -1)
  assert_eq "subject containing 'release/next' still bumps" "v1.0.1" "$version"
)

# ── Test: bump level matches the highest commit type ──
#
# version.sh's job is to take git-cliff's bump output and surface it
# verbatim on stdout. Cover one commit per type, plus the "highest
# wins" interaction.

echo ""
echo "Bump level:"
for case in \
  "fix: small bug (#1)|v1.0.1" \
  "feat: add thing (#2)|v1.1.0" \
  "feat!: redesign (#3)|v2.0.0" \
  "perf: faster boot (#4)|v1.0.1" \
  "security: redact secrets (#5)|v1.0.1"
do
  msg=${case%%|*}; expected=${case##*|}
  (
    tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
    make_repo "$tmp"; cd "$tmp"
    commit "$msg"
    actual=$(run_script "$tmp" --dry-run | head -1)
    assert_eq "${msg%%:*} → $expected" "$expected" "$actual"
  )
done
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "fix: small (#1)"
  commit "feat: bigger (#2)"
  commit "fix: another (#3)"
  actual=$(run_script "$tmp" --dry-run | head -1)
  assert_eq "feat among fixes wins minor bump" "v1.1.0" "$actual"
)

# ── Test: cliff.toml smoke test (every expected group heading appears) ──
#
# Pin that cliff.toml still routes the four commit types we care about
# into the four group headings notify-discord.sh and the changelog
# rely on. We don't assert ordering or per-bullet wording.

echo ""
echo "cliff.toml renders all expected group headings:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "fix: f (#1)"
  commit "feat: F (#2)"
  commit "perf: P (#3)"
  commit "security: S (#4)"
  commit "feat!: B (#5)"

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)

  assert_contains "Breaking heading" "### Breaking" "$changelog"
  assert_contains "Security heading" "### Security" "$changelog"
  assert_contains "Features heading" "### Features" "$changelog"
  assert_contains "Fixes heading"    "### Fixes"    "$changelog"
)

# ── Test: highlights flow into RELEASE_NOTES.md and changelog.mdx ──
#
# This is version.sh's main orchestration responsibility. The
# <!-- highlights-end --> marker in RELEASE_NOTES.md is what
# notify-discord.sh keys off to extract the Discord summary; if the
# marker drifts or the highlights aren't injected, Discord stops
# announcing prose. The highlights file itself must survive (single
# source of truth; release.yml clears it post-release).

echo ""
echo "Highlights integration:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "feat: dark mode (#50)"
  cat > RELEASE_HIGHLIGHTS.md <<'EOF'
Major theme overhaul.
EOF

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  release_notes=$(cat RELEASE_NOTES.md)
  highlights_after=$(cat RELEASE_HIGHLIGHTS.md)

  assert_contains "highlights reach changelog"      "Major theme overhaul"    "$changelog"
  assert_contains "highlights reach release notes"  "Major theme overhaul"    "$release_notes"
  assert_contains "Discord-extraction marker"       "<!-- highlights-end -->" "$release_notes"
  assert_contains "highlights file preserved"       "Major theme overhaul"    "$highlights_after"

  # The Discord summary is everything BEFORE the marker. Bullets must
  # not bleed into it (they belong below the marker, and the link
  # already says "see the changelog").
  discord=$(sed '/<!-- highlights-end -->/,$d' RELEASE_NOTES.md)
  assert_not_contains "Discord summary excludes bullets" "dark mode" "$discord"
)

# ── Test: --clear-highlights resets the file to a stub ──
#
# Invoked by release.yml after a successful release. Without this
# mode, RELEASE_HIGHLIGHTS.md would carry the previous release's
# prose into the next cycle.

echo ""
echo "--clear-highlights:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  echo "Stale prose from a shipped release." > "$tmp/RELEASE_HIGHLIGHTS.md"

  run_script "$tmp" --clear-highlights >/dev/null
  highlights_after=$(cat "$tmp/RELEASE_HIGHLIGHTS.md")
  assert_not_contains "stale prose removed" "Stale prose" "$highlights_after"
)

# ── Test: highlights HTML-comment stripping ──
#
# Regression test for a sed-range bug: `sed '/<!--/,/-->/d'` swallows
# everything after an inline `<!-- note -->` because sed can't match
# start and end on the same line. version.sh uses a stateful awk
# helper; pin that it handles both inline and multi-line comments.

echo ""
echo "Highlights HTML comments:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "feat: thing (#90)"
  cat > RELEASE_HIGHLIGHTS.md <<'EOF'
First paragraph.

<!-- inline note -->

Second paragraph.

<!--
multi-line
comment
-->

Third paragraph.
EOF

  run_script "$tmp" >/dev/null
  release_notes=$(cat RELEASE_NOTES.md)

  assert_contains "content before inline comment kept"     "First paragraph"  "$release_notes"
  assert_contains "content after inline comment kept"      "Second paragraph" "$release_notes"
  assert_contains "content after multi-line comment kept"  "Third paragraph"  "$release_notes"
  assert_not_contains "inline comment stripped"            "inline note"      "$release_notes"
  assert_not_contains "multi-line comment stripped"        "multi-line"       "$release_notes"
)

# ── Test: a literal `---` inside highlights survives ──
#
# Regression: the old format used `---` as the highlights-end marker,
# which collided with user-written horizontal rules. The
# `<!-- highlights-end -->` sentinel fixed it; this pins the fix.

echo ""
echo "Horizontal rule in highlights survives:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "feat: thing (#55)"
  cat > RELEASE_HIGHLIGHTS.md <<'EOF'
Theme A.

---

Theme B.
EOF

  run_script "$tmp" >/dev/null
  discord=$(sed '/<!-- highlights-end -->/,$d' RELEASE_NOTES.md)

  assert_contains "content before horizontal rule" "Theme A" "$discord"
  assert_contains "content after horizontal rule"  "Theme B" "$discord"
)

# ── Test: new changelog section is inserted ABOVE prior versions ──
#
# version.sh owns the awk insertion; if it ever appended at the end
# instead, the latest release would render at the bottom of the page.

echo ""
echo "Changelog insertion order:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "feat: new (#80)"

  run_script "$tmp" >/dev/null
  new_line=$(grep -n '^## v1.1.0' apps/website/src/content/docs/changelog.mdx | head -1 | cut -d: -f1)
  old_line=$(grep -n '^## v1.0.0' apps/website/src/content/docs/changelog.mdx | head -1 | cut -d: -f1)

  if [[ -n "$new_line" && -n "$old_line" && "$new_line" -lt "$old_line" ]]; then
    echo "  ✓ new section above prior section (lines $new_line < $old_line)"
    bump_score pass
  else
    echo "  ✗ new section not above prior section (new=$new_line, old=$old_line)"
    bump_score fail
  fi
)

# ── Summary ──

read -r pass fail < "$SCOREBOARD"
echo ""
echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]
