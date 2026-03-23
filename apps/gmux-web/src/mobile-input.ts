/**
 * Mobile keyboard input fixes for xterm.js.
 *
 * Problem: mobile keyboards (iOS autocorrect, Android predictive text) replace
 * words in xterm's hidden textarea rather than appending. xterm.js only
 * handles `inputType === 'insertText'` in its `_inputEvent` handler and has
 * no `beforeinput` listener at all, so replacement events either get ignored
 * or fall through to the composition path, causing the corrected text to be
 * concatenated after the original.
 *
 * Fix: intercept `beforeinput` events with `inputType === 'insertReplacementText'`
 * on the textarea, translate them into the terminal-compatible sequence of
 * backspaces (to erase the old text) followed by the replacement text, and
 * call `preventDefault()` so the `input` event never fires and xterm never
 * sees the change.
 *
 * This works because xterm's textarea accumulates typed characters (cleared
 * only on blur, Enter, or Ctrl+C). When a replacement fires, the textarea
 * still contains the original text and selectionStart/selectionEnd mark the
 * range being replaced.
 *
 * Assumption: the terminal cursor sits right after the last character in the
 * textarea, so backspace count = (textarea.value.length - selectionStart).
 * This holds for the normal mobile typing flow where autocorrect fires
 * immediately after typing a space or tapping a suggestion. It would break
 * if the user moved the terminal cursor (e.g. arrow keys) without the
 * textarea being cleared, but mobile on-screen keyboards don't have arrow
 * keys and autocorrect doesn't fire in that scenario.
 *
 * Not yet handled: voice dictation, which uses composition events rather than
 * `insertReplacementText`. The `/_/input-diagnostics` page can be used to
 * collect real event traces to guide a future fix for that case.
 */
import type { Terminal } from '@xterm/xterm'

type SendFn = (data: string) => void

/**
 * Attach a handler that intercepts mobile keyboard word-replacement events
 * and translates them into terminal-compatible input sequences.
 *
 * Must be called after `term.open()` so `term.textarea` exists.
 * `send` should be the raw PTY send function (not sendInput, to avoid
 * ctrl/alt modifier interference, same as paste).
 *
 * Returns a cleanup function.
 */
export function attachMobileInputHandler(
  term: Terminal,
  send: SendFn,
): () => void {
  const textarea = term.textarea
  if (!textarea) return () => {}

  const handler = (ev: InputEvent) => {
    if (ev.inputType !== 'insertReplacementText') return

    // The replacement text comes from ev.data (most browsers) or
    // ev.dataTransfer (Safari for some spell-check corrections).
    const newText = ev.data ?? ev.dataTransfer?.getData('text/plain') ?? ''
    if (!newText) return

    const start = textarea.selectionStart ?? 0
    const end = textarea.selectionEnd ?? start

    // If nothing is selected, this isn't really a replacement.
    if (start === end) return

    // Everything from `start` to the end of the textarea was already sent to
    // the PTY. Erase it all, then re-send replacement + preserved suffix.
    const suffix = textarea.value.substring(end)
    const charsToErase = textarea.value.length - start

    ev.preventDefault()
    ev.stopImmediatePropagation()

    send('\x7f'.repeat(charsToErase) + newText + suffix)

    // Keep the textarea in sync so the keyboard's internal model matches.
    const prefix = textarea.value.substring(0, start)
    textarea.value = prefix + newText + suffix
    textarea.selectionStart = textarea.selectionEnd = start + newText.length
  }

  // Capture phase so we fire before any other listeners on this element.
  textarea.addEventListener('beforeinput', handler, { capture: true })
  return () => textarea.removeEventListener('beforeinput', handler, { capture: true })
}
