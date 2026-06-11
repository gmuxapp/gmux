import { beforeAll, describe, expect, test } from 'vitest'
import type { Terminal } from '@xterm/xterm'
import { openLinkAtPoint } from './terminal-link'

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

interface FakeTermOptions {
  /** Called on mousemove; return a truthy link to simulate resolution. */
  resolveLink?: () => unknown
  withScreen?: boolean
  withLinkifier?: boolean
}

function makeFakeTerm(opts: FakeTermOptions = {}) {
  const { resolveLink, withScreen = true, withLinkifier = true } = opts
  const screen = new EventTarget()
  const received: { type: string, clientX: number, clientY: number }[] = []
  const linkifier: { currentLink?: unknown } = {}

  for (const type of ['mousemove', 'mousedown', 'mouseup']) {
    screen.addEventListener(type, ev => {
      const me = ev as MouseEvent
      received.push({ type: ev.type, clientX: me.clientX, clientY: me.clientY })
      if (ev.type === 'mousemove' && resolveLink) {
        linkifier.currentLink = resolveLink()
      }
    })
  }

  const term = {
    element: withScreen
      ? { querySelector: (sel: string) => (sel === '.xterm-screen' ? screen : null) }
      : null,
    _core: withLinkifier ? { linkifier } : {},
  } as unknown as Terminal

  return { term, received }
}

describe('openLinkAtPoint', () => {
  test('activates a resolved link with the full handshake, in order', () => {
    const { term, received } = makeFakeTerm({ resolveLink: () => ({ link: {} }) })

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
