import { beforeAll, describe, expect, test } from 'vitest'
import type { Terminal } from '@xterm/xterm'
import { linkAtPoint, openLinkAtPoint } from './terminal-link'

// Node has Event/EventTarget but not MouseEvent; polyfill enough for
// the synthetic handshake the module dispatches.
beforeAll(() => {
  if (typeof globalThis.MouseEvent === 'undefined') {
    class MouseEventPolyfill extends Event {
      clientX: number
      clientY: number
      button: number
      buttons: number
      constructor(type: string, init: MouseEventInit = {}) {
        super(type, init)
        this.clientX = init.clientX ?? 0
        this.clientY = init.clientY ?? 0
        this.button = init.button ?? 0
        this.buttons = init.buttons ?? 0
      }
    }
    // @ts-expect-error assigning polyfill into the global scope
    globalThis.MouseEvent = MouseEventPolyfill
  }
})

interface InternalLinkShape {
  text: string
  range: { start: { x: number, y: number }, end: { x: number, y: number } }
}

interface FakeTermOptions {
  /** Called on mousemove; return a link to simulate resolution. */
  resolveLink?: () => InternalLinkShape | undefined
  /** 1-based absolute buffer rows as strings (row 1 first). */
  bufferRows?: string[]
  cols?: number
  withScreen?: boolean
  withLinkifier?: boolean
}

function makeFakeTerm(opts: FakeTermOptions = {}) {
  const { resolveLink, bufferRows = [], cols = 80, withScreen = true, withLinkifier = true } = opts
  const screen = new EventTarget()
  const received: { type: string, clientX: number, clientY: number }[] = []
  const linkifier: { currentLink?: { link: InternalLinkShape } } = {}

  for (const type of ['mousemove', 'mousedown', 'mouseup']) {
    screen.addEventListener(type, ev => {
      const me = ev as MouseEvent
      received.push({ type: ev.type, clientX: me.clientX, clientY: me.clientY })
      if (ev.type === 'mousemove' && resolveLink) {
        const link = resolveLink()
        linkifier.currentLink = link ? { link } : undefined
      }
    })
  }

  const term = {
    element: withScreen
      ? { querySelector: (sel: string) => (sel === '.xterm-screen' ? screen : null) }
      : null,
    cols,
    buffer: {
      active: {
        getLine: (y: number) => {
          const text = bufferRows[y]
          if (text === undefined) return undefined
          return {
            translateToString: (_trim: boolean, fromCol: number, toCol: number) =>
              text.padEnd(cols).slice(fromCol, toCol),
          }
        },
      },
    },
    _core: withLinkifier ? { linkifier } : {},
  } as unknown as Terminal

  return { term, received }
}

describe('openLinkAtPoint', () => {
  const link = (): InternalLinkShape => ({
    text: 'https://example.com',
    range: { start: { x: 1, y: 1 }, end: { x: 19, y: 1 } },
  })

  test('activates a resolved link with the full handshake, in order', () => {
    const { term, received } = makeFakeTerm({ resolveLink: link })

    expect(openLinkAtPoint(term, 42, 17)).toBe(true)
    expect(received.map(e => e.type)).toEqual(['mousemove', 'mousedown', 'mouseup'])
    // All events target the tap coordinates so the Linkifier's position
    // checks (resolve, press, release) all hit the same cell.
    for (const ev of received) {
      expect(ev.clientX).toBe(42)
      expect(ev.clientY).toBe(17)
    }
  })

  test('stops after hover when no link resolves (no synthetic click side effects)', () => {
    const { term, received } = makeFakeTerm({ resolveLink: () => undefined })

    expect(openLinkAtPoint(term, 5, 5)).toBe(false)
    expect(received.map(e => e.type)).toEqual(['mousemove'])
  })

  test('returns false when the screen element is missing', () => {
    const { term, received } = makeFakeTerm({ withScreen: false })

    expect(openLinkAtPoint(term, 5, 5)).toBe(false)
    expect(received).toEqual([])
  })

  test('returns false when the internal linkifier is unavailable', () => {
    const { term, received } = makeFakeTerm({ withLinkifier: false })

    expect(openLinkAtPoint(term, 5, 5)).toBe(false)
    expect(received).toEqual([])
  })
})

describe('linkAtPoint', () => {
  test('never activates: only the hover is dispatched', () => {
    const { term, received } = makeFakeTerm({
      resolveLink: () => ({
        text: 'https://example.com',
        range: { start: { x: 1, y: 2 }, end: { x: 19, y: 2 } },
      }),
      // bufferRows[1] is buffer row y=2 (getLine is 0-based)
      bufferRows: ['', 'https://example.com rest of line'],
    })

    expect(linkAtPoint(term, 1, 1)).not.toBeNull()
    expect(received.map(e => e.type)).toEqual(['mousemove'])
  })

  test('plain URL: label equals uri', () => {
    const { term } = makeFakeTerm({
      resolveLink: () => ({
        text: 'https://example.com',
        // cols 5..23 on row 1 (1-based inclusive)
        range: { start: { x: 5, y: 1 }, end: { x: 23, y: 1 } },
      }),
      bufferRows: ['see https://example.com for details'],
    })

    expect(linkAtPoint(term, 1, 1)).toEqual({
      uri: 'https://example.com',
      label: 'https://example.com',
    })
  })

  test('OSC 8 link: label is the visible text, uri the hidden target', () => {
    const { term } = makeFakeTerm({
      resolveLink: () => ({
        text: 'https://evil.example/payload',
        range: { start: { x: 1, y: 1 }, end: { x: 10, y: 1 } },
      }),
      bufferRows: ['Click here to continue'],
    })

    expect(linkAtPoint(term, 1, 1)).toEqual({
      uri: 'https://evil.example/payload',
      label: 'Click here',
    })
  })

  test('label spans wrapped rows', () => {
    const cols = 10
    const { term } = makeFakeTerm({
      cols,
      resolveLink: () => ({
        text: 'https://a.io/xyz',
        // starts at col 5 of row 1, ends at col 10 of row 2
        range: { start: { x: 5, y: 1 }, end: { x: 10, y: 2 } },
      }),
      bufferRows: ['see https:', '//a.io/xyz'],
    })

    expect(linkAtPoint(term, 1, 1)?.label).toBe('https://a.io/xyz')
  })

  test('falls back to uri when a range row is missing from the buffer', () => {
    const { term } = makeFakeTerm({
      resolveLink: () => ({
        text: 'https://example.com',
        range: { start: { x: 1, y: 99 }, end: { x: 19, y: 99 } },
      }),
      bufferRows: ['only one row'],
    })

    expect(linkAtPoint(term, 1, 1)?.label).toBe('https://example.com')
  })

  test('returns null when nothing resolves', () => {
    const { term } = makeFakeTerm({ resolveLink: () => undefined })
    expect(linkAtPoint(term, 1, 1)).toBeNull()
  })
})
