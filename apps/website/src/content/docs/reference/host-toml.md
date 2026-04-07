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
hostname = "gmux"       # → gmux.your-tailnet.ts.net
allow = []               # additional login names (owner is auto-whitelisted)

# Auto-discover peers. All flags default to true.
[discovery]
tailscale = true         # discover other gmux instances on the tailnet
devcontainers = true     # subscribe to Docker events, register gmux containers

# Manual peers (remote gmuxd instances to aggregate sessions from).
[[peers]]
name = "server"
url = "http://10.0.0.5:8790"
token_file = "~/.config/gmux/tokens/server"
```

## Fields

### Top-level

| Field | Type | Default | Range | Description |
|-------|------|---------|-------|-------------|
| `port` | `number` | `8790` | 1–65535 | TCP port for the HTTP listener. |

### `[tailscale]`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | `boolean` | `false` | Enable Tailscale remote access. |
| `hostname` | `string` | `"gmux"` | Tailscale machine name (becomes `<hostname>.your-tailnet.ts.net`). Must be non-empty when enabled. Changing this value automatically clears the Tailscale state and re-registers the device under the new name on the next restart. |
| `allow` | `string[]` | `[]` | Additional Tailscale login names to allow (owner is auto-whitelisted). Each must contain `@`. |

### `[discovery]`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `tailscale` | `boolean` | `true` | Discover other gmux instances on the tailnet via `WatchIPNBus`. Only active when `tailscale.enabled` is also true. |
| `devcontainers` | `boolean` | `true` | Subscribe to Docker events and register any container with the gmux devcontainer feature as a peer. Skipped if the Docker CLI is not installed. |

### `[[peers]]` (array of tables)

One table per manual peer. Each peer requires `name`, `url`, and exactly one of `token`, `token_file`, `token_command`.

| Field | Type | Description |
|-------|------|-------------|
| `name` | `string` | Unique peer identifier. Appears in URLs (`/@name/`) and session IDs. |
| `url` | `string` | Base URL of the remote gmuxd, e.g. `http://host:8790`. |
| `token` | `string` | Inline bearer token. Quick but leaks into your dotfiles. |
| `token_file` | `string` | Path to a file containing the token. Tilde expansion is supported. |
| `token_command` | `string` | Shell command (via `sh -c`) whose stdout is the token. Use for 1Password / pass / op integrations. 10 second timeout. |

## Strict validation

The config file is strictly validated at startup. gmuxd refuses to start if:

- **Unknown keys** are present, catching typos like `alow` instead of `allow`
- **`allow` entries don't contain `@`**, likely not a valid Tailscale login name
- **`hostname` is empty** when Tailscale is enabled
- **`port` is out of range** (must be 1–65535)
- **A `[[peers]]` entry is missing required fields** (`name`, `url`) or specifies more than one token source
- **Two `[[peers]]` entries share the same `name`**
- **TOML syntax is invalid**

This is intentional. Silent fallback to defaults is dangerous for security settings. See [Security](/security) for the reasoning.
