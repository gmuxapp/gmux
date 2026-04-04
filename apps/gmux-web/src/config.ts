/**
 * User-configurable terminal settings.
 *
 * Three config files live in ~/.config/gmux/:
 *   - host.toml       — gmuxd behavior (port, network, tailscale)
 *   - settings.jsonc   — frontend preferences (terminal options, keybinds, UI prefs)
 *   - theme.jsonc      — terminal color palette (drop-in Windows Terminal theme compat)
 *
 * gmuxd reads the JSONC files from disk and serves them via GET /v1/frontend-config.
 * This module fetches, validates, merges with defaults, and produces resolved
 * objects ready for xterm.js and the keyboard handler.
 */
import type { ITerminalOptions, ITheme } from '@xterm/xterm'

// ── Platform detection ──

/**
 * Platform-aware "secondary" modifier: resolves to Cmd (meta) on macOS/iOS,
 * Ctrl on everything else. Lets users write cross-platform keybinds like
 * "secondary+alt+t" that do the right thing on each OS.
 */
export const IS_MAC = typeof navigator !== 'undefined' &&
  /mac|iphone|ipad|ipod/i.test(navigator.platform ?? '')
export const SECONDARY_MOD: 'meta' | 'ctrl' = IS_MAC ? 'meta' : 'ctrl'

// ── Theme config (colors only, WT theme compat) ──

/**
 * Color keys accepted in theme.jsonc.
 * Accepts both xterm.js names (magenta/brightMagenta) and Windows Terminal
 * names (purple/brightPurple) for drop-in compatibility with theme collections
 * like iTerm2-Color-Schemes' windowsterminal/ directory.
 */
export interface ThemeColors extends Partial<ITheme> {
  /** Windows Terminal compat alias for magenta. */
  purple?: string
  /** Windows Terminal compat alias for brightMagenta. */
  brightPurple?: string
  /** Windows Terminal theme name (ignored). */
  name?: string
}

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

/**
 * Normalize a theme colors object: map Windows Terminal names to xterm.js
 * names and strip non-ITheme keys.
 */
export function normalizeThemeColors(raw: ThemeColors): Partial<ITheme> {
  const { name: _name, purple, brightPurple, ...rest } = raw
  if (purple && !rest.magenta) rest.magenta = purple
  if (brightPurple && !rest.brightMagenta) rest.brightMagenta = brightPurple
  return rest
}

// ── Settings config (terminal options + keybinds + UI prefs) ──

/** User-facing schema for settings.jsonc. All fields optional; merged over defaults. */
export interface SettingsConfig {
  fontSize?: number
  fontFamily?: string
  fontWeight?: FontWeight
  fontWeightBold?: FontWeight
  lineHeight?: number
  letterSpacing?: number
  cursorStyle?: 'block' | 'underline' | 'bar'
  cursorBlink?: boolean
  cursorInactiveStyle?: 'outline' | 'block' | 'bar' | 'underline' | 'none'
  cursorWidth?: number
  scrollback?: number
  scrollSensitivity?: number
  fastScrollSensitivity?: number
  smoothScrollDuration?: number
  drawBoldTextInBrightColors?: boolean
  minimumContrastRatio?: number
  macOptionIsMeta?: boolean
  macCommandIsCtrl?: boolean
  wordSeparator?: string
  keybinds?: Keybind[]
}

type FontWeight = 'normal' | 'bold' | '100' | '200' | '300' | '400' | '500' | '600' | '700' | '800' | '900'

const DEFAULT_TERMINAL_OPTIONS: Required<Omit<SettingsConfig, 'keybinds' | 'macCommandIsCtrl'>> = {
  fontSize: 13,
  fontFamily: "'Fira Code', monospace",
  fontWeight: 'normal' as FontWeight,
  fontWeightBold: 'bold' as FontWeight,
  lineHeight: 1.0,
  letterSpacing: 0,
  cursorStyle: 'block',
  cursorBlink: true,
  cursorInactiveStyle: 'outline',
  cursorWidth: 1,
  scrollback: 5000,
  scrollSensitivity: 1,
  fastScrollSensitivity: 5,
  smoothScrollDuration: 0,
  drawBoldTextInBrightColors: true,
  minimumContrastRatio: 1,
  macOptionIsMeta: false,
  wordSeparator: ' ()[]{}\',"`:;',
}

/** Known top-level keys in settings.jsonc. */
const KNOWN_SETTINGS_KEYS = new Set([
  ...Object.keys(DEFAULT_TERMINAL_OPTIONS),
  'keybinds',
  'macCommandIsCtrl',
])

/** Clamp a number to [min, max]. */
function clamp(value: number, min: number, max: number): number {
  return Math.max(min, Math.min(max, value))
}

