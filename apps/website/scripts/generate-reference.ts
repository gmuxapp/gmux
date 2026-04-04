/**
 * Generate reference documentation from valibot schemas.
 *
 * Walks the schema tree recursively: objects become sections, arrays of
 * objects show their item fields as sub-sections, and primitives render
 * their type/default/range/description. No hardcoded field lists needed;
 * structure comes from the schema itself.
 *
 * Run: node --experimental-strip-types scripts/generate-reference.ts
 */
import { writeFileSync } from 'node:fs'
import { resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  SettingsSchema,
  ThemeColorsSchema,
  DEFAULT_THEME_COLORS,
} from '../../gmux-web/src/settings-schema.ts'

const __dirname = dirname(fileURLToPath(import.meta.url))
const DOCS_DIR = resolve(__dirname, '../src/content/docs/reference')
const GENERATED_COMMENT = '<!-- Generated from apps/gmux-web/src/settings-schema.ts — edit the schema, then run pnpm generate. -->\n'
const GENERATED_NOTE = [
  ':::note',
  'This page is generated from the [validation schema](https://github.com/gmuxapp/gmux/blob/main/apps/gmux-web/src/settings-schema.ts).',
  ':::',
  '',
].join('\n')

// ── Schema introspection ──

/**
 * Unwrap a valibot schema, collecting info at each layer.
 *
 * Valibot schemas nest like: optional(pipe(baseType, description, metadata, transform, ...))
 * This function peels the layers and returns a flat bag of properties.
 */
interface SchemaInfo {
  baseType: string
  description: string
  default_: unknown
  min?: number
  max?: number
  options?: readonly string[]
  /** For metadata.values: a map of valid values to their descriptions. */
  valueDocs?: Record<string, string>
  /** For arrays: the item schema (may be an object with .entries). */
  itemSchema?: any
  /** For objects: the entries map. */
  entries?: Record<string, any>
}

function inspect(schema: any): SchemaInfo {
  const info: SchemaInfo = { baseType: 'unknown', description: '', default_: undefined }

  // Unwrap optional
  if (schema.type === 'optional') {
    info.default_ = schema.default
    schema = schema.wrapped
  }

  // Walk pipe (or handle bare schema)
  const items = schema.pipe ?? [schema]
  for (const item of items) {
    switch (item.type) {
      case 'description':
        info.description = item.description
        break
      case 'metadata':
        if (item.metadata?.min !== undefined) info.min = item.metadata.min
        if (item.metadata?.max !== undefined) info.max = item.metadata.max
        if (item.metadata?.values) info.valueDocs = item.metadata.values
        break
      case 'string':
        info.baseType = 'string'
        break
      case 'number':
        info.baseType = 'number'
        break
      case 'boolean':
        info.baseType = 'boolean'
        break
      case 'picklist':
        info.baseType = 'enum'
        info.options = item.options
        break
      case 'array':
        info.baseType = 'array'
        info.itemSchema = item.item
        break
      case 'object':
        info.baseType = 'object'
        info.entries = item.entries
        break
      // transform, metadata, etc. — already handled or irrelevant
    }
  }

  return info
}

/**
 * Get the entries of a top-level schema (which is v.pipe(v.object(...), ...)).
 */
function topLevelEntries(schema: any): Record<string, any> {
  return schema.pipe[0].entries
}

// ── Markdown formatting ──

function formatDefault(val: unknown): string {
  if (val === undefined) return '*(none)*'
  if (typeof val === 'string') {
    const escaped = val
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
    return `<code>"${escaped}"</code>`
  }
  return `\`${JSON.stringify(val)}\``
}

function formatType(info: SchemaInfo): string {
  if (info.options) {
    return info.options.map(o => `\`"${o}"\``).join(' \\| ')
  }
  if (info.baseType === 'array') {
    return '`array` of objects'
  }
  return `\`${info.baseType}\``
}

