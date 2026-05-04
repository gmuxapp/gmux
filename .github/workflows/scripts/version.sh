#!/usr/bin/env bash
# Compute the next release version and render release-PR materials.
#
# The release PR body is the single source of truth for release prose.
# Humans edit the prose between the `<!-- prose-start -->` /
# `<!-- prose-end -->` markers in the PR body. The release workflow
# extracts that prose and passes it to this script via $PROSE; this
# script then bakes the prose + auto-generated bullets into
# `changelog.mdx` and emits a fresh PR body for the workflow to apply
# back to the PR.
#
# Commit type → section → version bump:
#
#   feat:                   Features   minor
#   perf:                   Features   patch
#   fix:                    Fixes      patch
#   security:               Security   patch
#   docs:                   Docs       no bump on its own
#   feat!: / fix!: /
#     security!: /
#     BREAKING CHANGE:      Breaking   major
#
# Refactor, chore, ci, test, style, build, and revert are skipped
# entirely. See cliff.toml for the full parser rules.
#
# Inputs (env):
#   PROSE         optional release prose (markdown). When empty, the
#                 PR body's prose section gets a hint comment and the
#                 changelog entry has no prose paragraph.
#   PR_BODY_OUT   path to write the composed PR body to. Defaults to
#                 `pr-body.md` at the repo root. Not committed; the
#                 caller passes it to `gh pr edit --body-file` or
#                 peter-evans `add-paths` it out of the commit.
#   GITHUB_TOKEN  used to enrich commits with PR links via the GitHub
#                 GraphQL API. Required in CI.
#
# Outputs:
#   stdout                 the next version tag (e.g. v1.2.0), or
#                          nothing if there are no releasable commits.
#                          On --dry-run, the version is followed by a
#                          preview of every artifact.
#   $CHANGELOG             new section inserted above the latest
#                          existing `## v…` heading.
#   .github/release-target one line: the next version tag. Gives the
#                          release commit a real diff to anchor on.
#   $PR_BODY_OUT           full PR body ready to paste into
#                          `release/next`.
#
# Usage:
#   .github/workflows/scripts/version.sh             # apply changes
#   .github/workflows/scripts/version.sh --dry-run   # preview only

set -euo pipefail
trap 'echo "error: ${BASH_SOURCE}:${LINENO}: ${BASH_COMMAND}" >&2' ERR

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
CHANGELOG="$ROOT/apps/website/src/content/docs/changelog.mdx"
RELEASE_TARGET="$ROOT/.github/release-target"
PR_BODY_OUT="${PR_BODY_OUT:-$ROOT/pr-body.md}"
PROSE="${PROSE:-}"

DRY_RUN=false
EXTRACT_PROSE=false
case "${1:-}" in
  --dry-run) DRY_RUN=true ;;
  --extract-prose) EXTRACT_PROSE=true ;;
  "") ;;
  *) echo "unknown flag: $1" >&2; exit 2 ;;
esac

# ── --extract-prose: parse a PR body from stdin, print the prose ──
#
# Reads a PR body on stdin, prints the content between the
# `<!-- prose-start -->` and `<!-- prose-end -->` markers, with HTML
# comments stripped and surrounding blank lines trimmed. Used by the
# release workflow to feed $PROSE back into a regen run.
#
# A naive `sed '/<!--/,/-->/d'` swallows everything after an inline
# `<!-- ... -->` because sed can't match start and end on the same
# line. The awk helper below tracks comment state correctly.

strip_html_comments() {
  awk '
    {
      line = $0
      out = ""
      while (length(line) > 0) {
        if (in_comment) {
          end = index(line, "-->")
          if (end == 0) { line = ""; break }
          line = substr(line, end + 3)
          in_comment = 0
        } else {
          start = index(line, "<!--")
          if (start == 0) { out = out line; line = ""; break }
          out = out substr(line, 1, start - 1)
          line = substr(line, start + 4)
          in_comment = 1
        }
      }
      if (!in_comment || length(out) > 0) print out
    }
  '
}

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

if $EXTRACT_PROSE; then
  awk '
    /<!-- prose-start -->/ { flag = 1; next }
    /<!-- prose-end -->/   { flag = 0 }
    flag
  ' | strip_html_comments | trim_blanks
  exit 0
fi

# ── Skip release-flow commits ──
#
# Prevents the workflow from re-triggering after merging a release PR
# or after a `chore(release):` follow-up. Release commits are produced
# by peter-evans/create-pull-request with the exact `release: vX.Y.Z`
# subject line (and a `(#N)` suffix once rebase-merged).

head_msg=$(git log -1 --format='%s')
if [[ "$head_msg" =~ ^release:\ v[0-9] ]]; then
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

context=$(git-cliff --unreleased --bump --context)
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

# ── Enrich commits with PR links ──
#
# GitHub does not append "(#N)" to commit subjects for rebase merges
# (only squash merges get that). We use the GitHub API to resolve each
# commit's PR number and inject markdown links into the raw_message
# before rendering.

