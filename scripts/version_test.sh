#!/usr/bin/env bash
# Tests for conventional commit parsing logic in version.sh.
#
# These test the title-matching regex and bump classification without
# requiring GitHub API access or a specific git history.
set -uo pipefail

pass=0
fail=0

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

# ── Parse a PR title and echo "type bump" or "skip" ──

parse_title() {
  local title="$1"
  local cc_re='^(feat|fix)(\([^)]+\))?(!)?: .+$'
  if [[ "$title" =~ $cc_re ]]; then
    local type="${BASH_REMATCH[1]}"
    local breaking="${BASH_REMATCH[3]}"
    if [[ -n "$breaking" ]]; then
      echo "$type major"
    elif [[ "$type" == "feat" ]]; then
      echo "$type minor"
    else
      echo "$type patch"
    fi
  else
    echo "skip"
  fi
}

# ── PR title classification ──

echo "PR title parsing:"

assert_eq "feat: minor"            "feat minor"  "$(parse_title 'feat: add new feature')"
assert_eq "fix: patch"             "fix patch"   "$(parse_title 'fix: resolve crash')"
assert_eq "feat!: major"           "feat major"  "$(parse_title 'feat!: remove old API')"
assert_eq "fix!: major"            "fix major"   "$(parse_title 'fix!: change error format')"
assert_eq "feat(web): minor"       "feat minor"  "$(parse_title 'feat(web): add dark mode')"
assert_eq "fix(core): patch"       "fix patch"   "$(parse_title 'fix(core): memory leak')"
assert_eq "feat(web)!: major"      "feat major"  "$(parse_title 'feat(web)!: redesign settings')"
assert_eq "docs: skip"             "skip"        "$(parse_title 'docs: update readme')"
assert_eq "ci: skip"               "skip"        "$(parse_title 'ci: fix workflow')"
assert_eq "refactor: skip"         "skip"        "$(parse_title 'refactor: extract module')"
assert_eq "chore: skip"            "skip"        "$(parse_title 'chore: update deps')"
assert_eq "no prefix: skip"        "skip"        "$(parse_title 'update the thing')"
assert_eq "release: skip"          "skip"        "$(parse_title 'release: v1.0.0')"

# ── Bump level precedence ──

echo ""
echo "Bump precedence:"

compute_bump() {
  local bump="none"
  for level in "$@"; do
    case "$level" in
      major) bump="major" ;;
      minor) [[ "$bump" != "major" ]] && bump="minor" ;;
      patch) [[ "$bump" == "none" ]] && bump="patch" ;;
    esac
  done
  echo "$bump"
}

assert_eq "patch only"             "patch" "$(compute_bump patch)"
assert_eq "minor only"             "minor" "$(compute_bump minor)"
assert_eq "major only"             "major" "$(compute_bump major)"
assert_eq "patch + minor = minor"  "minor" "$(compute_bump patch minor)"
assert_eq "minor + major = major"  "major" "$(compute_bump minor major)"
assert_eq "patch + major = major"  "major" "$(compute_bump patch major)"
assert_eq "major first still major" "major" "$(compute_bump major patch minor)"
assert_eq "none"                   "none"  "$(compute_bump)"

# ── Release commit detection ──

echo ""
echo "Release commit detection:"

is_release_commit() {
  local msg="$1"
  if [[ "$msg" =~ ^release:\ v[0-9] ]] || [[ "$msg" =~ release/next ]]; then
    echo "yes"
  else
    echo "no"
  fi
}

assert_eq "squash merge"     "yes" "$(is_release_commit 'release: v1.0.0')"
assert_eq "merge commit"     "yes" "$(is_release_commit 'Merge pull request #42 from gmuxapp/release/next')"
assert_eq "normal feat"      "no"  "$(is_release_commit 'feat: add feature')"
assert_eq "normal fix"       "no"  "$(is_release_commit 'fix: resolve bug')"
assert_eq "contains release" "no"  "$(is_release_commit 'docs: update release guide')"

# ── PR number extraction ──

echo ""
echo "PR number extraction:"

extract_pr() {
  local line="$1"
  if [[ "$line" =~ \(#([0-9]+)\) ]]; then
    echo "${BASH_REMATCH[1]}"
  elif [[ "$line" =~ ^Merge\ pull\ request\ #([0-9]+) ]]; then
    echo "${BASH_REMATCH[1]}"
  else
    echo "none"
  fi
}

assert_eq "squash merge"    "42"   "$(extract_pr 'feat: add feature (#42)')"
assert_eq "merge commit"    "42"   "$(extract_pr 'Merge pull request #42 from user/branch')"
assert_eq "no PR number"    "none" "$(extract_pr 'direct commit to main')"
assert_eq "number in parens" "123" "$(extract_pr 'fix: thing (#123)')"

# ── Summary ──

echo ""
echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]