// ── Recursive renderer ──

/**
 * Render a single schema field and recurse into children.
 *
 * depth controls the heading level: depth 0 = ###, depth 1 = ####, etc.
 * This gives us ### for top-level fields and #### for sub-fields (e.g.
 * keybind entry fields inside the keybinds array).
 */
function renderField(name: string, schema: any, depth: number, lines: string[]): void {
  const info = inspect(schema)
  const hashes = '#'.repeat(depth + 3) // depth 0 → ###, depth 1 → ####

  lines.push(`${hashes} \`${name}\``)
  lines.push('')
  if (info.description) {
    lines.push(info.description)
    lines.push('')
  }

  // Type / default / range metadata
  lines.push(`- **Type:** ${formatType(info)}`)
  if (info.default_ !== undefined) {
    lines.push(`- **Default:** ${formatDefault(info.default_)}`)
  }
  if (info.min !== undefined && info.max !== undefined) {
    lines.push(`- **Range:** ${info.min}–${info.max}`)
  }
  lines.push('')

  // If this field has documented values (e.g. keybind action), render them.
  if (info.valueDocs) {
    lines.push('| Value | Description |')
    lines.push('|-------|-------------|')
    for (const [val, desc] of Object.entries(info.valueDocs)) {
      lines.push(`| \`${val}\` | ${desc} |`)
    }
    lines.push('')
  }

  // Recurse into children: object entries or array item entries.
  const childEntries = info.entries ?? (
    info.itemSchema?.type === 'object' ? info.itemSchema.entries : null
  )
  if (childEntries) {
    if (info.baseType === 'array') {
      lines.push('Each entry has these fields:')
      lines.push('')
    }
    for (const [childName, childSchema] of Object.entries(childEntries)) {
      renderField(childName, childSchema, depth + 1, lines)
    }
  }
}

/**
 * Render all entries of a top-level schema.
 */
function renderEntries(schema: any, lines: string[]): void {
  const entries = topLevelEntries(schema)
  for (const [name, fieldSchema] of Object.entries(entries)) {
    renderField(name, fieldSchema, 0, lines)
  }
}

// ── Page prose (lives here so schema + docs change together) ──

const SETTINGS_EXAMPLE = `\
## Example

\`\`\`jsonc
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
    { "key": "ctrl+alt+g", "action": "sendText", "args": "git status\\r" },
    { "key": "ctrl+alt+w", "action": "none" }
  ]
}
\`\`\`
`

