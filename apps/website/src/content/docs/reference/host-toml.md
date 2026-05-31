---
title: host.toml
description: Reference for ~/.config/gmux/host.toml — daemon behavior.
tableOfContents:
  maxHeadingLevel: 3
---

`~/.config/gmux/host.toml` (or `$XDG_CONFIG_HOME/gmux/host.toml`)

Daemon behavior. gmuxd reads this file once at startup. Create or edit it manually. The only command that modifies this file is `gmuxd remote`, which can add the `[tailscale]` section with your confirmation. If the file does not exist, safe defaults are used. Changes require restarting gmuxd.

## Example

```toml
# TCP port for the HTTP listener.
# Default: 8790
port = 8790

# Optional Tailscale remote access.
# See the Remote Access guide for setup.
[tailscale]
enabled = false
allow = []               # additional login names (owner is auto-whitelisted)

# Auto-discover peers. All flags default to true.
[discovery]
tailscale = true         # discover other gmux instances on the tailnet
devcontainers = true     # subscribe to Docker events, register gmux containers
```

## Node identity

This host's name — what peers see in their UI and URLs — is **not** configured here. When Tailscale is enabled the name is your Tailscale machine name (owned and kept stable by Tailscale itself); otherwise it is the OS hostname. The first time the daemon joins a tailnet it requests `gmux-<hostname>`, and Tailscale keeps that name across restarts and container recreation. See [ADR 0007](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0007-host-identity-and-peer-urls.md).

## Connecting to other hosts

There is **no `[[peers]]` config**. Add a host you want to aggregate sessions from at runtime via **Settings → Hosts → Connect to host** (enter its URL; leave the token blank on your own tailnet). Connected hosts are saved to `peers.json` in the state directory, and the peer's name is taken from the host itself — you don't assign one. Hosts on the same tailnet are also discovered automatically.

## Fields

### Top-level

| Field | Type | Default | Range | Description |
|-------|------|---------|-------|-------------|
| `port` | `number` | `8790` | 1–65535 | TCP port for the HTTP listener. |

### `[tailscale]`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | `boolean` | `false` | Enable Tailscale remote access. |
| `allow` | `string[]` | `[]` | Additional Tailscale login names to allow (owner is auto-whitelisted). Each must contain `@`. |

### `[discovery]`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `tailscale` | `boolean` | `true` | Discover other gmux instances on the tailnet via `WatchIPNBus`. Only active when `tailscale.enabled` is also true. |
| `devcontainers` | `boolean` | `true` | Subscribe to Docker events and register any container with the gmux devcontainer feature **and** the `devcontainer.local_folder` label as a peer. Skipped if the Docker CLI is not installed. |

## Strict validation

The config file is strictly validated at startup. gmuxd refuses to start if:

- **Unknown keys** are present, catching typos like `alow` instead of `allow`
- **`allow` entries don't contain `@`**, likely not a valid Tailscale login name
- **`port` is out of range** (must be 1–65535)
- **TOML syntax is invalid**

Two keys were **removed** and are now rejected with a migration hint (ADR 0007):

- **`tailscale.hostname`** — the node name now comes from Tailscale / the OS hostname.
- **`[[peers]]`** — manual peers are runtime state; add them via *Connect to host* (stored in `peers.json`).

This is intentional. Silent fallback to defaults is dangerous for security settings. See [Security](/security) for the reasoning.
