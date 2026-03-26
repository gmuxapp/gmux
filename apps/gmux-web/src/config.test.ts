import { describe, it, expect, vi } from 'vitest'
import {
  mergeThemeConfig,
  normalizeThemeColors,
  resolveKeybinds,
  parseKeyCombo,
  keyComboToSequence,
  eventMatchesKeybind,
  DEFAULT_THEME_COLORS,
  DEFAULT_KEYBINDS,
  SECONDARY_MOD,
  type Keybind,
  type ResolvedKeybind,
} from './config'

// ── Theme merging ──

describe('mergeThemeConfig', () => {
  it('returns full defaults when given null', () => {
    const opts = mergeThemeConfig(null)
    expect(opts.fontSize).toBe(13)
    expect(opts.fontFamily).toBe("'Fira Code', monospace")
    expect(opts.cursorBlink).toBe(true)
    expect(opts.scrollback).toBe(5000)
    expect(opts.theme).toEqual(DEFAULT_THEME_COLORS)
  })

  it('returns full defaults when given undefined', () => {
    const opts = mergeThemeConfig(undefined)
    expect(opts.fontSize).toBe(13)
  })

  it('returns full defaults when given empty object', () => {
    const opts = mergeThemeConfig({})
    expect(opts.fontSize).toBe(13)
    expect(opts.theme).toEqual(DEFAULT_THEME_COLORS)
  })

  it('overrides only specified fields', () => {
    const opts = mergeThemeConfig({ fontSize: 16, cursorBlink: false })
    expect(opts.fontSize).toBe(16)
    expect(opts.cursorBlink).toBe(false)
    // Others remain default.
    expect(opts.fontFamily).toBe("'Fira Code', monospace")
    expect(opts.scrollback).toBe(5000)
  })

  it('deep-merges theme colors', () => {
    const opts = mergeThemeConfig({ theme: { background: '#000000' } })
    const theme = opts.theme as Record<string, unknown>
    expect(theme.background).toBe('#000000')
    // Other colors preserved.
    expect(theme.foreground).toBe(DEFAULT_THEME_COLORS.foreground)
    expect(theme.red).toBe(DEFAULT_THEME_COLORS.red)
  })

  it('clamps fontSize to valid range', () => {
    expect(mergeThemeConfig({ fontSize: 2 }).fontSize).toBe(6)
    expect(mergeThemeConfig({ fontSize: 100 }).fontSize).toBe(48)
  })

  it('clamps scrollback to valid range', () => {
    expect(mergeThemeConfig({ scrollback: -10 }).scrollback).toBe(0)
    expect(mergeThemeConfig({ scrollback: 999999 }).scrollback).toBe(100_000)
  })

  it('clamps lineHeight to valid range', () => {
    expect(mergeThemeConfig({ lineHeight: 0.1 }).lineHeight).toBe(0.5)
    expect(mergeThemeConfig({ lineHeight: 5 }).lineHeight).toBe(3)
  })

  it('clamps minimumContrastRatio to valid range', () => {
    expect(mergeThemeConfig({ minimumContrastRatio: 0 }).minimumContrastRatio).toBe(1)
    expect(mergeThemeConfig({ minimumContrastRatio: 50 }).minimumContrastRatio).toBe(21)
  })

  it('normalizes purple to magenta when theme comes via mergeThemeConfig', () => {
    const opts = mergeThemeConfig({
      theme: { purple: '#ff79c6', brightPurple: '#ff92df', background: '#282a36' },
    })
    const theme = opts.theme as Record<string, unknown>
    expect(theme.magenta).toBe('#ff79c6')
    expect(theme.brightMagenta).toBe('#ff92df')
    expect(theme.background).toBe('#282a36')
    // purple/brightPurple should not leak into the ITheme object
    expect(theme.purple).toBeUndefined()
    expect(theme.brightPurple).toBeUndefined()
  })

  it('warns about unknown keys', () => {
    const spy = vi.spyOn(console, 'warn').mockImplementation(() => {})
    mergeThemeConfig({ bogusKey: 42 } as any)
    expect(spy).toHaveBeenCalledWith(expect.stringContaining('unknown key "bogusKey"'))
    spy.mockRestore()
  })
})

