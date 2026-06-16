---
title: Devcontainers
description: See sessions from every dev container alongside your local sessions.
---

Add one line to your `devcontainer.json` and every session inside the container appears in your gmux dashboard automatically. No port forwarding, no token copying, no manual configuration.

## Setup

Add the gmux [devcontainer Feature](https://github.com/gmuxapp/features):

```json
{
  "image": "mcr.microsoft.com/devcontainers/base:debian",
  "features": {
    "ghcr.io/gmuxapp/features/gmux": {}
  }
}
```

That's it. Rebuild the container and any `gmux` session you start inside it shows up in your host's dashboard within seconds.

## How it works

The feature installs `gmux` and `gmuxd` into the container and starts the daemon automatically. The host gmuxd discovers the container through Docker events:

1. Container starts with `GMUXD_LISTEN=0.0.0.0` in its environment (set by the feature)
2. Host gmuxd detects the start event and reads the container's auth token via `docker exec`
3. Host connects to the container's gmuxd over the Docker bridge network
4. Container sessions stream into the host dashboard via the standard peer protocol

The container's sessions appear in the sidebar with a topology breadcrumb showing the chain (e.g. `workstation > alpine-dev`). Launching from the project hub's per-folder **+** button routes new sessions to the correct container.

When the container stops, its sessions are marked as disconnected. When it starts again, they reconnect.

## Pin a version

By default the feature installs the latest release. To pin:

```json
"ghcr.io/gmuxapp/features/gmux": {
  "version": "1.0.0"
}
```

## Pre-provisioned auth token

By default, a random token is generated on first container start. If you need a known token (for scripting or health checks), set `GMUXD_TOKEN`:

```json
{
  "features": {
    "ghcr.io/gmuxapp/features/gmux": {}
  },
  "containerEnv": {
    "GMUXD_TOKEN": "output-of-openssl-rand-hex-32"
  }
}
```

The token must be at least 64 hex characters. See [Environment variables](/reference/environment/#auth-token) for the full lifecycle.

:::note
With auto-discovery, you rarely need a pre-provisioned token. The host reads it from the container automatically.
:::

## Disabling auto-discovery

To keep containers from being auto-discovered (e.g. if you want to manage them as manual peers):

```toml
# ~/.config/gmux/host.toml
[discovery]
devcontainers = false
```

## Standalone container access

If you're running a container without host-side gmux (e.g. a remote server), add `forwardPorts` to access the container's UI directly:

```json
{
  "features": {
    "ghcr.io/gmuxapp/features/gmux": {}
  },
  "forwardPorts": [8790],
  "portsAttributes": {
    "8790": { "label": "gmux", "onAutoForward": "silent" }
  }
}
```

Then open the forwarded port and authenticate with `docker exec <container> gmux auth`.

For standalone Docker deployments without devcontainers, see [Running in Docker](/running-in-docker).
