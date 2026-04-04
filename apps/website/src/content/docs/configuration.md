---
title: Configuration
description: Config file, environment variables, and file paths.
---

gmux works out of the box with no configuration. This page covers guides and examples for everything you can customize. For complete field listings, see the [Reference](/reference/host-toml/) pages.

## Host config

`~/.config/gmux/host.toml` ([field reference](/reference/host-toml/))

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

`~/.config/gmux/theme.jsonc` ([field reference](/reference/theme/))

Terminal color palette. This file uses the same format as [Windows Terminal themes](https://github.com/mbadolato/iTerm2-Color-Schemes/tree/master/windowsterminal), so you can drop one in and it works: `purple`/`brightPurple` are automatically mapped to `magenta`/`brightMagenta`, and the `name` field is ignored.

```jsonc
{
  "background": "#282a36",
  "foreground": "#f8f8f2",
  "cursor": "#f8f8f2",
  "selectionBackground": "#44475a",
  "black": "#21222c",
  "red": "#ff5555",
  "green": "#50fa7b",
  "yellow": "#f1fa8c",
  "blue": "#bd93f9",
  "purple": "#ff79c6",   // mapped to magenta
  "cyan": "#8be9fd",
  "white": "#f8f8f2"
  // ... any xterm.js ITheme color keys
}
```

All fields are optional; omitted colors use the built-in default palette.

## Frontend settings

`~/.config/gmux/settings.jsonc` ([field reference](/reference/settings/))

Terminal options, keybinds, and other frontend preferences. All fields are optional.

```jsonc
{
  // Terminal appearance (non-color options; colors go in theme.jsonc)
  "fontSize": 14,
  "fontFamily": "'JetBrains Mono', monospace",
  "cursorStyle": "bar",
  "cursorBlink": true,
  "scrollback": 10000,

  // Keybind overrides
  "keybinds": [
    { "key": "ctrl+alt+t", "action": "sendKeys", "args": "ctrl+t" },
    { "key": "ctrl+alt+g", "action": "sendText", "args": "git status\r" },
    { "key": "ctrl+alt+w", "action": "none" }
  ]
}
```

Numeric values are clamped to safe ranges (e.g. fontSize 6-48, scrollback 0-100,000). Unknown keys produce a console warning.

### Keybinds

gmux ships a complete default keymap that is the source of truth for every keyboard shortcut. Every key combo that does something other than "send bytes to the terminal" is listed explicitly; nothing relies on implicit browser or xterm.js passthrough.

Your `keybinds` array layers on top: same-key entries override the defaults, and the `none` action disables a default.

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

These are ready to paste into the `keybinds` array in `~/.config/gmux/settings.jsonc`.

**Quick commands** -- bind key combos to common shell commands:

```jsonc
{ "keybinds": [
  { "key": "ctrl+alt+g", "action": "sendText", "args": "git status\r" },
  { "key": "ctrl+alt+d", "action": "sendText", "args": "git diff\r" },
  { "key": "ctrl+alt+l", "action": "sendText", "args": "git log --oneline -20\r" }
]}
```

**Vim-friendly** -- disable the Ctrl+C copy behavior so Ctrl+C always sends SIGINT (useful if you use visual mode for copying):

```jsonc
{ "keybinds": [
  { "key": "ctrl+c", "action": "none" }
]}
```

**Disable all browser workarounds** -- if you run gmux as a PWA or `--app` window, the browser doesn't steal Ctrl+T/N/W, so the Ctrl+Alt workarounds are unnecessary:

```jsonc
{ "keybinds": [
  { "key": "ctrl+alt+t", "action": "none" },
  { "key": "ctrl+alt+n", "action": "none" },
  { "key": "ctrl+alt+w", "action": "none" }
]}
```

### macCommandIsCtrl

On Mac, Command is the primary modifier, but terminals expect Ctrl. By default gmux maps a handful of Cmd shortcuts (copy, paste, select all, navigation). If you want *every* Cmd+character to send its Ctrl equivalent instead, set `macCommandIsCtrl` in `settings.jsonc`:

```jsonc
{
  "macCommandIsCtrl": true
}
```

With this enabled:

- **Cmd+A** sends Ctrl+A (beginning of line), not Select All
- **Cmd+K** sends Ctrl+K (kill to end of line)
- **Cmd+R** sends Ctrl+R (reverse search)
- **Cmd+C** still copies when text is selected, sends Ctrl+C (SIGINT) otherwise
- **Cmd+V** still pastes
- **Cmd+Shift+C** copies (Ctrl+Shift+C binding from the Linux defaults)
- **Cmd+Shift+A** sends Ctrl+Shift+A (CSI u / Kitty keyboard protocol sequence)
- **Cmd+Left/Right/Backspace** keep their navigation behavior (Home, End, delete to start of line)

Only single-character keys are remapped. Non-character keys (arrows, backspace, function keys) pass through to their normal keybinds. On Linux this option has no effect.

#### Interaction with custom keybinds

When `macCommandIsCtrl` is on, the keyboard handler transforms every Cmd+character event into a virtual Ctrl+character event *before* matching keybinds. This means:

- **`ctrl+a` bindings are what Cmd+A triggers.** Both the physical Ctrl+A and Cmd+A key presses resolve to your `ctrl+a` keybind.
- **`meta+a`, `cmd+a`, and `secondary+a` bindings are unreachable for character keys.** The transformation happens before keybind matching, so the resolved keybind list never sees the original Cmd modifier.
- **Non-character keys are unaffected.** `meta+left`, `meta+backspace`, etc. still match normally because the transform only applies to `ev.key.length === 1`.

In practice: when `macCommandIsCtrl` is on, write your keybinds with `ctrl`, not `cmd`/`meta`/`secondary`:

```jsonc
{
  "macCommandIsCtrl": true,
  "keybinds": [
    // ✓ Cmd+G and Ctrl+G both trigger this
    { "key": "ctrl+g", "action": "sendText", "args": "git status\r" },
    // ✗ Unreachable — Cmd+G is transformed to Ctrl+G before matching
    { "key": "cmd+g", "action": "sendText", "args": "will never fire" }
  ]
}
```

If you define both `ctrl+a` and `meta+a` with `macCommandIsCtrl` on, only the `ctrl+a` binding fires. The `meta+a` entry sits in the resolved list but no event ever matches it.

:::tip
If you want the same keybinds to work on both Mac and Linux, use `ctrl` modifiers and enable `macCommandIsCtrl` on Mac. This gives you a single set of bindings where Cmd on Mac and Ctrl on Linux both work.
:::

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
| `~/.config/gmux/host.toml` | Daemon config (port, tailscale) | User |
| `~/.config/gmux/settings.jsonc` | Frontend preferences (terminal options, keybinds) | User |
| `~/.config/gmux/theme.jsonc` | Terminal color palette (Windows Terminal compat) | User |
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