// ── Theme color normalization ──

describe('normalizeThemeColors', () => {
  it('passes through standard xterm.js keys unchanged', () => {
    const colors = normalizeThemeColors({ magenta: '#ff00ff', brightMagenta: '#ff88ff' })
    expect(colors.magenta).toBe('#ff00ff')
    expect(colors.brightMagenta).toBe('#ff88ff')
  })

  it('maps purple to magenta (Windows Terminal compat)', () => {
    const colors = normalizeThemeColors({ purple: '#ff00ff', brightPurple: '#ff88ff' })
    expect(colors.magenta).toBe('#ff00ff')
    expect(colors.brightMagenta).toBe('#ff88ff')
    expect((colors as any).purple).toBeUndefined()
    expect((colors as any).brightPurple).toBeUndefined()
  })

  it('prefers explicit magenta over purple alias', () => {
    const colors = normalizeThemeColors({ magenta: '#aaa', purple: '#bbb' })
    expect(colors.magenta).toBe('#aaa')
  })

  it('strips the name field', () => {
    const colors = normalizeThemeColors({ name: 'Dracula', background: '#282a36' } as any)
    expect((colors as any).name).toBeUndefined()
    expect(colors.background).toBe('#282a36')
  })

  it('handles a full Windows Terminal theme', () => {
    const wt = {
      name: '3024 Night',
      black: '#090300',
      red: '#db2d20',
      green: '#01a252',
      yellow: '#fded02',
      blue: '#01a0e4',
      purple: '#a16a94',
      cyan: '#b5e4f4',
      white: '#a5a2a2',
      brightBlack: '#5c5855',
      brightRed: '#e8bbd0',
      brightGreen: '#47413f',
      brightYellow: '#4a4543',
      brightBlue: '#807d7c',
      brightPurple: '#d6d5d4',
      brightCyan: '#cdab53',
      brightWhite: '#f7f7f7',
      background: '#090300',
      foreground: '#a5a2a2',
      cursorColor: '#a5a2a2',
      selectionBackground: '#4a4543',
    }
    const colors = normalizeThemeColors(wt)
    expect(colors.magenta).toBe('#a16a94')
    expect(colors.brightMagenta).toBe('#d6d5d4')
    expect(colors.black).toBe('#090300')
    expect((colors as any).name).toBeUndefined()
    expect((colors as any).purple).toBeUndefined()
  })
})

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

  it('resolves "secondary" to the platform-specific modifier', () => {
    const r = parseKeyCombo('secondary+c')
    // In the test environment (jsdom, non-Mac), secondary resolves to ctrl.
    expect(r[SECONDARY_MOD]).toBe(true)
    expect(r.baseKey).toBe('c')
  })

  it('resolves "secondary" with other modifiers', () => {
    const r = parseKeyCombo('secondary+alt+t')
    expect(r[SECONDARY_MOD]).toBe(true)
    expect(r.alt).toBe(true)
    expect(r.baseKey).toBe('t')
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
    // alt+ctrl+a = ESC + \x01
    expect(keyComboToSequence('alt+ctrl+a')).toBe('\x1b\x01')
  })

  it('resolves secondary in sendKeys args (non-Mac: secondary = ctrl)', () => {
    // "secondary+t" should produce the same sequence as "ctrl+t" on non-Mac.
    expect(keyComboToSequence('secondary+t')).toBe(keyComboToSequence(`${SECONDARY_MOD}+t`))
  })

  it('returns empty string for unrecognized named keys', () => {
    // Keys like "f1", "arrowup" have no simple escape sequence mapping.
    // The function should return empty rather than something wrong.
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
    expect(keys).toContain('secondary+alt+t')
  })

  it('adds new user keybinds', () => {
    const user: Keybind[] = [
      { key: 'ctrl+alt+n', action: 'sendKeys', args: 'ctrl+n' },
    ]
    const resolved = resolveKeybinds(user)
    expect(resolved.length).toBe(DEFAULT_KEYBINDS.length + 1)
    const added = resolved.find(r => r.baseKey === 'n')
    expect(added).toBeDefined()
    expect(added!.ctrl).toBe(true)
    expect(added!.alt).toBe(true)
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
    // The default "secondary+alt+t" resolves to ctrl+alt+t in tests (non-Mac).
    // Disabling via the resolved form should work.
    const user: Keybind[] = [
      { key: 'ctrl+alt+t', action: 'none' },
    ]
    const resolved = resolveKeybinds(user)
    const match = resolved.find(r => r.baseKey === 't' && r.ctrl && r.alt)
    expect(match).toBeUndefined()
  })

  it('normalizes key order for matching', () => {
    // "Alt+Ctrl+T" should override "ctrl+alt+t"
    const user: Keybind[] = [
      { key: 'Alt+Ctrl+T', action: 'sendText', args: 'test' },
    ]
    const resolved = resolveKeybinds(user)
    const match = resolved.find(r => r.baseKey === 't' && r.ctrl && r.alt)
    expect(match).toBeDefined()
    expect(match!.action).toBe('sendText')
  })

  it('resolves "secondary" to platform modifier in defaults', () => {
    // The default "secondary+alt+t" should resolve to ctrl+alt+t on non-Mac.
    const resolved = resolveKeybinds(null)
    const match = resolved.find(r => r.baseKey === 't' && r.alt)
    expect(match).toBeDefined()
    expect(match![SECONDARY_MOD]).toBe(true)
  })

  it('allows user to override default "secondary" keybind with resolved form', () => {
    // User writes "ctrl+alt+t" (the resolved form) to override the default "secondary+alt+t".
    const user: Keybind[] = [
      { key: 'ctrl+alt+t', action: 'sendText', args: 'custom' },
    ]
    const resolved = resolveKeybinds(user)
    const matches = resolved.filter(r => r.baseKey === 't' && r.ctrl && r.alt)
    expect(matches.length).toBe(1)
    expect(matches[0].action).toBe('sendText')
  })

  it('canonicalizes modifier aliases (control = ctrl, cmd = meta)', () => {
    // "control+c" must override the default "ctrl+c" binding
    const user: Keybind[] = [
      { key: 'control+c', action: 'sendText', args: 'overridden' },
    ]
    const resolved = resolveKeybinds(user)
    const ctrlC = resolved.filter(r => r.baseKey === 'c' && r.ctrl)
    expect(ctrlC.length).toBe(1)
    expect(ctrlC[0].action).toBe('sendText')
    expect(ctrlC[0].args).toBe('overridden')
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
    // "enter+shift" should still override the default "shift+enter"
    const user: Keybind[] = [
      { key: 'enter+shift', action: 'sendText', args: 'overridden' },
    ]
    const resolved = resolveKeybinds(user)
    const match = resolved.find(r => r.baseKey === 'enter' && r.shift)
    expect(match).toBeDefined()
    expect(match!.args).toBe('overridden')
    // Should have replaced, not added alongside the default.
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

  it('matches secondary+c against the platform modifier', () => {
    const kb = makeKeybind('secondary+c')
    // In test env (non-Mac), secondary = ctrl.
    const modKey = SECONDARY_MOD === 'meta' ? 'metaKey' : 'ctrlKey'
    expect(eventMatchesKeybind(makeEvent({ [modKey]: true, key: 'c' }), kb)).toBe(true)
    // The other modifier should not match.
    const otherKey = SECONDARY_MOD === 'meta' ? 'ctrlKey' : 'metaKey'
    expect(eventMatchesKeybind(makeEvent({ [otherKey]: true, key: 'c' }), kb)).toBe(false)
  })
})
