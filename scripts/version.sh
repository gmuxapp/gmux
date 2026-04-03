#!/usr/bin/env bash
# Consume .changesets/*.md files, update changelog.mdx, print new version.
#
# This script is called by the version workflow. It can also be run
# locally to preview what the next release will look like.
#
# Usage:
#   scripts/version.sh           # apply changes
#   scripts/version.sh --dry-run # print version and entries, change nothing
set -euo pipefail
trap 'echo "error: ${BASH_SOURCE}:${LINENO}: ${BASH_COMMAND}" >&2' ERR

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHANGELOG="$ROOT/apps/website/src/content/docs/changelog.mdx"
CHANGESETS_DIR="$ROOT/.changesets"

DRY_RUN=false
if [[ "${1:-}" == "--dry-run" ]]; then
  DRY_RUN=true
fi

# ── Collect changeset files ──

shopt -s nullglob
files=("$CHANGESETS_DIR"/*.md)
shopt -u nullglob

# Filter out README.md
changesets=()
for f in "${files[@]}"; do
  [[ "$(basename "$f")" == "README.md" ]] && continue
  changesets+=("$f")
done

if [[ ${#changesets[@]} -eq 0 ]]; then
  echo "No changesets found." >&2
  exit 0
fi

# ── Validate and determine bump level (highest wins) ──

bump="patch"
errors=()
for f in "${changesets[@]}"; do
  name="$(basename "$f")"
  # Parse bump from YAML frontmatter (between --- lines)
  level=$(awk '/^---$/{n++; next} n==1 && /^bump:/{print $2}' "$f" | tr -d '[:space:]')
  case "$level" in
    major) bump="major" ;;
    minor) [[ "$bump" != "major" ]] && bump="minor" ;;
    patch) ;;
    "") errors+=("$name: missing 'bump' field in frontmatter") ;;
    *) errors+=("$name: invalid bump level '$level' (expected patch, minor, or major)") ;;
  esac

  # Validate that the body isn't empty
  body=$(awk '/^---$/{n++; next} n>=2{print}' "$f" | sed '/./,$!d' | sed -e :a -e '/^\n*$/{$d;N;ba}')
  if [[ -z "$body" ]]; then
    errors+=("$name: empty body (write the changelog entry after the --- frontmatter)")
  fi
done

if [[ ${#errors[@]} -gt 0 ]]; then
  echo "Changeset validation errors:" >&2
  for err in "${errors[@]}"; do
    echo "  - $err" >&2
  done
  exit 1
fi

# ── Compute new version ──

# Use the highest semver tag across all branches, not just ancestors of HEAD.
current=$(git tag -l 'v*' | sort -V | tail -1)
current="${current:-v0.0.0}"
current="${current#v}"

IFS='.' read -r major minor patch_v <<< "$current"
case "$bump" in
  major) major=$((major + 1)); minor=0; patch_v=0 ;;
  minor) minor=$((minor + 1)); patch_v=0 ;;
  patch) patch_v=$((patch_v + 1)) ;;
esac
new_version="$major.$minor.$patch_v"

# ── Map changesets to PRs ──

# Derive the GitHub repo URL for PR links.
remote_url=$(git remote get-url origin 2>/dev/null || true)
repo_url=$(echo "$remote_url" | sed -E 's|\.git$||; s|^git@github\.com:|https://github.com/|')

declare -A pr_bumps       # pr_num -> highest bump level (major > minor > patch)
declare -A pr_changelogs  # pr_num -> concatenated changeset bodies

for f in "${changesets[@]}"; do
  file_bump=$(awk '/^---$/{n++; next} n==1 && /^bump:/{print $2}' "$f" | tr -d '[:space:]')
  body=$(awk '/^---$/{n++; next} n>=2{print}' "$f")
  body=$(echo "$body" | sed '/./,$!d' | sed -e :a -e '/^\n*$/{$d;N;ba}')

  # Find the PR that introduced this changeset (from merge commit message).
  pr_num=""
  commit_msg=$(git log --diff-filter=A -1 --format='%s' -- "$f" 2>/dev/null || true)
  if [[ "$commit_msg" =~ \(#([0-9]+)\) ]]; then
    pr_num="${BASH_REMATCH[1]}"
  fi

  # Track highest bump per PR.
  if [[ -n "$pr_num" ]]; then
    existing="${pr_bumps[$pr_num]:-}"
    case "$file_bump" in
      major) pr_bumps[$pr_num]="major" ;;
      minor) [[ "$existing" != "major" ]] && pr_bumps[$pr_num]="minor" ;;
      patch) [[ -z "$existing" ]] && pr_bumps[$pr_num]="patch" ;;
    esac
    pr_changelogs[$pr_num]+="$body"$'\n'
  fi
done

# ── Fetch PR content and build list ──

declare -A pr_titles
llm_input=""
breaking_items=()
feature_items=()
fix_items=()

for pr_num in $(echo "${!pr_bumps[@]}" | tr ' ' '\n' | sort -n); do
  pr_json=$(gh pr view "$pr_num" --json title,body 2>/dev/null || echo '{}')
  pr_title=$(echo "$pr_json" | jq -r '.title // "#'"$pr_num"'"')
  pr_body=$(echo "$pr_json" | jq -r '.body // ""')
  pr_titles[$pr_num]="$pr_title"

  llm_input+="## ${pr_title} (#${pr_num})"$'\n\n'
  llm_input+="### Changelog note"$'\n'
  llm_input+="${pr_changelogs[$pr_num]:-}"$'\n\n'
  if [[ -n "$pr_body" ]]; then
    llm_input+="### PR description"$'\n'
    llm_input+="${pr_body}"$'\n\n'
  fi

  item="- ${pr_title} ([#${pr_num}](${repo_url}/pull/${pr_num}))"
  case "${pr_bumps[$pr_num]}" in
    major) breaking_items+=("$item") ;;
    minor) feature_items+=("$item") ;;
    patch) fix_items+=("$item") ;;
  esac
done

# ── Summarize ──

summary=$(echo "$llm_input" | "$ROOT/scripts/summarize.sh" "v$new_version")

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
# The --- separator lets scripts/notify-discord.sh extract just the summary.

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
  # Insert before the first version heading
  awk -v section="$new_section" '
    !inserted && /^## v[0-9]/ {
      printf "%s\n", section
      inserted = 1
    }
    { print }
  ' "$CHANGELOG" > "$CHANGELOG.tmp"
else
  # No version headings yet, append
  cp "$CHANGELOG" "$CHANGELOG.tmp"
  printf '\n%s\n' "$new_section" >> "$CHANGELOG.tmp"
fi
mv "$CHANGELOG.tmp" "$CHANGELOG"

# ── Delete consumed changesets ──

for f in "${changesets[@]}"; do
  rm "$f"
done

echo "Updated $CHANGELOG"
echo "Deleted ${#changesets[@]} changeset(s)"