const KEYBINDS_GUIDE = `\
## Keybinds guide

gmux ships a complete default keymap. Every key combo that does something other than "send bytes to the terminal" is listed explicitly; nothing relies on implicit browser or xterm.js passthrough.

Your \`keybinds\` array layers on top: same-key entries override the defaults, and the \`none\` action disables a default. See [Keyboard shortcuts](/using-the-ui#keyboard-shortcuts) for the full default keymap.

### Key format

Key combos are case-insensitive and support these modifiers: \`ctrl\`, \`shift\`, \`alt\`, \`meta\` (or \`cmd\`/\`super\`). Modifier order doesn't matter: \`ctrl+alt+t\` and \`Alt+Ctrl+T\` are the same.

Supported key names: \`enter\`, \`escape\` (\`esc\`), \`tab\`, \`backspace\`, \`home\`, \`end\`, \`delete\` (\`del\`), \`insert\` (\`ins\`), \`pageup\` (\`page_up\`), \`pagedown\` (\`page_down\`), \`left\`, \`right\`, \`up\`, \`down\`.

### The \`secondary\` modifier

The virtual modifier \`secondary\` resolves to **Cmd** on macOS and **Ctrl** everywhere else. Useful for cross-platform configs:

\`\`\`jsonc
{ "key": "secondary+alt+t", "action": "sendKeys", "args": "ctrl+t" }
\`\`\`

Note: \`secondary\` works well for keys that do the same thing on both platforms. For copy/paste it is less useful because the shortcuts differ (Ctrl+Shift+C on Linux vs. Cmd+C on Mac), so the defaults handle each platform separately.

### macCommandIsCtrl

On Mac, Command is the primary modifier, but terminals expect Ctrl. By default gmux maps a handful of Cmd shortcuts (copy, paste, select all, navigation). If you want *every* Cmd+character to send its Ctrl equivalent instead, set \`macCommandIsCtrl\`:

\`\`\`jsonc
{
  "macCommandIsCtrl": true
}
\`\`\`

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

When \`macCommandIsCtrl\` is on, the keyboard handler transforms every Cmd+character event into a virtual Ctrl+character event *before* matching keybinds. This means:

- **\`ctrl+a\` bindings are what Cmd+A triggers.** Both the physical Ctrl+A and Cmd+A key presses resolve to your \`ctrl+a\` keybind.
- **\`meta+a\`, \`cmd+a\`, and \`secondary+a\` bindings are unreachable for character keys.** The transformation happens before keybind matching, so the resolved keybind list never sees the original Cmd modifier.
- **Non-character keys are unaffected.** \`meta+left\`, \`meta+backspace\`, etc. still match normally because the transform only applies to \`ev.key.length === 1\`.

In practice: when \`macCommandIsCtrl\` is on, write your keybinds with \`ctrl\`, not \`cmd\`/\`meta\`/\`secondary\`:

\`\`\`jsonc
{
  "macCommandIsCtrl": true,
  "keybinds": [
    // ✓ Cmd+G and Ctrl+G both trigger this
    { "key": "ctrl+g", "action": "sendText", "args": "git status\\r" },
    // ✗ Unreachable — Cmd+G is transformed to Ctrl+G before matching
    { "key": "cmd+g", "action": "sendText", "args": "will never fire" }
  ]
}
\`\`\`

:::tip
If you want the same keybinds to work on both Mac and Linux, use \`ctrl\` modifiers and enable \`macCommandIsCtrl\` on Mac. This gives you a single set of bindings where Cmd on Mac and Ctrl on Linux both work.
:::

### Starter templates

These are ready to paste into \`settings.jsonc\`.

**Quick commands** -- bind key combos to common shell commands:

\`\`\`jsonc
{
  "keybinds": [
    { "key": "ctrl+alt+g", "action": "sendText", "args": "git status\\r" },
    { "key": "ctrl+alt+d", "action": "sendText", "args": "git diff\\r" },
    { "key": "ctrl+alt+l", "action": "sendText", "args": "git log --oneline -20\\r" }
  ]
}
\`\`\`

**Vim-friendly** -- disable the Ctrl+C copy behavior so Ctrl+C always sends SIGINT (useful if you use visual mode for copying):

\`\`\`jsonc
{
  "keybinds": [
    { "key": "ctrl+c", "action": "none" }
  ]
}
\`\`\`

**Disable all browser workarounds** -- if you run gmux as a PWA or \`--app\` window, the browser doesn't steal Ctrl+T/N/W, so the Ctrl+Alt workarounds are unnecessary:

\`\`\`jsonc
{
  "keybinds": [
    { "key": "ctrl+alt+t", "action": "none" },
    { "key": "ctrl+alt+n", "action": "none" },
    { "key": "ctrl+alt+w", "action": "none" }
  ]
}
\`\`\`
`

// ── Page generation ──

