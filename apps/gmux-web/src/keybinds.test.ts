import { describe, it, expect, vi } from 'vitest'
import {
  resolveKeybinds,
  parseKeyCombo,
  keyComboToSequence,
  eventMatchesKeybind,
  DEFAULT_KEYBINDS,
  type ResolvedKeybind,
} from './keybinds'
import type { Keybind } from './settings-schema'

// ── Key combo parsing ──

describe('parseKeyCombo', () => {
  it('parses simple key', () => {
    expect(parseKeyCombo('t')).toEqual({ ctrl: false, shift: false, alt: false, meta: false, baseKey: 't' })
  })

  it('parses ctrl+key', () => {
    const r = parseKeyCombo('ctrl+c')
    expect(r.ctrl).toBe(true)
    expect(r.baseKey).toBe('c')
  })

  it('parses multi-modifier combo', () => {
    const r = parseKeyCombo('Ctrl+Alt+T')
    expect(r.ctrl).toBe(true)
    expect(r.alt).toBe(true)
    expect(r.baseKey).toBe('t')
  })

  it('parses shift+enter', () => {
    const r = parseKeyCombo('shift+enter')
    expect(r.shift).toBe(true)
    expect(r.baseKey).toBe('enter')
  })

  it('treats "control" as ctrl alias', () => {
    expect(parseKeyCombo('control+c').ctrl).toBe(true)
  })

  it('treats "cmd" and "super" as meta', () => {
    expect(parseKeyCombo('cmd+k').meta).toBe(true)
    expect(parseKeyCombo('super+k').meta).toBe(true)
  })
})

// ── Key combo to escape sequence ──

describe('keyComboToSequence', () => {
  it('converts ctrl+letter to control character', () => {
    expect(keyComboToSequence('ctrl+c')).toBe('\x03')
    expect(keyComboToSequence('ctrl+t')).toBe('\x14')
    expect(keyComboToSequence('ctrl+a')).toBe('\x01')
    expect(keyComboToSequence('ctrl+z')).toBe('\x1a')
  })

  it('converts enter to carriage return', () => {
    expect(keyComboToSequence('enter')).toBe('\r')
  })

  it('converts escape', () => {
    expect(keyComboToSequence('escape')).toBe('\x1b')
    expect(keyComboToSequence('esc')).toBe('\x1b')
  })

  it('converts tab', () => {
    expect(keyComboToSequence('tab')).toBe('\t')
  })

  it('converts backspace', () => {
    expect(keyComboToSequence('backspace')).toBe('\x7f')
  })

  it('prefixes with ESC for alt combos', () => {
    expect(keyComboToSequence('alt+b')).toBe('\x1bb')
    expect(keyComboToSequence('alt+f')).toBe('\x1bf')
  })

  it('handles alt+ctrl combo', () => {
    expect(keyComboToSequence('alt+ctrl+a')).toBe('\x1b\x01')
  })

  it('returns empty string for unrecognized named keys', () => {
    expect(keyComboToSequence('f1')).toBe('')
  })
})

// ── Keybind resolution ──

