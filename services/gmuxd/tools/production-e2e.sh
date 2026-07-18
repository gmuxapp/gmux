#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/../../.." && pwd)
image=${GMUX_E2E_IMAGE:-gmux-production-e2e:local}
profile=${GMUX_E2E_PROFILE:-fast}
case "$profile" in fast|extended) ;; *) echo 'GMUX_E2E_PROFILE must be fast or extended' >&2; exit 2;; esac
docker build -f "$root/services/gmuxd/tools/production-e2e.Dockerfile" -t "$image" "$root"
docker run --rm --network=none --pids-limit=512 --read-only \
  --tmpfs /e2e:rw,nosuid,nodev,mode=0700 --tmpfs /tmp:rw,exec,nosuid,nodev \
  -e HOME=/e2e/home -e XDG_STATE_HOME=/e2e/state -e XDG_CONFIG_HOME=/e2e/config -e XDG_RUNTIME_DIR=/e2e/run \
  -e TMPDIR=/tmp -e GMUX_E2E_PROFILE="$profile" -e GMUX_E2E_CONTAINER_GUARD=isolated-v1 \
  "$image"
