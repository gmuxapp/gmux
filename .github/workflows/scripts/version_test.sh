#!/usr/bin/env bash
# End-to-end tests for version.sh using a scratch git repository.
#
# Most of the versioning logic now lives in git-cliff, so these tests
# exercise the behaviors that version.sh still owns:
#
#   1. Release commit detection (loop prevention)
#   2. No-op when there are no releasable commits
#   3. Correct bump level (patch / minor / major)
#   4. Changelog insertion placement
#   5. Highlights prepended into changelog.mdx and RELEASE_NOTES.md
#   6. Highlights file cleared after release
#   7. RELEASE_NOTES.md format compatible with notify-discord.sh
set -uo pipefail

# Locate the real version.sh so we can run it against scratch repos.
REAL_SCRIPT="$(cd "$(dirname "$0")" && pwd)/version.sh"
REAL_CLIFF_TOML="$(cd "$(dirname "$0")/../../.." && pwd)/cliff.toml"

if ! command -v git-cliff >/dev/null 2>&1; then
  echo "SKIP: git-cliff is not installed" >&2
  exit 0
fi

# Use a scoreboard file so counts survive subshells used for test isolation.
SCOREBOARD=$(mktemp)
trap 'rm -f "$SCOREBOARD"' EXIT
echo "0 0" > "$SCOREBOARD"

bump_score() {
  local which="$1"
  read -r pass fail < "$SCOREBOARD"
  case "$which" in
    pass) pass=$((pass + 1)) ;;
    fail) fail=$((fail + 1)) ;;
  esac
  echo "$pass $fail" > "$SCOREBOARD"
}

assert_eq() {
  local label="$1" expected="$2" actual="$3"
  if [[ "$expected" == "$actual" ]]; then
    echo "  ✓ $label"
    bump_score pass
  else
    echo "  ✗ $label"
    echo "    expected: $(printf %q "$expected")"
    echo "    actual:   $(printf %q "$actual")"
    bump_score fail
  fi
}

assert_contains() {
  local label="$1" needle="$2" haystack="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    echo "  ✓ $label"
    bump_score pass
  else
    echo "  ✗ $label"
    echo "    expected to contain: $needle"
    echo "    actual: $haystack"
    bump_score fail
  fi
}

assert_not_contains() {
  local label="$1" needle="$2" haystack="$3"
  if [[ "$haystack" != *"$needle"* ]]; then
    echo "  ✓ $label"
    bump_score pass
  else
    echo "  ✗ $label"
    echo "    expected NOT to contain: $needle"
    echo "    actual: $haystack"
    bump_score fail
  fi
}

# ── Create a throwaway repo mirroring gmux's layout ──
#
# The script uses the repo root (three levels up from the script path)
# to find cliff.toml, apps/website/src/content/docs/changelog.mdx, and
# RELEASE_HIGHLIGHTS.md. We replicate that layout in a temp directory.

make_repo() {
  local tmp="$1"
  mkdir -p "$tmp/.github/workflows/scripts"
  mkdir -p "$tmp/apps/website/src/content/docs"
  cp "$REAL_SCRIPT" "$tmp/.github/workflows/scripts/version.sh"
  cp "$REAL_CLIFF_TOML" "$tmp/cliff.toml"

  # Seed changelog with one past release so the insertion path is exercised.
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

run_script() {
  local tmp="$1" arg="${2:-}"
  (
    cd "$tmp"
    bash .github/workflows/scripts/version.sh ${arg:+"$arg"} 2>&1
  )
}

# ── Test: no releasable commits (only docs/chore/refactor) ──

echo "No releasable commits:"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "docs: update readme"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "chore: bump deps"

  output=$(run_script "$tmp")
  assert_contains "docs+chore only exits with no release" "No releasable commits" "$output"

  # changelog.mdx should be unchanged
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  assert_not_contains "changelog.mdx unchanged" "## v1.0.1" "$changelog"
)

# ── Test: release commit detection ──

