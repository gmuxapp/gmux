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

// ── Page generation ──

function generateSettingsPage(): string {
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

  renderEntries(SettingsSchema, lines)

  return lines.join('\n')
}

function generateThemePage(): string {
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