/**
 * Build a complete ITerminalOptions object from user settings and theme.
 *
 * Merges user-provided terminal options over defaults, normalizes theme
 * colors, and clamps numeric values to safe ranges. Does not include
 * linkHandler or other non-serializable options; the caller adds those.
 */
export function buildTerminalOptions(
  settings: SettingsConfig | null | undefined,
  themeColors: ThemeColors | null | undefined,
): ITerminalOptions {
  // Merge scalar options from settings.
  const merged = { ...DEFAULT_TERMINAL_OPTIONS }
  if (settings) {
    // Warn about unknown keys.
    for (const key of Object.keys(settings)) {
      if (!KNOWN_SETTINGS_KEYS.has(key)) {
        console.warn(`settings.jsonc: unknown key "${key}" (ignored)`)
      }
    }
    for (const key of Object.keys(DEFAULT_TERMINAL_OPTIONS) as (keyof typeof DEFAULT_TERMINAL_OPTIONS)[]) {
      if (key in settings && settings[key] !== undefined) {
        ;(merged as any)[key] = settings[key]
      }
    }
  }

  // Clamp numeric values.
  merged.fontSize = clamp(merged.fontSize, 6, 48)
  merged.scrollback = clamp(merged.scrollback, 0, 100_000)
  merged.cursorWidth = clamp(merged.cursorWidth, 1, 10)
  merged.lineHeight = clamp(merged.lineHeight, 0.5, 3)
  merged.letterSpacing = clamp(merged.letterSpacing, -5, 20)
  merged.minimumContrastRatio = clamp(merged.minimumContrastRatio, 1, 21)

  // Merge theme colors.
  const userColors = themeColors ? normalizeThemeColors(themeColors) : {}
  const theme: ITheme = { ...DEFAULT_THEME_COLORS, ...userColors }

  return { ...merged, theme }
}

// ── Keybinds config ──

export interface Keybind {
  /**
   * Key combo, e.g. "ctrl+alt+t", "shift+enter". Case-insensitive.
   * The virtual modifier "secondary" resolves to Cmd on macOS and Ctrl elsewhere,
   * so "secondary+alt+t" works on both platforms.
   */
  key: string
  /**
   * Action to perform. Known actions:
   *   sendText        — send args as raw text to PTY
   *   sendKeys        — parse args as key combo, send escape sequence
   *   copyOrInterrupt — copy selection if any, else send SIGINT
   *   copy            — copy selection to clipboard (no SIGINT fallback)
   *   paste           — read clipboard and send to PTY
   *   selectAll       — select all terminal content
   *   none            — disable this binding (used to suppress a built-in)
   */
  action: string
  /** Argument for the action (e.g. the text or key combo to send). */
  args?: string
}

export interface ResolvedKeybind extends Keybind {
  /** Parsed: which modifiers are required. */
  ctrl: boolean
  shift: boolean
  alt: boolean
  meta: boolean
  /** Parsed: the base key name (lowercase), e.g. "enter", "c", "t". */
  baseKey: string
}

const KNOWN_ACTIONS = new Set(['sendText', 'sendKeys', 'copyOrInterrupt', 'copy', 'paste', 'selectAll', 'none'])

/**
 * Default keybinds, split by platform.
 *
 * These are the source of truth for all keyboard shortcuts. Every key combo
 * that does something other than "send bytes to the terminal" is listed here.
 * xterm.js only handles terminal input (characters, control codes, escape
 * sequences); all UI and clipboard actions go through this keymap.
 *
 * User keybinds from settings.jsonc are layered on top:
 * same-key entries override, action "none" disables a default.
 */

const UNIVERSAL_KEYBINDS: Keybind[] = [
  { key: 'shift+enter', action: 'sendText', args: '\n' },
  { key: 'ctrl+c', action: 'copyOrInterrupt' },
]

/** Linux / Windows defaults. */
const LINUX_KEYBINDS: Keybind[] = [
  { key: 'ctrl+shift+c', action: 'copy' },
  { key: 'ctrl+v',       action: 'paste' },
  { key: 'ctrl+shift+v', action: 'paste' },
  { key: 'ctrl+alt+t', action: 'sendKeys', args: 'ctrl+t' },
  { key: 'ctrl+alt+n', action: 'sendKeys', args: 'ctrl+n' },
  { key: 'ctrl+alt+w', action: 'sendKeys', args: 'ctrl+w' },
]

/** macOS defaults. Replicate iTerm2 / macOS Terminal conventions. */
const MAC_KEYBINDS: Keybind[] = [
  { key: 'meta+c', action: 'copy' },
  { key: 'meta+v', action: 'paste' },
  { key: 'meta+a', action: 'selectAll' },
  { key: 'meta+left',      action: 'sendKeys', args: 'home' },
  { key: 'meta+right',     action: 'sendKeys', args: 'end' },
  { key: 'meta+backspace', action: 'sendKeys', args: 'ctrl+u' },
  { key: 'meta+k',         action: 'sendKeys', args: 'ctrl+l' },
]

