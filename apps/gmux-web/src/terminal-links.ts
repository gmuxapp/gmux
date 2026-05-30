/**
 * terminal-links.ts — click-to-open URL detection for wterm's DOM renderer.
 *
 * wterm renders every cell as a <span> inside a .term-row div. There are no
 * <a> elements; URLs are plain text. This module detects clicks that land on
 * URL-like text and opens them.
 *
 * Strategy:
 *   1. caretRangeFromPoint → find the exact text-node offset at the click
 *   2. Walk the row's text nodes to get an absolute character offset
 *   3. Scan the full row text for URL matches
 *   4. If the click offset falls inside a match → open the URL
 *
 * The file:// scheme is intentionally excluded here because terminal.tsx
 * already installs a window.open interceptor that POSTs to /v1/open-path for
 * those. Any file:// URL detected here would be opened via that interceptor
 * automatically if we chose to include it; for now we keep the two paths
 * separate.
 */

// Matches http/https URLs. Trailing punctuation that is unlikely to be part
// of the URL is excluded by the negative character class.
const URL_PATTERN = /https?:\/\/[^\s\])"'>\\]+/g

/**
 * Given a MouseEvent on the terminal container, try to find a URL at the
 * clicked character position and open it.
 *
 * Returns true if a URL was found and opened (caller should stop further
 * processing such as focus-stealing). Returns false if no URL was detected.
 */
export function handleTerminalLinkClick(ev: MouseEvent): boolean {
  // Don't interfere with in-progress text selections.
  const sel = window.getSelection()
  if (sel && !sel.isCollapsed) return false

  // Find the character position under the pointer.
  const range = getRangeAtPoint(ev.clientX, ev.clientY)
  if (!range) return false

  const clickNode = range.startContainer
  const clickOffset = range.startOffset

  // Walk up to the nearest .term-row ancestor.
  const anchor = clickNode.nodeType === Node.TEXT_NODE
    ? clickNode.parentElement
    : clickNode instanceof Element ? clickNode : null
  const rowEl = anchor?.closest?.('.term-row')
  if (!rowEl) return false

  // Calculate the absolute character offset within the full row text.
  const absoluteOffset = absoluteTextOffset(rowEl, clickNode, clickOffset)
  if (absoluteOffset === -1) return false

  const rowText = rowEl.textContent ?? ''

  // Scan for URL matches and check if the click falls inside one.
  URL_PATTERN.lastIndex = 0
  let match: RegExpExecArray | null
  while ((match = URL_PATTERN.exec(rowText)) !== null) {
    const start = match.index
    const end = start + match[0].length
    if (absoluteOffset >= start && absoluteOffset < end) {
      const url = trimTrailingPunct(match[0])
      // window.open is patched in terminal.tsx to handle file:// URLs;
      // for http/https we open in a new tab directly.
      window.open(url, '_blank', 'noopener,noreferrer')
      return true
    }
  }

  return false
}

// ── Helpers ──────────────────────────────────────────────────────────────────

/**
 * Return a Range positioned at the character under (x, y), or null if the
 * point doesn't fall on a text node.
 */
function getRangeAtPoint(x: number, y: number): Range | null {
  // Standard (Chrome, Safari 15.4+)
  if (typeof document.caretRangeFromPoint === 'function') {
    return document.caretRangeFromPoint(x, y)
  }
  // Firefox
  const caretPos = (document as Document & {
    caretPositionFromPoint?: (x: number, y: number) => { offsetNode: Node; offset: number } | null
  }).caretPositionFromPoint?.(x, y)
  if (caretPos) {
    const r = document.createRange()
    r.setStart(caretPos.offsetNode, caretPos.offset)
    r.collapse(true)
    return r
  }
  return null
}

/**
 * Walk all text nodes inside `root` to compute the absolute character offset
 * of `targetNode` at `targetOffset`. Returns -1 if not found.
 */
function absoluteTextOffset(root: Element, targetNode: Node, targetOffset: number): number {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT)
  let accumulated = 0
  let node: Node | null
  while ((node = walker.nextNode()) !== null) {
    if (node === targetNode) return accumulated + targetOffset
    accumulated += node.textContent?.length ?? 0
  }
  return -1
}

/**
 * Strip trailing characters that are syntactically valid in a URL regex but
 * are almost always sentence punctuation rather than part of the URL:
 * trailing periods, commas, colons, semicolons, and closing brackets.
 */
function trimTrailingPunct(url: string): string {
  return url.replace(/[.,;:)\]'>]+$/, '')
}
