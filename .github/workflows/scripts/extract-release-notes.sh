#!/usr/bin/env bash
# Extract the body of the latest entry from changelog.mdx.
#
# `changelog.mdx` is the single source of truth for release prose.
# Both `notify-discord.sh` (for the Discord post) and `release.yml`
# (for GoReleaser's `--release-notes`) need the body of the most
# recent entry. This helper is the one place that knows how to find
# it, so a regression in one consumer can't disagree with the other.
#
# Output: the body of the latest entry, with the `## vX.Y.Z` heading
# stripped (consumers add their own context) and the trailing `---`
# entry separator stripped (it would render as an `<hr>` in the
# GitHub Release body and the Discord post). Leading and trailing
# blank lines are trimmed.
#
# Critically, only the *trailing* `---` is stripped: a `---` inside
# prose is a user-written horizontal rule and survives untouched.
# Consumers split prose from bullets at the first `### ` group
# heading.
#
# Usage:
#   extract-release-notes.sh [<changelog>]
#     defaults to apps/website/src/content/docs/changelog.mdx
set -euo pipefail

CHANGELOG="${1:-apps/website/src/content/docs/changelog.mdx}"

# Stage 1: take the body between the first `## v` heading (exclusive)
# and the next `## v` heading (exclusive).
#
# Stage 2: strip a single trailing `---` separator (and surrounding
# blank lines), plus leading blank lines. Any earlier `---` in the
# entry is preserved as user prose.
awk '
  /^## v/ { n++; if (n == 2) exit; next }
  n == 1 { print }
' "$CHANGELOG" \
  | awk '
    { lines[NR] = $0 }
    END {
      last = NR
      while (last >= 1 && lines[last] ~ /^[[:space:]]*$/) last--
      if (last >= 1 && lines[last] == "---") last--
      while (last >= 1 && lines[last] ~ /^[[:space:]]*$/) last--
      first = 1
      while (first <= last && lines[first] ~ /^[[:space:]]*$/) first++
      for (i = first; i <= last; i++) print lines[i]
    }
  '
