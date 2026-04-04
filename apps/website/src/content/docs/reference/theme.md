---
title: theme.jsonc
description: Complete field reference for ~/.config/gmux/theme.jsonc
tableOfContents:
  maxHeadingLevel: 3
---

<!-- Generated from apps/gmux-web/src/settings-schema.ts — edit the schema, then run pnpm generate. -->

:::note
This page is generated from the [validation schema](https://github.com/gmuxapp/gmux/blob/main/apps/gmux-web/src/settings-schema.ts).
:::

Terminal color palette. All fields are optional CSS color strings.
Omitted colors use the built-in defaults shown below.

This file is drop-in compatible with [Windows Terminal themes](https://github.com/mbadolato/iTerm2-Color-Schemes/tree/master/windowsterminal):
`purple`/`brightPurple` are mapped to `magenta`/`brightMagenta`, and the `name` field is ignored.

For guides and examples, see [Configuration](/configuration/#terminal-theme).

### `foreground`

Default text color.

- **Default:** `#d3d8de`

### `background`

Terminal background color.

- **Default:** `#0f141a`

### `cursor`

Cursor color.

- **Default:** `#d3d8de`

### `cursorAccent`

Cursor accent color (text under block cursor).

- **Default:** `#0f141a`

### `selectionBackground`

Selection highlight color.

- **Default:** `#2a3a4acc`

### `selectionForeground`

Text color inside selection.


### `selectionInactiveBackground`

Selection color when terminal is not focused.


### `black`

ANSI black.

- **Default:** `#151b21`

### `red`

ANSI red.

- **Default:** `#c25d66`

### `green`

ANSI green.

- **Default:** `#a3be8c`

### `yellow`

ANSI yellow.

- **Default:** `#ebcb8b`

### `blue`

ANSI blue.

- **Default:** `#81a1c1`

### `magenta`

ANSI magenta.

- **Default:** `#b48ead`

### `cyan`

ANSI cyan.

- **Default:** `#49b8b8`

### `white`

ANSI white.

- **Default:** `#d3d8de`

### `brightBlack`

ANSI bright black.

- **Default:** `#595e63`

### `brightRed`

ANSI bright red.

- **Default:** `#d06c75`

### `brightGreen`

ANSI bright green.

- **Default:** `#b4d19a`

### `brightYellow`

ANSI bright yellow.

- **Default:** `#f0d9a0`

### `brightBlue`

ANSI bright blue.

- **Default:** `#93b3d1`

### `brightMagenta`

ANSI bright magenta.

- **Default:** `#c9a3c4`

### `brightCyan`

ANSI bright cyan.

- **Default:** `#5fcece`

### `brightWhite`

ANSI bright white.

- **Default:** `#eceff4`

### `purple`

Alias for `magenta` (Windows Terminal compat).


### `brightPurple`

Alias for `brightMagenta` (Windows Terminal compat).


### `name`

Theme name (ignored, present in Windows Terminal theme files).

