import { describe, it, expect } from 'vitest'
import { hostStatus } from './host-status'

describe('hostStatus', () => {
  it('connected → Online', () => {
    expect(hostStatus('connected')).toEqual({ kind: 'online', label: 'Online' })
  })

  it('connecting → Connecting…', () => {
    expect(hostStatus('connecting')).toEqual({ kind: 'connecting', label: 'Connecting…' })
  })

  it('disconnected + auth error → Auth needed (carries detail)', () => {
    expect(hostStatus('disconnected', 'authentication failed')).toEqual({
      kind: 'auth', label: 'Auth needed', detail: 'authentication failed',
    })
  })

  it('disconnected + connection error → Offline (carries detail)', () => {
    expect(hostStatus('disconnected', 'connection refused')).toEqual({
      kind: 'offline', label: 'Offline', detail: 'connection refused',
    })
  })

  it('disconnected with no error → Offline', () => {
    expect(hostStatus('disconnected')).toEqual({ kind: 'offline', label: 'Offline' })
  })

  it('a TLS/cert failure is offline, not mislabeled as not-gmux', () => {
    expect(hostStatus('disconnected', 'TLS certificate error').kind).toBe('offline')
  })
})
