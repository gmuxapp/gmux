/**
 * Terminal keyboard handling.
 *
 * Intercepts browser-level shortcuts that conflict with terminal usage
 * and translates them into the correct PTY input sequences.
 *
 * Keybindings are data-driven: the resolved keybind list (from config.ts)
 * is iterated on each keydown. Each entry maps a key combo to an action.
 */
import type { Terminal } from '@xterm/xterm'
import {
  eventMatchesKeybind,
  keyComboToSequence,
  type ResolvedKeybind,
} from './config'

type SendFn = (data: string) => void

/**
 * Attach custom key event handler to an xterm Terminal.
 * Must be called before AttachAddon is loaded so it intercepts first.
 *
 * Paste (Ctrl+V / right-click / middle-click) is handled separately by
 * attachPasteHandler, which intercepts at the container level.
 */
export function attachKeyboardHandler(
  term: Terminal,
  send: SendFn,
  keybinds: ResolvedKeybind[],
): void {
  term.attachCustomKeyEventHandler((ev: KeyboardEvent) => {
    // Check each resolved keybind against the event.
    for (const kb of keybinds) {
      if (!eventMatchesKeybind(ev, kb)) continue

      // For shift+enter we need to block all event types (keydown, keypress,
      // keyup) to prevent the Kitty keyboard protocol sequence from leaking.
      if (kb.baseKey === 'enter' && kb.shift) {
        if (ev.type === 'keydown') executeAction(kb, term, send)
        ev.preventDefault()
        return false
      }

      // For other bindings, only act on keydown.
      if (ev.type !== 'keydown') return true

      const handled = executeAction(kb, term, send)
      if (handled) {
        // Prevent browser default (e.g. Cmd+Left navigating back on Mac).
        // xterm.js does not call preventDefault when the custom handler
        // returns false, so we must do it ourselves.
        ev.preventDefault()
        return false
      }
    }

    return true
  })
}

/**
 * Execute a keybind action. Returns true if the event should be consumed
 * (not passed to xterm), false if xterm should still process it.
 */
function executeAction(kb: ResolvedKeybind, term: Terminal, send: SendFn): boolean {
  switch (kb.action) {
    case 'sendText':
      send(kb.args ?? '')
      return true

    case 'sendKeys': {
      const seq = keyComboToSequence(kb.args ?? '')
      if (seq) send(seq)
      return true
    }

    case 'copyOrInterrupt': {
      const sel = term.getSelection()
      if (sel) {
        navigator.clipboard.writeText(sel)
        term.clearSelection()
        return true
      }
      // No selection: let xterm handle it (sends SIGINT).
      return false
    }

    default:
      return false
  }
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
 * - xterm routes paste through onData, which runs through sendInput, meaning
 *   an armed alt/ctrl modifier would corrupt pasted text.
 * - We need to own the bracketed-paste / newline conversion ourselves so it is
 *   always correct regardless of what mobile modifier state is active.
 *
 * Newline handling (matches xterm's prepareTextForTerminal + bracketTextForPaste):
 * - \r?\n is always converted to \r (carriage return) first. This is the
 *   terminal "Enter" signal and is what PTY applications expect regardless of
 *   bracketed mode.
 * - Bracketed paste mode (CSI ? 2004 h): the normalized text is then wrapped
 *   in \x1b[200~ ... \x1b[201~, and any ESC characters inside it are replaced
 *   with U+241B (SYMBOL FOR ESCAPE). This prevents an embedded \x1b[201~
 *   from terminating the bracket early and injecting commands.
 * - Non-bracketed mode: the \r-normalized text is sent as-is. Each \r becomes
 *   a command submission, which is expected raw-PTY behaviour.
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
    // Matches xterm's prepareTextForTerminal, done unconditionally before
    // any bracket wrapping, because that is what PTY applications expect.
    const normalized = text.replace(/\r?\n/g, '\r')

    if (term.modes.bracketedPasteMode) {
      // Sanitize ESC characters so that nothing inside the pasted text can
      // terminate the bracket early or inject escape sequences.
      // xterm uses U+241B SYMBOL FOR ESCAPE as the stand-in, matching
      // what bracketTextForPaste does internally.
      const sanitized = normalized.replace(/\x1b/g, '\u241b')
      send(`\x1b[200~${sanitized}\x1b[201~`)
    } else {
      send(normalized)
    }
  }

  container.addEventListener('paste', handler, { capture: true })
  return () => container.removeEventListener('paste', handler, { capture: true })
}
