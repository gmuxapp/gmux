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

gmux ships a complete default keymap that is the source of truth for every keyboard shortcut. Every key combo that does something other than "send bytes to the terminal" is listed explicitly; nothing relies on implicit browser or xterm.js passthrough.

Your `keybinds.jsonc` file layers on top: same-key entries override the defaults, and the `none` action disables a default. The file is a JSON array:

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
| `copy` | Copy selection to clipboard (does nothing if no selection; never sends SIGINT) |
| `paste` | Read system clipboard and send contents to the PTY |
| `selectAll` | Select all terminal content |
| `none` | Disable this key binding (removes a built-in default) |

### Key format

Key combos are case-insensitive and support these modifiers: `ctrl`, `shift`, `alt`, `meta` (or `cmd`/`super`). Modifier order doesn't matter: `ctrl+alt+t` and `Alt+Ctrl+T` are the same.

Supported key names: `enter`, `escape` (`esc`), `tab`, `backspace`, `home`, `end`, `delete` (`del`), `insert` (`ins`), `pageup` (`page_up`), `pagedown` (`page_down`), `left`, `right`, `up`, `down`.

### The `secondary` modifier

The virtual modifier `secondary` resolves to **Cmd** on macOS and **Ctrl** everywhere else. Useful for cross-platform configs:

```jsonc
{ "key": "secondary+alt+t", "action": "sendKeys", "args": "ctrl+t" }
```

Note: `secondary` works well for keys that do the same thing on both platforms. For copy/paste it is less useful because the shortcuts differ (Ctrl+Shift+C on Linux vs. Cmd+C on Mac), so the defaults handle each platform separately.

### User entries override defaults

User keybinds override built-in defaults that share the same key combo. See [Keyboard shortcuts](/using-the-ui#keyboard-shortcuts) for the full default keymap.

### Starter templates

These are ready to paste into `~/.config/gmux/keybinds.jsonc`.

**Quick commands** -- bind key combos to common shell commands:

```jsonc
[
  { "key": "ctrl+alt+g", "action": "sendText", "args": "git status\r" },
  { "key": "ctrl+alt+d", "action": "sendText", "args": "git diff\r" },
  { "key": "ctrl+alt+l", "action": "sendText", "args": "git log --oneline -20\r" }
]
```

**Vim-friendly** -- disable the Ctrl+C copy behavior so Ctrl+C always sends SIGINT (useful if you use visual mode for copying):

```jsonc
[
  { "key": "ctrl+c", "action": "none" }
]
```

**Disable all browser workarounds** -- if you run gmux as a PWA or `--app` window, the browser doesn't steal Ctrl+T/N/W, so the Ctrl+Alt workarounds are unnecessary:

```jsonc
[
  { "key": "ctrl+alt+t", "action": "none" },
  { "key": "ctrl+alt+n", "action": "none" },
  { "key": "ctrl+alt+w", "action": "none" }
]
```

**Mac Cmd-as-Ctrl** -- makes Command behave like Ctrl for terminal shortcuts on Mac. On Linux this changes nothing (secondary resolves to Ctrl, which already sends control codes natively). On Mac, Cmd+A sends Ctrl+A (beginning of line) instead of Select All, Cmd+K sends Ctrl+K (kill to end of line), Cmd+R sends Ctrl+R (reverse search), and so on. Cmd+C still copies when text is selected (otherwise sends SIGINT), and Cmd+V still pastes:

```jsonc
[
  // Clipboard: Cmd+C copies when there's a selection, sends Ctrl+C otherwise.
  // Cmd+V pastes. These override the Mac defaults with the same behavior.
  { "key": "secondary+c", "action": "copyOrInterrupt" },
  { "key": "secondary+v", "action": "paste" },

  // Line editing (readline / shell)
  { "key": "secondary+a", "action": "sendKeys", "args": "ctrl+a" },
  { "key": "secondary+e", "action": "sendKeys", "args": "ctrl+e" },
  { "key": "secondary+f", "action": "sendKeys", "args": "ctrl+f" },
  { "key": "secondary+b", "action": "sendKeys", "args": "ctrl+b" },
  { "key": "secondary+d", "action": "sendKeys", "args": "ctrl+d" },
  { "key": "secondary+h", "action": "sendKeys", "args": "ctrl+h" },
  { "key": "secondary+t", "action": "sendKeys", "args": "ctrl+t" },

  // Kill and yank
  { "key": "secondary+k", "action": "sendKeys", "args": "ctrl+k" },
  { "key": "secondary+u", "action": "sendKeys", "args": "ctrl+u" },
  { "key": "secondary+w", "action": "sendKeys", "args": "ctrl+w" },
  { "key": "secondary+y", "action": "sendKeys", "args": "ctrl+y" },

  // History
  { "key": "secondary+r", "action": "sendKeys", "args": "ctrl+r" },
  { "key": "secondary+s", "action": "sendKeys", "args": "ctrl+s" },
  { "key": "secondary+p", "action": "sendKeys", "args": "ctrl+p" },
  { "key": "secondary+n", "action": "sendKeys", "args": "ctrl+n" },
  { "key": "secondary+o", "action": "sendKeys", "args": "ctrl+o" },
  { "key": "secondary+g", "action": "sendKeys", "args": "ctrl+g" },

  // Screen and signals
  { "key": "secondary+l", "action": "sendKeys", "args": "ctrl+l" },
  { "key": "secondary+z", "action": "sendKeys", "args": "ctrl+z" },
  { "key": "secondary+x", "action": "sendKeys", "args": "ctrl+x" },
  { "key": "secondary+q", "action": "sendKeys", "args": "ctrl+q" },

  // Less common (ctrl+i = tab, ctrl+j = newline, ctrl+m = enter)
  { "key": "secondary+i", "action": "sendKeys", "args": "ctrl+i" },
  { "key": "secondary+j", "action": "sendKeys", "args": "ctrl+j" },
  { "key": "secondary+m", "action": "sendKeys", "args": "ctrl+m" }
]
```

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
