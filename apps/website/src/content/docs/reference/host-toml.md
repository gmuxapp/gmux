---
title: host.toml
description: Complete field reference for ~/.config/gmux/host.toml
tableOfContents:
  maxHeadingLevel: 3
---

Daemon behavior. Read once at startup; changes require restarting gmuxd.
If the file does not exist, safe defaults are used.

Validation is strict: unknown keys, invalid values, and bad TOML syntax
all cause gmuxd to refuse to start. See [Security](/security) for the reasoning.

For guides and examples, see [Configuration](/configuration/#host-config).

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
