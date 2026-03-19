/**
 * Terminal keyboard handling.
 *
 * Intercepts browser-level shortcuts that conflict with terminal usage
 * and translates them into the correct PTY input sequences.
 */
import type { Terminal } from '@xterm/xterm'

type SendFn = (data: string) => void

/**
 * Attach custom key event handler to an xterm Terminal.
 * Must be called before AttachAddon is loaded so it intercepts first.
 *
 * Handles:
 * - Shift+Enter → plain newline (blocks Kitty protocol \x1b[13;2u)
 * - Ctrl+C → copy selection if text selected, else pass through as SIGINT
 * - Ctrl+V → paste from clipboard
 * - Ctrl+Alt+T → send Ctrl+T (browser steals Ctrl+T for new tab)
 */
export function attachKeyboardHandler(term: Terminal, send: SendFn): void {
  term.attachCustomKeyEventHandler((ev: KeyboardEvent) => {
    // Shift+Enter → send plain newline
    // Block all event types (keydown, keypress, keyup) to prevent
    // the Kitty keyboard protocol sequence from leaking through
    if (ev.shiftKey && ev.key === 'Enter') {
      if (ev.type === 'keydown') send('\n')
      return false
    }

    // Only handle keydown for the rest
    if (ev.type !== 'keydown') return true

    // Ctrl+C → copy selection if text selected, otherwise pass through
    if (ev.ctrlKey && !ev.shiftKey && ev.key === 'c') {
      const sel = term.getSelection()
      if (sel) {
        navigator.clipboard.writeText(sel)
        term.clearSelection()
        return false
      }
      return true
    }

    // Ctrl+Alt+T → send Ctrl+T (browser intercepts Ctrl+T for new tab)
    if (ev.ctrlKey && ev.altKey && ev.key === 't') {
      send('\x14')
      return false
    }

    // Ctrl+V: let xterm handle paste natively via its textarea paste event.
    // Don't intercept — doing so causes double paste.

    return true
  })
}