echo ""
echo "Release commit detection:"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "release: v1.0.1"

  output=$(run_script "$tmp")
  assert_contains "release: vX.Y.Z triggers skip" "Release commit, skipping" "$output"
)

# ── Test: commits that mention "release/next" in their subject are NOT
# ── mistaken for release commits. Regression test: a prior loose
# ── regex `=~ release/next` matched anywhere in the subject line.

echo ""
echo "Release commit detection does not match substring matches:"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "fix: use fixed release/next branch for release PR (#13)"

  version=$(run_script "$tmp" --dry-run | head -1)
  assert_eq "fix that mentions release/next still triggers patch bump" "v1.0.1" "$version"
)

# ── Test: fix commit triggers patch bump ──

echo ""
echo "Patch bump (fix commit):"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "fix: handle nil pointer (#42)"

  version=$(run_script "$tmp" --dry-run | head -1)
  assert_eq "fix bumps patch" "v1.0.1" "$version"

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  assert_contains "new section heading"      "## v1.0.1" "$changelog"
  assert_contains "fix bullet text"          "handle nil pointer" "$changelog"
  assert_contains "PR link"                  "[#42](https://github.com/gmuxapp/gmux/pull/42)" "$changelog"
  assert_contains "Fixes group heading"      "### Fixes" "$changelog"
  assert_contains "previous version retained" "## v1.0.0" "$changelog"
)

# ── Test: feat commit triggers minor bump ──

echo ""
echo "Minor bump (feat commit):"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "feat: add dark mode (#50)"

  version=$(run_script "$tmp" --dry-run | head -1)
  assert_eq "feat bumps minor" "v1.1.0" "$version"
)

# ── Test: feat! commit triggers major bump ──

echo ""
echo "Major bump (breaking change):"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "feat!: redesign API (#99)"

  version=$(run_script "$tmp" --dry-run | head -1)
  assert_eq "feat! bumps major" "v2.0.0" "$version"

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  assert_contains "Breaking section present" "### Breaking" "$changelog"
)

# ── Test: mixed commits, highest bump wins ──

echo ""
echo "Mixed commits (highest bump wins):"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "fix: small bug (#1)"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "feat: big feature (#2)"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "fix: another bug (#3)"

  version=$(run_script "$tmp" --dry-run | head -1)
  assert_eq "fix+feat+fix bumps minor" "v1.1.0" "$version"

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  assert_contains "feature appears" "big feature" "$changelog"
  assert_contains "fix appears"     "small bug"   "$changelog"
  assert_contains "other fix"       "another bug" "$changelog"
)

# ── Test: highlights injected into both files ──

echo ""
echo "Highlights integration:"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "feat: add dark mode (#50)"

  cat > RELEASE_HIGHLIGHTS.md <<'EOF'
Major theme overhaul: every panel now supports a dark variant.

### Migration
Run `gmux migrate-theme` after updating.
EOF

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  assert_contains "highlights prose in changelog"     "Major theme overhaul" "$changelog"
  assert_contains "highlights subheading in changelog" "### Migration"        "$changelog"

  release_notes=$(cat RELEASE_NOTES.md)
  assert_contains "highlights in release notes"   "Major theme overhaul"     "$release_notes"
  assert_contains "marker in release notes"       "<!-- highlights-end -->" "$release_notes"
  assert_contains "bullets in release notes"      "add dark mode"            "$release_notes"

  # notify-discord.sh extraction: everything before the highlights-end marker.
  discord_summary=$(sed '/<!-- highlights-end -->/,$d' RELEASE_NOTES.md)
  assert_contains "Discord gets highlights"         "Major theme overhaul" "$discord_summary"
  assert_not_contains "Discord does not get bullets" "add dark mode"       "$discord_summary"

  # Highlights file should be cleared (contain only the stub comment)
  highlights_after=$(cat RELEASE_HIGHLIGHTS.md)
  assert_not_contains "highlights cleared after release" "Major theme overhaul" "$highlights_after"
  assert_contains "highlights stub retained" "<!--" "$highlights_after"
)

