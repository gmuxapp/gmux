#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
GOWORK=off go tool sqlc generate -f ../sqlc.yaml
