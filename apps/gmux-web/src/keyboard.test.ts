// @vitest-environment jsdom
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
// @vitest-environment jsdom

/** Build a minimal WTerm-like mock with a real DOM element for event testing. */
function makeTermMock(opts: { hasSelection?: boolean } = {}): {
  term: any
  element: HTMLElement
  sent: string[]
} {
  const element = document.createElement('div')
  const sent: string[] = []

  const term: any = {
    element,
    bridge: { bracketedPaste: () => false },
  }

  // Stub window.getSelection() for copy/interrupt tests
  if (opts.hasSelection !== undefined) {
    vi.stubGlobal('getSelection', vi.fn().mockReturnValue({
      toString: () => opts.hasSelection ? 'hello' : '',
      removeAllRanges: vi.fn(),
      addRange: vi.fn(),
    }))
  }

  return { term, element, sent }
}

/** Dispatch a synthetic KeyboardEvent on the element, return the event. */
function dispatchKey(element: HTMLElement, opts: {
  key: string
  code?: string
  ctrlKey?: boolean
  shiftKey?: boolean
  altKey?: boolean
  metaKey?: boolean
}): KeyboardEvent & { defaultPrevented: boolean } {
  const ev = new KeyboardEvent('keydown', {
    key: opts.key,
    code: opts.code ?? `Key${opts.key.toUpperCase()}`,
    ctrlKey: opts.ctrlKey ?? false,
    shiftKey: opts.shiftKey ?? false,
    altKey: opts.altKey ?? false,
    metaKey: opts.metaKey ?? false,
    bubbles: true,
    cancelable: true,
  })
  element.dispatchEvent(ev)
  return ev as KeyboardEvent & { defaultPrevented: boolean }
}

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