# ── Test: BREAKING CHANGE: footer triggers major bump and lands in Breaking ──
#
# git-cliff moves the footer out of the body and exposes it separately,
# so the commit parser must match on `footer =`, not `body =`. This test
# pins that behavior.

echo ""
echo "BREAKING CHANGE footer:"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "$(printf 'feat: new config format (#60)\n\nBREAKING CHANGE: old config format no longer supported.')"

  version=$(run_script "$tmp" --dry-run | head -1)
  assert_eq "BREAKING CHANGE footer bumps major" "v2.0.0" "$version"

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  assert_contains "Breaking section present"      "### Breaking"      "$changelog"
  assert_contains "breaking commit under Breaking" "new config format" "$changelog"
  # The commit must NOT also appear under Features.
  features_section=$(echo "$changelog" | awk '/^## v2\.0\.0/{found=1; next} found && /^## v/{exit} found && /^### /{section=$0; next} found{print section" | "$0}' | grep -c '### Features | - new config format' || true)
  assert_eq "breaking commit is NOT duplicated under Features" "0" "$features_section"
)

# ── Test: perf triggers patch bump and lands in Features ──

echo ""
echo "Perf commits trigger patch release:"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "perf: faster session resume (#70)"

  version=$(run_script "$tmp" --dry-run | head -1)
  assert_eq "perf bumps patch" "v1.0.1" "$version"

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  assert_contains "perf bullet under Features" "faster session resume" "$changelog"
  assert_contains "Features section exists"    "### Features"          "$changelog"
)

# ── Test: changelog insertion preserves prior release sections ──
#
# The awk insertion path should place the new section above the first
# existing `## v` heading without disturbing anything below it.

echo ""
echo "Changelog insertion preserves prior sections:"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "feat: new thing (#80)"

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  assert_contains "prior v1.0.0 heading still present" "## v1.0.0"         "$changelog"
  assert_contains "prior v1.0.0 bullet still present"  "everything"        "$changelog"
  assert_contains "new v1.1.0 heading present"         "## v1.1.0 - "      "$changelog"

  # New heading must come before old heading in the file.
  new_line=$(grep -n '^## v1.1.0' apps/website/src/content/docs/changelog.mdx | head -1 | cut -d: -f1)
  old_line=$(grep -n '^## v1.0.0' apps/website/src/content/docs/changelog.mdx | head -1 | cut -d: -f1)
  if [[ -n "$new_line" && -n "$old_line" && "$new_line" -lt "$old_line" ]]; then
    echo "  ✓ new section inserted above prior section"
    bump_score pass
  else
    echo "  ✗ new section not inserted above prior section (new line: $new_line, old line: $old_line)"
    bump_score fail
  fi
)

# ── Test: scope appears as bold tag in bullets ──

echo ""
echo "Scope rendering:"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "feat(peering): reconnect after system sleep (#30)"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "fix(web): overlap on narrow viewports (#31)"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "feat: unscoped feature (#32)"

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  assert_contains "scoped feat bullet has bold scope"   "**(peering)** reconnect after system sleep" "$changelog"
  assert_contains "scoped fix bullet has bold scope"    "**(web)** overlap on narrow viewports"      "$changelog"
  assert_contains "unscoped bullet has no scope prefix" "- unscoped feature"                         "$changelog"
  assert_not_contains "unscoped bullet has no empty bold" "- ****"                                   "$changelog"
)

# ── Test: date appears in version heading ──

echo ""
echo "Date in heading:"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "fix: today bug (#1)"

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  today=$(date -u +%Y-%m-%d)
  assert_contains "heading includes ISO date" "## v1.0.1 - $today" "$changelog"
)

# ── Test: security commits get their own section ──

echo ""
echo "Security section:"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "security: redact tokens from logs (#40)"

  version=$(run_script "$tmp" --dry-run | head -1)
  assert_eq "security bumps patch" "v1.0.1" "$version"

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  assert_contains "Security section heading" "### Security" "$changelog"
  assert_contains "Security bullet text"     "redact tokens from logs" "$changelog"
)

