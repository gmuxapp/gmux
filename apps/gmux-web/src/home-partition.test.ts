// Pins partitionByDay — the activity feed's day-grouping. It's the only
// piece of feed logic with non-trivial behavior (rows are visual, the
// section layout is presentation). These guard the contracts the design
// conversation pinned down: plain recency grouping (no status hoist),
// the Today/Yesterday/Last-<weekday>/date label tiers, newest-first sort
// with created_at fallback and a stable id tiebreaker, and totality.

import { describe, it, expect } from 'vitest'
import { partitionByDay } from './store'
import { makeSession } from './test-helpers'

// Constructed from LOCAL date parts (not a UTC string) so the calendar-
// day boundaries — which partitionByDay computes in local time — line up
// deterministically regardless of the runner's timezone. Noon leaves a
// 12h gap to midnight, keeping day splits crisp.
const NOW = new Date(2026, 5, 1, 12, 0, 0).getTime()
const HOUR = 60 * 60 * 1000
const DAY = 24 * HOUR
const NOW_YEAR = new Date(NOW).getFullYear()

function stamp(offsetMs: number): string {
  return new Date(NOW + offsetMs).toISOString()
}
// Mirror the implementation's localized labels so assertions stay
// timezone/locale-agnostic while still pinning the structure + prefix.
const weekdayLabel = (offsetMs: number) =>
  `Last ${new Intl.DateTimeFormat(undefined, { weekday: 'long' }).format(new Date(NOW + offsetMs))}`
const dateLabel = (offsetMs: number) => {
  const day = new Date(NOW + offsetMs)
  const opts: Intl.DateTimeFormatOptions = { month: 'short', day: 'numeric' }
  if (day.getFullYear() !== NOW_YEAR) opts.year = 'numeric'
  return new Intl.DateTimeFormat(undefined, opts).format(day)
}

const idle = (id: string, ageMs: number) =>
  makeSession({ id, cwd: '/x', alive: true, last_output_at: stamp(-ageMs) })

