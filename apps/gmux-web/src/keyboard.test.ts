import { describe, it, expect, vi, beforeEach } from 'vitest'
import { attachKeyboardHandler, ctrlSequenceFor, formatPasteText, pickBinaryDataTransferItem } from './keyboard'
import { resolveKeybinds } from './keybinds'

// ── Minimal fake DOM (no jsdom needed) ──────────────────────────────────────
//
// keyboard.ts only uses element.addEventListener / removeEventListener /
// dispatchEvent — a lightweight in-process fake is sufficient.

/** Minimal keyboard event — only the properties attachKeyboardHandler reads. */
class FakeKeyboardEvent {
  type: string
  key: string
  code: string
  ctrlKey: boolean
  shiftKey: boolean
  altKey: boolean
  metaKey: boolean
  bubbles: boolean
  cancelable: boolean
  defaultPrevented = false

  constructor(type: string, opts: {
    key?: string; code?: string; ctrlKey?: boolean; shiftKey?: boolean;
    altKey?: boolean; metaKey?: boolean; bubbles?: boolean; cancelable?: boolean
  } = {}) {
    this.type = type
    this.key  = opts.key  ?? ''
    this.code = opts.code ?? `Key${(opts.key ?? '').toUpperCase()}`
    this.ctrlKey    = opts.ctrlKey  ?? false
    this.shiftKey   = opts.shiftKey ?? false
    this.altKey     = opts.altKey   ?? false
    this.metaKey    = opts.metaKey  ?? false
    this.bubbles    = opts.bubbles  ?? false
    this.cancelable = opts.cancelable ?? false
  }

  preventDefault()             { this.defaultPrevented = true }
  stopImmediatePropagation()   {}
  stopPropagation()            {}
}

/** Minimal element with capture-phase addEventListener / dispatchEvent. */
class FakeElement {
  private _capture = new Map<string, Array<(ev: FakeKeyboardEvent) => void>>()
  private _bubble  = new Map<string, Array<(ev: FakeKeyboardEvent) => void>>()

  addEventListener(type: string, fn: (ev: any) => void, opts?: boolean | { capture?: boolean }) {
    const capture = typeof opts === 'boolean' ? opts : (opts?.capture ?? false)
    const map = capture ? this._capture : this._bubble
    if (!map.has(type)) map.set(type, [])
    map.get(type)!.push(fn)
  }

  removeEventListener(type: string, fn: (ev: any) => void, opts?: boolean | { capture?: boolean }) {
    const capture = typeof opts === 'boolean' ? opts : (opts?.capture ?? false)
    const map = capture ? this._capture : this._bubble
    const list = map.get(type)
    if (!list) return
    const idx = list.indexOf(fn)
    if (idx >= 0) list.splice(idx, 1)
  }

  dispatchEvent(ev: FakeKeyboardEvent): boolean {
    // Target phase: capture listeners fire before bubble listeners.
    const captures = this._capture.get(ev.type) ?? []
    const bubbles  = this._bubble.get(ev.type)  ?? []
    for (const fn of [...captures, ...bubbles]) fn(ev)
    return !ev.defaultPrevented
  }
}

// ── Test helpers ─────────────────────────────────────────────────────────────

// Build an array-like stand-in for DataTransferItemList.
function makeItems(
  entries: ReadonlyArray<{ kind: 'string' | 'file'; type: string }>,
): DataTransferItemList {
  const list: Record<string | number, unknown> = { length: entries.length }
  entries.forEach((e, i) => {
    list[i] = { ...e, getAsFile: () => null, getAsString: () => undefined }
  })
  return list as unknown as DataTransferItemList
}

/** Build a minimal WTerm-like mock with a fake DOM element for event testing. */
function makeTermMock(opts: { hasSelection?: boolean } = {}): {
  term: any
  element: FakeElement
  sent: string[]
} {
  const element = new FakeElement()
  const sent: string[] = []

  const term: any = {
    element,
    bridge: { bracketedPaste: () => false },
  }

  if (opts.hasSelection !== undefined) {
    vi.stubGlobal('getSelection', vi.fn().mockReturnValue({
      toString: () => opts.hasSelection ? 'hello' : '',
      removeAllRanges: vi.fn(),
      addRange: vi.fn(),
    }))
  }

  return { term, element, sent }
}

