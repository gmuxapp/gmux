#!/bin/bash
set -e

# Auto-update gmux binaries on start
if latest=$(curl -fsSL --connect-timeout 5 \
    https://api.github.com/repos/gmuxapp/gmux/releases/latest 2>/dev/null); then
  tag=$(echo "$latest" | grep -o '"tag_name": "[^"]*"' | cut -d'"' -f4)
  version=${tag#v}
  current=$(gmuxd version 2>/dev/null || echo "unknown")

  if [ -n "$version" ] && ! echo "$current" | grep -qF "$version"; then
    echo "Updating gmux: $current -> $version"
    url="https://github.com/gmuxapp/gmux/releases/download/${tag}/gmux_${version}_linux_amd64.tar.gz"
    curl -fsSL "$url" | tar xz -C /usr/local/bin/ gmux gmuxd
    echo "Done"
  else
    echo "gmux $version is current"
  fi
else
  echo "Skipping gmux update check (GitHub unreachable)"
fi

exec gmuxd start --replace