describe('partitionByDay', () => {
  describe('day labels', () => {
    it('leaves today unlabeled (kind today)', () => {
      const buckets = partitionByDay([idle('t', 2 * HOUR)], NOW)
      expect(buckets).toHaveLength(1)
      expect(buckets[0].label).toBeNull()
      expect(buckets[0].kind).toBe('today')
    })

    it('labels yesterday', () => {
      // NOW is noon; -15h lands at 21:00 the previous calendar day.
      const buckets = partitionByDay([idle('y', 15 * HOUR)], NOW)
      expect(buckets[0]).toMatchObject({ label: 'Yesterday', kind: 'named' })
    })

    it('names days 2–7 ago as "Last <weekday>"', () => {
      for (const d of [2, 3, 7]) {
        const buckets = partitionByDay([idle(`d${d}`, d * DAY)], NOW)
        expect(buckets[0]).toMatchObject({
          label: weekdayLabel(-d * DAY),
          kind: 'named',
        })
      }
    })

    it('falls back to a short date at 8+ days ago', () => {
      const buckets = partitionByDay([idle('old', 8 * DAY)], NOW)
      expect(buckets[0]).toMatchObject({ label: dateLabel(-8 * DAY), kind: 'dated' })
    })

    it('shows the year in dated labels only when it differs from now', () => {
      const buckets = partitionByDay([idle('same', 10 * DAY), idle('prev', 210 * DAY)], NOW)
      const label = (id: string) => buckets.find(b => b.sessions.some(s => s.id === id))!.label!
      expect(label('same')).not.toMatch(/\d{4}/) // May 2026 → no year
      expect(label('prev')).toMatch(/2025/) // ~Nov 2025 → year shown
    })
  })

  describe('grouping is plain recency (no status hoist)', () => {
    it('groups unread / working sessions by time, not by status', () => {
      // The whole point of the redesign: a "waiting" (unread) session
      // from days ago sinks to its day bucket instead of being pinned
      // to the top. Status lives on the dot, not the position.
      const unreadOld = makeSession({
        id: 'u', cwd: '/x', alive: true, unread: true, last_output_at: stamp(-3 * DAY),
      })
      const workingToday = makeSession({
        id: 'w', cwd: '/x', alive: true,
        status: { working: true, error: false }, last_output_at: stamp(-2 * HOUR),
      })
      const buckets = partitionByDay([unreadOld, workingToday], NOW)
      expect(buckets.map(b => b.kind)).toEqual(['today', 'named'])
      expect(buckets[0].sessions.map(s => s.id)).toEqual(['w'])
      expect(buckets[1]).toMatchObject({ label: weekdayLabel(-3 * DAY) })
      expect(buckets[1].sessions.map(s => s.id)).toEqual(['u'])
    })

    it('includes dead sessions, bucketed by time (sidebar totality)', () => {
      // partitionByDay itself never drops dead sessions — the home
      // dashboard filters them at the input; the sidebar keeps them so
      // Activity membership matches Projects.
      const buckets = partitionByDay([idle('yst', 15 * HOUR), makeSession({
        id: 'd', cwd: '/x', alive: false, last_output_at: stamp(-15 * HOUR),
      })], NOW)
      expect(buckets[0].label).toBe('Yesterday')
      expect(buckets[0].sessions.map(s => s.id).sort()).toEqual(['d', 'yst'])
    })
  })

  describe('bucket ordering', () => {
    it('orders newest-first: today, yesterday, named, then dated oldest-last', () => {
      const sessions = [
        idle('d40', 40 * DAY),
        idle('n3', 3 * DAY),
        idle('t', 2 * HOUR),
        idle('d10', 10 * DAY),
        idle('y', 15 * HOUR),
      ]
      const buckets = partitionByDay(sessions, NOW)
      expect(buckets.map(b => b.kind)).toEqual(['today', 'named', 'named', 'dated', 'dated'])
      expect(buckets.map(b => b.sessions[0].id)).toEqual(['t', 'y', 'n3', 'd10', 'd40'])
    })
  })

  describe('sort within a bucket', () => {
    it('sorts newest-first by last_output_at', () => {
      const buckets = partitionByDay(
        [idle('older', 50 * 60 * 1000), idle('newer', 10 * 60 * 1000)],
        NOW,
      )
      expect(buckets).toHaveLength(1)
      expect(buckets[0].sessions.map(s => s.id)).toEqual(['newer', 'older'])
    })

    it('falls back to created_at when last_output_at is missing', () => {
      const old = makeSession({
        id: 'old', cwd: '/x', alive: true,
        created_at: stamp(-40 * 60 * 1000), last_output_at: stamp(-40 * 60 * 1000),
      })
      const fresh = makeSession({
        id: 'fresh', cwd: '/x', alive: true, created_at: stamp(-1_000), // no last_output_at
      })
      const buckets = partitionByDay([old, fresh], NOW)
      expect(buckets[0].sessions.map(s => s.id)).toEqual(['fresh', 'old'])
    })

    it('breaks timestamp ties by id (deterministic, no flicker)', () => {
      const same = stamp(-1000)
      const make = (id: string) => makeSession({ id, cwd: '/x', alive: true, last_output_at: same })
      const [a, b, c] = [make('a'), make('b'), make('c')]
      for (const arr of [[a, b, c], [c, b, a], [b, a, c], [c, a, b]]) {
        const buckets = partitionByDay(arr, NOW)
        expect(buckets[0].sessions.map(s => s.id)).toEqual(['a', 'b', 'c'])
      }
    })
  })

  it('is total: every session lands in exactly one bucket', () => {
    const sessions = [
      idle('t', HOUR), idle('y', 15 * HOUR), idle('n', 4 * DAY),
      idle('d', 12 * DAY), idle('dd', 90 * DAY),
      makeSession({ id: 'dead', cwd: '/x', alive: false, last_output_at: stamp(-2 * DAY) }),
    ]
    const buckets = partitionByDay(sessions, NOW)
    const got = buckets.flatMap(b => b.sessions.map(s => s.id)).sort()
    expect(got).toEqual(sessions.map(s => s.id).sort())
  })
})
