/**
 * Selection helpers for wterm DOM rendering.
 * Uses getSelection() — no terminal-specific buffer API needed.
 */

export function getSelectionText(): string {
  return getSelection()?.toString() ?? ''
}

export function selectAllAndCopy(element: HTMLElement): void {
  const range = document.createRange()
  range.selectNodeContents(element)
  const sel = getSelection()
  sel?.removeAllRanges()
  sel?.addRange(range)
  const text = sel?.toString()
  if (text) navigator.clipboard.writeText(text).catch(() => {})
}

export function clearSelection(): void {
  getSelection()?.removeAllRanges()
}
