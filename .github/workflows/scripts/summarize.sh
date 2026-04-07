#!/usr/bin/env bash
# Generate or condense a release summary using OpenRouter's best free model.
#
# Usage:
#   .github/workflows/scripts/summarize.sh <version>              < notes.txt   # summarize
#   .github/workflows/scripts/summarize.sh --condense <max_chars> < summary.txt # condense
#
# Falls back to a placeholder (or truncation for --condense) if the API is
# unavailable or OPENROUTER_API_KEY is not set.
set -euo pipefail
trap 'echo "error: ${BASH_SOURCE}:${LINENO}: ${BASH_COMMAND}" >&2' ERR

condense=false
if [[ "${1:-}" == "--condense" ]]; then
  condense=true
  max_chars="${2:?Usage: summarize.sh --condense <max_chars>}"
else
  version="${1:?Usage: summarize.sh <version>}"
fi

input=$(cat)

if [[ -z "${OPENROUTER_API_KEY:-}" ]]; then
  echo "OPENROUTER_API_KEY not set, skipping summary." >&2
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
model=$(curl -sf 'https://openrouter.ai/api/v1/models' 2>/dev/null | jq -r '
  [.data[]
    | select(.id | endswith(":free"))
    | select(.context_length >= 32000)
  ] | .[0].id' 2>/dev/null || true)

if [[ -z "$model" || "$model" == "null" ]]; then
  echo "Could not select a model from OpenRouter." >&2
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
are the merged PRs for a release. Each entry has a PR title (conventional \
commit format) and its description for context. Summarize them into a \
Discord message for the project's community server.

Base the summary on what changed for users, not on implementation details. \
The PR descriptions are background context to help you understand the change, \
not content to surface verbatim.

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

# ── Call API with exponential backoff ──

max_attempts=6
for (( attempt=1; attempt<=max_attempts; attempt++ )); do
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

  result=$(echo "$response" | jq -r '.choices[0].message.content // empty' 2>/dev/null || true)
  if [[ -n "$result" ]]; then
    echo "$result"
    exit 0
  fi

  # Log the error details so failures are diagnosable.
  # OpenRouter nests provider errors in .error.metadata.raw
  error=$(
    echo "$response" | jq -r '
      if .error.metadata.raw then
        "\(.error.message): \(.error.metadata.raw)"
      elif .error.message then
        .error.message
      elif .error then
        (.error | tostring)
      else
        empty
      end' 2>/dev/null || true
  )
  if [[ -n "$error" ]]; then
    echo "Attempt $attempt/$max_attempts failed: $error" >&2
  else
    echo "Attempt $attempt/$max_attempts failed (raw): $(echo "$response" | head -c 500)" >&2
  fi

  if (( attempt < max_attempts )); then
    delay=$(( 2 ** attempt ))
    echo "Retrying in ${delay}s..." >&2
    sleep "$delay"
  fi
done

echo "Summary generation failed after $max_attempts attempts." >&2
if $condense; then
  echo "${input:0:$max_chars}"
else
  echo "_No summary available._"
fi