enrich_context() {
  local ctx_file="$1"
  local out_file="$2"

  local have_gh=true
  if [[ -z "${GITHUB_TOKEN:-}" ]] || ! command -v gh >/dev/null 2>&1; then
    have_gh=false
  fi

  if ! $have_gh; then
    if [[ "${CI:-}" == "true" ]]; then
      echo "error: GITHUB_TOKEN missing or gh not installed in CI; cannot enrich PR links" >&2
      exit 1
    fi
    cp "$ctx_file" "$out_file"
    return
  fi

  local shas
  shas=$(jq -r '.[0].commits[].id' "$ctx_file")
  if [[ -z "$shas" ]]; then
    cp "$ctx_file" "$out_file"
    return
  fi

  local query='query { repository(owner: "gmuxapp", name: "gmux") {'
  while IFS= read -r sha; do
    [[ -z "$sha" ]] && continue
    query+=" c_${sha}: object(oid: \"${sha}\") { ... on Commit { associatedPullRequests(first: 1) { nodes { number url } } } }"
  done <<< "$shas"
  query+=' } }'

  local map_file
  map_file=$(mktemp)
  if ! gh api graphql -f query="$query" --jq '
    .data.repository | to_entries
    | map(select(.value != null and (.value.associatedPullRequests.nodes | length > 0)))
    | map({
        key: (.key | sub("^c_"; "")),
        value: .value.associatedPullRequests.nodes[0]
      })
    | from_entries
  ' > "$map_file"; then
    echo "error: GraphQL PR lookup failed" >&2
    rm -f "$map_file"
    exit 1
  fi

  jq --slurpfile prmap "$map_file" '
    .[0].commits = [.[0].commits[] |
      . as $c |
      ($prmap[0][$c.id] // null) as $pr |
      ($c.raw_message | split("\n")[0]) as $subject |
      if $pr != null and ($subject | test("\\(#[0-9]+\\)|\\(\\[#[0-9]+\\]") | not) then
        (.raw_message | split("\n")) as $lines |
        .raw_message = ([$lines[0] + " ([#\($pr.number)](\($pr.url)))"] + $lines[1:] | join("\n"))
      else .
      end
    ]
  ' "$ctx_file" > "$out_file"

  rm -f "$map_file"
}

ctx_input=$(mktemp)
ctx_enriched=$(mktemp)
echo "$context" > "$ctx_input"
enrich_context "$ctx_input" "$ctx_enriched"
section=$(git-cliff --from-context "$ctx_enriched")
rm -f "$ctx_input" "$ctx_enriched"

# ── Split git-cliff output into heading and bullets ──
#
# git-cliff's body template emits:
#
#   ## vX.Y.Z - 2026-04-10
#
#   ### Features
#   - ...
#
#   ---
#
# Heading (line 1) goes verbatim into changelog.mdx. The bullets
# (everything between the heading and the `---`) are reused in both
# the changelog entry and the PR body.

heading=$(echo "$section" | sed -n '/^## v/{p;q;}')
bullets=$(echo "$section" | awk '
  /^## v/ { in_section = 1; next }
  /^---$/ { in_section = 0 }
  in_section { print }
' | trim_blanks)

prose_trimmed=$(printf '%s' "$PROSE" | trim_blanks)

# ── Compose changelog.mdx entry ──
#
#   ## vX.Y.Z - 2026-04-10
#
#   <prose>
#
#   ### Features
#   - ...
#
#   ---

changelog_entry="${heading}"$'\n'
if [[ -n "$prose_trimmed" ]]; then
  changelog_entry+=$'\n'"${prose_trimmed}"$'\n'
fi
changelog_entry+=$'\n'"${bullets}"$'\n\n'"---"

# ── Compose release PR body ──
#
# The body is the human-editable surface. Reviewers edit prose between
# the `<!-- prose-start -->` markers; everything else is regenerated by
# the workflow on every push to main and every PR-body edit.
#
# When prose is empty, the marker block holds only a hint comment, so
# editors discover the right place to type without seeing placeholder
# prose leak into the changelog.

prose_block=""
if [[ -n "$prose_trimmed" ]]; then
  prose_block="${prose_trimmed}"
else
  prose_block="<!-- Optional: add release prose here. Headings, paragraphs, lists. Leave empty for patch-only releases. -->"
fi

pr_body=$(cat <<EOF
Release **${new_version}**.

Merging creates the \`${new_version}\` tag and triggers the release build.

<!-- prose-start -->
${prose_block}
<!-- prose-end -->

<!-- bullets-start: do not edit, regenerated from commits -->
${bullets}
<!-- bullets-end -->
EOF
)

# ── Output ──

echo "$new_version"

if $DRY_RUN; then
  echo ""
  echo "── changelog.mdx entry ──"
  echo "$changelog_entry"
  echo ""
  echo "── PR body ──"
  echo "$pr_body"
  exit 0
fi

# Write the next-version marker. Touching this file gives the release
# commit a non-empty diff so peter-evans can attach it to a PR.
echo "$new_version" > "$RELEASE_TARGET"

# Write PR body for the caller to apply. Not tracked in git.
printf '%s\n' "$pr_body" > "$PR_BODY_OUT"

# Insert the new section above the most recent existing entry.
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

echo "Updated $CHANGELOG" >&2
echo "Updated $RELEASE_TARGET" >&2
echo "Wrote $PR_BODY_OUT" >&2
