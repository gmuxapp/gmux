/**
 * Keybind resolution, key parsing, and event matching.
 *
 * This module takes the raw keybind entries from settings.jsonc, layers them
 * over platform-specific defaults, and produces a resolved list that the
 * keyboard handler (keyboard.ts) iterates on each keydown.
 */
import type { Keybind } from './settings-schema'
import { KEYBIND_ACTIONS } from './settings-schema'

// ── Platform detection ──

/**
 * Detect Apple platforms.
 *
 * Prefers the modern `navigator.userAgentData.platform` (available in
 * Chromium 93+) and falls back to the deprecated `navigator.platform`
 * for Safari and older browsers.
 */
function detectMac(): boolean {
  if (typeof navigator === 'undefined') return false
  const platform =
    (navigator as { userAgentData?: { platform?: string } }).userAgentData?.platform
    ?? navigator.platform
    ?? ''
  return /mac|iphone|ipad|ipod/i.test(platform)
}

export const IS_MAC = detectMac()
export const SECONDARY_MOD: 'meta' | 'ctrl' = IS_MAC ? 'meta' : 'ctrl'

// ── Resolved keybind ──

export interface ResolvedKeybind extends Keybind {
  /** Parsed: which modifiers are required. */
  ctrl: boolean
  shift: boolean
  alt: boolean
  meta: boolean
  /** Parsed: the base key name (lowercase), e.g. "enter", "c", "t". */
  baseKey: string
}

// ── Default keybinds ──

const KNOWN_ACTIONS = new Set<string>(KEYBIND_ACTIONS)

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
  // Ctrl+Backspace/Delete: explicitly intercepted so the browser does not
  // swallow the event on xterm's hidden textarea. Sequences match what
  // gnome-terminal, xterm, and alacritty send.
  { key: 'ctrl+backspace', action: 'sendText', args: '\x08' },
  { key: 'ctrl+delete',    action: 'sendText', args: '\x1b[3;5~' },
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

// ── Key combo parsing ──

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

// ── Key combo to escape sequence ──

/**
 * Compute the xterm modifier parameter for a key combo.
 * Returns 0 when no modifiers are active (meaning: omit the parameter).
 * The encoding follows the xterm CSI convention: param = 1 + modifier bits,
 * where Shift=1, Alt=2, Ctrl=4. (Meta is not used in terminal CSI sequences.)
 */
function modifierParam(ctrl: boolean, shift: boolean, alt: boolean): number {
  const bits = (shift ? 1 : 0) | (alt ? 2 : 0) | (ctrl ? 4 : 0)
  return bits === 0 ? 0 : 1 + bits
}

/**
 * Apply a modifier parameter to a CSI letter sequence (e.g. \x1b[A).
 * Unmodified: \x1b[A. Modified: \x1b[1;5A (ctrl+up).
 */
function modifyCSILetter(letter: string, mod: number): string {
  if (mod === 0) return `\x1b[${letter}`
  return `\x1b[1;${mod}${letter}`
}

/**
 * Apply a modifier parameter to a CSI tilde sequence (e.g. \x1b[3~).
 * Unmodified: \x1b[N~. Modified: \x1b[N;5~ (ctrl+delete).
 */
function modifyCSITilde(n: number, mod: number): string {
  if (mod === 0) return `\x1b[${n}~`
  return `\x1b[${n};${mod}~`
}

/**
 * Convert a key combo string to the terminal escape sequence it represents.
 *
 * Single letters with ctrl produce traditional ASCII control codes (ctrl+c -> \x03).
 * Special keys (arrows, home, end, delete, etc.) use standard CSI encoding with
 * modifier parameters when ctrl, shift, or alt are present:
 *   ctrl+up    -> \x1b[1;5A
 *   ctrl+delete -> \x1b[3;5~
 *   shift+home  -> \x1b[1;2H
 */
export function keyComboToSequence(combo: string): string {
  const { ctrl, shift, alt, baseKey } = parseKeyCombo(combo)
  const mod = modifierParam(ctrl, shift, alt)

  // CSI-encoded keys: modifiers (including alt) are carried in the CSI
  // parameter, so no ESC prefix is needed. Return early for these.
  switch (baseKey) {
    case 'home':      return modifyCSILetter('H', mod)
    case 'end':       return modifyCSILetter('F', mod)
    case 'up':        case 'arrowup':    return modifyCSILetter('A', mod)
    case 'down':      case 'arrowdown':  return modifyCSILetter('B', mod)
    case 'right':     case 'arrowright': return modifyCSILetter('C', mod)
    case 'left':      case 'arrowleft':  return modifyCSILetter('D', mod)
    case 'delete':    case 'del':        return modifyCSITilde(3, mod)
    case 'pageup':    case 'page_up':    return modifyCSITilde(5, mod)
    case 'pagedown':  case 'page_down':  return modifyCSITilde(6, mod)
    case 'insert':    case 'ins':        return modifyCSITilde(2, mod)
  }

  // Simple sequences: alt is encoded as an ESC prefix (the traditional
  // approach for characters, backspace, tab, etc.).
  let seq = ''

  if (baseKey.length === 1 && ctrl) {
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
    // Ctrl+Backspace: \x08 (BS), matching gnome-terminal/xterm/alacritty.
    // Plain backspace: \x7f (DEL).
    seq = ctrl ? '\x08' : '\x7f'
  } else if (baseKey.length === 1) {
    seq = baseKey
  }

  if (alt && seq) {
    seq = '\x1b' + seq
  }

  return seq
}

// ── Keybind resolution ──

const MODIFIER_NAMES = new Set(['ctrl', 'control', 'shift', 'alt', 'meta', 'cmd', 'super', 'secondary'])

/**
 * Normalize a key string for deduplication.
 * Separates modifiers from the base key using the same modifier list as
 * parseKeyCombo, canonicalizes aliases, sorts modifiers alphabetically.
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
  const map = new Map<string, Keybind>()

  for (const kb of buildDefaults(macCommandIsCtrl)) {
    map.set(normalizeKeyString(kb.key), kb)
  }

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

  const result: ResolvedKeybind[] = []
  for (const kb of map.values()) {
    if (kb.action === 'none') continue
    const parsed = parseKeyCombo(kb.key)
    result.push({ ...kb, ...parsed })
  }
  return result
}

// ── Event matching ──

/**
 * Short key names used in keybind configs mapped to their KeyboardEvent.key
 * values (lowercased). Lets users write "meta+left" instead of "meta+arrowleft".
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
