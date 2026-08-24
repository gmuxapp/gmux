import { beforeEach, describe, expect, it } from 'vitest'
import { familyDrawerDotState } from './family-drawer'
import { activityMap } from './store'
import type { Session } from './types'

function session(status: Session['status'], unread = true): Session {
  return {
    id: 'selected', created_at: '2026-01-01T00:00:00Z', command: [], cwd: '',
    adapter: 'pi', alive: true, pid: null, exit_code: null,
    started_at: '2026-01-01T00:00:00Z', exited_at: null, title: 'selected',
    subtitle: '', status, unread, unread_token: 'token', resumable: false,
    socket_path: '',
  }
}

describe('family drawer selected dot', () => {
  beforeEach(() => { activityMap.value = new Map() })

  it.each([
    ['waiting', session(null), 'none'],
    ['waiting-error', session({ active: false, error: true }), 'none'],
    ['active-error', session({ active: true, error: true }), 'active-error'],
  ] as const)('%s', (_name, selected, expected) => {
    expect(familyDrawerDotState(selected, selected.id)).toBe(expected)
  })
})
