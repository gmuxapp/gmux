/**
 * Valibot schemas for settings.jsonc and theme.jsonc.
 *
 * These schemas are the single source of truth for:
 *   - Field names, types, defaults, and valid ranges
 *   - Descriptions (used by the doc generator)
 *   - Runtime validation and clamping
 *
 * The backend (gmuxd) serves these files as raw JSON; all semantic
 * validation happens here in the frontend.
 */
import * as v from 'valibot'
import type { ITerminalOptions, ITheme } from '@xterm/xterm'

// ── Helpers ──

/** Clamp a number to [min, max]. */
function clamp(n: number, min: number, max: number): number {
  return Math.max(min, Math.min(max, n))
}

/**
 * An optional number field with a default, valid range, and description.
 * Out-of-range values are clamped (not rejected) for user-friendliness.
 * The min/max are stored in metadata for doc generation.
 */
function clampedNumber(def: number, min: number, max: number, desc: string) {
  return v.optional(
    v.pipe(
      v.number(),
      v.description(desc),
      v.metadata({ min, max }),
      v.transform((n: number) => clamp(n, min, max)),
    ),
    def,
  )
}

/** An optional string field with a default and description. */
function optStr(def: string, desc: string) {
  return v.optional(
    v.pipe(v.string(), v.description(desc)),
    def,
  )
}

/** An optional boolean field with a default and description. */
function optBool(def: boolean, desc: string) {
  return v.optional(
    v.pipe(v.boolean(), v.description(desc)),
    def,
  )
}

// ── Font weight ──

const FONT_WEIGHTS = [
  'normal', 'bold',
  '100', '200', '300', '400', '500', '600', '700', '800', '900',
] as const

export const FontWeightSchema = v.picklist(FONT_WEIGHTS)

// ── Theme colors schema (theme.jsonc) ──

/**
 * Terminal color palette. Accepts xterm.js ITheme keys plus Windows Terminal
 * aliases (purple/brightPurple) for drop-in compatibility with theme
 * collections like iTerm2-Color-Schemes.
 *
 * The `name` field (present in WT theme files) is accepted and stripped.
 */
export const ThemeColorsSchema = v.pipe(
  v.object({
    // Standard xterm.js color keys.
    foreground:          v.optional(v.pipe(v.string(), v.description('Default text color.'))),
    background:          v.optional(v.pipe(v.string(), v.description('Terminal background color.'))),
    cursor:              v.optional(v.pipe(v.string(), v.description('Cursor color.'))),
    cursorAccent:        v.optional(v.pipe(v.string(), v.description('Cursor accent color (text under block cursor).'))),
    selectionBackground: v.optional(v.pipe(v.string(), v.description('Selection highlight color.'))),
    selectionForeground: v.optional(v.pipe(v.string(), v.description('Text color inside selection.'))),
    selectionInactiveBackground: v.optional(v.pipe(v.string(), v.description('Selection color when terminal is not focused.'))),

    // ANSI normal colors.
    black:   v.optional(v.pipe(v.string(), v.description('ANSI black.'))),
    red:     v.optional(v.pipe(v.string(), v.description('ANSI red.'))),
    green:   v.optional(v.pipe(v.string(), v.description('ANSI green.'))),
    yellow:  v.optional(v.pipe(v.string(), v.description('ANSI yellow.'))),
    blue:    v.optional(v.pipe(v.string(), v.description('ANSI blue.'))),
    magenta: v.optional(v.pipe(v.string(), v.description('ANSI magenta.'))),
    cyan:    v.optional(v.pipe(v.string(), v.description('ANSI cyan.'))),
    white:   v.optional(v.pipe(v.string(), v.description('ANSI white.'))),

    // ANSI bright colors.
    brightBlack:   v.optional(v.pipe(v.string(), v.description('ANSI bright black.'))),
    brightRed:     v.optional(v.pipe(v.string(), v.description('ANSI bright red.'))),
    brightGreen:   v.optional(v.pipe(v.string(), v.description('ANSI bright green.'))),
    brightYellow:  v.optional(v.pipe(v.string(), v.description('ANSI bright yellow.'))),
    brightBlue:    v.optional(v.pipe(v.string(), v.description('ANSI bright blue.'))),
    brightMagenta: v.optional(v.pipe(v.string(), v.description('ANSI bright magenta.'))),
    brightCyan:    v.optional(v.pipe(v.string(), v.description('ANSI bright cyan.'))),
    brightWhite:   v.optional(v.pipe(v.string(), v.description('ANSI bright white.'))),

    // Windows Terminal compatibility aliases.
    purple:       v.optional(v.pipe(v.string(), v.description('Alias for `magenta` (Windows Terminal compat).'))),
    brightPurple: v.optional(v.pipe(v.string(), v.description('Alias for `brightMagenta` (Windows Terminal compat).'))),

    // WT theme files include a name field; accept and strip it.
    name: v.optional(v.pipe(v.string(), v.description('Theme name (ignored, present in Windows Terminal theme files).'))),
  }),
  v.description('Terminal color palette. Drop in a Windows Terminal theme and it works.'),
)

