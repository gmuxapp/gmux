import type { WTerm } from '@wterm/dom'
import type { TerminalSize } from './terminal-io'

/**
 * Measure cols/rows that fit inside containerEl using the font rendered
 * inside term.element. Injects a temporary probe span; reads --term-row-height
 * from computed style for row height (set by wterm after init).
 * Returns null if element is not laid out or font not ready.
 */
export function measureTerminalFit(
  term: WTerm,
  containerEl: HTMLElement,
): TerminalSize | null {
  const { clientWidth: w, clientHeight: h } = containerEl
  if (w === 0 || h === 0) return null

  const probe = document.createElement('span')
  probe.style.cssText = 'visibility:hidden;position:absolute;pointer-events:none;white-space:pre'
  probe.textContent = 'W'
  term.element.appendChild(probe)
  const charWidth = probe.getBoundingClientRect().width
  probe.remove()

  if (!charWidth) return null

  const rowHeight =
    parseFloat(getComputedStyle(term.element).getPropertyValue('--term-row-height')) || 17

  return {
    cols: Math.max(1, Math.floor(w / charWidth)),
    rows: Math.max(1, Math.floor(h / rowHeight)),
  }
}