/**
 * Mac navigation keybinds preserved when macCommandIsCtrl is enabled.
 * Cmd+arrow and Cmd+Backspace keep their iTerm2 behavior; only
 * Cmd+<character> is remapped to Ctrl.
 */
const MAC_NAVIGATION_KEYBINDS: Keybind[] = [
  { key: 'meta+left',      action: 'sendKeys', args: 'home' },
  { key: 'meta+right',     action: 'sendKeys', args: 'end' },
  { key: 'meta+backspace', action: 'sendKeys', args: 'ctrl+u' },
]

/**
 * Build the default keybind list for the current platform.
 *
 * When macCommandIsCtrl is true on Mac, the defaults use the Linux clipboard
 * bindings (ctrl+c, ctrl+v, ctrl+shift+c, ctrl+shift+v) so that the
 * meta->ctrl transformation in the keyboard handler matches them. Mac
 * navigation shortcuts (Cmd+Left/Right/Backspace) are preserved.
 */
function buildDefaults(macCommandIsCtrl: boolean): Keybind[] {
  if (IS_MAC && macCommandIsCtrl) {
    return [...UNIVERSAL_KEYBINDS, ...LINUX_KEYBINDS, ...MAC_NAVIGATION_KEYBINDS]
  }
  return [...UNIVERSAL_KEYBINDS, ...(IS_MAC ? MAC_KEYBINDS : LINUX_KEYBINDS)]
}

export const DEFAULT_KEYBINDS: Keybind[] = buildDefaults(false)

/**
 * Parse a key combo string into modifier flags and a base key.
 * e.g. "ctrl+alt+t" -> { ctrl: true, alt: true, shift: false, meta: false, baseKey: "t" }
 */
export function parseKeyCombo(combo: string): { ctrl: boolean; shift: boolean; alt: boolean; meta: boolean; baseKey: string } {
  const parts = combo.toLowerCase().split('+')
  const mods = { ctrl: false, shift: false, alt: false, meta: false }
  let baseKey = ''
  for (const part of parts) {
    if (part === 'ctrl' || part === 'control') mods.ctrl = true
    else if (part === 'shift') mods.shift = true
    else if (part === 'alt') mods.alt = true
    else if (part === 'meta' || part === 'cmd' || part === 'super') mods.meta = true
    else if (part === 'secondary') mods[SECONDARY_MOD] = true
    else baseKey = part
  }
  return { ...mods, baseKey }
}

/**
 * Convert a key combo string to the terminal escape sequence it represents.
 * e.g. "ctrl+t" -> "\x14", "ctrl+c" -> "\x03"
 */
export function keyComboToSequence(combo: string): string {
  const { ctrl, alt, baseKey } = parseKeyCombo(combo)
  let seq = ''

  if (baseKey.length === 1 && ctrl) {
    // Ctrl + letter: ASCII control character (A=1, B=2, ..., Z=26)
    const code = baseKey.toUpperCase().charCodeAt(0)
    if (code >= 65 && code <= 90) {
      seq = String.fromCharCode(code - 64)
    }
  } else if (baseKey === 'enter') {
    seq = '\r'
  } else if (baseKey === 'escape' || baseKey === 'esc') {
    seq = '\x1b'
  } else if (baseKey === 'tab') {
    seq = '\t'
  } else if (baseKey === 'backspace') {
    seq = '\x7f'
  } else if (baseKey === 'home') {
    seq = '\x1b[H'
  } else if (baseKey === 'end') {
    seq = '\x1b[F'
  } else if (baseKey === 'delete' || baseKey === 'del') {
    seq = '\x1b[3~'
  } else if (baseKey === 'up' || baseKey === 'arrowup') {
    seq = '\x1b[A'
  } else if (baseKey === 'down' || baseKey === 'arrowdown') {
    seq = '\x1b[B'
  } else if (baseKey === 'right' || baseKey === 'arrowright') {
    seq = '\x1b[C'
  } else if (baseKey === 'left' || baseKey === 'arrowleft') {
    seq = '\x1b[D'
  } else if (baseKey === 'pageup' || baseKey === 'page_up') {
    seq = '\x1b[5~'
  } else if (baseKey === 'pagedown' || baseKey === 'page_down') {
    seq = '\x1b[6~'
  } else if (baseKey === 'insert' || baseKey === 'ins') {
    seq = '\x1b[2~'
  } else if (baseKey.length === 1) {
    seq = baseKey
  }

  // Alt prefix: ESC before the character.
  if (alt && seq) {
    seq = '\x1b' + seq
  }

  return seq
}

