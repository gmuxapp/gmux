/**
 * Terminal keyboard handling.
 *
 * The keymap (from config.ts) is the source of truth for every keyboard
 * shortcut: clipboard operations, UI actions, navigation remaps, and
 * browser-stolen key workarounds. xterm.js only handles terminal input
 * (characters, control codes, escape sequences). Nothing is left to
 * implicit xterm.js or browser passthrough.
 *
 * Keybindings are data-driven: the resolved keybind list is iterated on
 * each keydown. Each entry maps a key combo to an action.
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
 * Keyboard-triggered paste (Ctrl+V, Cmd+V, Ctrl+Shift+V) is handled here
 * via the `paste` action, which reads the clipboard through the Clipboard
 * API. Non-keyboard paste (right-click, middle-click, mobile paste button)
 * is handled separately by attachPasteHandler via the DOM paste event.
 */
export function attachKeyboardHandler(
  term: Terminal,
  send: SendFn,
  sendRaw: SendFn,
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
        if (ev.type === 'keydown') executeAction(kb, term, send, sendRaw)
        ev.preventDefault()
        return false
      }

      // For other bindings, only act on keydown.
      if (ev.type !== 'keydown') return true

      const handled = executeAction(kb, term, send, sendRaw)
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
 *
 * `send` is the input channel (may apply mobile ctrl/alt arm modifiers).
 * `sendRaw` bypasses modifier logic, used by paste to avoid corrupting
 * clipboard content.
 */
function executeAction(
  kb: ResolvedKeybind,
  term: Terminal,
  send: SendFn,
  sendRaw: SendFn,
): boolean {
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

    case 'copy': {
      const sel = term.getSelection()
      if (sel) {
        navigator.clipboard.writeText(sel)
        term.clearSelection()
      }
      // Always consume the event, even with no selection.
      // Unlike copyOrInterrupt, never falls through to SIGINT.
      return true
    }

    case 'paste': {
      // Read from the Clipboard API and send to the PTY. Uses sendRaw to
      // bypass mobile ctrl/alt arm logic (same as the DOM paste handler).
      // Requires clipboard-read permission in a secure context with a user
      // gesture (keydown qualifies). Falls back with a console warning if
      // the browser denies access.
      navigator.clipboard.readText().then(text => {
        if (text) {
          sendRaw(formatPasteText(text, term.modes.bracketedPasteMode))
        }
      }).catch((err) => {
        console.warn('Paste failed: clipboard access denied.', err)
      })
      return true
    }

    case 'selectAll':
      term.selectAll()
      return true

    default:
      return false
  }
}

/**
 * Normalize and optionally bracket-wrap text for pasting into a terminal.
 * Shared by the keybind paste action, the DOM paste handler, and the mobile
 * paste button.
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
 * Handles non-keyboard paste: right-click, middle-click (X11), mobile paste
 * button, and browser Edit menu. Keyboard-triggered paste (Ctrl+V, Cmd+V,
 * Ctrl+Shift+V) goes through the keymap's `paste` action instead.
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
