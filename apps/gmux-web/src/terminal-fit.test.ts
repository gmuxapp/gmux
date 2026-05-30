// @vitest-environment jsdom

import { describe, it, expect, vi, afterEach } from 'vitest'
import { measureTerminalFit } from './terminal-fit'
import type { WTerm } from '@wterm/dom'

afterEach(() => vi.restoreAllMocks())

function makeTerm(rowHeight = 17): { term: WTerm; container: HTMLElement } {
  const element = document.createElement('div')
  // Simulate wterm setting --term-row-height after init
  element.style.setProperty('--term-row-height', `${rowHeight}px`)

  const term = { element } as unknown as WTerm

  const container = document.createElement('div')
  return { term, container }
}

describe('measureTerminalFit', () => {
  it('returns null when container has zero width', () => {
    const { term, container } = makeTerm()
    vi.spyOn(container, 'clientWidth', 'get').mockReturnValue(0)
    vi.spyOn(container, 'clientHeight', 'get').mockReturnValue(400)
    expect(measureTerminalFit(term, container)).toBeNull()
  })

  it('returns null when container has zero height', () => {
    const { term, container } = makeTerm()
    vi.spyOn(container, 'clientWidth', 'get').mockReturnValue(800)
    vi.spyOn(container, 'clientHeight', 'get').mockReturnValue(0)
    expect(measureTerminalFit(term, container)).toBeNull()
  })

  it('returns null when char probe has zero width (font not loaded)', () => {
    const { term, container } = makeTerm()
    vi.spyOn(container, 'clientWidth', 'get').mockReturnValue(800)
    vi.spyOn(container, 'clientHeight', 'get').mockReturnValue(400)

    // getBoundingClientRect returns 0-width — font not ready
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockReturnValue(
      { width: 0, height: 17, top: 0, left: 0, right: 0, bottom: 17 } as DOMRect,
    )
    expect(measureTerminalFit(term, container)).toBeNull()
  })

  it('calculates cols and rows from container size and char dimensions', () => {
    const { term, container } = makeTerm(17)
    vi.spyOn(container, 'clientWidth', 'get').mockReturnValue(800)
    vi.spyOn(container, 'clientHeight', 'get').mockReturnValue(425)

    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockReturnValue(
      { width: 8, height: 17, top: 0, left: 0, right: 8, bottom: 17 } as DOMRect,
    )

    const result = measureTerminalFit(term, container)
    expect(result).toEqual({ cols: 100, rows: 25 }) // 800/8=100, 425/17=25
  })

  it('uses fallback row height of 17 when CSS var is missing', () => {
    const element = document.createElement('div')
    // No --term-row-height set
    const term = { element } as unknown as WTerm
    const container = document.createElement('div')

    vi.spyOn(container, 'clientWidth', 'get').mockReturnValue(680)
    vi.spyOn(container, 'clientHeight', 'get').mockReturnValue(340)
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockReturnValue(
      { width: 8, height: 17, top: 0, left: 0, right: 8, bottom: 17 } as DOMRect,
    )

    const result = measureTerminalFit(term, container)
    expect(result).toEqual({ cols: 85, rows: 20 }) // 680/8=85, 340/17=20
  })

  it('clamps to minimum of 1 col and 1 row', () => {
    const { term, container } = makeTerm(17)
    vi.spyOn(container, 'clientWidth', 'get').mockReturnValue(3)
    vi.spyOn(container, 'clientHeight', 'get').mockReturnValue(3)
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockReturnValue(
      { width: 8, height: 17, top: 0, left: 0, right: 8, bottom: 17 } as DOMRect,
    )

    const result = measureTerminalFit(term, container)
    expect(result).toEqual({ cols: 1, rows: 1 })
  })
})