/** Dispatch a synthetic key event on the element and return the event. */
function dispatchKey(element: FakeElement, opts: {
  key: string
  code?: string
  ctrlKey?: boolean
  shiftKey?: boolean
  altKey?: boolean
  metaKey?: boolean
}): FakeKeyboardEvent {
  const ev = new FakeKeyboardEvent('keydown', {
    key:      opts.key,
    code:     opts.code ?? `Key${opts.key.toUpperCase()}`,
    ctrlKey:  opts.ctrlKey  ?? false,
    shiftKey: opts.shiftKey ?? false,
    altKey:   opts.altKey   ?? false,
    metaKey:  opts.metaKey  ?? false,
    bubbles:    true,
    cancelable: true,
  })
  element.dispatchEvent(ev)
  return ev
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('pickBinaryDataTransferItem', () => {
  it('returns the first file with a non-text MIME', () => {
    const items = makeItems([
      { kind: 'string', type: 'text/plain' },
      { kind: 'file', type: 'image/png' },
      { kind: 'file', type: 'image/jpeg' },
    ])
    expect(pickBinaryDataTransferItem(items)?.type).toBe('image/png')
  })

  it('skips kind=string even when the MIME is non-text', () => {
    const items = makeItems([{ kind: 'string', type: 'application/json' }])
    expect(pickBinaryDataTransferItem(items)).toBe(null)
  })

  it('skips kind=file when the MIME is text/*', () => {
    const items = makeItems([{ kind: 'file', type: 'text/plain' }])
    expect(pickBinaryDataTransferItem(items)).toBe(null)
  })

  it('returns null on empty list', () => {
    expect(pickBinaryDataTransferItem(makeItems([]))).toBe(null)
  })

  it('returns the first qualifying file even when later items are also binary', () => {
    const items = makeItems([
      { kind: 'file', type: 'image/jpeg' },
      { kind: 'file', type: 'image/png' },
    ])
    expect(pickBinaryDataTransferItem(items)?.type).toBe('image/jpeg')
  })
})

describe('formatPasteText', () => {
  // ── Bracketed paste mode ──────────────────────────────────────────────────

  it('wraps text in bracket sequences when bracketedPasteMode is on', () => {
    expect(formatPasteText('hello', true)).toBe('\x1b[200~hello\x1b[201~')
  })

  it('normalizes \\n to \\r inside brackets', () => {
    expect(formatPasteText('a\nb', true)).toBe('\x1b[200~a\rb\x1b[201~')
  })

  it('normalizes \\r\\n to \\r inside brackets', () => {
    expect(formatPasteText('a\r\nb', true)).toBe('\x1b[200~a\rb\x1b[201~')
  })

  it('sanitizes ESC inside brackets to prevent early termination', () => {
    expect(formatPasteText('safe\x1b[201~injected', true))
      .toBe('\x1b[200~safe\u241b[201~injected\x1b[201~')
  })

  // ── Non-bracketed paste mode ──────────────────────────────────────────────
  // Newlines stay as \n so raw-mode apps can distinguish them from Enter (\r).

  it('keeps \\n as \\n in non-bracketed mode', () => {
    expect(formatPasteText('a\nb\nc', false)).toBe('a\nb\nc')
  })

  it('normalizes \\r\\n to \\n in non-bracketed mode', () => {
    expect(formatPasteText('a\r\nb', false)).toBe('a\nb')
  })

  it('normalizes bare \\r to \\n in non-bracketed mode', () => {
    expect(formatPasteText('a\rb', false)).toBe('a\nb')
  })

  it('handles mixed line endings in non-bracketed mode', () => {
    expect(formatPasteText('a\nb\r\nc\rd', false)).toBe('a\nb\nc\nd')
  })

  it('passes through text without newlines unchanged', () => {
    expect(formatPasteText('hello world', false)).toBe('hello world')
    expect(formatPasteText('hello world', true))
      .toBe('\x1b[200~hello world\x1b[201~')
  })
})

describe('ctrlSequenceFor', () => {
  it('produces traditional control codes for lowercase letters', () => {
    expect(ctrlSequenceFor('a')).toBe('\x01')
    expect(ctrlSequenceFor('c')).toBe('\x03')
    expect(ctrlSequenceFor('z')).toBe('\x1a')
  })

  it('produces CSI u sequences for uppercase letters (Ctrl+Shift)', () => {
    expect(ctrlSequenceFor('A')).toBe('\x1b[97;6u')
    expect(ctrlSequenceFor('C')).toBe('\x1b[99;6u')
    expect(ctrlSequenceFor('Z')).toBe('\x1b[122;6u')
  })

  it('handles special characters', () => {
    expect(ctrlSequenceFor('[')).toBe('\x1b')   // ESC
    expect(ctrlSequenceFor(']')).toBe('\x1d')   // GS
    expect(ctrlSequenceFor('\\')).toBe('\x1c')  // FS
  })

  it('returns null for multi-character strings and unknown chars', () => {
    expect(ctrlSequenceFor('ab')).toBeNull()
    expect(ctrlSequenceFor('1')).toBeNull()
    expect(ctrlSequenceFor('')).toBeNull()
  })
})

// ── attachKeyboardHandler — ghostty-web return-value contract ─────────────
//
// ghostty-web's customKeyEventHandler semantics are the INVERSE of xterm.js:
//   return true  → consumed (ghostty calls preventDefault + returns, key NOT sent to PTY)
//   return false → pass-through (ghostty encodes and sends key to PTY via WASM)

const DEFAULT_KEYBINDS = resolveKeybinds(null)

describe('attachKeyboardHandler — wterm DOM event interception', () => {
  const clipboardWriteText = vi.fn().mockResolvedValue(undefined)
  beforeEach(() => {
    clipboardWriteText.mockClear()
    vi.unstubAllGlobals()
    Object.defineProperty(globalThis, 'navigator', {
      value: { clipboard: { writeText: clipboardWriteText } },
      configurable: true,
    })
  })

  it('does not preventDefault for normal typing (no keybind match)', () => {
    const { term, element, sent } = makeTermMock()
    attachKeyboardHandler(term, (d) => sent.push(d), (d) => sent.push(d), DEFAULT_KEYBINDS)
    const ev = dispatchKey(element, { key: 'a' })
    expect(ev.defaultPrevented).toBe(false)
  })

  it('prevents default for ctrl+shift+c (copy)', () => {
    const { term, element } = makeTermMock({ hasSelection: true })
    attachKeyboardHandler(term, vi.fn(), vi.fn(), DEFAULT_KEYBINDS)
    const ev = dispatchKey(element, { key: 'c', ctrlKey: true, shiftKey: true })
    expect(ev.defaultPrevented).toBe(true)
  })

  it('prevents default for ctrl+c with selection (copyOrInterrupt copies)', () => {
    const { term, element } = makeTermMock({ hasSelection: true })
    attachKeyboardHandler(term, vi.fn(), vi.fn(), DEFAULT_KEYBINDS)
    const ev = dispatchKey(element, { key: 'c', ctrlKey: true })
    expect(ev.defaultPrevented).toBe(true)
  })

  it('does not prevent default for ctrl+c with no selection (copyOrInterrupt falls through to SIGINT)', () => {
    const { term, element } = makeTermMock({ hasSelection: false })
    attachKeyboardHandler(term, vi.fn(), vi.fn(), DEFAULT_KEYBINDS)
    const ev = dispatchKey(element, { key: 'c', ctrlKey: true })
    expect(ev.defaultPrevented).toBe(false)
  })

  it('prevents default for ctrl+v (paste)', () => {
    const { term, element } = makeTermMock()
    attachKeyboardHandler(term, vi.fn(), vi.fn(), DEFAULT_KEYBINDS)
    const ev = dispatchKey(element, { key: 'v', ctrlKey: true })
    expect(ev.defaultPrevented).toBe(true)
  })

  it('prevents default for ctrl+shift+v (paste)', () => {
    const { term, element } = makeTermMock()
    attachKeyboardHandler(term, vi.fn(), vi.fn(), DEFAULT_KEYBINDS)
    const ev = dispatchKey(element, { key: 'v', ctrlKey: true, shiftKey: true })
    expect(ev.defaultPrevented).toBe(true)
  })

  it('prevents default for shift+enter (sendText)', () => {
    const { term, element } = makeTermMock()
    attachKeyboardHandler(term, vi.fn(), vi.fn(), DEFAULT_KEYBINDS)
    const ev = dispatchKey(element, { key: 'Enter', shiftKey: true })
    expect(ev.defaultPrevented).toBe(true)
  })

  it('shift+enter sends \\n via send callback', () => {
    const { term, element, sent } = makeTermMock()
    attachKeyboardHandler(term, (d) => sent.push(d), vi.fn(), DEFAULT_KEYBINDS)
    dispatchKey(element, { key: 'Enter', shiftKey: true })
    expect(sent).toContain('\n')
  })

  it('cleanup removes listener — no more interception after dispose', () => {
    const { term, element } = makeTermMock()
    const cleanup = attachKeyboardHandler(term, vi.fn(), vi.fn(), DEFAULT_KEYBINDS)
    cleanup()
    // After cleanup, ctrl+v should NOT be prevented
    const ev = dispatchKey(element, { key: 'v', ctrlKey: true })
    expect(ev.defaultPrevented).toBe(false)
  })
})
