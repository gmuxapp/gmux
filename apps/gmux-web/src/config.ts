/**
 * User-configurable terminal settings.
 *
 * Three config files live in ~/.config/gmux/:
 *   - config.toml     — gmuxd behavior (port, network, tailscale)
 *   - theme.jsonc      — terminal appearance (colors, font, cursor, scrollback)
 *   - keybinds.jsonc   — key → action mappings
 *
 * gmuxd reads the JSONC files from disk and serves them via GET /v1/terminal-config.
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
const IS_MAC = typeof navigator !== 'undefined' &&
  /mac|iphone|ipad|ipod/i.test(navigator.platform ?? '')
export const SECONDARY_MOD: 'meta' | 'ctrl' = IS_MAC ? 'meta' : 'ctrl'

// ── Theme config ──

/** User-facing schema for theme.jsonc. All fields optional; merged over defaults. */
export interface ThemeConfig {
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
  wordSeparator?: string
  theme?: ThemeColors
}

type FontWeight = 'normal' | 'bold' | '100' | '200' | '300' | '400' | '500' | '600' | '700' | '800' | '900'

/**
 * Color keys accepted in the "theme" object.
 * Accepts both xterm.js names (magenta/brightMagenta) and Windows Terminal
 * names (purple/brightPurple) for drop-in compatibility with theme collections
 * like iTerm2-Color-Schemes' windowsterminal/ directory.
 */
