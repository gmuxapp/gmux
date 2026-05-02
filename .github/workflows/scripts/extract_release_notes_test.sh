#!/usr/bin/env bash
# Tests for extract-release-notes.sh.
#
# This helper is the shared dependency of `notify-discord.sh` and
# `release.yml`. A regression here changes both the GitHub Release
# body and the Discord post, so we pin its contract directly.
#
# Contract:
#
#   1. Returns the body of the latest entry (the first `## v` block).
#   2. Strips the version heading.
#   3. Strips exactly one trailing `---` entry separator if present.
#   4. Preserves any `---` that appears earlier (user-written
#      horizontal rules in prose).
#   5. Trims surrounding blank lines.
#   6. Empty/missing input → empty output, exit 0.

set -uo pipefail

REAL_SCRIPT="$(cd "$(dirname "$0")" && pwd)/extract-release-notes.sh"

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
    diff <(printf '%s' "$2") <(printf '%s' "$3") | sed 's/^/      /'
    bump_score fail
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

run_extract() {
  local changelog=$1
  bash "$REAL_SCRIPT" "$changelog"
}

# ── Test: typical entry returns prose + bullets, no heading, no trailing --- ──

echo "Typical entry:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  cat > "$tmp/changelog.mdx" <<'EOF'
## v1.5.0 - 2026-04-30

Some prose.

### Features
- foo ([#1](https://example/1))

---
## v1.4.0 - 2026-04-26

---
EOF

  out=$(run_extract "$tmp/changelog.mdx")
  expected=$'Some prose.\n\n### Features\n- foo ([#1](https://example/1))'
  assert_eq "exact body" "$expected" "$out"
)

# ── Test: trailing `---` is stripped ──
#
# Regression: an earlier release.yml awk left the trailing `---` in
# the output, which renders as `<hr>` at the bottom of the GitHub
# Release body. The helper must strip exactly the one trailing
# separator.

echo ""
echo "Trailing separator:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  cat > "$tmp/changelog.mdx" <<'EOF'
## v1.0.0 - 2026-04-30

### Fixes
- bar ([#1](https://example/1))

---
EOF

  out=$(run_extract "$tmp/changelog.mdx")
  # Last non-blank line must be the bullet, not `---`.
  last=$(echo "$out" | awk 'NF { last = $0 } END { print last }')
  assert_not_contains "last line is not the separator" "---" "$last"
  assert_contains     "last line is the bullet"        "bar" "$last"
)

# ── Test: a `---` inside prose is preserved ──
#
# Users may write horizontal rules in their release prose. Only the
# *trailing* `---` is the entry separator; earlier ones survive.

echo ""
echo "Horizontal rule in prose:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  cat > "$tmp/changelog.mdx" <<'EOF'
## v2.0.0 - 2026-05-01

Theme A.

---

Theme B.

### Features
- thing ([#1](https://example/1))

---
## v1.9.0 - 2026-04-15

---
EOF

  out=$(run_extract "$tmp/changelog.mdx")
  assert_contains "Theme A present"             "Theme A" "$out"
  assert_contains "horizontal rule preserved"   $'\n---\n' "$out"
  assert_contains "Theme B present"             "Theme B" "$out"
  assert_contains "bullets present"             "thing"   "$out"
  # And only ONE `---` should remain (the user's), not the trailing
  # entry separator.
  count=$(echo "$out" | grep -c '^---$' || true)
  assert_eq "exactly one --- (the user's)" "1" "$count"
)

# ── Test: prose-only entry (no bullets) ──
#
# A weird case but worth pinning: a release whose body is pure prose
# with no `### ` group heading still extracts cleanly.

echo ""
echo "Prose-only entry:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  cat > "$tmp/changelog.mdx" <<'EOF'
## v3.0.0 - 2026-05-02

Just a prose paragraph.

---
## v2.0.0 - 2026-05-01

---
EOF

  out=$(run_extract "$tmp/changelog.mdx")
  assert_eq "exact body" "Just a prose paragraph." "$out"
)

# ── Test: only one entry ever (first release) ──
#
# No second `## v` heading exists. The helper must still return the
# body of the only entry, stripping the trailing `---` if present.

echo ""
echo "First-ever release:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  cat > "$tmp/changelog.mdx" <<'EOF'
---
title: Changelog
---

## v0.1.0 - 2026-01-01

Initial release.

### Features
- everything ([#1](https://example/1))

---
EOF

  out=$(run_extract "$tmp/changelog.mdx")
  expected=$'Initial release.\n\n### Features\n- everything ([#1](https://example/1))'
  assert_eq "exact body" "$expected" "$out"
)

# ── Test: empty/missing input ──
#
# A degenerate changelog (no `## v` entries at all) returns empty
# output without erroring. Consumers fall back to their own defaults
# (notify-discord posts the changelog link only).

echo ""
echo "Degenerate input:"
(
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  cat > "$tmp/changelog.mdx" <<'EOF'
---
title: Changelog
---

No entries yet.
EOF

  out=$(run_extract "$tmp/changelog.mdx")
  assert_eq "empty body" "" "$out"
)

# ── Summary ──

read -r pass fail < "$SCOREBOARD"
echo ""
echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]
