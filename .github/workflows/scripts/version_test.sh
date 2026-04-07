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

# classify_title outputs: "<bump> <summary>" where bump is major/minor/patch/none
# and summary is "include" or "skip".
classify_title() {
  local title="$1"
  local cc_re='^([a-z]+)(\([^)]+\))?(!)?: .+$'
  local bump_re='^(feat|fix)$'
  local summary_re='^(feat|fix|docs|perf)$'
  if [[ ! "$title" =~ $cc_re ]]; then
    echo "none skip"
    return
  fi
  local type="${BASH_REMATCH[1]}"
  local breaking="${BASH_REMATCH[3]}"
  local bump="none" summary="skip"

  if [[ -n "$breaking" ]]; then
    bump="major"
    summary="include"
  elif [[ "$type" =~ $bump_re ]]; then
    case "$type" in
      feat) bump="minor" ;;
      fix)  bump="patch" ;;
    esac
    summary="include"
  elif [[ "$type" =~ $summary_re ]]; then
    summary="include"
  fi
  echo "$bump $summary"
}

# ── PR title classification ──

echo "Version bumps:"

assert_eq "feat: minor"            "minor include"  "$(classify_title 'feat: add new feature')"
assert_eq "fix: patch"             "patch include"  "$(classify_title 'fix: resolve crash')"
assert_eq "feat!: major"           "major include"  "$(classify_title 'feat!: remove old API')"
assert_eq "fix!: major"            "major include"  "$(classify_title 'fix!: change error format')"
assert_eq "feat(web): minor"       "minor include"  "$(classify_title 'feat(web): add dark mode')"
assert_eq "fix(core): patch"       "patch include"  "$(classify_title 'fix(core): memory leak')"
assert_eq "feat(web)!: major"      "major include"  "$(classify_title 'feat(web)!: redesign settings')"
assert_eq "fix(core)!: major"      "major include"  "$(classify_title 'fix(core)!: change error format')"
assert_eq "empty after colon"      "none skip"      "$(classify_title 'feat:')"
assert_eq "only space after colon" "none skip"      "$(classify_title 'feat: ')"
assert_eq "no prefix"              "none skip"      "$(classify_title 'update the thing')"
assert_eq "release: no bump"       "none skip"      "$(classify_title 'release: v1.0.0')"

echo ""
echo "Summary inclusion:"

assert_eq "docs: include"          "none include"   "$(classify_title 'docs: update readme')"
assert_eq "perf: include"          "none include"   "$(classify_title 'perf: optimize query')"
assert_eq "ci: skip"               "none skip"      "$(classify_title 'ci: fix workflow')"
assert_eq "chore: skip"            "none skip"      "$(classify_title 'chore: update deps')"
assert_eq "refactor: skip"         "none skip"      "$(classify_title 'refactor: extract module')"
assert_eq "test: skip"             "none skip"      "$(classify_title 'test: add unit tests')"
assert_eq "build: skip"            "none skip"      "$(classify_title 'build: update makefile')"
assert_eq "style: skip"            "none skip"      "$(classify_title 'style: format code')"
assert_eq "docs!: major + include" "major include"  "$(classify_title 'docs!: remove API docs')"
assert_eq "ci!: major + include"   "major include"  "$(classify_title 'ci!: drop node 18 support')"

# ── Strip prefix for changelog ──

echo ""
echo "Prefix stripping:"

strip_prefix() { echo "${1#*: }"; }

assert_eq "feat: simple"           "add dark mode"          "$(strip_prefix 'feat: add dark mode')"
assert_eq "fix(scope): scoped"     "handle nil pointer"     "$(strip_prefix 'fix(core): handle nil pointer')"
assert_eq "feat!: breaking"        "redesign API"           "$(strip_prefix 'feat!: redesign API')"
assert_eq "fix(web)!: scoped bang" "drop legacy endpoint"   "$(strip_prefix 'fix(web)!: drop legacy endpoint')"
assert_eq "colon in description"   "handle key: value pairs" "$(strip_prefix 'fix: handle key: value pairs')"

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

# ── Version computation ──

echo ""
echo "Version computation:"

compute_version() {
  local current="$1" bump="$2"
  local major minor patch_v
  IFS='.' read -r major minor patch_v <<< "$current"
  case "$bump" in
    major) major=$((major + 1)); minor=0; patch_v=0 ;;
    minor) minor=$((minor + 1)); patch_v=0 ;;
    patch) patch_v=$((patch_v + 1)) ;;
  esac
  echo "$major.$minor.$patch_v"
}

assert_eq "patch bump"            "1.0.1" "$(compute_version '1.0.0' patch)"
assert_eq "minor bump"            "1.1.0" "$(compute_version '1.0.0' minor)"
assert_eq "major bump"            "2.0.0" "$(compute_version '1.0.0' major)"
assert_eq "minor resets patch"    "1.3.0" "$(compute_version '1.2.5' minor)"
assert_eq "major resets all"      "2.0.0" "$(compute_version '1.2.5' major)"
assert_eq "from zero"             "0.0.1" "$(compute_version '0.0.0' patch)"
assert_eq "first minor"           "0.1.0" "$(compute_version '0.0.0' minor)"
assert_eq "first major"           "1.0.0" "$(compute_version '0.0.0' major)"

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
