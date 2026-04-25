#!/usr/bin/env bash
# Regenerate RELEASE_NOTES.md and changelog.mdx after editing
# RELEASE_HIGHLIGHTS.md on the open release/next branch.
#
# Use this when you want to tweak release prose after the release PR
# is already open:
#
#   1. Check out release/next.
#   2. Edit RELEASE_HIGHLIGHTS.md.
#   3. Run this script.
#   4. Push (force-push) the branch back up.
#
# What it does:
#
#   - Saves your current RELEASE_HIGHLIGHTS.md edits.
#   - Resets the working tree to the parent of the release commit
#     (undoing the auto-generated changelog.mdx and RELEASE_NOTES.md).
#   - Restores your highlights edits.
#   - Re-runs version.sh to regenerate the auto-generated files.
#   - Re-creates the `release: vX.Y.Z` commit.
#
# After it finishes, push with `git push -f origin release/next`.

set -euo pipefail
trap 'echo "error: ${BASH_SOURCE}:${LINENO}: ${BASH_COMMAND}" >&2' ERR

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

cur_branch=$(git rev-parse --abbrev-ref HEAD)
if [[ "$cur_branch" != "release/next" ]]; then
  echo "error: expected to be on 'release/next', got '$cur_branch'" >&2
  exit 1
fi

head_subject=$(git log -1 --format='%s')
version=$(echo "$head_subject" | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' || true)
if [[ -z "$version" ]]; then
  echo "error: HEAD is not a release commit (subject: $head_subject)" >&2
  exit 1
fi

# Refuse to run if the parent of the release commit is not origin/main.
# This catches the case where someone runs this script twice in a row
# (which would reset past main) or where release/next has somehow
# diverged from peter-evans's expected single-commit-on-top-of-main
# shape.
git fetch --quiet origin main
parent=$(git rev-parse HEAD~1)
main=$(git rev-parse origin/main)
if [[ "$parent" != "$main" ]]; then
  echo "error: HEAD~1 ($parent) is not origin/main ($main)." >&2
  echo "       release/next should be exactly one commit on top of main." >&2
  echo "       If you ran this script already, recreate the branch from origin/release/next first." >&2
  exit 1
fi

# Refuse to run if there are uncommitted changes other than to
# RELEASE_HIGHLIGHTS.md. The reset --hard below would silently discard
# them.
if ! git diff --quiet HEAD -- ':(exclude)RELEASE_HIGHLIGHTS.md'; then
  echo "error: uncommitted changes outside RELEASE_HIGHLIGHTS.md would be lost." >&2
  echo "       commit or stash them first." >&2
  git diff --stat HEAD -- ':(exclude)RELEASE_HIGHLIGHTS.md' >&2
  exit 1
fi

# 1. Save the current highlights (the human-edited source of truth).
saved=$(mktemp)
trap 'rm -f "$saved"' EXIT
cp RELEASE_HIGHLIGHTS.md "$saved"

# 2. Roll back the auto-generated release commit.
echo "Resetting to $(git rev-parse --short HEAD~1) (parent of release commit)..."
git reset --hard HEAD~1

# 3. Restore the human-edited highlights on top of the clean tree.
cp "$saved" RELEASE_HIGHLIGHTS.md

# 4. Regenerate.
.github/workflows/scripts/version.sh

# 5. Re-create the release commit using the user's local git identity.
git add -A
git commit --quiet -m "release: $version"

cat <<EOF

Regenerated release notes for $version.

Push with:
  git push --force-with-lease origin release/next
EOF
