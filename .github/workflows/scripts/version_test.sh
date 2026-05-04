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
#   - Orchestration: reading $PROSE, writing changelog.mdx, the
#     next-version marker, and the PR body
#   - Prose extraction (--extract-prose) from a PR body on stdin
#   - PR body composition (prose markers, bullets markers,
#     placeholder hint when prose is empty)
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
# touches: cliff.toml at the root, the changelog under apps/website/...
# Tag v1.0.0 on the initial commit so git-cliff's --bump always has a
# baseline.
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
# branch doesn't trip the test runner.
run_script() {
  local tmp="$1" arg="${2:-}"
  (
    cd "$tmp"
    CI= bash .github/workflows/scripts/version.sh ${arg:+"$arg"} 2>&1
  )
}

# Extract just the version tag from a run, ignoring noise like
# git-cliff's "a new version is available" notice on stderr.
run_version() {
  run_script "$@" | grep -m1 -E '^v[0-9]' || true
}

# Run with a custom $PROSE. Useful for asserting that prose flows into
# the right artifacts.
run_with_prose() {
  local tmp="$1" prose="$2"
  (
    cd "$tmp"
    CI= PROSE="$prose" bash .github/workflows/scripts/version.sh 2>&1
  )
}

commit() {
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "$1"
}

# ── Test: docs / chore alone do not trigger a release ──

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
  if [[ -e .github/release-target ]]; then
    echo "  ✗ release-target should not be created on a no-op run"; bump_score fail
  else
    echo "  ✓ release-target absent on a no-op run"; bump_score pass
  fi
)

# ── Test: HEAD = "release: vX.Y.Z" triggers loop-prevention skip ──

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

  version=$(run_version "$tmp" --dry-run)
  assert_eq "subject containing 'release/next' still bumps" "v1.0.1" "$version"
)

# ── Test: bump level matches the highest commit type ──

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
    actual=$(run_version "$tmp" --dry-run)
    assert_eq "${msg%%:*} → $expected" "$expected" "$actual"
  )
done
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "fix: small (#1)"
  commit "feat: bigger (#2)"
  commit "fix: another (#3)"
  actual=$(run_version "$tmp" --dry-run)
  assert_eq "feat among fixes wins minor bump" "v1.1.0" "$actual"
)

# ── Test: `(release)`-scoped commits are hidden from the changelog ──

echo ""
echo "(release)-scoped commits skipped from changelog:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "feat: real user feature (#1)"
  commit "feat(release): internal plumbing (#2)"
  commit "fix(release): more plumbing (#3)"

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)

  assert_contains     "user-visible feat present" "real user feature" "$changelog"
  assert_not_contains "feat(release) hidden"      "internal plumbing" "$changelog"
  assert_not_contains "fix(release) hidden"       "more plumbing"     "$changelog"
)

echo ""
echo "(release)-only history is a no-op release:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "feat(release): plumbing only (#1)"
  commit "fix(release): more plumbing (#2)"

  output=$(run_script "$tmp")
  assert_contains "exits with 'no releasable commits'" "No releasable commits" "$output"
)

echo ""
echo "breaking (release) commit still surfaces:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "feat(release)!: rename WEBHOOK_URL env var (#1)"

  version=$(run_version "$tmp" --dry-run)
  assert_eq "breaking release commit bumps major" "v2.0.0" "$version"

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  assert_contains "breaking release commit in Breaking section" "### Breaking"      "$changelog"
  assert_contains "breaking release commit body present"        "rename WEBHOOK_URL" "$changelog"
)

# ── Test: cliff.toml smoke test ──

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

# ── Test: $PROSE flows into changelog.mdx and PR body ──

echo ""
echo "Prose integration:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "feat: dark mode (#50)"

  run_with_prose "$tmp" "Major theme overhaul." >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  pr_body=$(cat pr-body.md)

  assert_contains "prose reaches changelog"     "Major theme overhaul" "$changelog"
  assert_contains "prose reaches PR body"       "Major theme overhaul" "$pr_body"
  assert_contains "PR body has prose-start marker" "<!-- prose-start -->" "$pr_body"
  assert_contains "PR body has prose-end marker"   "<!-- prose-end -->"   "$pr_body"
  assert_contains "PR body has bullets markers"    "<!-- bullets-start"   "$pr_body"
  assert_contains "PR body shows next version"     "Release **v1.1.0**"   "$pr_body"
)

