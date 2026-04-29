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
  IS_MAC,
  keyComboToSequence,
  type ResolvedKeybind,
} from './config'
import {
  firstBinaryType,
  uploadClipboardBlob,
  type UploadResult,
} from './clipboard-upload'
import { selectionToText } from './selection'

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
/**
 * Optional feedback channel for clipboard upload outcomes. Callers can
 * route this to a toast surface; the default is a `console` fallback so
 * silent failures aren't possible. Kept as a callback to keep keyboard.ts
 * neutral about the UI surface.
 */
export type PasteFeedback = (kind: 'info' | 'error', message: string) => void

/**
 * Default paste-feedback sink: routes errors to console.warn and info
 * messages to console.log under a `[paste]` tag. Exported so callers
 * that don't have their own toast UI yet can opt into the same console
 * shape used by `attachKeyboardHandler` and `attachPasteHandler`.
 */
export const defaultPasteFeedback: PasteFeedback = (kind, message) => {
  if (kind === 'error') console.warn('[paste]', message)
  else console.log('[paste]', message)
}

/**
 * Bind clipboard upload outcome to keyboard.ts call sites. Returns the
 * input bytes to type into the PTY on success, or null on failure (after
 * surfacing the error via feedback).
 */
async function uploadAndFormatPath(
  blob: Blob,
  sessionId: string,
  bracketedPasteMode: boolean,
  feedback: PasteFeedback,
): Promise<string | null> {
  const result: UploadResult = await uploadClipboardBlob(blob, sessionId)
  if (!result.ok) {
    feedback('error', pasteErrorMessage(result.error))
    return null
  }
  feedback('info', `Pasted to ${result.path}`)
  return formatPasteText(result.path, bracketedPasteMode)
}

function pasteErrorMessage(code: string): string {
  switch (code) {
    case 'too_large':
      return 'Clipboard item too large (limit 10MB)'
    case 'network':
      return 'Paste failed: gmuxd unreachable'
    case 'empty_body':
      return 'Clipboard item is empty'
    case 'not_found':
      return 'Paste failed: session not found'
    case 'write_failed':
      return 'Paste failed: could not write file'
    case 'server_error':
      return 'Paste failed: server error'
    default:
      return `Paste failed: ${code}`
  }
}

