#!/usr/bin/env bash
# Tests for scripts/version.sh
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$ROOT/scripts/version.sh"
CHANGESETS_DIR="$ROOT/.changesets"
CHANGELOG="$ROOT/apps/website/src/content/docs/changelog.mdx"

pass=0
fail=0

# Save original state
changelog_backup=$(cat "$CHANGELOG")
cleanup() {
  rm -f "$CHANGESETS_DIR"/test-*.md "$ROOT/RELEASE_NOTES.md"
  echo "$changelog_backup" > "$CHANGELOG"
}
trap cleanup EXIT

assert_eq() {
  local label="$1" expected="$2" actual="$3"
  if [[ "$expected" == "$actual" ]]; then
    echo "  ✓ $label"
    ((pass++))
  else
    echo "  ✗ $label"
    echo "    expected: $expected"
    echo "    actual:   $actual"
    ((fail++))
  fi
}

assert_contains() {
  local label="$1" needle="$2" haystack="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    echo "  ✓ $label"
    ((pass++))
  else
    echo "  ✗ $label"
    echo "    expected to contain: $needle"
    echo "    got: $haystack"
    ((fail++))
  fi
}

assert_exit() {
  local label="$1" expected="$2" actual="$3"
  if [[ "$expected" -eq "$actual" ]]; then
    echo "  ✓ $label"
    ((pass++))
  else
    echo "  ✗ $label (exit $actual, expected $expected)"
    ((fail++))
  fi
}

# ── Tests ──

echo "version.sh tests"
echo ""

# Test: no changesets
echo "no changesets:"
rm -f "$CHANGESETS_DIR"/test-*.md
output=$(bash "$SCRIPT" --dry-run 2>&1) || true
assert_contains "reports no changesets" "No changesets found" "$output"

# Test: patch bump
echo "patch bump:"
cat > "$CHANGESETS_DIR/test-a.md" << 'EOF'
---
bump: patch
---

- **Fixed a bug.** Description.
EOF
output=$(bash "$SCRIPT" --dry-run 2>&1)
assert_contains "version is patch bump" "v0.5.2 (patch)" "$output"
assert_contains "includes entry" "Fixed a bug" "$output"
rm "$CHANGESETS_DIR/test-a.md"

# Test: minor wins over patch
echo "minor wins over patch:"
cat > "$CHANGESETS_DIR/test-a.md" << 'EOF'
---
bump: patch
---

- **Bug fix.** Small thing.
EOF
cat > "$CHANGESETS_DIR/test-b.md" << 'EOF'
---
bump: minor
---

- **New feature.** Cool thing.
EOF
output=$(bash "$SCRIPT" --dry-run 2>&1)
assert_contains "version is minor bump" "v0.6.0 (minor)" "$output"
assert_contains "includes both entries" "Bug fix" "$output"
assert_contains "includes both entries" "New feature" "$output"
rm "$CHANGESETS_DIR"/test-*.md

# Test: major wins over all
echo "major wins over all:"
cat > "$CHANGESETS_DIR/test-a.md" << 'EOF'
---
bump: minor
---

- **Feature.** Thing.
EOF
cat > "$CHANGESETS_DIR/test-b.md" << 'EOF'
---
bump: major
---

- **Breaking change.** Big thing.
EOF
output=$(bash "$SCRIPT" --dry-run 2>&1)
assert_contains "version is major bump" "v1.0.0 (major)" "$output"
rm "$CHANGESETS_DIR"/test-*.md

# Test: missing bump field errors
echo "validation errors:"
cat > "$CHANGESETS_DIR/test-a.md" << 'EOF'
---
---

- **No bump.** Missing field.
EOF
output=$(bash "$SCRIPT" --dry-run 2>&1) && exit_code=0 || exit_code=$?
assert_exit "exits non-zero" 1 "$exit_code"
assert_contains "reports missing bump" "missing 'bump' field" "$output"
rm "$CHANGESETS_DIR"/test-*.md

# Test: empty body errors
echo "empty body:"
cat > "$CHANGESETS_DIR/test-a.md" << 'EOF'
---
bump: patch
---
EOF
output=$(bash "$SCRIPT" --dry-run 2>&1) && exit_code=0 || exit_code=$?
assert_exit "exits non-zero" 1 "$exit_code"
assert_contains "reports empty body" "empty body" "$output"
rm "$CHANGESETS_DIR"/test-*.md

# Test: actual consume (non-dry-run)
echo "consume:"
cat > "$CHANGESETS_DIR/test-a.md" << 'EOF'
---
bump: patch
---

- **Consumed entry.** Should appear in changelog.
EOF
output=$(bash "$SCRIPT" 2>&1)
assert_contains "reports update" "Updated" "$output"
assert_contains "reports deletion" "Deleted 1" "$output"
# Verify changelog was updated
assert_contains "changelog has new version" "## v0.5.2" "$(cat "$CHANGELOG")"
assert_contains "changelog has entry" "Consumed entry" "$(cat "$CHANGELOG")"
# Verify changeset was deleted
[[ ! -f "$CHANGESETS_DIR/test-a.md" ]]
assert_exit "changeset file deleted" 0 $?
# Verify RELEASE_NOTES.md was written
assert_contains "release notes written" "Consumed entry" "$(cat "$ROOT/RELEASE_NOTES.md")"
# Restore for subsequent tests
echo "$changelog_backup" > "$CHANGELOG"
rm -f "$ROOT/RELEASE_NOTES.md"

# ── Summary ──

echo ""
echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]
