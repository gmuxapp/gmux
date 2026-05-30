import type { ResolvedTerminalOptions } from './settings-schema'
import { DEFAULT_THEME_COLORS } from './settings-schema'

const COLOR_KEYS = [
  'black', 'red', 'green', 'yellow', 'blue', 'magenta', 'cyan', 'white',
  'brightBlack', 'brightRed', 'brightGreen', 'brightYellow',
  'brightBlue', 'brightMagenta', 'brightCyan', 'brightWhite',
] as const

/**
 * Apply a resolved terminal theme to the wterm element via CSS custom properties.
 * Covers colors 0–15, fg, bg, cursor, font-family, font-size, cursor-blink.
 * Call after term.init() and on terminalOptions changes.
 */
export function applyWtermTheme(
  element: HTMLElement,
  options: ResolvedTerminalOptions,
): void {
  const theme = options.theme ?? DEFAULT_THEME_COLORS
  const s = element.style

  s.setProperty('--term-fg',     theme.foreground  ?? '#d4d4d4')
  s.setProperty('--term-bg',     theme.background  ?? '#1e1e1e')
  s.setProperty('--term-cursor', theme.cursor      ?? '#aeafad')

  COLOR_KEYS.forEach((key, i) => {
    const val = (theme as Record<string, string>)[key]
    if (val) s.setProperty(`--term-color-${i}`, val)
  })

  s.setProperty('--term-font-family', options.fontFamily)
  s.setProperty('--term-font-size',   `${options.fontSize}px`)
  element.classList.toggle('cursor-blink', options.cursorBlink ?? false)
}