function generateSettingsPage(): string {
  const lines: string[] = []

  lines.push(`---`)
  lines.push(`title: settings.jsonc`)
  lines.push(`description: Reference for ~/.config/gmux/settings.jsonc — terminal options, keybinds, and UI preferences.`)
  lines.push(`tableOfContents:`)
  lines.push(`  maxHeadingLevel: 4`)
  lines.push(`---`)
  lines.push('')
  lines.push(GENERATED_COMMENT)
  lines.push(GENERATED_NOTE)
  lines.push('`~/.config/gmux/settings.jsonc` (or `$XDG_CONFIG_HOME/gmux/settings.jsonc`)')
  lines.push('')
  lines.push('Terminal options, keybinds, and frontend preferences. All fields are optional.')
  lines.push('Missing fields use the defaults shown below. Numeric values are clamped to')
  lines.push('their valid range (not rejected). Unknown keys produce a console warning.')
  lines.push('')
  lines.push(SETTINGS_EXAMPLE)

  lines.push('## Fields')
  lines.push('')
  renderEntries(SettingsSchema, lines)

  lines.push(KEYBINDS_GUIDE)

  return lines.join('\n')
}

function generateThemePage(): string {
  const defaults = DEFAULT_THEME_COLORS as Record<string, string | undefined>
  const lines: string[] = []

  lines.push(`---`)
  lines.push(`title: theme.jsonc`)
  lines.push(`description: Reference for ~/.config/gmux/theme.jsonc — terminal color palette.`)
  lines.push(`tableOfContents:`)
  lines.push(`  maxHeadingLevel: 3`)
  lines.push(`---`)
  lines.push('')
  lines.push(GENERATED_COMMENT)
  lines.push(GENERATED_NOTE)
  lines.push('`~/.config/gmux/theme.jsonc` (or `$XDG_CONFIG_HOME/gmux/theme.jsonc`)')
  lines.push('')
  lines.push('Terminal color palette. All fields are optional CSS color strings.')
  lines.push('Omitted colors use the built-in defaults shown below.')
  lines.push('')
  lines.push('This file is drop-in compatible with [Windows Terminal themes](https://github.com/mbadolato/iTerm2-Color-Schemes/tree/master/windowsterminal):')
  lines.push('`purple`/`brightPurple` are mapped to `magenta`/`brightMagenta`, and the `name` field is ignored.')
  lines.push('')
  lines.push('## Example')
  lines.push('')
  lines.push('```jsonc')
  lines.push('{')
  lines.push('  "background": "#282a36",')
  lines.push('  "foreground": "#f8f8f2",')
  lines.push('  "cursor": "#f8f8f2",')
  lines.push('  "selectionBackground": "#44475a",')
  lines.push('  "black": "#21222c",')
  lines.push('  "red": "#ff5555",')
  lines.push('  "green": "#50fa7b",')
  lines.push('  "yellow": "#f1fa8c",')
  lines.push('  "blue": "#bd93f9",')
  lines.push('  "purple": "#ff79c6",   // mapped to magenta')
  lines.push('  "cyan": "#8be9fd",')
  lines.push('  "white": "#f8f8f2"')
  lines.push('}')
  lines.push('```')
  lines.push('')

  lines.push('## Fields')
  lines.push('')

  // Theme is flat (all optional strings), so we render each field with
  // its default color from DEFAULT_THEME_COLORS where available.
  const entries = topLevelEntries(ThemeColorsSchema)
  for (const [name, schema] of Object.entries(entries)) {
    const info = inspect(schema)
    const def = defaults[name]

    lines.push(`### \`${name}\``)
    lines.push('')
    if (info.description) {
      lines.push(info.description)
      lines.push('')
    }
    if (def) {
      lines.push(`- **Default:** \`${def}\``)
    }
    lines.push('')
  }

  return lines.join('\n')
}

// ── Main ──

function main() {
  const settings = generateSettingsPage()
  const theme = generateThemePage()

  writeFileSync(resolve(DOCS_DIR, 'settings.md'), settings)
  writeFileSync(resolve(DOCS_DIR, 'theme.md'), theme)

  console.log('Generated reference docs:')
  console.log(`  ${resolve(DOCS_DIR, 'settings.md')}`)
  console.log(`  ${resolve(DOCS_DIR, 'theme.md')}`)
}

main()