export type ThemeColors = v.InferInput<typeof ThemeColorsSchema>

// ── Keybind schema ──

export const KEYBIND_ACTIONS = [
  'sendText', 'sendKeys', 'copyOrInterrupt', 'copy', 'paste', 'selectAll', 'none',
] as const

/** Per-action descriptions. Source of truth for runtime warnings and doc generation. */
export const KEYBIND_ACTION_DESCRIPTIONS: Record<string, string> = {
  sendText:         'Send `args` as literal text to the PTY.',
  sendKeys:         'Parse `args` as a key combo and send its escape sequence (e.g. `"ctrl+t"` sends `^T`).',
  copyOrInterrupt:  'Copy selection if text is selected, otherwise send SIGINT (`^C`).',
  copy:             'Copy selection to clipboard. Does nothing if no text is selected.',
  paste:            'Read system clipboard and send contents to the PTY.',
  selectAll:        'Select all terminal content.',
  none:             'Disable this key combo (removes a built-in default).',
}

export const KeybindSchema = v.object({
  key: v.pipe(
    v.string(),
    v.description(
      'Key combo, e.g. "ctrl+alt+t", "shift+enter". Case-insensitive. ' +
      'Modifiers: ctrl, shift, alt, meta (or cmd/super). ' +
      'The virtual modifier "secondary" resolves to Cmd on macOS and Ctrl elsewhere.',
    ),
  ),
  action: v.pipe(
    v.string(),
    v.description('Action to perform.'),
    v.metadata({ values: KEYBIND_ACTION_DESCRIPTIONS }),
  ),
  args: v.optional(v.pipe(
    v.string(),
    v.description('Argument for the action (e.g. the text or key combo to send).'),
  )),
})

export type Keybind = v.InferOutput<typeof KeybindSchema>

// ── Settings schema (settings.jsonc) ──

/** Field definitions for settings.jsonc, separated for introspection. */
const settingsEntries = {
    fontSize:         clampedNumber(13, 6, 48, 'Terminal font size in pixels.'),
    fontFamily:       optStr("'Fira Code', monospace", 'Font family (CSS font-family value).'),
    fontWeight:       v.optional(v.pipe(FontWeightSchema, v.description('Font weight for normal text.')), 'normal' as const),
    fontWeightBold:   v.optional(v.pipe(FontWeightSchema, v.description('Font weight for bold text.')), 'bold' as const),
    lineHeight:       clampedNumber(1.0, 0.5, 3, 'Line height multiplier.'),
    letterSpacing:    clampedNumber(0, -5, 20, 'Extra letter spacing in pixels.'),
    cursorStyle:      v.optional(
                        v.pipe(v.picklist(['block', 'underline', 'bar']), v.description('Cursor shape.')),
                        'block' as const,
                      ),
    cursorBlink:      optBool(true, 'Whether the cursor blinks.'),
    cursorInactiveStyle: v.optional(
                           v.pipe(
                             v.picklist(['outline', 'block', 'bar', 'underline', 'none']),
                             v.description('Cursor style when the terminal is not focused.'),
                           ),
                           'outline' as const,
                         ),
    cursorWidth:      clampedNumber(1, 1, 10, 'Cursor width in pixels (only applies to bar cursor).'),
    scrollback:       clampedNumber(5000, 0, 100_000, 'Maximum number of lines kept in the scrollback buffer.'),
    scrollSensitivity: clampedNumber(1, 0.1, 10, 'Scroll speed multiplier for mouse wheel.'),
    fastScrollSensitivity: clampedNumber(5, 1, 50, 'Scroll speed multiplier when holding Alt.'),
    smoothScrollDuration: clampedNumber(0, 0, 500, 'Smooth scroll animation duration in milliseconds. 0 disables.'),
    drawBoldTextInBrightColors: optBool(true, 'Whether to render bold text in bright ANSI colors.'),
    minimumContrastRatio: clampedNumber(1, 1, 21, 'Minimum contrast ratio for text. 1 disables contrast adjustment.'),
    macOptionIsMeta:  optBool(false, 'Treat the macOS Option key as Meta (sends ESC prefix). When false, Option produces special characters.'),
    wordSeparator:    optStr(' ()[]{}\',"`:;', 'Characters treated as word boundaries for double-click selection.'),

    // Keybinds (validated structurally; semantic validation in keybinds.ts).
    keybinds: v.optional(v.pipe(
      v.array(KeybindSchema),
      v.description('Key-to-action mappings. Layered on top of platform-specific defaults.'),
    )),

    macCommandIsCtrl: v.optional(v.pipe(
      v.boolean(),
      v.description(
        'On macOS, remap every Cmd+character to its Ctrl equivalent. ' +
        'Cmd+arrow/backspace keep their navigation behavior. ' +
        'When enabled, define keybinds with ctrl (not cmd/meta/secondary) ' +
        'since Cmd events are transformed before matching.',
      ),
    )),
} as const

