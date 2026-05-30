/**
 * Selection helpers for wterm DOM rendering.
 * Uses window.getSelection() — no terminal-specific buffer API needed.
 */

export function getSelectionText(): string {
  return window.getSelection()?.toString() ?? ''
}

export function selectAllAndCopy(element: HTMLElement): void {
  const range = document.createRange()
  range.selectNodeContents(element)
  const sel = window.getSelection()
  sel?.removeAllRanges()
  sel?.addRange(range)
  const text = sel?.toString()
  if (text) navigator.clipboard.writeText(text).catch(() => {})
}

export function clearSelection(): void {
  window.getSelection()?.removeAllRanges()
}