# ── Test: security appears before Features in the ordering ──

echo ""
echo "Security ordering:"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "feat: new feature (#1)"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "security: patch vulnerability (#2)"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "fix: small bug (#3)"

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)

  # Extract group heading order within the new release section.
  order=$(echo "$changelog" | awk '/^## v1\.1\.0/{found=1; next} found && /^## v/{exit} found && /^### /{print}')
  expected_order=$(printf '### Security\n### Features\n### Fixes')
  assert_eq "Security appears before Features and Fixes" "$expected_order" "$order"
)

# ── Test: inline HTML comments in highlights do not swallow content ──
#
# Regression test for a sed-range bug: `sed '/<!--/,/-->/d'` swallows
# everything after an inline `<!-- note -->` because sed won't match
# start and end on the same line. The script uses a stateful awk
# helper instead.

echo ""
echo "Inline HTML comments in highlights:"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "feat: something (#90)"

  cat > RELEASE_HIGHLIGHTS.md <<'EOF'
First paragraph.

<!-- inline note from the author -->

Second paragraph survives the inline comment.

<!--
This whole block should be dropped.
-->

Third paragraph also survives.
EOF

  run_script "$tmp" >/dev/null
  changelog=$(cat apps/website/src/content/docs/changelog.mdx)
  assert_contains "first paragraph present"                      "First paragraph"                        "$changelog"
  assert_contains "second paragraph survives inline comment"     "Second paragraph survives"             "$changelog"
  assert_contains "third paragraph survives multi-line comment"  "Third paragraph also survives"         "$changelog"
  assert_not_contains "inline comment text is stripped"          "inline note from the author"           "$changelog"
  assert_not_contains "multi-line comment text is stripped"      "This whole block should be dropped"    "$changelog"
)

# ── Test: empty highlights file produces clean release notes ──

echo ""
echo "Empty highlights produces clean output:"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "fix: tiny bug (#7)"

  # Leave RELEASE_HIGHLIGHTS.md as the default stub (comment only)
  run_script "$tmp" >/dev/null

  release_notes=$(cat RELEASE_NOTES.md)
  discord_summary=$(sed '/<!-- highlights-end -->/,$d' RELEASE_NOTES.md | awk '
    { lines[NR] = $0 }
    END {
      first = 1
      while (first <= NR && lines[first] ~ /^[[:space:]]*$/) first++
      last = NR
      while (last >= first && lines[last] ~ /^[[:space:]]*$/) last--
      for (i = first; i <= last; i++) print lines[i]
    }
  ')
  assert_eq "Discord summary empty when no highlights" "" "$discord_summary"
  assert_contains "release notes has bullets"         "tiny bug" "$release_notes"
)

# ── Test: highlights with literal `---` (horizontal rule) survive ──
#
# Regression test for the old `---` separator: if a user used a
# markdown horizontal rule inside their highlights, the old
# `sed '/^---$/,$d'` would truncate the summary at the first `---`,
# losing the rest of the prose. With `<!-- highlights-end -->` as the
# sentinel, user-written `---` lines pass through unchanged.

echo ""
echo "Horizontal rules in highlights pass through:"
(
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  make_repo "$tmp"
  cd "$tmp"
  git -c commit.gpgsign=false commit --allow-empty --quiet -m "feat: add thing (#55)"

  cat > RELEASE_HIGHLIGHTS.md <<'EOF'
First major theme.

---

Second major theme, after a horizontal rule.
EOF

  run_script "$tmp" >/dev/null

  discord_summary=$(sed '/<!-- highlights-end -->/,$d' RELEASE_NOTES.md)
  assert_contains "first theme present"           "First major theme"         "$discord_summary"
  assert_contains "horizontal rule preserved"     "---"                        "$discord_summary"
  assert_contains "second theme after rule present" "Second major theme"       "$discord_summary"
  assert_not_contains "bullets not in Discord"    "add thing"                 "$discord_summary"
)

# ── Summary ──

read -r pass fail < "$SCOREBOARD"
echo ""
echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]
