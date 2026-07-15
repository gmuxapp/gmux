#!/usr/bin/env bash
# Fails when the checked-in sqlc output under internal/centralstore/internal/db
# does not match a fresh regeneration. VCS-neutral: compares file hashes before
# and after regenerating. Note: on failure the regenerated (correct) files are
# left in place.
set -euo pipefail
cd "$(dirname "$0")/.."

generated_dir=internal/centralstore/internal/db

hash_generated() {
  find "$generated_dir" -name '*.go' -type f | LC_ALL=C sort | xargs sha256sum
}

before=$(hash_generated)
./tools/generate.sh
after=$(hash_generated)

if [ "$before" != "$after" ]; then
  echo "error: generated sqlc code is out of date; commit the result of tools/generate.sh" >&2
  exit 1
fi
