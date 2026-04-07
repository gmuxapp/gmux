#!/usr/bin/env bash
# Scan merged PRs since the last tag, generate release notes, open a PR.
#
# PR titles must follow conventional commits:
#   feat: ...     → minor bump
#   fix: ...      → patch bump
#   feat!: ...    → major bump (breaking)
#   fix!: ...     → major bump (breaking)
#
# Other prefixes (ci:, docs:, refactor:, etc.) are not releasable.
#
# Usage:
#   .github/workflows/scripts/version.sh           # apply changes
#   .github/workflows/scripts/version.sh --dry-run # print version and entries, change nothing
set -euo pipefail
trap 'echo "error: ${BASH_SOURCE}:${LINENO}: ${BASH_COMMAND}" >&2' ERR

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
CHANGELOG="$ROOT/apps/website/src/content/docs/changelog.mdx"

DRY_RUN=false
if [[ "${1:-}" == "--dry-run" ]]; then
  DRY_RUN=true
fi

# ── Skip release commits ──

# Skip if HEAD is a release commit (squash merge: "release: vX.Y.Z",
# merge commit: "Merge pull request #N from .../release/next").
head_msg=$(git log -1 --format='%s')
if [[ "$head_msg" =~ ^release:\ v[0-9] ]] || [[ "$head_msg" =~ release/next ]]; then
  echo "Release commit, skipping." >&2
  exit 0
fi

# ── Find PRs since last tag ──

last_tag=$(git tag -l 'v*' | sort -V | tail -1)

if [[ -z "$last_tag" ]]; then
  # No tags yet; scan entire history.
  git_range="HEAD"
  last_tag="v0.0.0"
else
  git_range="${last_tag}..HEAD"
fi

pr_nums=()
while IFS= read -r line; do
  if [[ "$line" =~ \(#([0-9]+)\) ]]; then
    pr_nums+=("${BASH_REMATCH[1]}")
  elif [[ "$line" =~ ^Merge\ pull\ request\ #([0-9]+) ]]; then
    pr_nums+=("${BASH_REMATCH[1]}")
  fi
done < <(git log "${git_range}" --format='%s' --first-parent)

# Deduplicate and sort.
if [[ ${#pr_nums[@]} -gt 0 ]]; then
  mapfile -t pr_nums < <(printf '%s\n' "${pr_nums[@]}" | sort -un)
fi

# ── Classify PRs by conventional commit prefix ──

remote_url=$(git remote get-url origin 2>/dev/null || true)
repo_url=$(echo "$remote_url" | sed -E 's|\.git$||; s|^git@github\.com:|https://github.com/|')

bump="none"
declare -A pr_bumps
declare -A pr_titles
declare -A pr_bodies
breaking_items=()
feature_items=()
fix_items=()

cc_re='^(feat|fix)(\([^)]+\))?(!)?: .+$'

for pr_num in "${pr_nums[@]}"; do
  pr_json=$(gh pr view "$pr_num" --json title,body 2>/dev/null || echo '{}')
  title=$(echo "$pr_json" | jq -r '.title // ""')
  body=$(echo "$pr_json" | jq -r '.body // ""')

  # Parse conventional commit prefix: type(scope)!: description
  if [[ "$title" =~ $cc_re ]]; then
    type="${BASH_REMATCH[1]}"
    breaking="${BASH_REMATCH[3]}"
  else
    # Not a releasable PR (ci:, docs:, refactor:, etc.)
    continue
  fi

  # Determine bump level.
  if [[ -n "$breaking" ]]; then
    bump_level="major"
  elif [[ "$type" == "feat" ]]; then
    bump_level="minor"
  else
    bump_level="patch"
  fi

  pr_bumps[$pr_num]="$bump_level"
  pr_titles[$pr_num]="$title"
  pr_bodies[$pr_num]="$body"

  # Track overall bump (highest wins).
  case "$bump_level" in
    major) bump="major" ;;
    minor) [[ "$bump" != "major" ]] && bump="minor" ;;
    patch) [[ "$bump" == "none" ]] && bump="patch" ;;
  esac

  # Build PR list item (strip conventional commit prefix for readability).
  description="${title#*: }"
  item="- ${description} ([#${pr_num}](${repo_url}/pull/${pr_num}))"
  case "$bump_level" in
    major) breaking_items+=("$item") ;;
    minor) feature_items+=("$item") ;;
    patch) fix_items+=("$item") ;;
  esac
done

if [[ "$bump" == "none" ]]; then
  echo "No releasable PRs since ${last_tag}." >&2
  exit 0
fi

# ── Compute new version ──

current="${last_tag#v}"
IFS='.' read -r major minor patch_v <<< "$current"
case "$bump" in
  major) major=$((major + 1)); minor=0; patch_v=0 ;;
  minor) minor=$((minor + 1)); patch_v=0 ;;
  patch) patch_v=$((patch_v + 1)) ;;
esac
new_version="$major.$minor.$patch_v"

# ── Build LLM input ──

llm_input=""
for pr_num in $(echo "${!pr_bumps[@]}" | tr ' ' '\n' | sort -n); do
  llm_input+="## ${pr_titles[$pr_num]} (#${pr_num})"$'\n\n'
  if [[ -n "${pr_bodies[$pr_num]}" ]]; then
    llm_input+="${pr_bodies[$pr_num]}"$'\n\n'
  fi
done

# ── Summarize ──

summary=$(echo "$llm_input" | "$(dirname "$0")/summarize.sh" "v$new_version")

# ── Build grouped PR list ──

pr_list=""
if [[ ${#breaking_items[@]} -gt 0 ]]; then
  pr_list+="### Breaking"$'\n'
  for item in "${breaking_items[@]}"; do pr_list+="$item"$'\n'; done
  pr_list+=$'\n'
fi
if [[ ${#feature_items[@]} -gt 0 ]]; then
  pr_list+="### Features"$'\n'
  for item in "${feature_items[@]}"; do pr_list+="$item"$'\n'; done
  pr_list+=$'\n'
fi
if [[ ${#fix_items[@]} -gt 0 ]]; then
  pr_list+="### Fixes"$'\n'
  for item in "${fix_items[@]}"; do pr_list+="$item"$'\n'; done
  pr_list+=$'\n'
fi
pr_list=$(echo "$pr_list" | sed -e :a -e '/^\n*$/{$d;N;ba}')

# ── Output ──

echo "v$new_version ($bump)"

if $DRY_RUN; then
  echo ""
  echo "$summary"
  echo ""
  echo "---"
  echo ""
  echo "$pr_list"
  exit 0
fi

# ── Write release notes file ──
#
# GoReleaser uses this via --release-notes to populate the GitHub Release body.
# The --- separator lets notify-discord.sh extract just the summary.

cat > "$ROOT/RELEASE_NOTES.md" <<EOF
${summary}

---

${pr_list}
EOF

# ── Update changelog.mdx ──
#
# Insert new version section before the first "## v" heading.
# If no such heading exists, append to the end.

new_section="## v${new_version}

${summary}

${pr_list}

---"

if grep -q '^## v[0-9]' "$CHANGELOG"; then
  awk -v section="$new_section" '
    !inserted && /^## v[0-9]/ {
      printf "%s\n", section
      inserted = 1
    }
    { print }
  ' "$CHANGELOG" > "$CHANGELOG.tmp"
else
  cp "$CHANGELOG" "$CHANGELOG.tmp"
  printf '\n%s\n' "$new_section" >> "$CHANGELOG.tmp"
fi
mv "$CHANGELOG.tmp" "$CHANGELOG"

echo "Updated $CHANGELOG"
