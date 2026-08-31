import { afterEach, describe, expect, it, vi } from 'vitest'
import { applyArmedModifiers, ctrlSequenceFor, formatPasteText, handlePasteAction, type PasteDestination, pickBinaryDataTransferItem } from './keyboard'

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

afterEach(() => vi.unstubAllGlobals())

function destination(sessionId: string, send: (text: string) => boolean): PasteDestination {
  return { sessionId, bracketedPasteMode: false, send }
}

function binaryClipboard(getType: () => Promise<Blob>): Clipboard {
  return {
    read: async () => [{ types: ['image/png'], getType }],
  } as unknown as Clipboard
}

describe('paste destination binding', () => {
  it('uses one namespaced destination for both upload and delivery', async () => {
    const sent: string[] = []
    let requestURL = ''
    vi.stubGlobal('navigator', { clipboard: binaryClipboard(async () => new Blob(['png'], { type: 'image/png' })) })
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      requestURL = url
      return new Response(JSON.stringify({ ok: true, data: { path: '/tmp/b.png' } }))
    }))

    await handlePasteAction(destination('abc@paste-container', text => { sent.push(text); return true }), vi.fn())

    expect(requestURL).toBe('/v1/sessions/abc%40paste-container/clipboard')
    expect(sent).toEqual(['/tmp/b.png'])
  })

  it('does not inject a delayed A upload after its connection is replaced', async () => {
    let finish!: (response: Response) => void
    vi.stubGlobal('navigator', { clipboard: binaryClipboard(async () => new Blob(['png'])) })
    vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>(resolve => { finish = resolve })))
    let currentConnection = 'A'
    const sent: string[] = []
    const feedback = vi.fn()
    const action = handlePasteAction(destination('A', text => {
      if (currentConnection !== 'A') return false
      sent.push(text)
      return true
    }), feedback)

    await vi.waitFor(() => expect(finish).toBeTypeOf('function'))
    currentConnection = 'B'
    finish(new Response(JSON.stringify({ ok: true, data: { path: '/tmp/a.png' } })))
    await action

    expect(sent).toEqual([])
    expect(feedback).toHaveBeenCalledWith('error', 'Paste cancelled: terminal connection changed')
  })

  it('does not use a replacement socket for stale completion', async () => {
    let connection = {}
    const captured = connection
    const sent: string[] = []
    vi.stubGlobal('navigator', { clipboard: binaryClipboard(async () => new Blob(['png'])) })
    vi.stubGlobal('fetch', vi.fn(async () => {
      connection = {}
      return new Response(JSON.stringify({ ok: true, data: { path: '/tmp/stale.png' } }))
    }))
    const feedback = vi.fn()

    await handlePasteAction(destination('A', text => {
      if (connection !== captured) return false
      sent.push(text)
      return true
    }), feedback)

    expect(sent).toEqual([])
    expect(feedback).toHaveBeenCalledWith('error', expect.stringContaining('connection changed'))
  })

  it('surfaces ClipboardItem.getType failure without uploading or sending', async () => {
    const fetch = vi.fn()
    const send = vi.fn(() => true)
    const feedback = vi.fn()
    vi.stubGlobal('navigator', { clipboard: binaryClipboard(async () => { throw new Error('gone') }) })
    vi.stubGlobal('fetch', fetch)

    await handlePasteAction(destination('A', send), feedback)

    expect(fetch).not.toHaveBeenCalled()
    expect(send).not.toHaveBeenCalled()
    expect(feedback).toHaveBeenCalledWith('error', 'Paste failed: could not read clipboard item')
  })
})

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

