import { describe, expect, it } from 'vitest'
import {
  clickIsEditable,
  encodeSgrClick,
  parseOsc133,
  type PromptRegion,
} from './protocol.js'

describe('parseOsc133', () => {
  it('parses a bare mark kind', () => {
    const r = parseOsc133('A')
    expect(r.kind).toBe('A')
    expect(r.params.size).toBe(0)
  })

  it('parses key=value parameters', () => {
    const r = parseOsc133('A;click_events=1;k=v')
    expect(r.kind).toBe('A')
    expect(r.params.get('click_events')).toBe('1')
    expect(r.params.get('k')).toBe('v')
  })

  it('maps bare tokens to empty string (e.g. exit code on D)', () => {
    const r = parseOsc133('D;0')
    expect(r.kind).toBe('D')
    expect(r.params.get('0')).toBe('')
  })

  it('handles empty payload', () => {
    const r = parseOsc133('')
    expect(r.kind).toBe('')
    expect(r.params.size).toBe(0)
  })
})

describe('encodeSgrClick', () => {
  it('emits a 1-based SGR left-button press+release', () => {
    expect(encodeSgrClick({ col: 0, row: 0, line: 0 })).toBe('\x1b[<0;1;1M\x1b[<0;1;1m')
    expect(encodeSgrClick({ col: 4, row: 2, line: 9 })).toBe('\x1b[<0;5;3M\x1b[<0;5;3m')
  })
})

describe('clickIsEditable', () => {
  const base: PromptRegion = {
    armed: true,
    commandRunning: false,
    startLine: 10,
    endLine: 12,
  }

  it('accepts clicks inside the editable range', () => {
    expect(clickIsEditable(base, 10)).toBe(true)
    expect(clickIsEditable(base, 11)).toBe(true)
    expect(clickIsEditable(base, 12)).toBe(true)
  })

  it('rejects clicks outside the range', () => {
    expect(clickIsEditable(base, 9)).toBe(false)
    expect(clickIsEditable(base, 13)).toBe(false)
  })

  it('rejects when not armed', () => {
    expect(clickIsEditable({ ...base, armed: false }, 11)).toBe(false)
  })

  it('rejects while a command is running', () => {
    expect(clickIsEditable({ ...base, commandRunning: true }, 11)).toBe(false)
  })

  it('rejects when the region is unknown', () => {
    expect(clickIsEditable({ ...base, startLine: undefined }, 11)).toBe(false)
    expect(clickIsEditable({ ...base, endLine: undefined }, 11)).toBe(false)
  })

  it('tolerates inverted start/end lines', () => {
    expect(clickIsEditable({ ...base, startLine: 12, endLine: 10 }, 11)).toBe(true)
  })
})