/**
 * Merge user keybinds with built-in defaults.
 * User entries override built-ins with the same normalized key.
 * Returns fully resolved keybinds (with parsed modifiers).
 *
 * When macCommandIsCtrl is true, the defaults are adjusted for Mac so
 * that Cmd+character events (transformed to Ctrl by the keyboard handler)
 * match the Linux-style ctrl bindings.
 */
export function resolveKeybinds(
  user: Keybind[] | null | undefined,
  macCommandIsCtrl = false,
): ResolvedKeybind[] {
  // Build a map of keybinds, keyed by normalized combo.
  const map = new Map<string, Keybind>()

  // Defaults first (platform-aware).
  for (const kb of buildDefaults(macCommandIsCtrl)) {
    map.set(normalizeKeyString(kb.key), kb)
  }

  // User overrides.
  if (user) {
    for (const kb of user) {
      if (!kb.key || !kb.action) {
        console.warn('settings.jsonc keybinds: entry missing "key" or "action", skipping', kb)
        continue
      }
      if (!KNOWN_ACTIONS.has(kb.action)) {
        console.warn(`settings.jsonc keybinds: unknown action "${kb.action}" for key "${kb.key}"`)
      }
      map.set(normalizeKeyString(kb.key), kb)
    }
  }

  // Resolve and filter out "none" actions.
  const result: ResolvedKeybind[] = []
  for (const kb of map.values()) {
    if (kb.action === 'none') continue
    const parsed = parseKeyCombo(kb.key)
    result.push({ ...kb, ...parsed })
  }
  return result
}

const MODIFIER_NAMES = new Set(['ctrl', 'control', 'shift', 'alt', 'meta', 'cmd', 'super', 'secondary'])

/**
 * Normalize a key string for deduplication.
 * Canonicalizes modifier aliases (control->ctrl, cmd/super->meta), sorts
 * modifiers alphabetically, and lowercases everything.
 * e.g. "Ctrl+Alt+T" and "alt+ctrl+t" both become "alt+ctrl+t"
 * Handles non-standard ordering like "enter+shift" -> "shift+enter".
 */
function normalizeKeyString(key: string): string {
  const parts = key.toLowerCase().split('+')
  const mods: string[] = []
  let baseKey = ''
  for (const part of parts) {
    if (part === 'ctrl' || part === 'control' || (part === 'secondary' && SECONDARY_MOD === 'ctrl')) mods.push('ctrl')
    else if (part === 'meta' || part === 'cmd' || part === 'super' || (part === 'secondary' && SECONDARY_MOD === 'meta')) mods.push('meta')
    else if (MODIFIER_NAMES.has(part)) mods.push(part)
    else baseKey = part
  }
  mods.sort()
  return [...mods, baseKey].join('+')
}

/**
 * Short key names used in keybind configs mapped to their KeyboardEvent.key
 * values (lowercased). Lets users write "meta+left" instead of "meta+arrowleft".
 *
 * Every alias accepted by keyComboToSequence (for sendKeys) must also be
 * listed here so that eventMatchesKeybind can match the corresponding
 * KeyboardEvent. Without an entry, a keybind using the alias would produce
 * the correct escape sequence but never fire because the event key name
 * (e.g. "escape") wouldn't match the alias (e.g. "esc").
 */
const KEY_ALIASES: Record<string, string> = {
  left: 'arrowleft',
  right: 'arrowright',
  up: 'arrowup',
  down: 'arrowdown',
  esc: 'escape',
  del: 'delete',
  ins: 'insert',
  page_up: 'pageup',
  page_down: 'pagedown',
}

/**
 * Test whether a KeyboardEvent matches a resolved keybind.
 */
export function eventMatchesKeybind(ev: KeyboardEvent, kb: ResolvedKeybind): boolean {
  if (ev.ctrlKey !== kb.ctrl) return false
  if (ev.shiftKey !== kb.shift) return false
  if (ev.altKey !== kb.alt) return false
  if (ev.metaKey !== kb.meta) return false
  const expected = KEY_ALIASES[kb.baseKey] ?? kb.baseKey
  return ev.key.toLowerCase() === expected
}

// ── Fetching ──

export interface FrontendConfig {
  settings: SettingsConfig | null
  themeColors: ThemeColors | null
}

/**
 * Fetch frontend config from the backend.
 * Returns nulls for missing files (the caller merges with defaults).
 */
export async function fetchFrontendConfig(): Promise<FrontendConfig> {
  try {
    const resp = await fetch('/v1/frontend-config')
    if (!resp.ok) return { settings: null, themeColors: null }
    const json = await resp.json()
    const data = json.data ?? {}
    return {
      settings: data.settings ?? null,
      themeColors: data.theme ?? null,
    }
  } catch {
    return { settings: null, themeColors: null }
  }
}
