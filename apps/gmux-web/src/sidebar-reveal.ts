/** Reveal-selected-row geometry, split out from the sidebar component so
 *  the scroll decision is testable without a DOM.
 *
 *  Mobile: when the off-canvas sidebar opens we want the selected session
 *  in view rather than dumping the user at the top. But the row may not be
 *  in the DOM yet (the drawer can open before the list renders, and the
 *  selection can change while it stays open), so the caller retries until
 *  a row exists — this decides whether, once found, a scroll is needed. */

interface Rect {
  top: number
  bottom: number
}

/** True when `row` is not fully inside `container`'s viewport and should
 *  therefore be scrolled into view. A row already fully visible stays put
 *  so we never jump the list needlessly. */
export function needsReveal(container: Rect, row: Rect): boolean {
  return !(row.top >= container.top && row.bottom <= container.bottom)
}