export function attachKeyboardHandler(
  term: Terminal,
  send: SendFn,
  sendRaw: SendFn,
  keybinds: ResolvedKeybind[],
  macCommandIsCtrl = false,
  sessionId = '',
  onPasteFeedback: PasteFeedback = defaultPasteFeedback,
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

    // macCommandIsCtrl: on Mac, Cmd+<character> is treated as Ctrl+<character>.
    // Transform the event for keybind matching, then synthesize the ctrl
    // sequence if no keybind handles it. Only applies to single-character
    // keys; Cmd+arrow/backspace keep their default behavior.
    if (macCommandIsCtrl && IS_MAC && ev.metaKey && !ev.ctrlKey && ev.key.length === 1) {
      if (ev.type !== 'keydown') {
        ev.preventDefault()
        return false
      }

      // Match keybinds as if Ctrl were pressed instead of Cmd.
      const virtualMods = {
        ctrlKey: true, shiftKey: ev.shiftKey,
        altKey: ev.altKey, metaKey: false,
        key: ev.key,
      } as KeyboardEvent

      for (const kb of keybinds) {
        if (!eventMatchesKeybind(virtualMods, kb)) continue
        const handled = executeAction(kb, term, send, sendRaw, sessionId, onPasteFeedback)
        if (handled) { ev.preventDefault(); return false }
      }

      // No keybind matched. Synthesize the ctrl code directly.
      const seq = ctrlSequenceFor(ev.key)
      if (seq) send(seq)
      ev.preventDefault()
      return false
    }

    // Check each resolved keybind against the event.
    for (const kb of keybinds) {
      if (!eventMatchesKeybind(ev, kb)) continue

      // For shift+enter we need to block all event types (keydown, keypress,
      // keyup) to prevent the Kitty keyboard protocol sequence from leaking.
      if (kb.baseKey === 'enter' && kb.shift) {
        if (ev.type === 'keydown') executeAction(kb, term, send, sendRaw, sessionId, onPasteFeedback)
        ev.preventDefault()
        return false
      }

      // For other bindings, only act on keydown.
      if (ev.type !== 'keydown') return true

      const handled = executeAction(kb, term, send, sendRaw, sessionId, onPasteFeedback)
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
  sessionId: string,
  onPasteFeedback: PasteFeedback,
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
      const sel = selectionToText(term)
      if (sel) {
        navigator.clipboard.writeText(sel)
        term.clearSelection()
        return true
      }
      // No selection: let xterm handle it (sends SIGINT).
      return false
    }

    case 'copy': {
      const sel = selectionToText(term)
      if (sel) {
        navigator.clipboard.writeText(sel)
        term.clearSelection()
      }
      // Always consume the event, even with no selection.
      // Unlike copyOrInterrupt, never falls through to SIGINT.
      return true
    }

    case 'paste': {
      // Inspect the clipboard for binary content first; binary always
      // wins over text (see PRD). Falls back to readText() when only
      // text/* representations are present, preserving prior behavior.
      // Both branches use sendRaw to bypass mobile ctrl/alt arm logic.
      //
      // Requires clipboard-read permission in a secure context. The
      // keydown counts as a user gesture. Permission denial is reported
      // via onPasteFeedback so users see why nothing happened.
      void handlePasteAction({
        sessionId,
        bracketedPasteMode: term.modes.bracketedPasteMode,
        feedback: onPasteFeedback,
        emit: sendRaw,
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
 * Translate a single character into its Ctrl+<key> sequence.
 *
 * Lowercase letters produce the traditional ASCII control code (a=\x01).
 * Uppercase letters imply Shift, emitting a CSI u (Kitty keyboard protocol)
 * sequence: ESC [ <codepoint> ; 6 u  (modifiers = 1 + Shift(1) + Ctrl(4) = 6).
 * Special characters (@, [, \, ], ^, _, ?) produce their standard Ctrl codes.
 *
 * Returns null for characters with no Ctrl equivalent.
 */
export function ctrlSequenceFor(ch: string): string | null {
  if (ch.length !== 1) return null

  // Uppercase letter: Ctrl+Shift via CSI u.
  if (ch >= 'A' && ch <= 'Z') {
    return `\x1b[${ch.toLowerCase().charCodeAt(0)};6u`
  }
  // Lowercase letter: traditional control code.
  if (ch >= 'a' && ch <= 'z') {
    return String.fromCharCode(ch.charCodeAt(0) - 96) // a=1, b=2, ..., z=26
  }
  switch (ch) {
    case '@':    return '\x00'
    case '[':    return '\x1b'
    case '\\':   return '\x1c'
    case ']':    return '\x1d'
    case '^':    return '\x1e'
    case '_':    return '\x1f'
    case '?':    return '\x7f'
    case '\x7f': return '\x08' // Backspace: Ctrl+H
    default:     return null
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
 * The `sendRaw` parameter must be sendRawInput (not sendInput) so that
 * paste is never transformed by the ctrl/alt modifier logic. The name
 * encodes the contract: the keybind paste action takes the same care
 * (see executeAction's `case 'paste'`).
 *
 * Returns a cleanup function.
 */
export function attachPasteHandler(
  term: Terminal,
  container: HTMLElement,
  sendRaw: SendFn,
  sessionId = '',
  onPasteFeedback: PasteFeedback = defaultPasteFeedback,
): () => void {
  const handler = (ev: ClipboardEvent) => {
    const data = ev.clipboardData
    if (!data) return

    // Binary takes precedence: scan items for the first non-text MIME
    // and route to the upload helper. Pull the Blob synchronously
    // before the event handler returns; clipboardData becomes invalid
    // after.
    const binaryItem = pickBinaryDataTransferItem(data.items)
    if (binaryItem) {
      const blob = binaryItem.getAsFile()
      ev.stopPropagation()
      ev.preventDefault()
      if (!blob) {
        onPasteFeedback('error', 'Paste failed: could not read clipboard item')
        return
      }
      if (!sessionId) {
        onPasteFeedback('error', 'Paste failed: no session bound')
        return
      }
      void uploadAndFormatPath(blob, sessionId, term.modes.bracketedPasteMode, onPasteFeedback)
        .then(out => { if (out !== null) sendRaw(out) })
      return
    }

    const text = data.getData('text/plain') ?? ''
    if (!text) return

    // Stop the event before it reaches xterm's textarea listener.
    ev.stopPropagation()
    ev.preventDefault()

    sendRaw(formatPasteText(text, term.modes.bracketedPasteMode))
  }

  container.addEventListener('paste', handler, { capture: true })
  return () => container.removeEventListener('paste', handler, { capture: true })
}

/**
 * Walk a DataTransferItemList and return the first item whose kind is
 * 'file' and whose MIME is not text/*. Returns null if no such item
 * exists or the list is empty.
 *
 * Two predicates, both load-bearing:
 *  - `kind === 'file'` excludes 'string' items (their getAsFile() is
 *    always null and their bytes live in getAsString()'s callback).
 *  - non-text/* implements the "binary intent wins" rule: a clipboard
 *    with both an image and its alt-text representation surfaces the
 *    image.
 *
 * Exported for testability; the rule is small but the cost of getting
 * it subtly wrong (e.g. uploading a string-kind item that returns null
 * from getAsFile) is a confusing failure-toast race.
 */
export function pickBinaryDataTransferItem(items: DataTransferItemList): DataTransferItem | null {
  for (let i = 0; i < items.length; i++) {
    const item = items[i]
    if (item.kind === 'file' && !item.type.startsWith('text/')) return item
  }
  return null
}

/**
 * Read the system clipboard via the Clipboard API and route binary
 * payloads to the upload helper, falling back to text extracted from
 * the same clipboard items (no second clipboard read). Mirrors the
 * DOM paste handler's shape so both entry points behave identically.
 *
 * `navigator.clipboard.read()` is missing on a few browsers (Firefox
 * default config, older Safari) and may throw on text-only clipboards
 * in some Chromium builds. Both cases fall back to `readText()`.
 *
 * Exported so the mobile toolbar paste button can reuse the exact same
 * inspect-and-route logic as the keybind path. Without this, the mobile
 * button would be text-only.
 */
export async function handlePasteAction(args: {
  sessionId: string
  bracketedPasteMode: boolean
  feedback: PasteFeedback
  emit: SendFn
}): Promise<void> {
  const { sessionId, bracketedPasteMode, feedback, emit } = args
  const reader = navigator.clipboard

  if (typeof reader?.read === 'function') {
    let items: ClipboardItems | null = null
    try {
      items = await reader.read()
    } catch {
      // read() unavailable in this state; fall through to readText().
    }
    if (items !== null) {
      // Binary intent wins.
      for (const item of items) {
        const binMime = firstBinaryType(item.types)
        if (!binMime) continue
        const blob = await item.getType(binMime)
        if (!sessionId) {
          feedback('error', 'Paste failed: no session bound')
          return
        }
        const out = await uploadAndFormatPath(blob, sessionId, bracketedPasteMode, feedback)
        if (out !== null) emit(out)
        return
      }
      // No binary; extract text from the items we already have so we
      // don't trigger a second clipboard permission prompt.
      for (const item of items) {
        if (!item.types.includes('text/plain')) continue
        const blob = await item.getType('text/plain')
        const text = await blob.text()
        if (text) emit(formatPasteText(text, bracketedPasteMode))
        return
      }
      return // clipboard had only types we don't handle
    }
  }

  try {
    const text = await reader.readText()
    if (text) emit(formatPasteText(text, bracketedPasteMode))
  } catch (err) {
    feedback('error', 'Paste failed: clipboard access denied')
    console.warn('Paste failed: clipboard access denied.', err)
  }
}
