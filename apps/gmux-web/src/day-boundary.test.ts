/** Day-relative labels ("Today", "Yesterday", "Last <weekday>") are a
 * function of the wall clock, but every other trigger in the store is a
 * function of *data*. #518's incremental reconciliation made that gap
 * observable: an identical snapshot commits as a no-op, and a quiet tab
 * commits nothing at all, so a sidebar left open across midnight kept
 * calling yesterday "Today" until some unrelated update arrived.
 *
 * watchDayBoundary is the clock trigger: one timer per app lifetime, armed
 * for the next local midnight, re-armed after it fires, cleared on unmount.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  _rawSessions, _setRawWorld, homePartition, sessionsLoaded, urlHash, urlPath,
  urlSearch, watchDayBoundary, worldLoaded,
} from './store'
import { makeSession } from './test-helpers'

;(globalThis as { window?: unknown }).window ??= globalThis

// Local (not UTC) parts: the partition computes calendar days in local time.
const NOW = new Date(2026, 0, 12, 15, 0, 0).getTime()
const HOUR = 60 * 60 * 1000

beforeEach(() => {
  vi.useFakeTimers({ now: NOW })
  _setRawWorld({ projects: [], peers: [], peerProjects: {} })
  _rawSessions.value = [makeSession({
    id: 'a', cwd: '/x', alive: true, last_output_at: new Date(NOW - HOUR).toISOString(),
  })]
  sessionsLoaded.value = true
  worldLoaded.value = true
  urlPath.value = '/'
  urlSearch.value = ''
  urlHash.value = ''
})

afterEach(() => {
  vi.useRealTimers()
  _rawSessions.value = []
})

const labels = () => homePartition.value.map(b => ({ label: b.label, kind: b.kind }))

describe('watchDayBoundary', () => {
  it('relabels the buckets at the next local midnight without any snapshot', () => {
    const stop = watchDayBoundary()
    expect(labels()).toEqual([{ label: null, kind: 'today' }])

    // Just before midnight: nothing has moved yet.
    vi.advanceTimersByTime(8 * HOUR + 59 * 60 * 1000)
    expect(labels()).toEqual([{ label: null, kind: 'today' }])

    // Across midnight, with no store mutation of any kind.
    vi.advanceTimersByTime(2 * 60 * 1000)
    expect(labels()).toEqual([{ label: 'Yesterday', kind: 'named' }])
    stop()
  })

  it('re-arms, so a second midnight also relabels', () => {
    const stop = watchDayBoundary()
    vi.advanceTimersByTime(9 * HOUR + 60 * 1000)
    expect(labels()).toEqual([{ label: 'Yesterday', kind: 'named' }])

    vi.advanceTimersByTime(24 * HOUR)
    expect(labels()).toEqual([{ label: expect.stringMatching(/^Last /), kind: 'named' }])
    stop()
  })

  it('does not poll: exactly one timer is pending at a time', () => {
    const stop = watchDayBoundary()
    expect(vi.getTimerCount()).toBe(1)
    vi.advanceTimersByTime(9 * HOUR + 60 * 1000)
    expect(vi.getTimerCount()).toBe(1)
    stop()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('stops on unmount: a cleared watch never relabels', () => {
    const stop = watchDayBoundary()
    // Read once so the projection is cached at this clock, then unmount:
    // without the timer's signal write nothing invalidates it again.
    expect(labels()).toEqual([{ label: null, kind: 'today' }])
    stop()
    vi.advanceTimersByTime(48 * HOUR)
    expect(labels()).toEqual([{ label: null, kind: 'today' }])
  })
})

// The arm target is computed from calendar parts (new Date(y, m, d + 1)) rather
// than "+24h" precisely so a DST day still lands on local midnight. Pinned in a
// zone that shifts: 2026-03-08 is the US spring-forward, a 23-hour day.
describe('watchDayBoundary across a DST transition', () => {
  const savedTZ = process.env.TZ

  beforeEach(() => { process.env.TZ = 'America/New_York' })
  // Assigning an undefined savedTZ back would store the *string* "undefined",
  // which Node resolves to UTC — and vitest reuses worker processes across
  // files, so that would silently move any later TZ-sensitive test to UTC.
  afterEach(() => {
    if (savedTZ === undefined) delete process.env.TZ
    else process.env.TZ = savedTZ
  })

  it('arms 23 hours for the short day, not 24', () => {
    // Sat 2026-03-07 22:00 EST. If the process did not pick the zone up, this
    // fails loudly rather than asserting something vacuous.
    const start = new Date(2026, 2, 7, 22, 0, 0)
    expect(start.getTimezoneOffset()).toBe(300) // EST = UTC-5
    vi.setSystemTime(start)

    const setTimeoutSpy = vi.spyOn(globalThis, 'setTimeout')
    const stop = watchDayBoundary()
    const firstDelay = setTimeoutSpy.mock.calls[0][1] as number
    // 22:00 → 00:00 next day (+1 s of slack), no DST shift in between.
    expect(firstDelay).toBe(2 * HOUR + 1_000)

    vi.advanceTimersByTime(firstDelay)
    const secondDelay = setTimeoutSpy.mock.calls[1][1] as number
    // Sun 2026-03-08 00:00 EST → Mon 2026-03-09 00:00 EDT is 23 hours (the
    // re-arm runs a second past midnight, so the slack is already spent).
    expect(secondDelay).toBe(23 * HOUR)

    stop()
    setTimeoutSpy.mockRestore()
  })
})
