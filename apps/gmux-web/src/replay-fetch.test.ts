import { describe, expect, test, vi } from 'vitest'
import { fetchScrollback } from './replay-fetch'

function ok(body: BodyInit, init?: ResponseInit): Response {
  return new Response(body, { status: 200, ...init })
}

describe('fetchScrollback', () => {
  test('200 with bytes returns kind=bytes with the response body', async () => {
    const payload = new Uint8Array([0x68, 0x69, 0x0d, 0x0a]) // "hi\r\n"
    const fakeFetch = vi.fn().mockResolvedValue(ok(payload))

    const result = await fetchScrollback('1j6y9mx6', fakeFetch)

    expect(fakeFetch).toHaveBeenCalledWith('/v1/sessions/1j6y9mx6/scrollback')
    expect(result).toEqual({ kind: 'bytes', bytes: payload })
  })

  test('200 with empty body returns kind=empty', async () => {
    const fakeFetch = vi.fn().mockResolvedValue(ok(new Uint8Array(0)))

    const result = await fetchScrollback('1lm71dfs', fakeFetch)

    expect(result).toEqual({ kind: 'empty' })
  })

  test('404 returns kind=not-found', async () => {
    const fakeFetch = vi.fn().mockResolvedValue(new Response('not found', { status: 404 }))

    const result = await fetchScrollback('13stq9rd', fakeFetch)

    expect(result).toEqual({ kind: 'not-found' })
  })

  test('5xx returns kind=error with status and message', async () => {
    const fakeFetch = vi.fn().mockResolvedValue(new Response('boom', { status: 500, statusText: 'Internal Server Error' }))

    const result = await fetchScrollback('1u44a1lf', fakeFetch)

    expect(result).toEqual({ kind: 'error', status: 500, message: 'Internal Server Error' })
  })

  test('network failure returns kind=error with status 0', async () => {
    const fakeFetch = vi.fn().mockRejectedValue(new Error('connection refused'))

    const result = await fetchScrollback('1108gm0e', fakeFetch)

    expect(result).toEqual({ kind: 'error', status: 0, message: 'connection refused' })
  })

  test('arrayBuffer rejection mid-body returns kind=error instead of throwing', async () => {
    // Simulate the wire dropping after headers arrive: the
    // response is a 200 but reading the body rejects.
    const body = new ReadableStream({
      start(controller) {
        controller.error(new Error('aborted'))
      },
    })
    const fakeFetch = vi.fn().mockResolvedValue(new Response(body, { status: 200 }))

    const result = await fetchScrollback('1j6y9mx6', fakeFetch)

    expect(result.kind).toBe('error')
    if (result.kind === 'error') {
      expect(result.status).toBe(200)
      expect(result.message).toContain('aborted')
    }
  })

  test('peer-owned session id is URL-encoded', async () => {
    const fakeFetch = vi.fn().mockResolvedValue(ok(new Uint8Array(0)))

    await fetchScrollback('1j6y9mx6@hs', fakeFetch)

    // @ would otherwise be unsafe in some HTTP routers; verify the call site
    // round-trips through encodeURIComponent.
    expect(fakeFetch).toHaveBeenCalledWith('/v1/sessions/1j6y9mx6%40hs/scrollback')
  })
})
