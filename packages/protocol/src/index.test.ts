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
      id: '1vshk4fu',
      kind: 'pi',
      alive: true,
      pid: 12345,
      title: 'test session',
      status: { active: true },
      terminal_cols: 120,
      terminal_rows: 40,
    })

    expect(result.id).toBe('1vshk4fu')
    expect(result.alive).toBe(true)
    expect(result.status?.active).toBe(true)
    expect(result.terminal_cols).toBe(120)
    expect(result.terminal_rows).toBe(40)
  })

  it('parses session with null status', () => {
    const result = SessionSchema.parse({
      id: '155mk8b7',
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
      id: '1vshk4fu',
      session: {
        id: '1vshk4fu',
        kind: 'pi',
        alive: true,
        status: { active: true },
      },
    })

    expect(event.type).toBe('session-upsert')
    if (event.type === 'session-upsert') {
      expect(event.session.alive).toBe(true)
    }
  })

  it('defaults the orthogonal status facts', () => {
    const status = SessionStatusSchema.parse({ active: true })
    expect(status).toEqual({ active: true, error: false, interrupted: false })
    const stopped = SessionStatusSchema.parse({ active: false, interrupted: true })
    expect(stopped?.interrupted).toBe(true)
  })

  it('validates session-remove event', () => {
    const event = SessionEventSchema.parse({
      type: 'session-remove',
      id: '1vshk4fu',
    })
    expect(event.type).toBe('session-remove')
  })

  it('builds typed success envelopes', () => {
    const Schema = successEnvelope(SessionStatusSchema)
    const parsed = Schema.parse({ ok: true, data: { active: false } })
    expect(parsed.data.active).toBe(false)
  })
})
