import { describe, expect, test, vi } from 'vitest'
import { fetchScrollback } from './replay-fetch'

function ok(body: BodyInit, init?: ResponseInit): Response {
  return new Response(body, { status: 200, ...init })
}

describe('fetchScrollback', () => {
  test('200 with bytes returns kind=bytes with the response body', async () => {
    const payload = new Uint8Array([0x68, 0x69, 0x0d, 0x0a]) // "hi\r\n"
    const fakeFetch = vi.fn().mockResolvedValue(ok(payload))

    const result = await fetchScrollback('sess-abc', fakeFetch)

    expect(fakeFetch).toHaveBeenCalledWith('/v1/sessions/sess-abc/scrollback')
    expect(result).toEqual({ kind: 'bytes', bytes: payload })
  })

  test('200 with empty body returns kind=empty', async () => {
    const fakeFetch = vi.fn().mockResolvedValue(ok(new Uint8Array(0)))

    const result = await fetchScrollback('sess-empty', fakeFetch)

    expect(result).toEqual({ kind: 'empty' })
  })

  test('404 returns kind=not-found', async () => {
    const fakeFetch = vi.fn().mockResolvedValue(new Response('not found', { status: 404 }))

    const result = await fetchScrollback('sess-missing', fakeFetch)

    expect(result).toEqual({ kind: 'not-found' })
  })

  test('5xx returns kind=error with status and message', async () => {
    const fakeFetch = vi.fn().mockResolvedValue(new Response('boom', { status: 500, statusText: 'Internal Server Error' }))

    const result = await fetchScrollback('sess-bad', fakeFetch)

    expect(result).toEqual({ kind: 'error', status: 500, message: 'Internal Server Error' })
  })

  test('network failure returns kind=error with status 0', async () => {
    const fakeFetch = vi.fn().mockRejectedValue(new Error('connection refused'))

    const result = await fetchScrollback('sess-x', fakeFetch)

    expect(result).toEqual({ kind: 'error', status: 0, message: 'connection refused' })
  })

  test('peer-owned session id is URL-encoded', async () => {
    const fakeFetch = vi.fn().mockResolvedValue(ok(new Uint8Array(0)))

    await fetchScrollback('sess-abc@hs', fakeFetch)

    // @ would otherwise be unsafe in some HTTP routers; verify the call site
    // round-trips through encodeURIComponent.
    expect(fakeFetch).toHaveBeenCalledWith('/v1/sessions/sess-abc%40hs/scrollback')
  })
})
