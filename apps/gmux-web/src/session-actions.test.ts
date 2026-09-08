import { describe, it, expect } from 'vitest'
import { lifecycleAction, relaunchDirectoryNotice, relaunchVerb } from './session-actions'

const sess = (over: Partial<{
  alive: boolean; resumable: boolean; adapter: string; relaunch: string
}>) => ({
  alive: false,
  resumable: false,
  adapter: 'shell',
  ...over,
})

describe('lifecycleAction', () => {
  it('alive session offers Restart', () => {
    expect(lifecycleAction(sess({ alive: true }))).toEqual({
      id: 'restart', label: 'Restart session', shortLabel: 'Restart', disabled: false,
    })
  })

  it('alive wins over resumable (state, not history, decides)', () => {
    expect(lifecycleAction(sess({ alive: true, resumable: true }))!.id).toBe('restart')
  })

  it("dead session offers the daemon's verb, not an adapter guess", () => {
    // The daemon owns the policy: a shell that the daemon says it will
    // resume says Resume, and an agent it will only rerun says Rerun.
    expect(lifecycleAction(sess({ resumable: true, adapter: 'shell', relaunch: 'resume' })))
      .toEqual({ id: 'relaunch', verb: 'resume', label: 'Resume session', shortLabel: 'Resume', disabled: false })
    expect(lifecycleAction(sess({ resumable: true, adapter: 'claude', relaunch: 'rerun' })))
      .toEqual({ id: 'relaunch', verb: 'rerun', label: 'Rerun session', shortLabel: 'Rerun', disabled: false })
  })

  it('dead shell with a rerun verdict offers Rerun (the reported bug)', () => {
    expect(lifecycleAction(sess({ resumable: true, adapter: 'shell', relaunch: 'rerun' })))
      .toEqual({ id: 'relaunch', verb: 'rerun', label: 'Rerun session', shortLabel: 'Rerun', disabled: false })
  })

  it('dead session with no relaunch verdict offers nothing, even if resumable is stale', () => {
    // resumable and relaunch are emitted together by any daemon that knows
    // the field, so relaunch-absent + resumable-true only happens on the
    // legacy peer path below.
    expect(lifecycleAction(sess({}))).toBeNull()
    expect(lifecycleAction(sess({ resumable: false, relaunch: undefined }))).toBeNull()
  })

  it('falls back to the adapter guess for pre-relaunch daemons', () => {
    for (const adapter of ['claude', 'codex', 'pi']) {
      expect(lifecycleAction(sess({ resumable: true, adapter }))!.shortLabel).toBe('Resume')
    }
    expect(lifecycleAction(sess({ resumable: true, adapter: 'shell' }))!.shortLabel).toBe('Rerun')
    expect(lifecycleAction(sess({ resumable: true, adapter: 'future-agent' }))!.shortLabel).toBe('Rerun')
  })

  it('pending disables the action and shows busy label per verb', () => {
    expect(lifecycleAction(sess({ resumable: true, relaunch: 'resume' }), true)).toEqual({
      id: 'relaunch', verb: 'resume', label: 'Resuming…', shortLabel: 'Resuming…', disabled: true,
    })
    expect(lifecycleAction(sess({ resumable: true, relaunch: 'rerun' }), true)).toEqual({
      id: 'relaunch', verb: 'rerun', label: 'Rerunning…', shortLabel: 'Rerunning…', disabled: true,
    })
  })

  it('pending does not affect an alive session', () => {
    expect(lifecycleAction(sess({ alive: true }), true)!.disabled).toBe(false)
  })
})

describe('relaunchVerb', () => {
  it('prefers the wire verdict over both resumable and the adapter name', () => {
    expect(relaunchVerb({ relaunch: 'rerun', resumable: false, adapter: 'claude' })).toBe('rerun')
    expect(relaunchVerb({ relaunch: 'resume', resumable: false, adapter: 'shell' })).toBe('resume')
  })

  it('is null when the daemon offers no relaunch', () => {
    expect(relaunchVerb({ resumable: false, adapter: 'shell' })).toBeNull()
  })
})

describe('relaunchDirectoryNotice', () => {
  it('is silent when the daemon relaunched where the session lived', () => {
    expect(relaunchDirectoryNotice('rerun', {})).toBeNull()
    expect(relaunchDirectoryNotice('rerun', { pid: 42 })).toBeNull()
    expect(relaunchDirectoryNotice('resume', undefined)).toBeNull()
  })

  it('names the substituted directory, because a rerun runs a recorded command there', () => {
    expect(relaunchDirectoryNotice('rerun', {
      original_cwd: '/gone/project', fallback_cwd: '/home/user',
    })).toBe('Rerunning in /home/user instead (/gone/project no longer exists)')
    expect(relaunchDirectoryNotice('resume', { fallback_cwd: '/home/user' }))
      .toBe('Resumed in /home/user instead')
  })
})

describe('unknown relaunch verbs (forward compatibility)', () => {
  it('a verb this client does not know degrades to the resumable fallback', () => {
    // A newer daemon may grow a third verb; the row must still render.
    expect(relaunchVerb({ relaunch: 'teleport', resumable: true, adapter: 'shell' })).toBe('rerun')
    expect(relaunchVerb({ relaunch: 'teleport', resumable: false, adapter: 'shell' })).toBeNull()
  })
})
