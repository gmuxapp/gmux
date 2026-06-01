import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest'
import { measureTerminalFit } from './terminal-fit'
import type { WTerm } from '@wterm/dom'

// ── Minimal fake DOM (no jsdom needed) ──────────────────────────────────────
//
// measureTerminalFit uses:
//   - containerEl.clientWidth / clientHeight  (spied via vi.spyOn getter)
//   - getComputedStyle(containerEl).paddingLeft/Right/Top/Bottom  (for padding)
//   - document.createElement('span')          (stubbed globally)
//   - probe.style.cssText = ...               (ignored by the fake)
//   - probe.textContent = 'W'                 (ignored by the fake)
//   - term.element.appendChild(probe)         (no-op in the fake)
//   - probe.getBoundingClientRect()           (spied on FakeDomElement.prototype)
//   - probe.remove()                          (no-op in the fake)
//   - getComputedStyle(term.element).getPropertyValue('--term-row-height')
//     (stubbed globally to read from element._styles)

class FakeDomElement {
  private _styles = new Map<string, string>()
  private _styleObj: {
    cssText: string
    setProperty(k: string, v: string): void
    getPropertyValue(k: string): string
    readonly paddingLeft: string
    readonly paddingRight: string
    readonly paddingTop: string
    readonly paddingBottom: string
  }
  textContent = ''

  constructor() {
    const styles = this._styles
    this._styleObj = {
      cssText: '',
      setProperty(k: string, v: string) { styles.set(k, v) },
      getPropertyValue(k: string) { return styles.get(k) ?? '' },
      get paddingLeft()   { return styles.get('paddingLeft')   ?? '' },
      get paddingRight()  { return styles.get('paddingRight')  ?? '' },
      get paddingTop()    { return styles.get('paddingTop')    ?? '' },
      get paddingBottom() { return styles.get('paddingBottom') ?? '' },
    }
  }

  get style() { return this._styleObj }

  // Spyable getters — vi.spyOn(instance, 'clientWidth', 'get').mockReturnValue(n)
  get clientWidth()  { return 0 }
  get clientHeight() { return 0 }

  appendChild(_child: FakeDomElement) { /* no-op */ }
  remove() { /* no-op */ }

  // Spied via vi.spyOn(FakeDomElement.prototype, 'getBoundingClientRect')
  getBoundingClientRect(): DOMRect {
    return { width: 0, height: 0, top: 0, left: 0, right: 0, bottom: 0, x: 0, y: 0, toJSON() { return this } }
  }
}

// ── Setup/teardown ────────────────────────────────────────────────────────────

beforeEach(() => {
  vi.stubGlobal('document', {
    createElement: (_tag: string) => new FakeDomElement(),
  })
  vi.stubGlobal('getComputedStyle', (el: FakeDomElement) => el.style)
})

afterEach(() => vi.restoreAllMocks())

// ── Helpers ───────────────────────────────────────────────────────────────────

