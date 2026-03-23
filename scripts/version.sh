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

# ── Collect changelog entries ──

# Extract body (everything after the second ---) from each changeset.
entries=""
for f in "${changesets[@]}"; do
  body=$(awk '/^---$/{n++; next} n>=2{print}' "$f")
  body=$(echo "$body" | sed '/./,$!d' | sed -e :a -e '/^\n*$/{$d;N;ba}')
  entries+="$body"$'\n\n'
done
# Remove trailing newlines
entries=$(echo "$entries" | sed -e :a -e '/^\n*$/{$d;N;ba}')

# ── Output ──

echo "v$new_version ($bump)"

if $DRY_RUN; then
  echo ""
  echo "$entries"
  exit 0
fi

# ── Write release notes file ──
#
# GoReleaser uses this via --release-notes to populate the GitHub Release body.
# Kept at a fixed path so .goreleaser.yml doesn't need to change per release.

echo "$entries" > "$ROOT/RELEASE_NOTES.md"

# ── Update changelog.mdx ──
#
# Insert new version section before the first "## v" heading.
# If no such heading exists, append to the end.

new_section="## v${new_version}

${entries}

---
"

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
