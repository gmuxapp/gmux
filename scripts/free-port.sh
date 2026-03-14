#!/usr/bin/env bash
# Kill any process listening on the given port. Used by dev tasks to avoid EADDRINUSE.
# Usage: free-port.sh <port>
set -euo pipefail

port="${1:?usage: free-port.sh <port>}"
pid=$(lsof -ti "tcp:$port" 2>/dev/null || true)
if [ -n "$pid" ]; then
  echo "port $port in use by pid $pid — killing"
  kill $pid 2>/dev/null || true
  # Wait briefly for the port to free up
  sleep 0.3
fi
