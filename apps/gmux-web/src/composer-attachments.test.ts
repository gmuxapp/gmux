import { afterEach, describe, expect, it, vi } from 'vitest'
import type { CompleteAttachment, PendingAttachment } from '@assistant-ui/react'
import {
  attachmentErrorMessage,
  attachmentPaths,
  composeMessageWithAttachments,
  makeAttachmentAdapter,
} from './composer-attachments'

describe('composeMessageWithAttachments', () => {
  it('returns text unchanged when there are no attachments', () => {
    expect(composeMessageWithAttachments('hello', [])).toBe('hello')
    expect(composeMessageWithAttachments('', [])).toBe('')
  })

  it('appends a single bare path after the text with a trailing space', () => {
    expect(composeMessageWithAttachments('look at this', ['/tmp/paste-1.png'])).toBe(
      'look at this /tmp/paste-1.png ',
    )
  })

  it('sends only the path(s) when text is empty', () => {
    expect(composeMessageWithAttachments('', ['/tmp/paste-1.png'])).toBe('/tmp/paste-1.png ')
  })

  it('joins multiple paths with spaces', () => {
    expect(composeMessageWithAttachments('files', ['/tmp/a.png', '/tmp/b.pdf'])).toBe(
      'files /tmp/a.png /tmp/b.pdf ',
    )
  })

  it('does not add bracketed-paste escapes', () => {
    const out = composeMessageWithAttachments('x', ['/tmp/a.png'])
    expect(out).not.toContain('\x1b[200~')
    expect(out).not.toContain('\x1b[201~')
  })

  it('trims trailing whitespace from user text before splicing', () => {
    expect(composeMessageWithAttachments('hi   ', ['/tmp/a.png'])).toBe('hi /tmp/a.png ')
  })
})

describe('attachmentPaths', () => {
  const complete = (text: string): CompleteAttachment => ({
    id: text,
    type: 'file',
    name: 'f',
    content: [{ type: 'text', text }],
    status: { type: 'complete' },
  })

  it('returns [] for undefined', () => {
    expect(attachmentPaths(undefined)).toEqual([])
  })

  it('extracts path text parts in order', () => {
    expect(attachmentPaths([complete('/tmp/a.png'), complete('/tmp/b.pdf')])).toEqual([
      '/tmp/a.png',
      '/tmp/b.pdf',
    ])
  })

  it('skips non-text and empty parts', () => {
    const att: CompleteAttachment = {
      id: 'x',
      type: 'image',
      name: 'x',
      content: [
        { type: 'image', image: 'data:...' },
        { type: 'text', text: '' },
        { type: 'text', text: '/tmp/c.png' },
      ],
      status: { type: 'complete' },
    }
    expect(attachmentPaths([att])).toEqual(['/tmp/c.png'])
  })
})

describe('attachmentErrorMessage', () => {
  it('maps known codes', () => {
    expect(attachmentErrorMessage('too_large')).toMatch(/10MB/)
    expect(attachmentErrorMessage('network')).toMatch(/unreachable/)
    expect(attachmentErrorMessage('not_found')).toMatch(/session not found/)
  })

  it('falls back to the raw code', () => {
    expect(attachmentErrorMessage('weird')).toBe('Attach failed: weird')
  })
})

describe('makeAttachmentAdapter', () => {
  afterEach(() => vi.restoreAllMocks())

  const file = () => new File(['data'], 'shot.png', { type: 'image/png' })

  it('uploads on add and completes with the returned path', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ ok: true, data: { path: '/tmp/paste-7.png' } }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    const adapter = makeAttachmentAdapter('sess-1')
    const pending = await (adapter.add({ file: file() }) as Promise<PendingAttachment>)
    expect(pending.status).toEqual({ type: 'requires-action', reason: 'composer-send' })
    expect(pending.name).toBe('shot.png')

    const complete = await adapter.send(pending)
    expect(complete.status).toEqual({ type: 'complete' })
    expect(attachmentPaths([complete])).toEqual(['/tmp/paste-7.png'])
  })

  it('throws human copy on upload failure', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ ok: false, error: { code: 'too_large' } }), {
        status: 413,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    const adapter = makeAttachmentAdapter('sess-1')
    await expect(adapter.add({ file: file() })).rejects.toThrow(/10MB/)
  })

  it('posts to the session clipboard endpoint', async () => {
    const spy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ ok: true, data: { path: '/tmp/p.png' } }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    const adapter = makeAttachmentAdapter('sess-XY')
    await adapter.add({ file: file() })
    expect(spy).toHaveBeenCalledWith(
      '/v1/sessions/sess-XY/clipboard',
      expect.objectContaining({ method: 'POST' }),
    )
  })
})