export const SettingsSchema = v.pipe(
  v.object(settingsEntries),
  v.description('Frontend preferences: terminal options, keybinds, and UI settings.'),
)

export type SettingsConfig = v.InferInput<typeof SettingsSchema>
export type ResolvedSettings = v.InferOutput<typeof SettingsSchema>

// ── Default theme colors ──

export const DEFAULT_THEME_COLORS: ITheme = {
  background: '#0f141a',
  foreground: '#d3d8de',
  cursor: '#d3d8de',
  cursorAccent: '#0f141a',
  selectionBackground: '#2a3a4acc',
  black: '#151b21',
  red: '#c25d66',
  green: '#a3be8c',
  yellow: '#ebcb8b',
  blue: '#81a1c1',
  magenta: '#b48ead',
  cyan: '#49b8b8',
  white: '#d3d8de',
  brightBlack: '#595e63',
  brightRed: '#d06c75',
  brightGreen: '#b4d19a',
  brightYellow: '#f0d9a0',
  brightBlue: '#93b3d1',
  brightMagenta: '#c9a3c4',
  brightCyan: '#5fcece',
  brightWhite: '#eceff4',
}

// ── Normalization and building ──

/**
 * Normalize a theme colors object: map Windows Terminal names to xterm.js
 * names and strip non-ITheme keys (name, purple, brightPurple).
 */
export function normalizeThemeColors(raw: ThemeColors): Partial<ITheme> {
  const { name: _name, purple, brightPurple, ...rest } = raw
  if (purple && !rest.magenta) rest.magenta = purple
  if (brightPurple && !rest.brightMagenta) rest.brightMagenta = brightPurple
  return rest
}

/**
 * Warn about unknown keys in the user's settings.jsonc.
 * Called before parsing so the user sees which keys were ignored.
 */
function warnUnknownKeys(raw: Record<string, unknown>): void {
  const known = new Set(Object.keys(settingsEntries))
  for (const key of Object.keys(raw)) {
    if (!known.has(key)) {
      console.warn(`settings.jsonc: unknown key "${key}" (ignored)`)
    }
  }
}

/**
 * Build a complete ITerminalOptions object from user settings and theme.
 *
 * Parses and validates settings via the valibot schema (which fills defaults
 * and clamps numeric values), normalizes theme colors, and merges both into
 * an object ready for `new Terminal(...)`.
 *
 * Does not include linkHandler or other non-serializable options; the caller
 * adds those separately.
 */
export function buildTerminalOptions(
  rawSettings: SettingsConfig | null | undefined,
  rawTheme: ThemeColors | null | undefined,
): ITerminalOptions {
  // Warn about unknown keys before parsing (parse strips them).
  if (rawSettings && typeof rawSettings === 'object') {
    warnUnknownKeys(rawSettings as Record<string, unknown>)
  }

  // Parse settings through the schema: fills defaults, clamps numbers.
  const result = v.safeParse(SettingsSchema, rawSettings ?? {})
  const settings = result.success ? result.output : v.parse(SettingsSchema, {})

  if (!result.success) {
    console.warn('settings.jsonc: validation errors, using defaults.', result.issues)
  }

  // Merge theme colors.
  const userColors = rawTheme ? normalizeThemeColors(rawTheme) : {}
  const theme: ITheme = { ...DEFAULT_THEME_COLORS, ...userColors }

  // Build ITerminalOptions from parsed settings (exclude non-terminal keys).
  const { keybinds: _kb, macCommandIsCtrl: _mac, ...terminalOpts } = settings
  return { ...terminalOpts, theme }
}