function makeTerm(rowHeight = 17): { term: WTerm; container: FakeDomElement } {
  const element = new FakeDomElement()
  element.style.setProperty('--term-row-height', `${rowHeight}px`)
  const term = { element } as unknown as WTerm
  const container = new FakeDomElement()
  return { term, container }
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('measureTerminalFit', () => {
  it('returns null when container has zero width', () => {
    const { term, container } = makeTerm()
    vi.spyOn(container, 'clientWidth',  'get').mockReturnValue(0)
    vi.spyOn(container, 'clientHeight', 'get').mockReturnValue(400)
    expect(measureTerminalFit(term, container as unknown as HTMLElement)).toBeNull()
  })

  it('returns null when container has zero height', () => {
    const { term, container } = makeTerm()
    vi.spyOn(container, 'clientWidth',  'get').mockReturnValue(800)
    vi.spyOn(container, 'clientHeight', 'get').mockReturnValue(0)
    expect(measureTerminalFit(term, container as unknown as HTMLElement)).toBeNull()
  })

  it('returns null when char probe has zero width (font not loaded)', () => {
    const { term, container } = makeTerm()
    vi.spyOn(container, 'clientWidth',  'get').mockReturnValue(800)
    vi.spyOn(container, 'clientHeight', 'get').mockReturnValue(400)

    vi.spyOn(FakeDomElement.prototype, 'getBoundingClientRect').mockReturnValue(
      { width: 0, height: 17, top: 0, left: 0, right: 0, bottom: 17, x: 0, y: 0, toJSON() { return this } },
    )
    expect(measureTerminalFit(term, container as unknown as HTMLElement)).toBeNull()
  })

  it('calculates cols and rows from container size and char dimensions', () => {
    const { term, container } = makeTerm(17)
    vi.spyOn(container, 'clientWidth',  'get').mockReturnValue(800)
    vi.spyOn(container, 'clientHeight', 'get').mockReturnValue(425)

    vi.spyOn(FakeDomElement.prototype, 'getBoundingClientRect').mockReturnValue(
      { width: 8, height: 17, top: 0, left: 0, right: 8, bottom: 17, x: 0, y: 0, toJSON() { return this } },
    )

    const result = measureTerminalFit(term, container as unknown as HTMLElement)
    expect(result).toEqual({ cols: 100, rows: 25 }) // 800/8=100, 425/17=25
  })

  it('uses fallback row height of 17 when CSS var is missing', () => {
    const element = new FakeDomElement()
    // No --term-row-height set
    const term = { element } as unknown as WTerm
    const container = new FakeDomElement()

    vi.spyOn(container, 'clientWidth',  'get').mockReturnValue(680)
    vi.spyOn(container, 'clientHeight', 'get').mockReturnValue(340)
    vi.spyOn(FakeDomElement.prototype, 'getBoundingClientRect').mockReturnValue(
      { width: 8, height: 17, top: 0, left: 0, right: 8, bottom: 17, x: 0, y: 0, toJSON() { return this } },
    )

    const result = measureTerminalFit(term, container as unknown as HTMLElement)
    expect(result).toEqual({ cols: 85, rows: 20 }) // 680/8=85, 340/17=20
  })

  it('clamps to minimum of 1 col and 1 row', () => {
    const { term, container } = makeTerm(17)
    vi.spyOn(container, 'clientWidth',  'get').mockReturnValue(3)
    vi.spyOn(container, 'clientHeight', 'get').mockReturnValue(3)
    vi.spyOn(FakeDomElement.prototype, 'getBoundingClientRect').mockReturnValue(
      { width: 8, height: 17, top: 0, left: 0, right: 8, bottom: 17, x: 0, y: 0, toJSON() { return this } },
    )

    const result = measureTerminalFit(term, container as unknown as HTMLElement)
    expect(result).toEqual({ cols: 1, rows: 1 })
  })

  it('subtracts container padding from usable dimensions', () => {
    // Simulates padding: 0 6px 6px 6px — the real terminal-container style.
    // clientWidth=812, clientHeight=431 → usable 800×425 → 100 cols, 25 rows.
    const { term, container } = makeTerm(17)
    vi.spyOn(container, 'clientWidth',  'get').mockReturnValue(812)
    vi.spyOn(container, 'clientHeight', 'get').mockReturnValue(431)
    container.style.setProperty('paddingLeft',   '6px')
    container.style.setProperty('paddingRight',  '6px')
    container.style.setProperty('paddingTop',    '0px')
    container.style.setProperty('paddingBottom', '6px')
    vi.spyOn(FakeDomElement.prototype, 'getBoundingClientRect').mockReturnValue(
      { width: 8, height: 17, top: 0, left: 0, right: 8, bottom: 17, x: 0, y: 0, toJSON() { return this } },
    )

    const result = measureTerminalFit(term, container as unknown as HTMLElement)
    // Without padding subtraction this would be cols: 101, rows: 25
    expect(result).toEqual({ cols: 100, rows: 25 })
  })
})
