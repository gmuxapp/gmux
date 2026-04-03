#!/usr/bin/env bash
# Send a Discord release notification via webhook.
#
# Expects these environment variables:
#   WEBHOOK_URL    - Discord webhook URL (skips silently if empty)
#   VERSION        - Release version tag (e.g. v0.9.0)
#   ROLE_BREAKING  - Discord role ID for breaking changes
#   ROLE_FEATURE   - Discord role ID for feature releases
#   ROLE_PATCH     - Discord role ID for patch releases
set -euo pipefail

if [[ -z "${WEBHOOK_URL:-}" ]]; then
  echo "DISCORD_WEBHOOK_URL not set, skipping notification"
  exit 0
fi

: "${VERSION:?}"
: "${ROLE_BREAKING:?}"
: "${ROLE_FEATURE:?}"
: "${ROLE_PATCH:?}"

# ── Extract summary ──

# RELEASE_NOTES.md has the summary above a --- separator, then the PR list.
summary=$(sed '/^---$/,$d' RELEASE_NOTES.md)
# Trim trailing blank lines.
summary=$(echo "$summary" | sed -e :a -e '/^\n*$/{$d;N;ba}')

# ── Determine bump type ──

prev_tag=$(git tag -l 'v*' | sort -V | grep -B1 "^${VERSION}$" | head -1)
cur_major=$(echo "$VERSION" | sed 's/^v//' | cut -d. -f1)
cur_minor=$(echo "$VERSION" | sed 's/^v//' | cut -d. -f2)
prev_major=$(echo "$prev_tag" | sed 's/^v//' | cut -d. -f1)
prev_minor=$(echo "$prev_tag" | sed 's/^v//' | cut -d. -f2)

if [[ "$cur_major" != "$prev_major" ]]; then
  role_id="$ROLE_BREAKING"
elif [[ "$cur_minor" != "$prev_minor" ]]; then
  role_id="$ROLE_FEATURE"
else
  role_id="$ROLE_PATCH"
fi

# ── Send ──

header="<@&${role_id}>
## gmux ${VERSION}"
footer="[See the changelog for details.](https://gmux.app/changelog)"
overhead=$(( ${#header} + ${#footer} + 4 ))

# Condense the summary if the full message would exceed Discord's 2000 char limit.
if [[ $(( ${#summary} + overhead )) -gt 2000 ]]; then
  max_chars=$(( 2000 - overhead ))
  echo "Summary too long for Discord ($(( ${#summary} + overhead )) chars), condensing..."
  summary=$(echo "$summary" | "$(dirname "$0")/summarize.sh" --condense "$max_chars")
fi

message="${header}

${summary}

${footer}"

# Hard truncation as a last resort.
if [[ ${#message} -gt 2000 ]]; then
  message="${message:0:1997}..."
fi

payload=$(jq -n \
  --arg content "$message" \
  --arg role_id "$role_id" \
  '{
    content: $content,
    allowed_mentions: { roles: [$role_id] }
  }')

curl -sf -H "Content-Type: application/json" -d "$payload" "$WEBHOOK_URL"
echo "Discord notification sent for ${VERSION}"
