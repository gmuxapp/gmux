---
title: Running in Docker
description: Run gmux in a container with your dev tools and Tailscale access.
---

> For the planned native devcontainer integration, see [Peer Discovery & Aggregation](/planned/peer-discovery-aggregation#devcontainers).

## What you get

A Docker container with:
- gmux and gmuxd (auto-updated on each start)
- Your choice of dev tools (language runtimes, CLIs, etc.)
- Tailscale remote access (its own device on your tailnet)
- A persistent workspace for repos
- Standard devcontainer base image

## Template

### Dockerfile

```dockerfile
FROM mcr.microsoft.com/devcontainers/base:debian

# Project-specific tools (add your own here)
RUN apt-get update && apt-get install -y --no-install-recommends \
    fish \
    && rm -rf /var/lib/apt/lists/*

# gmux (auto-updated on each container start)
ARG GMUX_VERSION=0.8.0
ADD https://github.com/gmuxapp/gmux/releases/download/v${GMUX_VERSION}/gmux_${GMUX_VERSION}_linux_amd64.tar.gz /tmp/gmux.tar.gz
RUN tar xzf /tmp/gmux.tar.gz -C /usr/local/bin/ gmux gmuxd && rm /tmp/gmux.tar.gz

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["entrypoint.sh"]
```

### entrypoint.sh

```bash
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
```

### compose.yaml

```yaml
services:
  dev:
    build: .
    container_name: dev
    restart: unless-stopped
    environment:
      - TERM=xterm-256color
      - SHELL=/usr/bin/fish  # or /bin/bash, /bin/zsh
    volumes:
      # Persistent workspace for repos
      - ./data/workspace:/workspace
      # gmux config and Tailscale state (container-specific)
      - ./data/gmux-config:/root/.config/gmux
      - ./data/gmux-state:/root/.local/state/gmux
    healthcheck:
      test: ["CMD-SHELL", "curl -fsS http://localhost:8790/ >/dev/null"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 15s
```

## Setup

### 1. Create the directory structure

```bash
mkdir -p data/{workspace,gmux-config,gmux-state}
```

### 2. Configure gmux

```bash
cat > data/gmux-config/host.toml << 'EOF'
[tailscale]
enabled = true
hostname = "dev"
EOF
```

Pick a hostname that's unique on your tailnet.

### 3. Build and start

```bash
docker compose up -d --build
```

### 4. Authenticate Tailscale

Check the logs for the login URL:

```bash
docker logs dev 2>&1 | grep "login.tailscale.com"
```

Visit the URL to register the container as a device on your tailnet.

### 5. Access

Open `https://dev.your-tailnet.ts.net` in your browser.

## Customization

### Adding tools

Add packages to the `apt-get install` line in the Dockerfile, or install language-specific tools via their own mechanisms (rustup, fnm, etc.). Rebuild with:

```bash
docker compose up -d --build
```

### Persistent home

To persist installed tools and shell history across container rebuilds, mount a volume at the container's home directory:

```yaml
volumes:
  - ./data/home:/root
  # Override gmux config/state so they don't conflict with host
  - ./data/gmux-config:/root/.config/gmux
  - ./data/gmux-state:/root/.local/state/gmux
```

### Shared home with host

If you manage dotfiles with chezmoi or a similar tool and want the container to share your host's config:

```yaml
volumes:
  - /path/to/your/home:/root
  # Container-specific overrides
  - ./data/gmux-config:/root/.config/gmux
  - ./data/gmux-state:/root/.local/state/gmux
```

The overlay mounts for gmux config and state ensure the container has its own Tailscale identity and hostname, separate from the host.

### Multiple projects

Run separate containers for different projects. Each gets its own Tailscale hostname:

```bash
# data/project-a/gmux-config/host.toml
[tailscale]
enabled = true
hostname = "project-a"

# data/project-b/gmux-config/host.toml
[tailscale]
enabled = true
hostname = "project-b"
```

This works but means switching between browser tabs per project. See [Peer Discovery & Aggregation](/planned/peer-discovery-aggregation) for the planned single-dashboard solution.
