// Pins the home dashboard's section partition. The partition is the
// only piece of dashboard logic with non-trivial behavior (the row
// component is visual, the section layout is presentation); these
// tests guard the behavior contracts the design conversation pinned
// down: section priority, sort key, and the recency-bucket boundaries.

import { describe, it, expect } from 'vitest'
import {
  partitionForHome,
  partitionForProject,
} from './store'
import { makeSession } from './test-helpers'

// Constructed from LOCAL date parts (not a UTC string) so the
// calendar-day bucket boundaries — which partitionForHome computes in
// local time — line up deterministically regardless of the test
// runner's timezone. Noon leaves a comfortable 12h gap to local
// midnight, keeping the "earlier today" / "yesterday" splits crisp.
const NOW = new Date(2026, 5, 1, 12, 0, 0).getTime()
const HOUR = 60 * 60 * 1000
const DAY = 24 * HOUR

function stamp(offsetMs: number): string {
  return new Date(NOW + offsetMs).toISOString()
}

describe('partitionForHome', () => {
  describe('section assignment', () => {
    it('puts unread alive sessions in needsAttention', () => {
      const s = makeSession({ id: 's1', cwd: '/x', alive: true, unread: true })
      const { needsAttention, running, buckets } = partitionForHome([s], NOW)
      expect(needsAttention.map(s => s.id)).toEqual(['s1'])
      expect(running).toEqual([])
      expect(buckets).toEqual([])
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
      // a recency bucket (the historical behavior) fails loudly.
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
      const { needsAttention, running, buckets } = partitionForHome(sessions, NOW)
      expect(needsAttention).toEqual([])
      expect(running).toEqual([])
      expect(buckets).toEqual([])
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

  describe('recency buckets', () => {
    // Buckets hold idle-alive sessions (live but not currently
    // working/unread). Dead sessions never reach them since home
    // filters them out at the input.
    const idle = (id: string, ageMs: number) => makeSession({
      id, cwd: '/x', alive: true, last_activity_at: stamp(-ageMs),
    })

    it('groups idle-alive sessions into the four recency buckets', () => {
      const sessions = [
        idle('h', 30 * 60 * 1000), // 30m ago  → Last hour
        idle('t', 3 * HOUR),       // 09:00     → Earlier today
        idle('y', 15 * HOUR),      // prev 21:00 → Yesterday
        idle('w', 3 * DAY),        // 3 days     → Earlier this week
      ]
      const { buckets } = partitionForHome(sessions, NOW)
      expect(buckets.map(b => b.label)).toEqual([
        'Last hour', 'Earlier today', 'Yesterday', 'Earlier this week',
      ])
      expect(buckets.map(b => b.sessions.map(s => s.id))).toEqual([
        ['h'], ['t'], ['y'], ['w'],
      ])
    })

    it('prioritizes the rolling Last hour window over the calendar day', () => {
      // A session 40m ago is "Last hour" even though it is also part
      // of "today". The rolling window wins.
      const { buckets } = partitionForHome([idle('a', 40 * 60 * 1000)], NOW)
      expect(buckets).toHaveLength(1)
      expect(buckets[0].label).toBe('Last hour')
    })

    it('drops sessions older than a week', () => {
      const { buckets } = partitionForHome([idle('ancient', 8 * DAY)], NOW)
      expect(buckets).toEqual([])
    })

    it('omits empty buckets', () => {
      // Only an earlier-this-week session present: the three newer
      // buckets don't render at all.
      const { buckets } = partitionForHome([idle('w', 3 * DAY)], NOW)
      expect(buckets.map(b => b.label)).toEqual(['Earlier this week'])
    })

    it('sorts newest-first within a bucket', () => {
      const { buckets } = partitionForHome(
        [idle('older', 50 * 60 * 1000), idle('newer', 10 * 60 * 1000)],
        NOW,
      )
      expect(buckets).toHaveLength(1)
      expect(buckets[0].sessions.map(s => s.id)).toEqual(['newer', 'older'])
    })

    it('returns no buckets when every session is working or unread', () => {
      const working = { label: 'x', working: true, error: false }
      const sessions = [
        makeSession({ id: 'a', cwd: '/x', alive: true, status: working }),
        makeSession({ id: 'b', cwd: '/x', alive: true, unread: true }),
      ]
      const { buckets } = partitionForHome(sessions, NOW)
      expect(buckets).toEqual([])
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
    // A session last active a week ago would be dropped from home's
    // recency buckets (>7 days) but must remain on the project page.
    const weekAgo = new Date(NOW - 7 * DAY).toISOString()
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
    const longAgo = new Date(NOW - 30 * DAY).toISOString()
    const s = makeSession({
      id: 'old-exit',
      cwd: '/x',
      alive: false,
      last_activity_at: longAgo,
    })
    const { rest } = partitionForProject([s])
    expect(rest.map(x => x.id)).toEqual(['old-exit'])
  })

  it('does not cap or window `rest`', () => {
    // Generate a large set so a regression that accidentally reused
    // home's bucketing/dropping would show up as a shorter list.
    const sessions = Array.from({ length: 15 }, (_, i) =>
      makeSession({ id: `s${i}`, cwd: '/x', alive: false }),
    )
    const { rest } = partitionForProject(sessions)
    expect(rest.length).toBe(15)
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