# ── Test: empty prose puts the hint comment in the PR body, not the changelog ──
#
# A first-time editor needs a visible cue ("type prose here"); the
# changelog page should not show that cue.

echo ""
echo "Empty prose:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "fix: minor (#10)"

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  pr_body=$(cat pr-body.md)

  assert_contains     "PR body shows the hint" "add release prose here" "$pr_body"
  assert_not_contains "changelog has no hint"  "add release prose here" "$changelog"
)

# ── Test: --extract-prose round-trips PROSE through a PR body ──
#
# The workflow reads the open PR body, extracts prose, and feeds it
# back as PROSE. This pins that round-trip is lossless for plain
# markdown and that the empty-prose hint is treated as empty (not as
# literal prose).

echo ""
echo "--extract-prose:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "feat: thing (#42)"

  # Round-trip 1: real prose survives.
  run_with_prose "$tmp" "Real prose paragraph. Multi-sentence. With detail." >/dev/null
  pr_body=$(cat pr-body.md)
  extracted=$(printf '%s' "$pr_body" | bash .github/workflows/scripts/version.sh --extract-prose)
  assert_eq "extracted prose round-trips" "Real prose paragraph. Multi-sentence. With detail." "$extracted"

  # Round-trip 2: empty prose extracts as empty (hint comment is stripped).
  rm -f pr-body.md
  run_script "$tmp" >/dev/null
  pr_body=$(cat pr-body.md)
  extracted=$(printf '%s' "$pr_body" | bash .github/workflows/scripts/version.sh --extract-prose)
  assert_eq "empty-prose hint extracts as empty" "" "$extracted"
)

# ── Test: --extract-prose handles multi-line and inline HTML comments ──
#
# Editors may leave HTML comments inside prose. The Discord/Changelog
# pipeline shouldn't see them.

echo ""
echo "--extract-prose strips HTML comments:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cp "$REAL_SCRIPT" "$tmp/version.sh"

  body=$(cat <<'EOF'
Release **v1.0.0**.

<!-- prose-start -->
First paragraph.

<!-- inline comment -->

Second paragraph.

<!--
multi-line
note
-->

Third paragraph.
<!-- prose-end -->

bullets...
EOF
)
  extracted=$(printf '%s' "$body" | bash "$tmp/version.sh" --extract-prose)
  assert_contains     "first paragraph kept"      "First paragraph"  "$extracted"
  assert_contains     "second paragraph kept"     "Second paragraph" "$extracted"
  assert_contains     "third paragraph kept"      "Third paragraph"  "$extracted"
  assert_not_contains "inline comment stripped"   "inline comment"   "$extracted"
  assert_not_contains "multi-line comment stripped" "multi-line"     "$extracted"
)

# ── Test: a literal `---` inside prose survives ──
#
# Regression: an earlier format used `---` as the highlights-end
# marker, which collided with horizontal rules in user prose. The
# explicit `<!-- prose-end -->` sentinel sidesteps that.

echo ""
echo "Horizontal rule in prose survives:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "feat: thing (#55)"

  prose=$(printf 'Theme A.\n\n---\n\nTheme B.')
  run_with_prose "$tmp" "$prose" >/dev/null
  pr_body=$(cat pr-body.md)

  # Re-extract through the round-trip and confirm both halves survive.
  extracted=$(printf '%s' "$pr_body" | bash .github/workflows/scripts/version.sh --extract-prose)
  assert_contains "content before horizontal rule survives round-trip" "Theme A" "$extracted"
  assert_contains "content after horizontal rule survives round-trip"  "Theme B" "$extracted"
)

# ── Test: .github/release-target gets the next version ──
#
# This file gives the release commit a non-empty diff (peter-evans
# needs file changes to commit). It must contain exactly the next
# version tag; no extra whitespace, no prefix.

echo ""
echo "release-target marker:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"; cd "$tmp"
  commit "feat: thing (#60)"

  run_script "$tmp" >/dev/null
  target=$(cat .github/release-target)
  assert_eq "release-target contains next version" "v1.1.0" "$target"
)

# ── Test: new changelog section inserted ABOVE prior versions ──

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
