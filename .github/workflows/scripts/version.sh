#!/usr/bin/env bash
# Scan unreleased commits via git-cliff, generate release notes,
# and open a release PR.
#
# Commits must follow conventional commits:
#   feat: ...              → minor bump
#   fix: ...               → patch bump
#   feat!: / fix!: / ...   → major bump (breaking)
#   BREAKING CHANGE: footer → major bump
#
# Other types appear in the changelog where applicable (docs, perf)
# or are skipped entirely (refactor, chore, ci, test, style, build).
#
# Optional prose for the release goes in RELEASE_HIGHLIGHTS.md at the
# repo root. Its contents are injected into the changelog section
# between the version heading and the grouped bullet lists. The file
# is cleared automatically after each release.
#
# Usage:
#   .github/workflows/scripts/version.sh           # apply changes
#   .github/workflows/scripts/version.sh --dry-run # print version and entries, change nothing
set -euo pipefail
trap 'echo "error: ${BASH_SOURCE}:${LINENO}: ${BASH_COMMAND}" >&2' ERR

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
CHANGELOG="$ROOT/apps/website/src/content/docs/changelog.mdx"
HIGHLIGHTS="$ROOT/RELEASE_HIGHLIGHTS.md"

DRY_RUN=false
if [[ "${1:-}" == "--dry-run" ]]; then
  DRY_RUN=true
fi

# ── Skip release commits ──
#
# Prevents the workflow from re-triggering after merging a release PR.

head_msg=$(git log -1 --format='%s')
if [[ "$head_msg" =~ ^release:\ v[0-9] ]] || [[ "$head_msg" =~ release/next ]]; then
  echo "Release commit, skipping." >&2
  exit 0
fi

# ── Require git-cliff ──

if ! command -v git-cliff >/dev/null 2>&1; then
  echo "error: git-cliff is not installed. Install it with: cargo install git-cliff" >&2
  exit 1
fi

# ── Check for releasable commits ──
#
# git-cliff's --bump defaults to a patch bump whenever any tracked
# commit exists (including docs-only releases). We only release when
# there's at least one commit in a Features, Fixes, Breaking, or
# Security group.

context=$(git-cliff --unreleased --bump --context 2>/dev/null || echo '[]')
releasable_count=$(echo "$context" | jq '
  if length == 0 then 0
  else [.[0].commits[] | select(.group | test("Features|Fixes|Breaking|Security"))] | length
  end
')

last_tag=$(git tag -l 'v*' | sort -V | tail -1)
last_tag="${last_tag:-v0.0.0}"

if [[ "${releasable_count:-0}" -eq 0 ]]; then
  echo "No releasable commits since ${last_tag}." >&2
  exit 0
fi

new_version=$(echo "$context" | jq -r '.[0].version')

# ── Generate changelog section ──
#
# Produces:
#   ## vX.Y.Z
#
#   ### Features
#   - foo ([#N](https://github.com/gmuxapp/gmux/pull/N))
#
#   ### Fixes
#   - bar ([#N](https://github.com/gmuxapp/gmux/pull/N))
#
#   ---

section=$(git-cliff --unreleased --bump)

# ── Helper: strip leading and trailing blank lines ──

trim_blanks() {
  awk '
    { lines[NR] = $0 }
    END {
      first = 1
      while (first <= NR && lines[first] ~ /^[[:space:]]*$/) first++
      last = NR
      while (last >= first && lines[last] ~ /^[[:space:]]*$/) last--
      for (i = first; i <= last; i++) print lines[i]
    }
  '
}

# ── Read highlights (optional prose for the release) ──

highlights=""
if [[ -s "$HIGHLIGHTS" ]]; then
  # Drop HTML comment blocks, then strip surrounding whitespace.
  highlights=$(sed -e '/<!--/,/-->/d' "$HIGHLIGHTS" | trim_blanks)
fi

# ── Compose RELEASE_NOTES.md ──
#
# Format (backward compatible with notify-discord.sh):
#
#   <highlights prose>
#
#   ---
#
#   ### Features
#   - ...
#
#   ### Fixes
#   - ...
#
# notify-discord.sh extracts everything before the first `---` as the
# Discord summary, so highlights (if any) become the notification body.

# Reuse git-cliff's heading line verbatim so we pick up the date suffix.
heading=$(echo "$section" | sed -n '/^## v/{p;q;}')
bullets=$(echo "$section" | awk '
  /^## v/ { in_section = 1; next }
  /^---$/ { in_section = 0 }
  in_section { print }
' | trim_blanks)

release_notes=""
if [[ -n "$highlights" ]]; then
  release_notes+="${highlights}"$'\n\n'
fi
release_notes+="---"$'\n\n'"${bullets}"

# ── Compose changelog.mdx entry ──
#
# Format (matches the historical layout):
#
#   ## vX.Y.Z - 2026-04-10
#
#   <highlights prose>
#
#   ### Features
#   - ...
#
#   ---

changelog_entry="${heading}"$'\n'
if [[ -n "$highlights" ]]; then
  changelog_entry+=$'\n'"${highlights}"$'\n'
fi
changelog_entry+=$'\n'"${bullets}"$'\n\n'"---"

# ── Output ──

echo "$new_version"

if $DRY_RUN; then
  echo ""
  echo "── changelog.mdx entry ──"
  echo "$changelog_entry"
  echo ""
  echo "── RELEASE_NOTES.md ──"
  echo "$release_notes"
  exit 0
fi

# ── Write RELEASE_NOTES.md ──

printf '%s\n' "$release_notes" > "$ROOT/RELEASE_NOTES.md"

# ── Update changelog.mdx ──
#
# Insert new version section before the first "## v" heading.
# If no such heading exists, append to the end.

if grep -q '^## v[0-9]' "$CHANGELOG"; then
  awk -v entry="$changelog_entry" '
    !inserted && /^## v[0-9]/ {
      printf "%s\n", entry
      inserted = 1
    }
    { print }
  ' "$CHANGELOG" > "$CHANGELOG.tmp"
else
  cp "$CHANGELOG" "$CHANGELOG.tmp"
  printf '\n%s\n' "$changelog_entry" >> "$CHANGELOG.tmp"
fi
mv "$CHANGELOG.tmp" "$CHANGELOG"

# ── Clear RELEASE_HIGHLIGHTS.md ──
#
# The release PR includes the cleared state so the next cycle starts
# fresh. The stub content explains the workflow to future editors.

cat > "$HIGHLIGHTS" <<'STUB'
<!--
Optional prose for the next release. Edit this file to add a high-level
summary, topic paragraphs, or migration notes. When version.sh runs, the
content is injected into the changelog section between the version
heading and the grouped bullet lists.

The file is cleared automatically after each release. Leave empty for
releases that don't need prose (the auto-generated bullet list is
enough for patch-only releases).

Format: plain markdown. Supports headings, paragraphs, and lists.
Example:

    Peers now automatically reconnect after system suspend, no restart
    needed.

    ### Project matching
    `projects.json` replaces separate `remote` and `paths` fields with a
    unified `match` array.
-->
STUB

echo "Updated $CHANGELOG" >&2
echo "Updated $HIGHLIGHTS (cleared)" >&2
