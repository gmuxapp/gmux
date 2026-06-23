import { describe, expect, it } from 'vitest'
import {
  SessionEventSchema,
  SessionSchema,
  successEnvelope,
  SessionStatusSchema,
} from './index.js'

describe('protocol schemas', () => {
  it('parses session (schema v2)', () => {
    const result = SessionSchema.parse({
      id: 'sess-1',
      kind: 'pi',
      alive: true,
      pid: 12345,
      title: 'test session',
      status: { working: true },
      terminal_cols: 120,
      terminal_rows: 40,
    })

    expect(result.id).toBe('sess-1')
    expect(result.alive).toBe(true)
    expect(result.status?.working).toBe(true)
    expect(result.terminal_cols).toBe(120)
    expect(result.terminal_rows).toBe(40)
  })

  it('parses session with null status', () => {
    const result = SessionSchema.parse({
      id: 'sess-2',
      kind: 'generic',
      alive: false,
      status: null,
    })

    expect(result.status).toBeNull()
    expect(result.alive).toBe(false)
  })

  it('validates session-upsert event', () => {
    const event = SessionEventSchema.parse({
      type: 'session-upsert',
      id: 'sess-1',
      session: {
        id: 'sess-1',
        kind: 'pi',
        alive: true,
        status: { working: true },
      },
    })

    expect(event.type).toBe('session-upsert')
    if (event.type === 'session-upsert') {
      expect(event.session.alive).toBe(true)
    }
  })

  it('validates session-remove event', () => {
    const event = SessionEventSchema.parse({
      type: 'session-remove',
      id: 'sess-1',
    })
    expect(event.type).toBe('session-remove')
  })

  it('builds typed success envelopes', () => {
    const Schema = successEnvelope(SessionStatusSchema)
    const parsed = Schema.parse({ ok: true, data: { working: false } })
    expect(parsed.data.working).toBe(false)
  })
})
