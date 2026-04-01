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
 * Detect a touch-primary device (coarse pointer).
 *
 * Uses only the pointer media query, NOT maxTouchPoints, so the result
 * matches the CSS `@media (pointer: coarse)` query that controls mobile
 * toolbar visibility. A touchscreen laptop (pointer: fine, touchpoints > 0)
 * correctly stays in desktop mode: Enter sends \r and there is no toolbar.
 */
function isTouchDevice(): boolean {
  return window.matchMedia('(pointer: coarse)').matches
}

/**
 * Attach custom key event handler to an xterm Terminal.
 * Must be called before AttachAddon is loaded so it intercepts first.
 *
 * On mobile (touch) devices, bare Enter sends \n (newline) instead of \r
 * (carriage return / submit). This lets mobile users compose multi-line
 * input; the dedicated send button in the mobile toolbar handles submission.
 * Desktop behavior is unchanged: Enter sends \r as usual.
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
    // Mobile Enter → newline (not submit).
    // Bare Enter with no modifiers on a touch device sends \n so the user
    // can compose multi-line messages. The mobile toolbar send button sends
    // \r when they are ready to submit.
    if (ev.key === 'Enter' && !ev.shiftKey && !ev.ctrlKey && !ev.altKey && !ev.metaKey
        && isTouchDevice()) {
      if (ev.type === 'keydown') send('\n')
      ev.preventDefault()
      return false
    }

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
 * Normalize and optionally bracket-wrap text for pasting into a terminal.
 * Shared by the DOM paste handler and the mobile paste button.
 *
 * With bracketedPasteMode on, the receiving app owns newline handling, so
 * we normalize to \r inside the brackets (standard terminal convention).
 *
 * With bracketedPasteMode off, newlines are kept as \n. This preserves the
 * distinction between \n (literal newline) and \r (Enter / submit) so that
 * applications running in raw mode (coding agents, editors) can treat pasted
 * newlines as content rather than as command submissions. Shells that treat
 * both \n and \r as accept-line are unaffected.
 */
export function formatPasteText(text: string, bracketedPasteMode: boolean): string {
  if (bracketedPasteMode) {
    // Normalize to \r inside brackets (standard terminal paste convention).
    const normalized = text.replace(/\r?\n/g, '\r')
    // Sanitize ESC so nothing inside the text can break out of the bracket.
    const sanitized = normalized.replace(/\x1b/g, '\u241b')
    return `\x1b[200~${sanitized}\x1b[201~`
  }

  // Non-bracketed: keep \n so pasted newlines stay as newlines.
  // Normalize \r\n → \n and bare \r → \n for consistency.
  return text.replace(/\r\n/g, '\n').replace(/\r/g, '\n')
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
 * Newline handling:
 * - Bracketed paste mode (CSI ? 2004 h): \r?\n → \r (standard convention),
 *   then wrapped in \x1b[200~ ... \x1b[201~. ESC characters inside are
 *   replaced with U+241B to prevent bracket escape.
 * - Non-bracketed mode: newlines stay as \n so that applications running in
 *   raw mode can distinguish pasted newlines (\n) from Enter (\r). Shells
 *   that treat both as accept-line are unaffected.
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

    send(formatPasteText(text, term.modes.bracketedPasteMode))
  }

  container.addEventListener('paste', handler, { capture: true })
  return () => container.removeEventListener('paste', handler, { capture: true })
}
