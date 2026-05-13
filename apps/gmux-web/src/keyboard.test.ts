import { describe, it, expect, vi, beforeEach } from 'vitest'
import { attachKeyboardHandler, ctrlSequenceFor, formatPasteText, pickBinaryDataTransferItem } from './keyboard'
import { resolveKeybinds } from './keybinds'

// Build an array-like stand-in for DataTransferItemList. Vitest runs in
// node by default, where the real DOM type isn't available; a plain
// indexed object with .length matches what the function actually reads.
function makeItems(
  entries: ReadonlyArray<{ kind: 'string' | 'file'; type: string }>,
): DataTransferItemList {
  const list: Record<string | number, unknown> = { length: entries.length }
  entries.forEach((e, i) => {
    list[i] = { ...e, getAsFile: () => null, getAsString: () => undefined }
  })
  return list as unknown as DataTransferItemList
}

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
    // A 'string' item's getAsFile() returns null; uploading it would
    // produce a confusing failure-toast race. The kind check is what
    // prevents that.
    const items = makeItems([{ kind: 'string', type: 'application/json' }])
    expect(pickBinaryDataTransferItem(items)).toBe(null)
  })

  it('skips kind=file when the MIME is text/*', () => {
    // Some browsers expose dragged .txt files as kind='file' with type
    // 'text/plain'. We treat those as text and let the existing text
    // paste path handle them, not the binary upload path.
    const items = makeItems([{ kind: 'file', type: 'text/plain' }])
    expect(pickBinaryDataTransferItem(items)).toBe(null)
  })

  it('returns null on empty list', () => {
    expect(pickBinaryDataTransferItem(makeItems([]))).toBe(null)
  })

  it('returns the first qualifying file even when later items are also binary', () => {
    // Order matters: the function must not pick "the most preferred"
    // image type, just the first qualifying one. Browsers are responsible
    // for ordering the representations sensibly.
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
    // An attacker could embed \x1b[201~ to break out of bracketed paste.
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
    // Ctrl+Shift+A: ESC [ 97 ; 6 u
    expect(ctrlSequenceFor('A')).toBe('\x1b[97;6u')
    expect(ctrlSequenceFor('C')).toBe('\x1b[99;6u')
    expect(ctrlSequenceFor('Z')).toBe('\x1b[122;6u')
  })

  it('handles special characters', () => {
    expect(ctrlSequenceFor('[')).toBe('\x1b')  // ESC
    expect(ctrlSequenceFor(']')).toBe('\x1d')  // GS
    expect(ctrlSequenceFor('\\')).toBe('\x1c') // FS
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
//
// This test suite pins the correct return values so a regression is caught
// immediately rather than at e2e time.

type Handler = (ev: KeyboardEvent) => boolean

/** Build a minimal Terminal mock that captures the registered handler. */
function makeTermMock(opts: { hasSelection?: boolean } = {}): {
  term: any
  getHandler: () => Handler
  sent: string[]
} {
  let handler: Handler = () => false
  const sent: string[] = []

  const term: any = {
    element: null,  // no container — keypress suppression listener is a no-op
    attachCustomKeyEventHandler: (fn: Handler) => { handler = fn },
    hasBracketedPaste: () => false,
    getSelectionPosition: () => opts.hasSelection
      ? { start: { x: 0, y: 0 }, end: { x: 5, y: 0 } }
      : undefined,
    getScrollbackLength: () => 0,
    getViewportY: () => 0,
    hasSelection: () => !!opts.hasSelection,
    clearSelection: vi.fn(),
    selectAll: vi.fn(),
    buffer: {
      active: {
        getLine: () => ({
          isWrapped: false,
          translateToString: () => 'hello',
        }),
      },
    },
    cols: 80,
  }

  return { term, getHandler: () => handler, sent }
}

/** Build a synthetic KeyboardEvent. */
function makeKey(opts: {
  key: string
  code?: string
  type?: string
  ctrlKey?: boolean
  shiftKey?: boolean
  altKey?: boolean
  metaKey?: boolean
}): KeyboardEvent {
  return {
    key: opts.key,
    code: opts.code ?? `Key${opts.key.toUpperCase()}`,
    type: opts.type ?? 'keydown',
    ctrlKey: opts.ctrlKey ?? false,
    shiftKey: opts.shiftKey ?? false,
    altKey: opts.altKey ?? false,
    metaKey: opts.metaKey ?? false,
    preventDefault: vi.fn(),
  } as unknown as KeyboardEvent
}

const DEFAULT_KEYBINDS = resolveKeybinds(null)

describe('attachKeyboardHandler — ghostty-web return-value contract', () => {
  // clipboard API doesn't exist in Node; mock it for tests that call copy actions
  const clipboardWriteText = vi.fn().mockResolvedValue(undefined)
  beforeEach(() => {
    clipboardWriteText.mockClear()
    Object.defineProperty(globalThis, 'navigator', {
      value: { clipboard: { writeText: clipboardWriteText } },
      configurable: true,
    })
  })
  it('returns false for normal typing (no keybind match) — ghostty pass-through', () => {
    const { term, getHandler, sent } = makeTermMock()
    const send = (d: string) => sent.push(d)
    attachKeyboardHandler(term, send, send, DEFAULT_KEYBINDS)
    const h = getHandler()

    // Plain letter: not a keybind
    expect(h(makeKey({ key: 'a' }))).toBe(false)
    // Arrow key: not a keybind
    expect(h(makeKey({ key: 'ArrowLeft', code: 'ArrowLeft' }))).toBe(false)
    // Digit
    expect(h(makeKey({ key: '1' }))).toBe(false)
  })

  it('returns true for ctrl+shift+c (copy) — ghostty consumed', () => {
    const { term, getHandler } = makeTermMock({ hasSelection: true })
    const send = vi.fn()
    attachKeyboardHandler(term, send, send, DEFAULT_KEYBINDS)
    const h = getHandler()

    const ev = makeKey({ key: 'c', ctrlKey: true, shiftKey: true })
    expect(h(ev)).toBe(true)
  })

  it('returns true for ctrl+c with selection (copyOrInterrupt copies) — ghostty consumed', () => {
    const { term, getHandler } = makeTermMock({ hasSelection: true })
    const send = vi.fn()
    attachKeyboardHandler(term, send, send, DEFAULT_KEYBINDS)
    const h = getHandler()

    expect(h(makeKey({ key: 'c', ctrlKey: true }))).toBe(true)
  })

  it('returns false for ctrl+c with no selection (copyOrInterrupt → SIGINT) — ghostty pass-through', () => {
    const { term, getHandler } = makeTermMock({ hasSelection: false })
    const send = vi.fn()
    attachKeyboardHandler(term, send, send, DEFAULT_KEYBINDS)
    const h = getHandler()

    // No selection: should fall through so ghostty sends ^C to PTY
    expect(h(makeKey({ key: 'c', ctrlKey: true }))).toBe(false)
  })

  it('returns true for ctrl+v (paste) — ghostty consumed', () => {
    const { term, getHandler } = makeTermMock()
    const send = vi.fn()
    attachKeyboardHandler(term, send, send, DEFAULT_KEYBINDS)
    const h = getHandler()

    expect(h(makeKey({ key: 'v', ctrlKey: true }))).toBe(true)
  })

  it('returns true for ctrl+shift+v (paste) — ghostty consumed', () => {
    const { term, getHandler } = makeTermMock()
    const send = vi.fn()
    attachKeyboardHandler(term, send, send, DEFAULT_KEYBINDS)
    const h = getHandler()

    expect(h(makeKey({ key: 'v', ctrlKey: true, shiftKey: true }))).toBe(true)
  })

  it('returns true for shift+enter (sendText) — ghostty consumed', () => {
    const { term, getHandler } = makeTermMock()
    const send = vi.fn()
    attachKeyboardHandler(term, send, send, DEFAULT_KEYBINDS)
    const h = getHandler()

    expect(h(makeKey({ key: 'Enter', shiftKey: true }))).toBe(true)
  })

  it('shift+enter on keydown sends \\n via send, not via ghostty PTY path', () => {
    const { term, getHandler } = makeTermMock()
    const sent: string[] = []
    const send = (d: string) => sent.push(d)
    attachKeyboardHandler(term, send, send, DEFAULT_KEYBINDS)
    const h = getHandler()

    h(makeKey({ key: 'Enter', shiftKey: true, type: 'keydown' }))
    expect(sent).toContain('\n')
  })

  it('returns false for keypress events on unmatched keys — ghostty pass-through', () => {
    const { term, getHandler } = makeTermMock()
    const send = vi.fn()
    attachKeyboardHandler(term, send, send, DEFAULT_KEYBINDS)
    const h = getHandler()

    // keypress type, no keybind
    expect(h(makeKey({ key: 'a', type: 'keypress' }))).toBe(false)
  })
})
