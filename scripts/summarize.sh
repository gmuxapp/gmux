#!/usr/bin/env bash
# Generate or condense a release summary using OpenRouter's best free model.
#
# Usage:
#   scripts/summarize.sh <version>              < notes.txt   # summarize
#   scripts/summarize.sh --condense <max_chars> < summary.txt # condense
#
# Falls back to a placeholder (or truncation for --condense) if the API is
# unavailable or OPENROUTER_API_KEY is not set.
set -euo pipefail

condense=false
if [[ "${1:-}" == "--condense" ]]; then
  condense=true
  max_chars="${2:?Usage: summarize.sh --condense <max_chars>}"
else
  version="${1:?Usage: summarize.sh <version>}"
fi

input=$(cat)

if [[ -z "${OPENROUTER_API_KEY:-}" ]]; then
  if $condense; then
    echo "${input:0:$max_chars}"
  else
    echo "_No summary available._"
  fi
  exit 0
fi

# ── Pick model ──

# First free model in OpenRouter's default ordering (correlates with
# popularity) with a reasonable context window.
model=$(curl -sf 'https://openrouter.ai/api/v1/models' | jq -r '
  [.data[]
    | select(.id | endswith(":free"))
    | select(.context_length >= 32000)
  ] | .[0].id')

if [[ -z "$model" || "$model" == "null" ]]; then
  echo "Could not select a model." >&2
  if $condense; then
    echo "${input:0:$max_chars}"
  else
    echo "_No summary available._"
  fi
  exit 0
fi

echo "Using model: $model" >&2

# ── Build prompt ──

if $condense; then
  prompt="Condense the following release summary to ${max_chars} characters \
or fewer. Preserve the section structure (**Breaking changes**, **Features**, \
**Fixes**). Keep the high-level picture and key migration actions; cut \
implementation details and specific flag/env var names. Do not add anything \
new. Output only the condensed summary.

${input}"
  max_tokens=500
else
  prompt="gmux is an open-source terminal multiplexer with a web UI. Below \
is the content for a release. Each entry has a user-facing changelog note \
followed by the PR description for context. Summarize them into a Discord \
message for the project's community server.

Base the summary on the changelog notes, not the PR implementation details. \
The PR descriptions are background context to help you understand the change, \
not content to surface to users.

Be direct, technical, and accurate. Assume readers are developers who use \
the tool daily. No hype, no filler, no calls to action.

Group by change type, skipping empty sections: breaking changes first, then \
features, then fixes. Multiple entries may be part of the same effort; cover \
them once. A link to the full changelog follows the summary, so focus on the \
highlights rather than being exhaustive.

Use Discord markdown. Use - for bullet points. Do not include a title, \
heading, or links.

Changelog for ${version}:
${input}"
  max_tokens=800
fi

# ── Call API with retries ──

for attempt in 1 2 3; do
  response=$(curl -s https://openrouter.ai/api/v1/chat/completions \
    -H "Authorization: Bearer $OPENROUTER_API_KEY" \
    -H "Content-Type: application/json" \
    -d "$(jq -n \
      --arg model "$model" \
      --arg prompt "$prompt" \
      --argjson max_tokens "$max_tokens" \
      '{
        model: $model,
        messages: [{role: "user", content: $prompt}],
        max_tokens: $max_tokens
      }')")

  result=$(echo "$response" | jq -r '.choices[0].message.content // empty')
  if [[ -n "$result" ]]; then
    echo "$result"
    exit 0
  fi

  echo "Attempt $attempt failed, retrying in $((attempt * 2))s..." >&2
  sleep $((attempt * 2))
done

echo "Summary generation failed after 3 attempts." >&2
if $condense; then
  echo "${input:0:$max_chars}"
else
  echo "_No summary available._"
fi
