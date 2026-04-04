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

export const IS_MAC = typeof navigator !== 'undefined' &&
  /mac|iphone|ipad|ipod/i.test(navigator.platform ?? '')
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
 * Convert a key combo string to the terminal escape sequence it represents.
 * e.g. "ctrl+t" -> "\x14", "ctrl+c" -> "\x03"
 */
export function keyComboToSequence(combo: string): string {
  const { ctrl, alt, baseKey } = parseKeyCombo(combo)
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
