/**
 * Long-press recognizer for the terminal's touch handler.
 *
 * Pure timer state machine: the caller feeds it touch lifecycle calls
 * and decides what a long-press *does* via the callback. Movement
 * cancellation is deliberately not handled here — the touch handler in
 * terminal.tsx already tracks a move threshold for tap-vs-pan
 * discrimination, and a second threshold would drift from it. The
 * caller calls `cancel()` when its own threshold trips.
 *
 * A long-press is a distinct intent from a tap: once the timer fires,
 * `end()` reports it so the caller suppresses its tap behavior
 * (link-open / keyboard toggle) — even when the callback found nothing
 * actionable under the finger. Holding the terminal for half a second
 * should never pop the keyboard. `cancel()` only disarms a *pending*
 * timer; a fired press stays fired until `end()`/`start()` reset it,
 * so finger movement after an action sheet opened can't resurrect the
 * tap. Future consumers (copy-paragraph) extend the callback, not this
 * state machine.
 */

export interface LongPressRecognizer {
  /** Arm the timer for a new touch at (x, y). Resets prior state. */
  start(x: number, y: number): void
  /** Disarm a pending timer (finger moved, second finger, …). A press
   * that already fired stays fired. */
  cancel(): void
  /** Touch ended: returns true if the long-press fired (caller should
   * suppress its tap behavior). Resets. */
  end(): boolean
}

export const LONG_PRESS_MS = 500

export function createLongPressRecognizer(
  onLongPress: (x: number, y: number) => void,
  holdMs: number = LONG_PRESS_MS,
): LongPressRecognizer {
  let timer: ReturnType<typeof setTimeout> | null = null
  let fired = false

  const disarm = () => {
    if (timer !== null) {
      clearTimeout(timer)
      timer = null
    }
  }

  return {
    start(x, y) {
      disarm()
      fired = false
      timer = setTimeout(() => {
        timer = null
        fired = true
        onLongPress(x, y)
      }, holdMs)
    },
    cancel() {
      disarm()
    },
    end() {
      disarm()
      const wasFired = fired
      fired = false
      return wasFired
    },
  }
}
