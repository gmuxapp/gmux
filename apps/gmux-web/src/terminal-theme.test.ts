// @vitest-environment jsdom
import { describe, it, expect } from 'vitest'
import { applyWtermTheme } from './terminal-theme'
import type { ResolvedTerminalOptions } from './settings-schema'

function makeOptions(overrides: Partial<ResolvedTerminalOptions> = {}): ResolvedTerminalOptions {
  return {
    fontSize: 14,
    fontFamily: "'Fira Code', monospace",
    cursorBlink: false,
    cursorStyle: 'block',
    scrollback: 5000,
    smoothScrollDuration: 0,
    theme: {
      foreground: '#d4d4d4',
      background: '#1e1e1e',
      cursor: '#aeafad',
      black: '#000000',
      red: '#cd3131',
      green: '#0dbc79',
      yellow: '#e5e510',
      blue: '#2472c8',
      magenta: '#bc3fbc',
      cyan: '#11a8cd',
      white: '#e5e5e5',
      brightBlack: '#666666',
      brightRed: '#f14c4c',
      brightGreen: '#23d18b',
      brightYellow: '#f5f543',
      brightBlue: '#3b8eea',
      brightMagenta: '#d670d6',
      brightCyan: '#29b8db',
      brightWhite: '#e5e5e5',
    },
    ...overrides,
  } as ResolvedTerminalOptions
}

describe('applyWtermTheme', () => {
  it('sets foreground, background and cursor CSS variables', () => {
    const el = document.createElement('div')
    applyWtermTheme(el, makeOptions())
    expect(el.style.getPropertyValue('--term-fg')).toBe('#d4d4d4')
    expect(el.style.getPropertyValue('--term-bg')).toBe('#1e1e1e')
    expect(el.style.getPropertyValue('--term-cursor')).toBe('#aeafad')
  })

  it('sets color palette variables 0–15', () => {
    const el = document.createElement('div')
    applyWtermTheme(el, makeOptions())
    expect(el.style.getPropertyValue('--term-color-0')).toBe('#000000')  // black
    expect(el.style.getPropertyValue('--term-color-1')).toBe('#cd3131')  // red
    expect(el.style.getPropertyValue('--term-color-7')).toBe('#e5e5e5')  // white
    expect(el.style.getPropertyValue('--term-color-8')).toBe('#666666')  // brightBlack
    expect(el.style.getPropertyValue('--term-color-15')).toBe('#e5e5e5') // brightWhite
  })

  it('sets font-family and font-size variables', () => {
    const el = document.createElement('div')
    applyWtermTheme(el, makeOptions({ fontSize: 13, fontFamily: "'JetBrains Mono'" }))
    expect(el.style.getPropertyValue('--term-font-family')).toBe("'JetBrains Mono'")
    expect(el.style.getPropertyValue('--term-font-size')).toBe('13px')
  })

  it('toggles cursor-blink class', () => {
    const el = document.createElement('div')
    applyWtermTheme(el, makeOptions({ cursorBlink: true }))
    expect(el.classList.contains('cursor-blink')).toBe(true)

    applyWtermTheme(el, makeOptions({ cursorBlink: false }))
    expect(el.classList.contains('cursor-blink')).toBe(false)
  })

  it('uses default colors when theme is undefined', () => {
    const el = document.createElement('div')
    const opts = makeOptions()
    // @ts-expect-error – testing undefined theme path
    opts.theme = undefined
    applyWtermTheme(el, opts)
    // Defaults from DEFAULT_THEME_COLORS should apply
    expect(el.style.getPropertyValue('--term-fg')).not.toBe('')
    expect(el.style.getPropertyValue('--term-bg')).not.toBe('')
  })
})
