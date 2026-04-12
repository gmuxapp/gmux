/**
 * Mobile keyboard input fixes for xterm.js.
 *
 * Problem: mobile keyboards (iOS autocorrect, dictation, predictive text)
 * replace words in xterm's hidden textarea rather than appending. xterm.js
 * doesn't distinguish replacements from appends, so each replacement
 * re-sends text that was already on screen, causing cascading duplication.
 *
 * The replacement signal differs by platform:
 *
 *   iOS Safari: a single insertText (or insertReplacementText) with a
 *   non-collapsed selection (selectionStart < selectionEnd).
 *
 *   Android Chrome: a deleteContentBackward with non-collapsed selection,
 *   immediately followed by an insertText with collapsed selection. Same
 *   logical operation, split into two DOM events.
 *
 * Fix: two-phase interception.
 *
 *   beforeinput (textarea, capture): detect the replacement signal (iOS:
 *   non-collapsed selection on insertText; Android: deleteContentBackward
 *   with non-collapsed selection, carried forward to the next insertText).
 *   Send backspaces to erase from the replacement start to the end of the
 *   textarea.
 *
 *   input (container, capture): fires before xterm's handler on the textarea
 *   because capture goes parent-first. We stopImmediatePropagation() to
 *   prevent xterm from also sending ev.data, then send the replacement text
 *   plus the preserved suffix ourselves.
 *
 * Android has an additional complication: keydown events with keyCode 229
 * trigger xterm's CompositionHelper._handleAnyTextareaChanges, which uses
 * String.replace(oldValue, '') to diff the textarea. This works for pure
 * appends but produces garbage when the keyboard modifies the middle of the
 * string (the old value isn't a substring of the new value, so replace()
 * returns the entire textarea). We neutralize this by resetting
 * textarea.value to its pre-autocorrect state after sending the correct
 * data, so the deferred diff sees no change.
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
  /** When set, reset textarea.value after sending to neutralize xterm's
   *  _handleAnyTextareaChanges deferred diff (Android keyCode-229 path). */
  resetValue?: string
}

/** Tracks a deleteContentBackward with non-collapsed selection so the
 *  immediately following insertText can be recognized as a replacement. */
interface TrackedDeletion {
  preDeleteValue: string
  deleteStart: number
  deleteEnd: number
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
  let trackedDeletion: TrackedDeletion | null = null

  // Phase 1: detect replacement and send backspaces.
  const onBeforeInput = (ev: InputEvent) => {
    // Android autocorrect: the keyboard splits word corrections into
    // deleteContentBackward (non-collapsed) + insertText (collapsed).
    // Track the deletion so we can combine it with the following insert.
    if (ev.inputType === 'deleteContentBackward') {
      const start = textarea.selectionStart ?? 0
      const end = textarea.selectionEnd ?? start
      // Non-collapsed: potential Android autocorrect start. Track it.
      // Collapsed: normal backspace. Clear any stale tracking.
      trackedDeletion = start < end
        ? { preDeleteValue: textarea.value, deleteStart: start, deleteEnd: end }
        : null
      return
    }

    if (ev.inputType !== 'insertText' && ev.inputType !== 'insertReplacementText') {
      trackedDeletion = null
      return
    }

    const start = textarea.selectionStart ?? 0
    const end = textarea.selectionEnd ?? start

    // Android autocorrect phase 2: insertText immediately after a tracked
    // deletion completes the replacement pair.
    if (trackedDeletion && start === end) {
      const newText = ev.data ?? ev.dataTransfer?.getData('text/plain') ?? ''
      if (!newText) { trackedDeletion = null; return }

      const { preDeleteValue, deleteStart, deleteEnd } = trackedDeletion
      trackedDeletion = null

      const suffix = preDeleteValue.substring(deleteEnd)
      const charsToErase = preDeleteValue.length - deleteStart

      send('\x7f'.repeat(charsToErase))
      pending = { newText, suffix, resetValue: preDeleteValue }
      return
    }

    trackedDeletion = null

    // Collapsed selection = normal append, let xterm handle it.
    if (start === end) return

    // iOS replacement: a single insertText/insertReplacementText with
    // non-collapsed selection.
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
    const { newText, suffix, resetValue } = pending
    pending = null

    // Prevent xterm's _inputEvent from also sending ev.data.
    ev.stopImmediatePropagation()

    send(newText + suffix)

    // Android: reset textarea to the pre-autocorrect value. xterm's
    // CompositionHelper._handleAnyTextareaChanges (triggered by keydown 229)
    // captured this same value as oldValue and will diff against it in a
    // deferred setTimeout(0). By restoring it, the diff sees no change.
    if (resetValue !== undefined) {
      textarea.value = resetValue
      textarea.selectionStart = textarea.selectionEnd = resetValue.length
    }
  }

  textarea.addEventListener('beforeinput', onBeforeInput, { capture: true })
  container.addEventListener('input', onInput, { capture: true })

  return () => {
    textarea.removeEventListener('beforeinput', onBeforeInput, { capture: true })
    container.removeEventListener('input', onInput, { capture: true })
  }
}
