/**
 * Mobile keyboard input fixes for xterm.js.
 *
 * Problem: mobile keyboards (iOS autocorrect, dictation, predictive text)
 * replace words in xterm's hidden textarea rather than appending. The
 * replacement is signaled by a non-collapsed selection (selectionStart <
 * selectionEnd) in a beforeinput event. xterm.js doesn't distinguish
 * replacements from appends: its _inputEvent handler sends the full ev.data
 * for every insertText event, so each replacement re-sends the entire text
 * that was already on screen, causing cascading duplication.
 *
 * iOS Safari fires these replacements as inputType='insertText' (not
 * 'insertReplacementText'), so we must handle both.
 *
 * Fix: two-phase interception.
 *
 *   beforeinput (textarea, capture): when we detect a replacement (selStart <
 *   selEnd), send backspaces to erase from the replacement start to the end
 *   of the textarea (everything that was already sent to the PTY). Save the
 *   replacement text and any suffix for phase two.
 *
 *   input (container, capture): fires before xterm's handler on the textarea
 *   because capture goes parent-first. We stopImmediatePropagation() to
 *   prevent xterm from also sending ev.data, then send the replacement text
 *   plus the preserved suffix ourselves.
 *
 * This approach never calls preventDefault(), so it works regardless of
 * whether the browser considers beforeinput cancelable for the given
 * inputType and element type (a known cross-browser inconsistency).
 *
 * Assumption: the terminal cursor sits right after the last character in the
 * textarea. This holds for the normal mobile typing flow where replacements
 * fire immediately after typing. Mobile on-screen keyboards don't have arrow
 * keys, and autocorrect/dictation don't fire after cursor movement.
 *
 * See also: /_/input-diagnostics for collecting real event traces.
 */
import type { Terminal } from '@xterm/xterm'

type SendFn = (data: string) => void

interface PendingReplacement {
  newText: string
  suffix: string
}

/**
 * Attach a handler that intercepts mobile keyboard word-replacement events
 * and translates them into terminal-compatible input sequences.
 *
 * Must be called after `term.open()` so `term.textarea` exists.
 * `container` should be the parent element of xterm's textarea (needed to
 * intercept input events in the capture phase before xterm sees them).
 * `send` should be the raw PTY send function (not sendInput, to avoid
 * ctrl/alt modifier interference; same convention as paste).
 *
 * Returns a cleanup function.
 */
export function attachMobileInputHandler(
  term: Terminal,
  container: HTMLElement,
  send: SendFn,
): () => void {
  const textarea = term.textarea
  if (!textarea) return () => {}

  let pending: PendingReplacement | null = null

  // Phase 1: detect replacement and send backspaces.
  const onBeforeInput = (ev: InputEvent) => {
    if (ev.inputType !== 'insertText' && ev.inputType !== 'insertReplacementText') return

    const start = textarea.selectionStart ?? 0
    const end = textarea.selectionEnd ?? start

    // Collapsed selection = normal append, let xterm handle it.
    if (start === end) return

    const newText = ev.data ?? ev.dataTransfer?.getData('text/plain') ?? ''
    if (!newText) return

    const suffix = textarea.value.substring(end)
    const charsToErase = textarea.value.length - start

    // Erase from the replacement start to the end of the textarea.
    // All of this text was already sent to the PTY.
    send('\x7f'.repeat(charsToErase))

    // Phase 2 will send the replacement text + suffix after we prevent
    // xterm from double-sending ev.data.
    pending = { newText, suffix }
  }

  // Phase 2: intercept the input event before xterm, send replacement + suffix.
  // Registered on the container (parent) so capture phase fires before
  // xterm's capture-phase handler on the textarea itself.
  const onInput = (ev: Event) => {
    if (!pending) return
    const { newText, suffix } = pending
    pending = null

    // Prevent xterm's _inputEvent from also sending ev.data.
    ev.stopImmediatePropagation()

    send(newText + suffix)
  }

  textarea.addEventListener('beforeinput', onBeforeInput, { capture: true })
  container.addEventListener('input', onInput, { capture: true })

  return () => {
    textarea.removeEventListener('beforeinput', onBeforeInput, { capture: true })
    container.removeEventListener('input', onInput, { capture: true })
  }
}
