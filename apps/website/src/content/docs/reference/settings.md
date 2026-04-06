---
title: settings.jsonc
description: Reference for ~/.config/gmux/settings.jsonc — terminal options, keybinds, and UI preferences.
tableOfContents:
  maxHeadingLevel: 4
---

<!-- Generated from apps/gmux-web/src/settings-schema.ts — edit the schema, then run pnpm generate. -->

:::note
This page is generated from the [validation schema](https://github.com/gmuxapp/gmux/blob/main/apps/gmux-web/src/settings-schema.ts).
:::

`~/.config/gmux/settings.jsonc` (or `$XDG_CONFIG_HOME/gmux/settings.jsonc`)

Terminal options, keybinds, and frontend preferences. All fields are optional.
Missing fields use the defaults shown below. Numeric values are clamped to
their valid range (not rejected). Unknown keys produce a console warning.

## Example

```jsonc
{
  // Terminal appearance (colors go in theme.jsonc)
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

## Fields

### `fontSize`

Terminal font size in pixels.

- **Type:** `number`
- **Default:** `13`
- **Range:** 6–48

### `fontFamily`

Font family (CSS font-family value).

- **Type:** `string`
- **Default:** <code>"'Fira Code', monospace"</code>

### `fontWeight`

Font weight for normal text.

- **Type:** `"normal"` \| `"bold"` \| `"100"` \| `"200"` \| `"300"` \| `"400"` \| `"500"` \| `"600"` \| `"700"` \| `"800"` \| `"900"`
- **Default:** <code>"normal"</code>

### `fontWeightBold`

Font weight for bold text.

- **Type:** `"normal"` \| `"bold"` \| `"100"` \| `"200"` \| `"300"` \| `"400"` \| `"500"` \| `"600"` \| `"700"` \| `"800"` \| `"900"`
- **Default:** <code>"bold"</code>

### `lineHeight`

Line height multiplier.

- **Type:** `number`
- **Default:** `1`
- **Range:** 0.5–3

### `letterSpacing`

Extra letter spacing in pixels.

- **Type:** `number`
- **Default:** `0`
- **Range:** -5–20

### `cursorStyle`

Cursor shape.

- **Type:** `"block"` \| `"underline"` \| `"bar"`
- **Default:** <code>"bar"</code>

### `cursorBlink`

Whether the cursor blinks.

- **Type:** `boolean`
- **Default:** `false`

### `cursorInactiveStyle`

Cursor style when the terminal is not focused.

- **Type:** `"outline"` \| `"block"` \| `"bar"` \| `"underline"` \| `"none"`
- **Default:** <code>"outline"</code>

### `cursorWidth`

Cursor width in pixels (only applies to bar cursor).

- **Type:** `number`
- **Default:** `1`
- **Range:** 1–10

### `scrollback`

Maximum number of lines kept in the scrollback buffer.

- **Type:** `number`
- **Default:** `5000`
- **Range:** 0–100000

### `scrollSensitivity`

Scroll speed multiplier for mouse wheel.

- **Type:** `number`
- **Default:** `1`
- **Range:** 0.1–10

### `fastScrollSensitivity`

Scroll speed multiplier when holding Alt.

- **Type:** `number`
- **Default:** `5`
- **Range:** 1–50

### `smoothScrollDuration`

Smooth scroll animation duration in milliseconds. 0 disables.

- **Type:** `number`
- **Default:** `0`
- **Range:** 0–500

### `drawBoldTextInBrightColors`

Whether to render bold text in bright ANSI colors.

- **Type:** `boolean`
- **Default:** `false`

### `minimumContrastRatio`

Minimum contrast ratio for text. 1 disables contrast adjustment.

- **Type:** `number`
- **Default:** `1`
- **Range:** 1–21

### `macOptionIsMeta`

Treat the macOS Option key as Meta (sends ESC prefix). When false, Option produces special characters like ñ and ß.

- **Type:** `boolean`
- **Default:** `true`

### `wordSeparator`

Characters treated as word boundaries for double-click selection.

- **Type:** `string`
- **Default:** <code>" ()[]{}',&quot;`:;"</code>

### `keybinds`

Key-to-action mappings. Layered on top of platform-specific defaults.

- **Type:** `array` of objects

Each entry has these fields:

#### `key`

Key combo, e.g. "ctrl+alt+t", "shift+enter". Case-insensitive. Modifiers: ctrl, shift, alt, meta (or cmd/super). The virtual modifier "secondary" resolves to Cmd on macOS and Ctrl elsewhere.

- **Type:** `string`

#### `action`

Action to perform.

- **Type:** `string`

| Value | Description |
|-------|-------------|
| `sendText` | Send `args` as literal text to the PTY. |
| `sendKeys` | Parse `args` as a key combo and send its escape sequence (e.g. `"ctrl+t"` sends `^T`). |
| `copyOrInterrupt` | Copy selection if text is selected, otherwise send SIGINT (`^C`). |
| `copy` | Copy selection to clipboard. Does nothing if no text is selected. |
| `paste` | Read system clipboard and send contents to the PTY. |
| `selectAll` | Select all terminal content. |
| `none` | Disable this key combo (removes a built-in default). |

#### `args`

Argument for the action (e.g. the text or key combo to send).

- **Type:** `string`

### `macCommandIsCtrl`

On macOS, remap every Cmd+character to its Ctrl equivalent. Cmd+arrow/backspace keep their navigation behavior. When enabled, define keybinds with ctrl (not cmd/meta/secondary) since Cmd events are transformed before matching.

- **Type:** `boolean`

## Keybinds guide

gmux ships a complete default keymap. Every key combo that does something other than "send bytes to the terminal" is listed explicitly; nothing relies on implicit browser or xterm.js passthrough.

Your `keybinds` array layers on top: same-key entries override the defaults, and the `none` action disables a default. See [Keyboard shortcuts](/using-the-ui#keyboard-shortcuts) for the full default keymap.

### Key format

Key combos are case-insensitive and support these modifiers: `ctrl`, `shift`, `alt`, `meta` (or `cmd`/`super`). Modifier order doesn't matter: `ctrl+alt+t` and `Alt+Ctrl+T` are the same.

Supported key names: `enter`, `escape` (`esc`), `tab`, `backspace`, `home`, `end`, `delete` (`del`), `insert` (`ins`), `pageup` (`page_up`), `pagedown` (`page_down`), `left`, `right`, `up`, `down`.

### The `secondary` modifier

The virtual modifier `secondary` resolves to **Cmd** on macOS and **Ctrl** everywhere else. Useful for cross-platform configs:

```jsonc
{ "key": "secondary+alt+t", "action": "sendKeys", "args": "ctrl+t" }
```

Note: `secondary` works well for keys that do the same thing on both platforms. For copy/paste it is less useful because the shortcuts differ (Ctrl+Shift+C on Linux vs. Cmd+C on Mac), so the defaults handle each platform separately.

### macCommandIsCtrl

On Mac, Command is the primary modifier, but terminals expect Ctrl. By default gmux maps a handful of Cmd shortcuts (copy, paste, select all, navigation). If you want *every* Cmd+character to send its Ctrl equivalent instead, set `macCommandIsCtrl`:

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

:::tip
If you want the same keybinds to work on both Mac and Linux, use `ctrl` modifiers and enable `macCommandIsCtrl` on Mac. This gives you a single set of bindings where Cmd on Mac and Ctrl on Linux both work.
:::

### Starter templates

These are ready to paste into `settings.jsonc`.

**Quick commands** -- bind key combos to common shell commands:

```jsonc
{
  "keybinds": [
    { "key": "ctrl+alt+g", "action": "sendText", "args": "git status\r" },
    { "key": "ctrl+alt+d", "action": "sendText", "args": "git diff\r" },
    { "key": "ctrl+alt+l", "action": "sendText", "args": "git log --oneline -20\r" }
  ]
}
```

**Vim-friendly** -- disable the Ctrl+C copy behavior so Ctrl+C always sends SIGINT (useful if you use visual mode for copying):

```jsonc
{
  "keybinds": [
    { "key": "ctrl+c", "action": "none" }
  ]
}
```

**Disable all browser workarounds** -- if you run gmux as a PWA or `--app` window, the browser doesn't steal Ctrl+T/N/W, so the Ctrl+Alt workarounds are unnecessary:

```jsonc
{
  "keybinds": [
    { "key": "ctrl+alt+t", "action": "none" },
    { "key": "ctrl+alt+n", "action": "none" },
    { "key": "ctrl+alt+w", "action": "none" }
  ]
}
```
