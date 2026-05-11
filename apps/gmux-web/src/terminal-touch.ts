/**
 * Pure helpers for mobile text-selection improvements.
 * Extracted for testability; used by terminal.tsx.
 */

/**
 * Returns true if WebGL renderer should be used.
 * On coarse-pointer (touch) devices we skip WebGL and fall back to the DOM
 * renderer so that xterm text lives in real DOM nodes, which lets the OS
 * long-press text-selection gesture find something to select.
 */
export function shouldUseWebgl(
  matchMedia: (query: string) => { matches: boolean },
): boolean {
  return !matchMedia('(pointer: coarse)').matches
}

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
