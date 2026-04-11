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

# RELEASE_NOTES.md has the highlights prose above a `<!-- highlights-end -->`
# marker, then the generated bullet list. The marker is invisible in
# rendered markdown; we stop reading at it.
summary=$(sed '/<!-- highlights-end -->/,$d' RELEASE_NOTES.md)
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

if [[ -n "$summary" ]]; then
  message="${header}

${summary}

${footer}"
else
  message="${header}

${footer}"
fi

# Discord's message limit is 2000 chars. Truncate the summary (not the
# header or footer) so the link always survives.
if [[ ${#message} -gt 2000 ]]; then
  overhead=$(( ${#header} + ${#footer} + 6 ))
  max_summary=$(( 2000 - overhead - 4 ))  # 4 for trailing "...\n"
  if (( max_summary > 0 )); then
    summary="${summary:0:max_summary}..."
    message="${header}

${summary}

${footer}"
  else
    message="${header}

${footer}"
  fi
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
