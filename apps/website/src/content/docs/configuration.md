---
title: Configuration
description: Config file, environment variables, and file paths.
---

gmux works out of the box with no configuration. This page documents everything you can customize.

## Config file

`~/.config/gmux/config.toml` (or `$XDG_CONFIG_HOME/gmux/config.toml`)

gmuxd reads this file at startup. Create it manually — gmuxd never writes to it. If the file doesn't exist, safe defaults are used.

```toml
# TCP port for the HTTP listener.
# Default: 8790
port = 8790

# Optional tailscale remote access.
# See the Remote Access guide for setup.
[tailscale]
enabled = false
hostname = "gmux"       # → gmux.your-tailnet.ts.net
allow = []               # additional login names (owner is auto-whitelisted)
```

### Strict validation

The config file is strictly validated at startup. gmuxd refuses to start if:

- **Unknown keys** are present — catches typos like `alow` instead of `allow`
- **`allow` entries don't contain `@`** — likely not a valid tailscale login name
- **`hostname` is empty** when tailscale is enabled
- **`port` is out of range** (must be 1-65535)
- **TOML syntax is invalid**

This is intentional — silent fallback to defaults is dangerous for security settings. See [Security](/security) for the reasoning.

## Terminal theme

`~/.config/gmux/theme.jsonc`

Customize terminal appearance: colors, font, cursor, scrollback, and more. All fields are optional; anything you omit uses the built-in default.

```jsonc
{
  "fontSize": 14,
  "fontFamily": "'JetBrains Mono', monospace",
  "cursorStyle": "bar",
  "cursorBlink": true,
  "scrollback": 10000,
  "theme": {
    "background": "#282a36",
    "foreground": "#f8f8f2"
    // ... any xterm.js ITheme color keys
  }
}
```

You can drop in a [Windows Terminal theme](https://github.com/mbadolato/iTerm2-Color-Schemes/tree/master/windowsterminal) and it works out of the box: `purple`/`brightPurple` are automatically mapped to `magenta`/`brightMagenta`, and the `name` field is ignored.

Numeric values are clamped to safe ranges (e.g. fontSize 6-48, scrollback 0-100,000). Unknown keys produce a console warning.

## Terminal keybinds

`~/.config/gmux/keybinds.jsonc`

Remap keys the browser steals, send raw text, or disable built-in bindings. The file is a JSON array of keybind entries:

```jsonc
[
  // Remap Ctrl+Alt+T to send Ctrl+T (browser steals Ctrl+T for new tab)
  { "key": "ctrl+alt+t", "action": "sendKeys", "args": "ctrl+t" },

  // Send raw text to the terminal
  { "key": "ctrl+alt+g", "action": "sendText", "args": "git status\r" },

  // Disable a built-in binding
  { "key": "ctrl+alt+w", "action": "none" }
]
```

### Actions

| Action | Description |
|--------|-------------|
| `sendKeys` | Parse `args` as a key combo and send its escape sequence (e.g. `"ctrl+t"` sends `^T`) |
| `sendText` | Send `args` as raw text to the PTY |
| `copyOrInterrupt` | Copy selection to clipboard if text is selected, otherwise send SIGINT |
| `none` | Disable this key binding (removes a built-in default) |

### Key format

Key combos are case-insensitive and support these modifiers: `ctrl`, `shift`, `alt`, `meta` (or `cmd`/`super`). Modifier order doesn't matter: `ctrl+alt+t` and `Alt+Ctrl+T` are the same.

Arrow keys can use short names: `left`, `right`, `up`, `down`. Named keys like `enter`, `escape`, `tab`, `backspace`, `home`, `end`, `delete` are also supported.

### The `secondary` modifier

The virtual modifier `secondary` resolves to **Cmd** on macOS and **Ctrl** everywhere else. This lets you write cross-platform keybinds:

```jsonc
{ "key": "secondary+alt+t", "action": "sendKeys", "args": "ctrl+t" }
```

### User entries override defaults

User keybinds override built-in defaults that share the same key combo. To see the current defaults, check [Keyboard shortcuts](/using-the-ui#keyboard-shortcuts).

## Environment variables

### gmuxd (daemon)

| Variable | Purpose | Default |
|----------|---------|---------|
| `GMUXD_LISTEN` | TCP bind address | `127.0.0.1` |
| `XDG_CONFIG_HOME` | Base directory for config file | `~/.config` |
| `XDG_STATE_HOME` | Base directory for runtime state (socket, auth token) | `~/.local/state` |

### gmux (CLI and session runner)

| Variable | Purpose | Default |
|----------|---------|---------|

| `GMUX_ADAPTER` | Force a specific adapter instead of auto-detection | *(auto)* |
| `GMUX_SOCKET_DIR` | Directory for session Unix sockets | `/tmp/gmux-sessions` |

### Set by gmux in the child process

These are available inside every session launched by gmux. Use them to detect gmux or report status back:

| Variable | Purpose | Example |
|----------|---------|---------|
| `GMUX_SOCKET` | Unix socket path for callbacks to the runner | `/tmp/gmux-sessions/sess-abc123.sock` |
| `GMUX_SESSION_ID` | Unique session identifier | `sess-abc123` |
| `GMUX_ADAPTER` | Name of the matched adapter | `pi`, `shell` |
| `GMUX_VERSION` | Protocol version | `0.1.0` |

See [Adapter Architecture](/develop/adapter-architecture) for how to use the child-to-runner API.

## File paths

| Path | Purpose | Created by |
|------|---------|------------|
| `~/.config/gmux/config.toml` | Daemon config (port, tailscale) | User |
| `~/.config/gmux/theme.jsonc` | Terminal appearance (colors, font, cursor) | User |
| `~/.config/gmux/keybinds.jsonc` | Key-to-action mappings | User |
| `~/.local/state/gmux/gmuxd.sock` | Daemon Unix socket (local IPC) | gmuxd |
| `~/.local/state/gmux/auth-token` | Bearer token for TCP authentication | gmuxd |
| `~/.local/state/gmux/tsnet/` | Tailscale state (when enabled) | gmuxd |
| `/tmp/gmux-sessions/*.sock` | Live session Unix sockets | gmux |

### Adapter-specific paths

| Path | Purpose | Used by |
|------|---------|---------|
| `~/.pi/agent/sessions/` | Pi conversation files (JSONL) | gmuxd (for session discovery and resume) |

## Port

The default port is **8790**. To change it in the config file:

```toml
port = 9999
```

## Bind address

By default, the TCP listener binds to `127.0.0.1` (localhost only). All TCP connections require bearer token authentication.

To bind to all interfaces (containers, VPN setups):

```bash
GMUXD_LISTEN=0.0.0.0 gmuxd start
```

The bind address is controlled exclusively by the `GMUXD_LISTEN` environment variable. It is not a config file option because it is a deployment concern, not a user preference.
