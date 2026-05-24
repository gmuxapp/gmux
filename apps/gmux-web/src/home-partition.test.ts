// Pins the home dashboard's section partition. The partition is the
// only piece of dashboard logic with non-trivial behavior (the row
// component is visual, the section layout is presentation); these
// tests guard the behavior contracts the design conversation pinned
// down: section priority, sort key, and the Recent floor/window/cap.

import { describe, it, expect } from 'vitest'
import {
  partitionForHome,
  partitionForProject,
  RECENT_WINDOW_MS,
  RECENT_FLOOR,
  RECENT_CAP,
} from './store'
import { makeSession } from './test-helpers'

const NOW = Date.parse('2026-06-01T12:00:00Z')

function stamp(offsetMs: number): string {
  return new Date(NOW + offsetMs).toISOString()
}

describe('partitionForHome', () => {
  describe('section assignment', () => {
    it('puts unread alive sessions in needsAttention', () => {
      const s = makeSession({ id: 's1', cwd: '/x', alive: true, unread: true })
      const { needsAttention, running, recent } = partitionForHome([s], NOW)
      expect(needsAttention.map(s => s.id)).toEqual(['s1'])
      expect(running).toEqual([])
      expect(recent).toEqual([])
    })

    it('does NOT escalate error-only sessions to needsAttention', () => {
      // status.error mostly fires from background subcommands that
      // exited non-zero without the agent itself halting, so it
      // doesn't earn the "needs attention" slot. The per-row error
      // dot still surfaces the state. This test pins the predicate
      // and prevents a regression to the old `unread || error`
      // shape that lit up the section on every failed subcommand.
      // We deliberately don't assert which fallthrough bucket the
      // session lands in; that's covered by other tests.
      const s = makeSession({
        id: 's1', cwd: '/x', alive: true,
        status: { label: 'failed', working: false, error: true },
      })
      const { needsAttention } = partitionForHome([s], NOW)
      expect(needsAttention).toEqual([])
    })

    it('puts alive+working sessions in running', () => {
      const s = makeSession({
        id: 's1', cwd: '/x', alive: true,
        status: { label: 'building', working: true, error: false },
      })
      const { needsAttention, running } = partitionForHome([s], NOW)
      expect(needsAttention).toEqual([])
      expect(running.map(s => s.id)).toEqual(['s1'])
    })

    it('excludes dead sessions from every home bucket', () => {
      // Home is alive-only: dead sessions live exclusively in the
      // project page's "All sessions" section. This pins the
      // contract so a regression that lets dead sessions leak into
      // Recent (the historical behavior) fails loudly.
      const sessions = [
        makeSession({
          id: 'dead', cwd: '/x', alive: false,
          last_activity_at: stamp(-5 * 60 * 1000),
        }),
        makeSession({
          id: 'dead-unread', cwd: '/x', alive: false, unread: true,
          last_activity_at: stamp(-5 * 60 * 1000),
        }),
      ]
      const { needsAttention, running, recent } = partitionForHome(sessions, NOW)
      expect(needsAttention).toEqual([])
      expect(running).toEqual([])
      expect(recent).toEqual([])
    })
  })

  describe('sort order', () => {
    const working = { label: 'x', working: true, error: false }

    it('sorts each section newest-first by last_activity_at', () => {
      const a = makeSession({ id: 'a', cwd: '/x', alive: true, status: working, last_activity_at: stamp(-10_000) })
      const b = makeSession({ id: 'b', cwd: '/x', alive: true, status: working, last_activity_at: stamp(-1_000) })
      const c = makeSession({ id: 'c', cwd: '/x', alive: true, status: working, last_activity_at: stamp(-5_000) })
      const { running } = partitionForHome([a, b, c], NOW)
      expect(running.map(s => s.id)).toEqual(['b', 'c', 'a'])
    })

    it('falls back to created_at when last_activity_at is missing', () => {
      // A brand-new working session has no last_activity_at (per the
      // daemon's rule: creation doesn't bump). The sort still needs
      // to place it somewhere sensible. created_at fallback puts new
      // sessions at the top, where they belong.
      const old = makeSession({
        id: 'old', cwd: '/x', alive: true, status: working,
        created_at: stamp(-3600_000), last_activity_at: stamp(-3600_000),
      })
      const fresh = makeSession({
        id: 'fresh', cwd: '/x', alive: true, status: working,
        created_at: stamp(-1_000), // no last_activity_at
      })
      const { running } = partitionForHome([old, fresh], NOW)
      expect(running.map(s => s.id)).toEqual(['fresh', 'old'])
    })

    it('breaks timestamp ties by id (deterministic, no flicker)', () => {
      // Sessions with identical timestamps (notably the corpses
      // persisted before last_activity_at existed, all falling back
      // to a similar created_at) must sort the same way every time.
      // Without a stable tiebreaker, the input order from
      // sessions.value rebuilding on each SSE event would re-order
      // the visible list. We use id as the tiebreaker; input order
      // must not affect output.
      const same = stamp(-1000)
      const make = (id: string) => makeSession({
        id, cwd: '/x', alive: true, status: working, last_activity_at: same,
      })
      const a = make('a')
      const b = make('b')
      const c = make('c')
      const orderings = [[a, b, c], [c, b, a], [b, a, c], [c, a, b]]
      for (const arr of orderings) {
        const { running } = partitionForHome(arr, NOW)
        expect(running.map(s => s.id)).toEqual(['a', 'b', 'c'])
      }
    })
  })

  describe('recent window and floor', () => {
    // Recent is for idle-alive sessions (live but not currently
    // working). Dead sessions never reach this bucket since home
    // filters them out at the input.
    const idle = (id: string, ageMs: number) => makeSession({
      id, cwd: '/x', alive: true, last_activity_at: stamp(-ageMs),
    })

    it('includes only entries inside the window when there are enough', () => {
      const sessions = [
        idle('a', 10 * 60 * 1000),
        idle('b', 20 * 60 * 1000),
        idle('c', 30 * 60 * 1000),
        idle('d', RECENT_WINDOW_MS + 60_000),
      ]
      const { recent } = partitionForHome(sessions, NOW)
      expect(recent.map(s => s.id)).toEqual(['a', 'b', 'c'])
    })

    it('falls back to top-floor by recency when fewer than floor are in-window', () => {
      // 1 inside window, 4 older. Floor takes top RECENT_FLOOR=3 by
      // recency overall so the user always sees a non-trivial
      // section after they've used the daemon at all.
      const sessions = [
        idle('inside', 5 * 60 * 1000),
        idle('old1', 2 * 3600_000),
        idle('old2', 3 * 3600_000),
        idle('old3', 4 * 3600_000),
        idle('old4', 5 * 3600_000),
      ]
      const { recent } = partitionForHome(sessions, NOW)
      expect(recent.map(s => s.id)).toEqual(['inside', 'old1', 'old2'])
      expect(recent).toHaveLength(RECENT_FLOOR)
    })

    it('caps the window-qualified set at RECENT_CAP', () => {
      // Pathological case: many recent transitions in a busy hour.
      // The cap stops the dashboard from scrolling forever.
      const sessions = Array.from({ length: RECENT_CAP + 5 }, (_, i) =>
        idle(`s${i}`, (i + 1) * 1000),
      )
      const { recent } = partitionForHome(sessions, NOW)
      expect(recent).toHaveLength(RECENT_CAP)
      expect(recent[0].id).toBe('s0')
      expect(recent[RECENT_CAP - 1].id).toBe(`s${RECENT_CAP - 1}`)
    })

    it('returns an empty Recent when every session is working or unread', () => {
      // No idle-alive leftovers → Recent stays empty even with the
      // floor (the floor pulls from leftover, not from
      // attention/running).
      const working = { label: 'x', working: true, error: false }
      const sessions = [
        makeSession({ id: 'a', cwd: '/x', alive: true, status: working }),
        makeSession({ id: 'b', cwd: '/x', alive: true, status: working }),
      ]
      const { recent } = partitionForHome(sessions, NOW)
      expect(recent).toEqual([])
    })
  })
})

