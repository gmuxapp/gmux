#!/usr/bin/env bash
# Send a Discord release notification via webhook.
#
# Expects these environment variables:
#   WEBHOOK_URL    - Discord webhook URL (skips silently if empty)
#   VERSION        - Release version tag (e.g. v0.9.0)
#   ROLE_BREAKING  - Discord role ID for breaking changes
#   ROLE_FEATURE   - Discord role ID for feature releases
#   ROLE_PATCH     - Discord role ID for patch releases
#   DRY_RUN        - If "1", print the JSON payload to stdout and skip
#                    the actual webhook call. Used for local debugging
#                    and tests.
set -euo pipefail
trap 'echo "error: ${BASH_SOURCE}:${LINENO}: ${BASH_COMMAND}" >&2' ERR

if [[ -z "${WEBHOOK_URL:-}" ]]; then
  echo "DISCORD_WEBHOOK_URL not set, skipping notification"
  exit 0
fi

: "${VERSION:?}"
: "${ROLE_BREAKING:?}"
: "${ROLE_FEATURE:?}"
: "${ROLE_PATCH:?}"

# ── Extract summary ──
#
# `extract-release-notes.sh` returns the body of the latest entry
# (no heading, no trailing `---`). We split it at the first `### `
# group heading to get prose vs bullets. Horizontal rules inside
# prose survive: the trailing `---` we already stripped is the
# per-entry separator, not a user-written rule.
#
# When prose is empty (a release without curated highlights), fall
# back to the bullet list so Discord still sees what changed. An
# empty announcement with only a changelog link is worse than a
# slightly verbose one: most subscribers just want to know if there's
# anything they care about without having to click through.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHANGELOG="${CHANGELOG:-apps/website/src/content/docs/changelog.mdx}"

trim_blanks() { sed -e '/./,$!d' -e :a -e '/^\n*$/{$d;N;ba}'; }

body=$(bash "$SCRIPT_DIR/extract-release-notes.sh" "$CHANGELOG")

prose=$(echo "$body" | awk '/^### / { exit } { print }' | trim_blanks)
bullets=$(echo "$body" | awk '/^### / { f = 1 } f { print }' | trim_blanks)

summary="$prose"
if [[ -z "${summary//[[:space:]]/}" ]]; then
  summary="$bullets"
fi

# ── Determine bump type ──

# Use git's own "nearest tag reachable from VERSION's parent" rather
# than scanning all tags. The old approach (`git tag -l 'v*' | sort -V
# | grep -B1 ...`) breaks when tags are pushed out of order or VERSION
# isn't yet in the local tag list; `git describe` always picks the
# actual predecessor on the release history.
prev_tag=$(git describe --tags --abbrev=0 "${VERSION}^" 2>/dev/null || true)
cur_major=$(echo "$VERSION" | sed 's/^v//' | cut -d. -f1)
cur_minor=$(echo "$VERSION" | sed 's/^v//' | cut -d. -f2)
prev_major=$(echo "$prev_tag" | sed 's/^v//' | cut -d. -f1)
prev_minor=$(echo "$prev_tag" | sed 's/^v//' | cut -d. -f2)

# Always ping every role at or below the bump severity:
#   patch    → patch
#   feature  → patch + feature
#   breaking → patch + feature + breaking
# This way each role's subscribers see every release that affects them.

role_ids=("$ROLE_PATCH")
if [[ -z "$prev_tag" ]]; then
  : # No predecessor (first-ever release): patch-only ping, same as the old behavior.
elif [[ "$cur_major" != "$prev_major" ]]; then
  role_ids+=("$ROLE_FEATURE" "$ROLE_BREAKING")
elif [[ "$cur_minor" != "$prev_minor" ]]; then
  role_ids+=("$ROLE_FEATURE")
fi

mentions=""
for id in "${role_ids[@]}"; do
  mentions+="<@&${id}> "
done
mentions="${mentions% }"

# ── Send ──

header="${mentions}
## gmux ${VERSION}"
footer="[See the changelog for details.](https://gmux.app/changelog)"

compose_message() {
  local body=$1
  if [[ -n "$body" ]]; then
    printf '%s\n\n%s\n\n%s' "$header" "$body" "$footer"
  else
    printf '%s\n\n%s' "$header" "$footer"
  fi
}

# Trim a summary so the full message fits in Discord's 2000-char limit.
# Cuts at the last paragraph break under the limit when possible, then
# falls back to a sentence break, then a newline, then a hard cut.
# Returns the trimmed summary (still without ellipsis suffix).
trim_summary() {
  local body=$1 max=$2
  local candidate="${body:0:max}"
  local marker pos
  for marker in $'\n\n' '. ' $'\n'; do
    pos="${candidate%"$marker"*}"
    # Require at least half the budget so we don't cut at an early
    # break and throw away most of the prose.
    if [[ "$pos" != "$candidate" && ${#pos} -gt $(( max / 2 )) ]]; then
      printf '%s' "${candidate:0:${#pos}}"
      return
    fi
  done
  printf '%s' "$candidate"
}

message=$(compose_message "$summary")
if [[ ${#message} -gt 2000 ]]; then
  # 6 chars for the two "\n\n" separators between sections,
  # 4 more for the trailing ellipsis we'll add to the summary.
  overhead=$(( ${#header} + ${#footer} + 6 ))
  max_summary=$(( 2000 - overhead - 4 ))
  if (( max_summary > 0 )); then
    summary="$(trim_summary "$summary" "$max_summary")…"
  else
    summary=""
  fi
  message=$(compose_message "$summary")
fi

# flags: 4 = SUPPRESS_EMBEDS. Stops Discord from generating a link
# preview card for the changelog URL.
payload=$(jq -n \
  --arg content "$message" \
  --argjson role_ids "$(printf '%s\n' "${role_ids[@]}" | jq -R . | jq -s .)" \
  '{
    content: $content,
    flags: 4,
    allowed_mentions: { roles: $role_ids }
  }')

if [[ "${DRY_RUN:-}" == "1" ]]; then
  printf '%s\n' "$payload"
  exit 0
fi

curl -sf -H "Content-Type: application/json" -d "$payload" "$WEBHOOK_URL"
echo "Discord notification sent for ${VERSION}"
