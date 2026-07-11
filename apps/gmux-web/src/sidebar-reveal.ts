/** Reveal-selected-row geometry, split out from the sidebar component so
 *  the scroll decision is testable without a DOM.
 *
 *  Mobile: when the off-canvas sidebar opens we want the selected session
 *  in view rather than dumping the user at the top. The selected row is
 *  guaranteed present whenever a selection exists — it's pinned past the
 *  `?filter=` and the sidebar only mounts with data loaded — so the
 *  caller reads once; this decides whether a scroll is actually needed. */

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
