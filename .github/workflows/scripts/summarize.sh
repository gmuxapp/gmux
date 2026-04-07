#!/usr/bin/env bash
# Generate or condense a release summary using OpenRouter's free model router.
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

# ── Build prompt ──

# OpenRouter's free router picks a random available free model. Including
# a vision input filters the pool to larger, more capable models.
model="openrouter/free"
context_image="https://gmux.app/og.png"
script_dir=$(cd "$(dirname "$0")" && pwd)

if $condense; then
  prompt="Condense the following release summary to ${max_chars} characters \
or fewer. Preserve the section structure (**Breaking changes**, **Features**, \
**Fixes**). Keep the high-level picture and key migration actions; cut \
implementation details and specific flag/env var names. Do not add anything \
new. Output only the condensed summary.

${input}"
  max_tokens=500
else
  guidelines=$(cat "$script_dir/summarize-prompt.md")
  prompt="${guidelines}

## Changelog for ${version}

${input}"
  max_tokens=800
fi

# ── Call API with exponential backoff ──

max_attempts=8
for (( attempt=1; attempt<=max_attempts; attempt++ )); do
  response=$(curl -s https://openrouter.ai/api/v1/chat/completions \
    -H "Authorization: Bearer $OPENROUTER_API_KEY" \
    -H "Content-Type: application/json" \
    -d "$(jq -n \
      --arg model "$model" \
      --arg prompt "$prompt" \
      --arg image "$context_image" \
      --argjson max_tokens "$max_tokens" \
      '{
        model: $model,
        messages: [{role: "user", content: [
          {type: "image_url", image_url: {url: $image}},
          {type: "text", text: $prompt}
        ]}],
        max_tokens: $max_tokens
      }')")

  result=$(echo "$response" | jq -r '.choices[0].message.content // empty' 2>/dev/null || true)
  result=$(echo "$result" | sed '/^[[:space:]]*$/d')  # strip blank lines
  if [[ -n "$result" ]]; then
    used=$(echo "$response" | jq -r '.model // "unknown"' 2>/dev/null)
    echo "Generated summary using $used" >&2
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
    delay=$(( 10 * (2 ** (attempt - 1)) ))  # 10, 20, 40, 80, 160
    (( delay > 160 )) && delay=160
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
