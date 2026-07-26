#!/usr/bin/env bash
set -euo pipefail
[[ -f /.dockerenv ]] || { echo 'REFUSING: production E2E must run in Docker' >&2; exit 97; }
[[ ${GMUX_E2E_CONTAINER_GUARD:-} == isolated-v1 ]] || { echo 'REFUSING: isolation guard absent' >&2; exit 97; }
[[ ${HOME:-} == /e2e/home && ${XDG_STATE_HOME:-} == /e2e/state && ${XDG_CONFIG_HOME:-} == /e2e/config && ${XDG_RUNTIME_DIR:-} == /e2e/run ]] || { echo 'REFUSING: private HOME/XDG paths absent' >&2; exit 97; }
mkdir -p "$HOME" "$XDG_STATE_HOME" "$XDG_CONFIG_HOME" "$XDG_RUNTIME_DIR" /e2e/tmp
cd /src/services/gmuxd
go test ./cmd/gmuxd -run '^TestProductionContainerE2E$' -count=1 -timeout "${GMUX_E2E_TIMEOUT:-5m}" -v
# Test processes must all have joined. PID 1 is this script; only shell helpers
# used by this assertion may remain.
if pgrep -x gmuxd >/dev/null; then echo 'orphan gmuxd process after E2E' >&2; ps aux >&2; exit 1; fi
