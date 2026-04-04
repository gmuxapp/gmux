/**
 * Generate reference documentation from valibot schemas.
 *
 * Reads the schema objects from apps/gmux-web/src/settings-schema.ts,
 * walks their structure, and emits markdown files into the reference/
 * docs directory.
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
  KEYBIND_ACTIONS,
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

interface FieldInfo {
  name: string
  type: string
  default_: unknown
  description: string
  min?: number
  max?: number
  options?: readonly string[]
}

/**
 * Extract documentation-relevant info from a single schema entry.
 * Handles the valibot shapes: optional(pipe(base, description, metadata, ...))
 */
function extractField(name: string, schema: any): FieldInfo {
  const info: FieldInfo = { name, type: 'unknown', default_: undefined, description: '' }

  // Unwrap optional
  if (schema.type === 'optional') {
    info.default_ = schema.default
    schema = schema.wrapped
  }

  // Walk pipe for description, metadata, base type
  if (schema.pipe) {
    for (const item of schema.pipe) {
      if (item.type === 'description') {
        info.description = item.description
      } else if (item.type === 'metadata' && item.metadata) {
        if (item.metadata.min !== undefined) info.min = item.metadata.min
        if (item.metadata.max !== undefined) info.max = item.metadata.max
      } else if (item.type === 'picklist' && item.options) {
        info.type = 'enum'
        info.options = item.options
      } else if (item.type === 'number') {
        info.type = 'number'
      } else if (item.type === 'string') {
        info.type = 'string'
      } else if (item.type === 'boolean') {
        info.type = 'boolean'
      } else if (item.type === 'array') {
        info.type = 'array'
      }
    }
  } else {
    // No pipe, just a raw schema
    info.type = schema.type ?? 'unknown'
    if (schema.type === 'picklist') {
      info.type = 'enum'
      info.options = schema.options
    }
  }

  return info
}

function extractEntries(schema: any): Record<string, any> {
  // Schema is v.pipe(v.object(entries), v.description(...))
  // pipe[0] is the object schema with .entries
  return schema.pipe[0].entries
}

// ── Markdown formatting ──

function formatDefault(val: unknown): string {
  if (val === undefined) return '*(none)*'
  if (typeof val === 'string') {
    // Use a <code> tag to avoid markdown backtick escaping issues
    // when the value itself contains backticks or special characters.
    const escaped = val
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
    return `<code>"${escaped}"</code>`
  }
  return `\`${JSON.stringify(val)}\``
}

function formatType(info: FieldInfo): string {
  if (info.options) {
    return info.options.map(o => `\`"${o}"\``).join(' \\| ')
  }
  return `\`${info.type}\``
}

function formatRange(info: FieldInfo): string {
  if (info.min !== undefined && info.max !== undefined) {
    return `${info.min}–${info.max}`
  }
  return ''
}

// ── Settings page generation ──

/** Logical groupings for the settings reference page. */
const SETTINGS_GROUPS = [
  {
    title: 'Font',
    fields: ['fontSize', 'fontFamily', 'fontWeight', 'fontWeightBold'],
  },
  {
    title: 'Cursor',
    fields: ['cursorStyle', 'cursorBlink', 'cursorInactiveStyle', 'cursorWidth'],
  },
  {
    title: 'Scrolling',
    fields: ['scrollback', 'scrollSensitivity', 'fastScrollSensitivity', 'smoothScrollDuration'],
  },
  {
    title: 'Text rendering',
    fields: ['lineHeight', 'letterSpacing', 'drawBoldTextInBrightColors', 'minimumContrastRatio'],
  },
  {
    title: 'Input',
    fields: ['macOptionIsMeta', 'wordSeparator'],
  },
  {
    title: 'Keybinds',
    fields: ['keybinds', 'macCommandIsCtrl'],
  },
]