describe('applyArmedModifiers', () => {
  it('passes data through unchanged when nothing is armed', () => {
    expect(applyArmedModifiers('a', false, false))
      .toEqual({ seq: 'a', ctrlApplied: false, altApplied: false, shiftApplied: false })
    expect(applyArmedModifiers('\r', false, false))
      .toEqual({ seq: '\r', ctrlApplied: false, altApplied: false, shiftApplied: false })
  })

  it('applies ctrl to single characters via control codes', () => {
    expect(applyArmedModifiers('c', true, false))
      .toEqual({ seq: '\x03', ctrlApplied: true, altApplied: false, shiftApplied: false })
  })

  it('applies alt to single characters via ESC prefix', () => {
    expect(applyArmedModifiers('b', false, true))
      .toEqual({ seq: '\x1bb', ctrlApplied: false, altApplied: true, shiftApplied: false })
  })

  it('combines ctrl+alt on lowercase characters (ESC + control code)', () => {
    expect(applyArmedModifiers('c', true, true))
      .toEqual({ seq: '\x1b\x03', ctrlApplied: true, altApplied: true, shiftApplied: false })
  })

  it('folds alt into the CSI-u modifier for uppercase ctrl combos', () => {
    // ctrl only: shift+ctrl = 6 (matches ctrlSequenceFor)
    expect(applyArmedModifiers('A', true, false).seq).toBe('\x1b[97;6u')
    // ctrl+alt: shift+alt+ctrl = 8
    expect(applyArmedModifiers('A', true, true).seq).toBe('\x1b[97;8u')
  })

  it('leaves ctrl armed when the payload has no ctrl encoding', () => {
    expect(applyArmedModifiers('1', true, false))
      .toEqual({ seq: '1', ctrlApplied: false, altApplied: false, shiftApplied: false })
  })

  it('injects modifier params into CSI cursor-key sequences', () => {
    expect(applyArmedModifiers('\x1b[D', true, false).seq).toBe('\x1b[1;5D')  // ctrl+left = word-left
    expect(applyArmedModifiers('\x1b[C', false, true).seq).toBe('\x1b[1;3C')  // alt+right
    expect(applyArmedModifiers('\x1b[A', true, true).seq).toBe('\x1b[1;7A')   // ctrl+alt+up
  })

  it('encodes armed modifiers on BackTab without splitting the key', () => {
    expect(applyArmedModifiers('\x1b[Z', false, false).seq).toBe('\x1b[Z')
    expect(applyArmedModifiers('\x1b[Z', true, false).seq).toBe('\x1b[1;5Z')
    expect(applyArmedModifiers('\x1b[Z', false, true).seq).toBe('\x1b[1;3Z')
  })

  it('encodes alt+enter as ESC CR', () => {
    expect(applyArmedModifiers('\r', false, true))
      .toEqual({ seq: '\x1b\r', ctrlApplied: false, altApplied: true, shiftApplied: false })
  })

  it('encodes ctrl+enter / ctrl+tab / ctrl+esc as CSI-u (no legacy encoding exists)', () => {
    expect(applyArmedModifiers('\r', true, false).seq).toBe('\x1b[13;5u')
    expect(applyArmedModifiers('\t', true, false).seq).toBe('\x1b[9;5u')
    expect(applyArmedModifiers('\x1b', true, false).seq).toBe('\x1b[27;5u')
    expect(applyArmedModifiers('\r', true, true).seq).toBe('\x1b[13;7u')
  })

  it('ESC-prefixes alt for keys without special handling', () => {
    expect(applyArmedModifiers('\t', false, true).seq).toBe('\x1b\t')
    expect(applyArmedModifiers('\n', false, true).seq).toBe('\x1b\n')
  })

  it('composes armed shift with Tab and printable letters', () => {
    expect(applyArmedModifiers('\t', false, false, true))
      .toEqual({ seq: '\x1b[Z', ctrlApplied: false, altApplied: false, shiftApplied: true })
    expect(applyArmedModifiers('a', false, false, true).seq).toBe('A')
    expect(applyArmedModifiers('A', false, false, true).seq).toBe('A')
  })

  it('composes shift with supported terminal modifier encodings', () => {
    expect(applyArmedModifiers('\x1b[A', false, false, true).seq).toBe('\x1b[1;2A')
    expect(applyArmedModifiers('\x1b[D', true, true, true).seq).toBe('\x1b[1;8D')
    expect(applyArmedModifiers('c', true, false, true).seq).toBe('\x1b[99;6u')
    expect(applyArmedModifiers('c', true, true, true).seq).toBe('\x1b[99;8u')
  })

  it('consumes shift when ctrl fallback emits a non-letter control code', () => {
    for (const [input, output] of [['@', '\x00'], ['[', '\x1b'], ['\x7f', '\x08']] as const) {
      expect(applyArmedModifiers(input, true, false, true))
        .toEqual({ seq: output, ctrlApplied: true, altApplied: false, shiftApplied: true })
    }
  })

  it('does not guess layout-dependent shift mappings and consumes on composed text', () => {
    expect(applyArmedModifiers('1', false, false, true))
      .toEqual({ seq: '1', ctrlApplied: false, altApplied: false, shiftApplied: true })
    expect(applyArmedModifiers('dictated text', false, false, true))
      .toEqual({ seq: 'dictated text', ctrlApplied: false, altApplied: false, shiftApplied: true })
  })
})
