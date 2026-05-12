/**
 * Pure helpers for mobile touch handling.
 * Extracted for testability; used by terminal.tsx.
 */

/**
 * Returns true if the terminal textarea should be focused on touchend.
 * We skip focus when:
 *  - the touch was a drag (`moved`), or
 *  - the OS long-press text-selection gesture fired (`wasLongPress`),
 *    because focusing the textarea would collapse the selection.
 */
export function shouldFocusOnTouchEnd({
  moved,
  wasLongPress,
}: {
  moved: boolean
  wasLongPress: boolean
}): boolean {
  return !moved && !wasLongPress
}
