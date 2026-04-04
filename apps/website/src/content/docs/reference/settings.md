---
title: settings.jsonc
description: Complete field reference for ~/.config/gmux/settings.jsonc
tableOfContents:
  maxHeadingLevel: 4
---

<!-- Generated from apps/gmux-web/src/settings-schema.ts — edit the schema, then run pnpm generate. -->

:::note
This page is generated from the [validation schema](https://github.com/gmuxapp/gmux/blob/main/apps/gmux-web/src/settings-schema.ts).
:::

All fields are optional. Missing fields use the defaults shown below.
Numeric values are clamped to their valid range (not rejected).

For guides and examples, see [Configuration](/configuration/#frontend-settings).

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
- **Default:** <code>"block"</code>

### `cursorBlink`

Whether the cursor blinks.

- **Type:** `boolean`
- **Default:** `true`

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
- **Default:** `true`

### `minimumContrastRatio`

Minimum contrast ratio for text. 1 disables contrast adjustment.

- **Type:** `number`
- **Default:** `1`
- **Range:** 1–21

### `macOptionIsMeta`

Treat the macOS Option key as Meta (sends ESC prefix). When false, Option produces special characters.

- **Type:** `boolean`
- **Default:** `false`

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
