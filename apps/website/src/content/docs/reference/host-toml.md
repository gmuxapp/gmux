---
title: host.toml
description: Reference for ~/.config/gmux/host.toml — daemon behavior.
tableOfContents:
  maxHeadingLevel: 3
---

`~/.config/gmux/host.toml` (or `$XDG_CONFIG_HOME/gmux/host.toml`)

Daemon behavior. gmuxd reads this file once at startup. Create it manually; gmuxd never writes to it. If the file does not exist, safe defaults are used. Changes require restarting gmuxd.

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
| `hostname` | `string` | `"gmux"` | Tailscale machine name (becomes `<hostname>.your-tailnet.ts.net`). Must be non-empty when enabled. |
| `allow` | `string[]` | `[]` | Additional Tailscale login names to allow (owner is auto-whitelisted). Each must contain `@`. |

## Strict validation

The config file is strictly validated at startup. gmuxd refuses to start if:

- **Unknown keys** are present, catching typos like `alow` instead of `allow`
- **`allow` entries don't contain `@`**, likely not a valid Tailscale login name
- **`hostname` is empty** when Tailscale is enabled
- **`port` is out of range** (must be 1–65535)
- **TOML syntax is invalid**

This is intentional. Silent fallback to defaults is dangerous for security settings. See [Security](/security) for the reasoning.
