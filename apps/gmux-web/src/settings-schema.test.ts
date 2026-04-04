import { describe, it, expect, vi } from 'vitest'
import {
  buildTerminalOptions,
  normalizeThemeColors,
  DEFAULT_THEME_COLORS,
} from './settings-schema'

describe('buildTerminalOptions', () => {
  it('returns full defaults when given nulls', () => {
    const opts = buildTerminalOptions(null, null)
    expect(opts.fontSize).toBe(13)
    expect(opts.fontFamily).toBe("'Fira Code', monospace")
    expect(opts.cursorBlink).toBe(true)
    expect(opts.scrollback).toBe(5000)
    expect(opts.theme).toEqual(DEFAULT_THEME_COLORS)
  })

  it('returns full defaults when given undefined', () => {
    const opts = buildTerminalOptions(undefined, undefined)
    expect(opts.fontSize).toBe(13)
  })

  it('returns full defaults when given empty objects', () => {
    const opts = buildTerminalOptions({}, {})
    expect(opts.fontSize).toBe(13)
    expect(opts.theme).toEqual(DEFAULT_THEME_COLORS)
  })

  it('overrides only specified settings fields', () => {
    const opts = buildTerminalOptions({ fontSize: 16, cursorBlink: false }, null)
    expect(opts.fontSize).toBe(16)
    expect(opts.cursorBlink).toBe(false)
    expect(opts.fontFamily).toBe("'Fira Code', monospace")
    expect(opts.scrollback).toBe(5000)
  })

  it('merges theme colors from separate theme argument', () => {
    const opts = buildTerminalOptions(null, { background: '#000000' })
    const theme = opts.theme as Record<string, unknown>
    expect(theme.background).toBe('#000000')
    expect(theme.foreground).toBe(DEFAULT_THEME_COLORS.foreground)
    expect(theme.red).toBe(DEFAULT_THEME_COLORS.red)
  })

  it('combines settings and theme correctly', () => {
    const opts = buildTerminalOptions(
      { fontSize: 18, cursorStyle: 'bar' },
      { background: '#000', purple: '#f0f' },
    )
    expect(opts.fontSize).toBe(18)
    expect(opts.cursorStyle).toBe('bar')
    const theme = opts.theme as Record<string, unknown>
    expect(theme.background).toBe('#000')
    expect(theme.magenta).toBe('#f0f')
    expect(opts.scrollback).toBe(5000)
    expect(theme.foreground).toBe(DEFAULT_THEME_COLORS.foreground)
  })

  it('clamps fontSize to valid range', () => {
    expect(buildTerminalOptions({ fontSize: 2 }, null).fontSize).toBe(6)
    expect(buildTerminalOptions({ fontSize: 100 }, null).fontSize).toBe(48)
  })

  it('clamps scrollback to valid range', () => {
    expect(buildTerminalOptions({ scrollback: -10 }, null).scrollback).toBe(0)
    expect(buildTerminalOptions({ scrollback: 999999 }, null).scrollback).toBe(100_000)
  })

  it('clamps lineHeight to valid range', () => {
    expect(buildTerminalOptions({ lineHeight: 0.1 }, null).lineHeight).toBe(0.5)
    expect(buildTerminalOptions({ lineHeight: 5 }, null).lineHeight).toBe(3)
  })

  it('clamps minimumContrastRatio to valid range', () => {
    expect(buildTerminalOptions({ minimumContrastRatio: 0 }, null).minimumContrastRatio).toBe(1)
    expect(buildTerminalOptions({ minimumContrastRatio: 50 }, null).minimumContrastRatio).toBe(21)
  })

  it('normalizes purple to magenta in theme colors', () => {
    const opts = buildTerminalOptions(null, {
      purple: '#ff79c6',
      brightPurple: '#ff92df',
      background: '#282a36',
    })
    const theme = opts.theme as Record<string, unknown>
    expect(theme.magenta).toBe('#ff79c6')
    expect(theme.brightMagenta).toBe('#ff92df')
    expect(theme.background).toBe('#282a36')
    expect(theme.purple).toBeUndefined()
    expect(theme.brightPurple).toBeUndefined()
  })

  it('warns about unknown keys in settings', () => {
    const spy = vi.spyOn(console, 'warn').mockImplementation(() => {})
    buildTerminalOptions({ bogusKey: 42 } as any, null)
    expect(spy).toHaveBeenCalledWith(expect.stringContaining('unknown key "bogusKey"'))
    spy.mockRestore()
  })

  it('ignores keybinds and macCommandIsCtrl keys without warning', () => {
    const spy = vi.spyOn(console, 'warn').mockImplementation(() => {})
    buildTerminalOptions({ keybinds: [], macCommandIsCtrl: true }, null)
    expect(spy).not.toHaveBeenCalled()
    spy.mockRestore()
  })

  it('does not leak keybinds or macCommandIsCtrl into terminal options', () => {
    const opts = buildTerminalOptions(
      { keybinds: [{ key: 'ctrl+c', action: 'none' }], macCommandIsCtrl: true },
      null,
    )
    expect((opts as any).keybinds).toBeUndefined()
    expect((opts as any).macCommandIsCtrl).toBeUndefined()
  })

  it('falls back to defaults for invalid settings', () => {
    const spy = vi.spyOn(console, 'warn').mockImplementation(() => {})
    // fontSize as a string instead of number should trigger safeParse failure
    const opts = buildTerminalOptions({ fontSize: 'big' } as any, null)
    expect(opts.fontSize).toBe(13) // default
    expect(spy).toHaveBeenCalled()
    spy.mockRestore()
  })
})

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