function generateSettingsPage(): string {
  const entries = extractEntries(SettingsSchema)
  const lines: string[] = []

  lines.push(`---`)
  lines.push(`title: settings.jsonc`)
  lines.push(`description: Complete field reference for ~/.config/gmux/settings.jsonc`)
  lines.push(`tableOfContents:`)
  lines.push(`  maxHeadingLevel: 4`)
  lines.push(`---`)
  lines.push('')
  lines.push(GENERATED_COMMENT)
  lines.push(GENERATED_NOTE)
  lines.push('All fields are optional. Missing fields use the defaults shown below.')
  lines.push('Numeric values are clamped to their valid range (not rejected).')
  lines.push('')
  lines.push('For guides and examples, see [Configuration](/configuration/#frontend-settings).')
  lines.push('')

  const documented = new Set<string>()

  for (const group of SETTINGS_GROUPS) {
    lines.push(`## ${group.title}`)
    lines.push('')

    for (const fieldName of group.fields) {
      documented.add(fieldName)
      const schema = entries[fieldName]
      if (!schema) {
        console.warn(`Warning: field "${fieldName}" in group "${group.title}" not found in schema`)
        continue
      }

      const info = extractField(fieldName, schema)
      lines.push(`#### \`${fieldName}\``)
      lines.push('')
      if (info.description) {
        lines.push(info.description)
        lines.push('')
      }

      // Metadata list
      lines.push(`- **Type:** ${formatType(info)}`)
      lines.push(`- **Default:** ${formatDefault(info.default_)}`)
      const range = formatRange(info)
      if (range) {
        lines.push(`- **Range:** ${range}`)
      }
      lines.push('')
    }
  }

  // Safety check: warn about fields not in any group
  for (const name of Object.keys(entries)) {
    if (!documented.has(name)) {
      console.warn(`Warning: settings field "${name}" is not in any documentation group`)
    }
  }

  // Keybind actions reference table
  lines.push('## Keybind actions')
  lines.push('')
  lines.push('These are the valid values for the `action` field in keybind entries.')
  lines.push('')
  lines.push('| Action | Description |')
  lines.push('|--------|-------------|')

  const actionDescriptions: Record<string, string> = {
    sendKeys: 'Parse `args` as a key combo and send its escape sequence (e.g. `"ctrl+t"` sends `^T`).',
    sendText: 'Send `args` as literal text to the PTY.',
    copyOrInterrupt: 'Copy selection if text is selected, otherwise send SIGINT (`^C`).',
    copy: 'Copy selection to clipboard. Does nothing if no text is selected.',
    paste: 'Read system clipboard and send contents to the PTY.',
    selectAll: 'Select all terminal content.',
    none: 'Disable this key combo (removes a built-in default).',
  }

  for (const action of KEYBIND_ACTIONS) {
    const desc = actionDescriptions[action] ?? ''
    lines.push(`| \`${action}\` | ${desc} |`)
  }
  lines.push('')

  return lines.join('\n')
}

// ── Theme page generation ──

const THEME_GROUPS = [
  {
    title: 'General',
    fields: ['foreground', 'background', 'cursor', 'cursorAccent',
             'selectionBackground', 'selectionForeground', 'selectionInactiveBackground'],
  },
  {
    title: 'Normal colors (ANSI 0–7)',
    fields: ['black', 'red', 'green', 'yellow', 'blue', 'magenta', 'cyan', 'white'],
  },
  {
    title: 'Bright colors (ANSI 8–15)',
    fields: ['brightBlack', 'brightRed', 'brightGreen', 'brightYellow',
             'brightBlue', 'brightMagenta', 'brightCyan', 'brightWhite'],
  },
  {
    title: 'Windows Terminal aliases',
    fields: ['purple', 'brightPurple', 'name'],
  },
]

function generateThemePage(): string {
  const entries = extractEntries(ThemeColorsSchema)
  const defaults = DEFAULT_THEME_COLORS as Record<string, string | undefined>
  const lines: string[] = []

  lines.push(`---`)
  lines.push(`title: theme.jsonc`)
  lines.push(`description: Complete field reference for ~/.config/gmux/theme.jsonc`)
  lines.push(`tableOfContents:`)
  lines.push(`  maxHeadingLevel: 3`)
  lines.push(`---`)
  lines.push('')
  lines.push(GENERATED_COMMENT)
  lines.push(GENERATED_NOTE)
  lines.push('Terminal color palette. All fields are optional CSS color strings.')
  lines.push('Omitted colors use the built-in defaults shown below.')
  lines.push('')
  lines.push('This file is drop-in compatible with [Windows Terminal themes](https://github.com/mbadolato/iTerm2-Color-Schemes/tree/master/windowsterminal):')
  lines.push('`purple`/`brightPurple` are mapped to `magenta`/`brightMagenta`, and the `name` field is ignored.')
  lines.push('')
  lines.push('For guides and examples, see [Configuration](/configuration/#terminal-theme).')
  lines.push('')

  for (const group of THEME_GROUPS) {
    lines.push(`### ${group.title}`)
    lines.push('')
    lines.push('| Field | Default | Description |')
    lines.push('|-------|---------|-------------|')

    for (const fieldName of group.fields) {
      const schema = entries[fieldName]
      if (!schema) continue
      const info = extractField(fieldName, schema)
      const def = defaults[fieldName]
      const defCol = def ? `\`${def}\`` : '*(none)*'
      lines.push(`| \`${fieldName}\` | ${defCol} | ${info.description} |`)
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
