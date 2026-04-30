import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  MAX_CLIPBOARD_BYTES,
  firstBinaryType,
  uploadClipboardBlob,
  type UploadResult,
} from './clipboard-upload'

describe('firstBinaryType', () => {
  it('returns the first non-text MIME', () => {
    expect(firstBinaryType(['text/plain', 'image/png', 'text/html'])).toBe('image/png')
  })

  it('returns null when only text MIMEs are present', () => {
    expect(firstBinaryType(['text/plain', 'text/html', 'text/uri-list'])).toBe(null)
  })

  it('returns null on empty input', () => {
    expect(firstBinaryType([])).toBe(null)
  })

  it('treats image/svg+xml as binary even though it is structurally textual', () => {
    // An SVG copied from a graphics app is the user's intended payload;
    // upload preserves intent. Same reasoning as image/png: prefer binary
    // when binary is present.
    expect(firstBinaryType(['text/plain', 'image/svg+xml'])).toBe('image/svg+xml')
  })

  it('preserves order: first non-text wins', () => {
    expect(firstBinaryType(['image/png', 'image/jpeg'])).toBe('image/png')
    expect(firstBinaryType(['image/jpeg', 'image/png'])).toBe('image/jpeg')
  })
})

describe('uploadClipboardBlob', () => {
  const originalFetch = globalThis.fetch
  afterEach(() => {
    globalThis.fetch = originalFetch
  })

  function makeBlob(size: number, type = 'image/png'): Blob {
    return new Blob([new Uint8Array(size)], { type })
  }

  it('rejects oversized blobs without calling fetch', async () => {
    const fetchSpy = vi.fn()
    globalThis.fetch = fetchSpy as unknown as typeof fetch

    const blob = makeBlob(MAX_CLIPBOARD_BYTES + 1)
    const result = await uploadClipboardBlob(blob, 'sess-1')

    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error).toBe('too_large')
    expect(fetchSpy).not.toHaveBeenCalled()
  })

  it('POSTs to the session-scoped endpoint with the blob MIME and bytes', async () => {
    let captured: { url: string; init: RequestInit } | null = null
    globalThis.fetch = (async (url: string, init: RequestInit) => {
      captured = { url, init }
      return new Response(
        JSON.stringify({ ok: true, data: { path: '/tmp/paste-1.png' } }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      )
    }) as unknown as typeof fetch

    const blob = makeBlob(64, 'image/png')
    const result = await uploadClipboardBlob(blob, 'sess-1')

    expect(result).toEqual<UploadResult>({ ok: true, path: '/tmp/paste-1.png' })
    expect(captured!.url).toBe('/v1/sessions/sess-1/clipboard')
    expect(captured!.init.method).toBe('POST')
    const headers = new Headers(captured!.init.headers)
    expect(headers.get('Content-Type')).toBe('image/png')
    expect(captured!.init.body).toBe(blob)
  })

  it('maps server error JSON to a recognizable error string', async () => {
    globalThis.fetch = (async () =>
      new Response(
        JSON.stringify({ ok: false, error: { code: 'too_large', message: 'too big' } }),
        { status: 413, headers: { 'Content-Type': 'application/json' } },
      )) as unknown as typeof fetch

    const result = await uploadClipboardBlob(makeBlob(64), 'sess-1')
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error).toBe('too_large')
  })

  it('falls back to a generic error when the response has no error code', async () => {
    globalThis.fetch = (async () =>
      new Response('upstream exploded', { status: 500 })) as unknown as typeof fetch

    const result = await uploadClipboardBlob(makeBlob(64), 'sess-1')
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error).toBe('server_error')
  })

  it('reports network failure distinctly from server errors', async () => {
    globalThis.fetch = (async () => {
      throw new TypeError('Failed to fetch')
    }) as unknown as typeof fetch

    const result = await uploadClipboardBlob(makeBlob(64), 'sess-1')
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error).toBe('network')
  })

  it('URL-encodes the session id', async () => {
    let capturedURL = ''
    globalThis.fetch = (async (url: string) => {
      capturedURL = url
      return new Response(JSON.stringify({ ok: true, data: { path: '/tmp/paste-1.png' } }))
    }) as unknown as typeof fetch

    await uploadClipboardBlob(makeBlob(64), 'has spaces/and/slashes')
    expect(capturedURL).toBe('/v1/sessions/has%20spaces%2Fand%2Fslashes/clipboard')
  })
})