interface ThemeColors extends Partial<ITheme> {
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

const DEFAULT_THEME_CONFIG: Required<Omit<ThemeConfig, 'theme'>> = {
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

/** Known top-level keys in theme.jsonc. */
const KNOWN_THEME_KEYS = new Set([
  ...Object.keys(DEFAULT_THEME_CONFIG),
  'theme',
])

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

/** Clamp a number to [min, max]. */
function clamp(value: number, min: number, max: number): number {
  return Math.max(min, Math.min(max, value))
}

/**
 * Merge user theme config over defaults, producing a complete ITerminalOptions
 * object ready to pass to `new Terminal(...)`.
 *
 * Does not include linkHandler or other non-serializable options; the caller
 * adds those separately.
 */
export function mergeThemeConfig(user: ThemeConfig | null | undefined): ITerminalOptions {
  if (!user) {
    return { ...DEFAULT_THEME_CONFIG, theme: { ...DEFAULT_THEME_COLORS } }
  }

  // Warn about unknown keys.
  for (const key of Object.keys(user)) {
    if (!KNOWN_THEME_KEYS.has(key)) {
      console.warn(`theme.jsonc: unknown key "${key}" (ignored)`)
    }
  }

  // Merge scalar options.
  const merged = { ...DEFAULT_THEME_CONFIG }
  for (const key of Object.keys(DEFAULT_THEME_CONFIG) as (keyof typeof DEFAULT_THEME_CONFIG)[]) {
    if (key in user && user[key] !== undefined) {
      ;(merged as any)[key] = user[key]
    }
  }

  // Clamp numeric values.
  merged.fontSize = clamp(merged.fontSize, 6, 48)
  merged.scrollback = clamp(merged.scrollback, 0, 100_000)
  merged.cursorWidth = clamp(merged.cursorWidth, 1, 10)
  merged.lineHeight = clamp(merged.lineHeight, 0.5, 3)
  merged.letterSpacing = clamp(merged.letterSpacing, -5, 20)
  merged.minimumContrastRatio = clamp(merged.minimumContrastRatio, 1, 21)

  // Deep-merge theme colors.
  const userColors = user.theme ? normalizeThemeColors(user.theme) : {}
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
 * User keybinds from ~/.config/gmux/keybinds.jsonc are layered on top:
 * same-key entries override, action "none" disables a default.
 */

const UNIVERSAL_KEYBINDS: Keybind[] = [
  { key: 'shift+enter', action: 'sendText', args: '\n' },
  { key: 'ctrl+c', action: 'copyOrInterrupt' },
]

/** Linux / Windows defaults. */
const LINUX_KEYBINDS: Keybind[] = [
  // Clipboard: standard Linux terminal shortcuts (GNOME Terminal, Konsole,
  // Alacritty, Windows Terminal). Ctrl+V is intercepted here rather than
  // left to the browser passthrough so that xterm.js does not send \x16
  // (quoted-insert) to the PTY before the paste content arrives.
  { key: 'ctrl+shift+c', action: 'copy' },
  { key: 'ctrl+v',       action: 'paste' },
  { key: 'ctrl+shift+v', action: 'paste' },

  // Chrome/Firefox reserve Ctrl+T/N/W for tab management; they cannot be
  // intercepted by JavaScript.  Ctrl+Alt+<key> is the conventional workaround.
  { key: 'ctrl+alt+t', action: 'sendKeys', args: 'ctrl+t' },
  { key: 'ctrl+alt+n', action: 'sendKeys', args: 'ctrl+n' },
  { key: 'ctrl+alt+w', action: 'sendKeys', args: 'ctrl+w' },
]

/** macOS defaults. Replicate iTerm2 / macOS Terminal conventions. */
const MAC_KEYBINDS: Keybind[] = [
  // Clipboard and selection: explicit bindings replace the implicit xterm.js
  // passthrough chain (keydown → browser clipboard DOM event → xterm handler)
  // so that every shortcut is visible, overridable, and consistently handled.
  { key: 'meta+c', action: 'copy' },
  { key: 'meta+v', action: 'paste' },
  { key: 'meta+a', action: 'selectAll' },

  // Navigation: Cmd+arrow produces Home/End (iTerm2 convention).
  // Without these, Cmd+Left navigates the browser back.
  { key: 'meta+left',      action: 'sendKeys', args: 'home' },
  { key: 'meta+right',     action: 'sendKeys', args: 'end' },
  { key: 'meta+backspace', action: 'sendKeys', args: 'ctrl+u' },
  { key: 'meta+k',         action: 'sendKeys', args: 'ctrl+l' },
]

export const DEFAULT_KEYBINDS: Keybind[] = [
  ...UNIVERSAL_KEYBINDS,
  ...(IS_MAC ? MAC_KEYBINDS : LINUX_KEYBINDS),
]

/**
 * Parse a key combo string into modifier flags and a base key.
 * e.g. "ctrl+alt+t" → { ctrl: true, alt: true, shift: false, meta: false, baseKey: "t" }
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
 * e.g. "ctrl+t" → "\x14", "ctrl+c" → "\x03"
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
 */
export function resolveKeybinds(user: Keybind[] | null | undefined): ResolvedKeybind[] {
  // Build a map of keybinds, keyed by normalized combo.
  const map = new Map<string, Keybind>()

  // Defaults first.
  for (const kb of DEFAULT_KEYBINDS) {
    map.set(normalizeKeyString(kb.key), kb)
  }

  // User overrides.
  if (user) {
    for (const kb of user) {
      if (!kb.key || !kb.action) {
        console.warn('keybinds.jsonc: entry missing "key" or "action", skipping', kb)
        continue
      }
      if (!KNOWN_ACTIONS.has(kb.action)) {
        console.warn(`keybinds.jsonc: unknown action "${kb.action}" for key "${kb.key}"`)
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
 * Canonicalizes modifier aliases (control→ctrl, cmd/super→meta), sorts
 * modifiers alphabetically, and lowercases everything.
 * e.g. "Ctrl+Alt+T" and "alt+ctrl+t" both become "alt+ctrl+t"
 * e.g. "control+c" and "ctrl+c" both become "ctrl+c"
 * Handles non-standard ordering like "enter+shift" → "shift+enter".
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
 * values (lowercased).  Lets users write "meta+left" instead of "meta+arrowleft".
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

export interface TerminalConfig {
  themeConfig: ThemeConfig | null
  keybindsConfig: Keybind[] | null
}

/**
 * Fetch terminal config from the backend.
 * Returns nulls for missing files (the caller merges with defaults).
 */
export async function fetchTerminalConfig(): Promise<TerminalConfig> {
  try {
    const resp = await fetch('/v1/terminal-config')
    if (!resp.ok) return { themeConfig: null, keybindsConfig: null }
    const json = await resp.json()
    const data = json.data ?? {}
    return {
      themeConfig: data.theme ?? null,
      keybindsConfig: data.keybinds ?? null,
    }
  } catch {
    return { themeConfig: null, keybindsConfig: null }
  }
}
