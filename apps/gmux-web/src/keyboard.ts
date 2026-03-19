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
 * - Ctrl+Alt+T → send Ctrl+T (browser steals Ctrl+T for new tab)
 *
 * Paste (Ctrl+V / right-click / middle-click) is handled separately by
 * attachPasteHandler, which intercepts at the container level.
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

    return true
  })
}

/**
 * Attach a paste handler to the terminal container using the DOM capture phase.
 *
 * By listening with { capture: true } on an ancestor of xterm's internal
 * textarea, we intercept the paste event *before* xterm's own handler fires.
 * stopPropagation() keeps the event from reaching the textarea at all, so
 * there is no double-paste.
 *
 * Why not let xterm handle paste natively?
 * - xterm routes paste through onData, which runs through sendInput — meaning
 *   an armed alt/ctrl modifier would corrupt pasted text.
 * - We need to own the bracketed-paste / newline conversion ourselves so it is
 *   always correct regardless of what mobile modifier state is active.
 *
 * Newline handling (matches xterm's prepareTextForTerminal + bracketTextForPaste):
 * - \r?\n is always converted to \r (carriage return) first.  This is the
 *   terminal "Enter" signal and is what PTY applications expect regardless of
 *   bracketed mode.
 * - Bracketed paste mode (CSI ? 2004 h): the normalized text is then wrapped
 *   in \x1b[200~ … \x1b[201~, and any ESC characters inside it are replaced
 *   with U+241B (␛, SYMBOL FOR ESCAPE).  This prevents an embedded \x1b[201~
 *   from terminating the bracket early and injecting commands.
 * - Non-bracketed mode: the \r-normalized text is sent as-is.  Each \r becomes
 *   a command submission — expected raw-PTY behaviour.
 *
 * `send` should be sendRawInput (not sendInput) so that paste is never
 * transformed by the ctrl/alt modifier logic.
 *
 * Returns a cleanup function.
 */
export function attachPasteHandler(
  term: Terminal,
  container: HTMLElement,
  send: SendFn,
): () => void {
  const handler = (ev: ClipboardEvent) => {
    const text = ev.clipboardData?.getData('text/plain') ?? ''
    if (!text) return

    // Stop the event before it reaches xterm's textarea listener.
    ev.stopPropagation()
    ev.preventDefault()

    // Always normalize line endings to \r (carriage return = terminal Enter).
    // Matches xterm's prepareTextForTerminal — done unconditionally, before
    // any bracket wrapping, because that is what PTY applications expect.
    const normalized = text.replace(/\r?\n/g, '\r')

    if (term.modes.bracketedPasteMode) {
      // Sanitize ESC characters so that nothing inside the pasted text can
      // terminate the bracket early or inject escape sequences.
      // xterm uses U+241B SYMBOL FOR ESCAPE (␛) as the stand-in, matching
      // what bracketTextForPaste does internally (module 7861 in xterm source).
      const sanitized = normalized.replace(/\x1b/g, '\u241b')
      send(`\x1b[200~${sanitized}\x1b[201~`)
    } else {
      send(normalized)
    }
  }

  container.addEventListener('paste', handler, { capture: true })
  return () => container.removeEventListener('paste', handler, { capture: true })
}
