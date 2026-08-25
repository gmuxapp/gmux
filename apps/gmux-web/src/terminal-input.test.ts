import { describe, expect, test } from 'vitest'
import { canSendTerminalInput, type TerminalInputConnection } from './terminal-input'

function connection(sessionId = 'session', readyState = 1): TerminalInputConnection {
  return { sessionId, ws: { readyState } }
}

describe('canSendTerminalInput', () => {
  test('drops input while the initial claim is fenced', () => {
    const current = connection()
    expect(canSendTerminalInput(false, current, current, 'session')).toBe(false)
  })

  test('allows input for the claimed current open connection', () => {
    const current = connection()
    expect(canSendTerminalInput(true, current, current, 'session')).toBe(true)
  })

  test('drops a capability holding a stale connection identity', () => {
    const stale = connection()
    const current = connection()
    expect(canSendTerminalInput(true, current, stale, 'session')).toBe(false)
  })

  test('drops input for a closed socket', () => {
    const current = connection('session', 3)
    expect(canSendTerminalInput(true, current, current, 'session')).toBe(false)
  })
})