describe('resolveKeybinds', () => {
  it('returns defaults when given null', () => {
    const resolved = resolveKeybinds(null)
    expect(resolved.length).toBe(DEFAULT_KEYBINDS.length)
    const keys = resolved.map(r => r.key)
    expect(keys).toContain('shift+enter')
    expect(keys).toContain('ctrl+c')
    expect(keys).toContain('ctrl+alt+t')
  })

  it('adds new user keybinds', () => {
    const user: Keybind[] = [
      { key: 'ctrl+alt+n', action: 'sendKeys', args: 'ctrl+n' },
    ]
    const resolved = resolveKeybinds(user)
    const added = resolved.find(r => r.baseKey === 'n' && r.ctrl && r.alt)
    expect(added).toBeDefined()
    expect(added!.args).toBe('ctrl+n')
  })

  it('overrides a default keybind', () => {
    const user: Keybind[] = [
      { key: 'ctrl+c', action: 'sendText', args: 'hello' },
    ]
    const resolved = resolveKeybinds(user)
    const ctrlC = resolved.find(r => r.baseKey === 'c' && r.ctrl)
    expect(ctrlC).toBeDefined()
    expect(ctrlC!.action).toBe('sendText')
    expect(ctrlC!.args).toBe('hello')
  })

  it('disables a keybind with action "none"', () => {
    const user: Keybind[] = [
      { key: 'ctrl+alt+t', action: 'none' },
    ]
    const resolved = resolveKeybinds(user)
    const match = resolved.find(r => r.baseKey === 't' && r.ctrl && r.alt)
    expect(match).toBeUndefined()
  })

  it('normalizes key order for matching', () => {
    const user: Keybind[] = [
      { key: 'Alt+Ctrl+T', action: 'sendText', args: 'test' },
    ]
    const resolved = resolveKeybinds(user)
    const match = resolved.find(r => r.baseKey === 't' && r.ctrl && r.alt)
    expect(match).toBeDefined()
    expect(match!.action).toBe('sendText')
  })

  it('warns about entries missing key or action', () => {
    const spy = vi.spyOn(console, 'warn').mockImplementation(() => {})
    resolveKeybinds([{ key: '', action: 'sendText' }])
    expect(spy).toHaveBeenCalled()
    spy.mockRestore()
  })

  it('warns about unknown actions', () => {
    const spy = vi.spyOn(console, 'warn').mockImplementation(() => {})
    resolveKeybinds([{ key: 'ctrl+x', action: 'bogusAction' }])
    expect(spy).toHaveBeenCalledWith(expect.stringContaining('unknown action'))
    spy.mockRestore()
  })

  it('returns defaults when given empty array', () => {
    const resolved = resolveKeybinds([])
    expect(resolved.length).toBe(DEFAULT_KEYBINDS.length)
  })

  it('handles non-standard modifier order (base key first)', () => {
    const user: Keybind[] = [
      { key: 'enter+shift', action: 'sendText', args: 'overridden' },
    ]
    const resolved = resolveKeybinds(user)
    const match = resolved.find(r => r.baseKey === 'enter' && r.shift)
    expect(match).toBeDefined()
    expect(match!.args).toBe('overridden')
    const enterBindings = resolved.filter(r => r.baseKey === 'enter' && r.shift)
    expect(enterBindings.length).toBe(1)
  })
})

// ── Event matching ──

describe('eventMatchesKeybind', () => {
  function makeKeybind(key: string, action = 'sendText'): ResolvedKeybind {
    const parsed = parseKeyCombo(key)
    return { key, action, ...parsed }
  }

  function makeEvent(overrides: Partial<KeyboardEvent>): KeyboardEvent {
    return {
      ctrlKey: false,
      shiftKey: false,
      altKey: false,
      metaKey: false,
      key: '',
      ...overrides,
    } as KeyboardEvent
  }

  it('matches ctrl+c', () => {
    const kb = makeKeybind('ctrl+c')
    expect(eventMatchesKeybind(makeEvent({ ctrlKey: true, key: 'c' }), kb)).toBe(true)
  })

  it('rejects mismatched modifier', () => {
    const kb = makeKeybind('ctrl+c')
    expect(eventMatchesKeybind(makeEvent({ key: 'c' }), kb)).toBe(false)
    expect(eventMatchesKeybind(makeEvent({ ctrlKey: true, altKey: true, key: 'c' }), kb)).toBe(false)
  })

  it('matches shift+enter', () => {
    const kb = makeKeybind('shift+enter')
    expect(eventMatchesKeybind(makeEvent({ shiftKey: true, key: 'Enter' }), kb)).toBe(true)
  })

  it('is case-insensitive on key name', () => {
    const kb = makeKeybind('ctrl+t')
    expect(eventMatchesKeybind(makeEvent({ ctrlKey: true, key: 'T' }), kb)).toBe(true)
  })

  it('resolves key aliases (left -> ArrowLeft)', () => {
    const kb = makeKeybind('meta+left')
    expect(eventMatchesKeybind(makeEvent({ metaKey: true, key: 'ArrowLeft' }), kb)).toBe(true)
    expect(eventMatchesKeybind(makeEvent({ metaKey: true, key: 'left' }), kb)).toBe(false)
  })
})
