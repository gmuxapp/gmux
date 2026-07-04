import { describe, it, expect } from 'vitest'
import { lifecycleAction, showMobileBar } from './session-actions'

const sess = (over: Partial<{ alive: boolean; resumable: boolean; adapter: string }>) => ({
  alive: false,
  resumable: false,
  adapter: 'shell',
  ...over,
})

describe('lifecycleAction', () => {
  it('alive session offers Restart', () => {
    expect(lifecycleAction(sess({ alive: true }))).toEqual({
      id: 'restart', label: 'Restart session', disabled: false,
    })
  })

  it('alive wins over resumable (state, not history, decides)', () => {
    expect(lifecycleAction(sess({ alive: true, resumable: true }))!.id).toBe('restart')
  })

  it('dead resumable agent offers Resume', () => {
    for (const adapter of ['claude', 'codex', 'pi']) {
      expect(lifecycleAction(sess({ resumable: true, adapter }))).toEqual({
        id: 'resume', label: 'Resume session', disabled: false,
      })
    }
  })

  it('dead resumable non-agent offers Rerun', () => {
    expect(lifecycleAction(sess({ resumable: true, adapter: 'shell' }))).toEqual({
      id: 'resume', label: 'Rerun session', disabled: false,
    })
    // Unknown adapter kinds default to the safe Rerun label.
    expect(lifecycleAction(sess({ resumable: true, adapter: 'future-agent' }))!.label)
      .toBe('Rerun session')
  })

  it('dead non-resumable session offers nothing', () => {
    expect(lifecycleAction(sess({}))).toBeNull()
  })

  it('resuming disables the action and shows busy label per kind', () => {
    expect(lifecycleAction(sess({ resumable: true, adapter: 'claude' }), true)).toEqual({
      id: 'resume', label: 'Resuming…', disabled: true,
    })
    expect(lifecycleAction(sess({ resumable: true, adapter: 'shell' }), true)).toEqual({
      id: 'resume', label: 'Rerunning…', disabled: true,
    })
  })

  it('resuming does not affect an alive session', () => {
    expect(lifecycleAction(sess({ alive: true }), true)!.disabled).toBe(false)
  })
})

describe('showMobileBar', () => {
  it('shows for an alive session', () => {
    expect(showMobileBar({ id: 'a' })).toBe(true)
  })

  it('shows for a dead session (☰ must stay reachable on touch)', () => {
    expect(showMobileBar({ id: 'dead' })).toBe(true)
  })

  it('hides when nothing is selected', () => {
    expect(showMobileBar(null)).toBe(false)
  })
})