describe('partitionForProject', () => {
  // The project page diverges from home in one specific way: the
  // third bucket holds every remaining session, with no recency
  // window or cap. These tests pin that divergence; section
  // assignment for Needs attention / Running is identical to home
  // and covered by the partitionForHome suite above.

  it('keeps idle-alive sessions from arbitrarily long ago in `rest`', () => {
    // A session that was last active a week ago would be filtered
    // out of home's Recent (1h window, 10-cap) but must remain on
    // the project page.
    const weekAgo = new Date(NOW - 7 * 24 * 60 * 60 * 1000).toISOString()
    const s = makeSession({
      id: 'ancient',
      cwd: '/x',
      alive: true,
      status: { label: '', working: false, error: false },
      last_activity_at: weekAgo,
    })
    const { rest } = partitionForProject([s])
    expect(rest.map(x => x.id)).toEqual(['ancient'])
  })

  it('keeps exited sessions in `rest` regardless of age', () => {
    const longAgo = new Date(NOW - 30 * 24 * 60 * 60 * 1000).toISOString()
    const s = makeSession({
      id: 'old-exit',
      cwd: '/x',
      alive: false,
      last_activity_at: longAgo,
    })
    const { rest } = partitionForProject([s])
    expect(rest.map(x => x.id)).toEqual(['old-exit'])
  })

  it('does not cap `rest` at RECENT_CAP', () => {
    // Generate more entries than the home cap so a regression that
    // accidentally reused partitionForHome's slicing would show.
    const sessions = Array.from({ length: RECENT_CAP + 5 }, (_, i) =>
      makeSession({ id: `s${i}`, cwd: '/x', alive: false }),
    )
    const { rest } = partitionForProject(sessions)
    expect(rest.length).toBe(RECENT_CAP + 5)
  })

  it('routes unread-alive to needsAttention and working-alive to running', () => {
    // Smoke test that the two non-rest buckets still split as expected.
    const att = makeSession({ id: 'att', cwd: '/x', alive: true, unread: true })
    const run = makeSession({
      id: 'run', cwd: '/x', alive: true,
      status: { label: 'building', working: true, error: false },
    })
    const idle = makeSession({ id: 'idle', cwd: '/x', alive: true })
    const { needsAttention, running, rest } = partitionForProject([att, run, idle])
    expect(needsAttention.map(s => s.id)).toEqual(['att'])
    expect(running.map(s => s.id)).toEqual(['run'])
    expect(rest.map(s => s.id)).toEqual(['idle'])
  })

  it('routes dead-unread sessions to `rest`, not needsAttention', () => {
    // Mirrors partitionForHome's alive-only Waiting predicate:
    // a dead session with unread output is no longer "waiting on
    // you"; it belongs in the All-sessions tail where dead
    // sessions live.
    const s = makeSession({
      id: 'dead-unread', cwd: '/x', alive: false, unread: true,
    })
    const { needsAttention, rest } = partitionForProject([s])
    expect(needsAttention).toEqual([])
    expect(rest.map(x => x.id)).toEqual(['dead-unread'])
  })
})
