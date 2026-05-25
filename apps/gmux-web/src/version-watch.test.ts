// Pins the version-mismatch reload guard. The watcher has one job
// the user actually feels: it must not loop, and it must not yank
// the page out from under them. These tests pin:
//   1) decideBundleState's three outputs
//   2) navigateWithReload's behavior under each state
//   3) the loop-guard marker is set at reload time

import { describe, it, expect, beforeEach } from 'vitest'
import {
  decideBundleState,
  navigateWithReload,
  bundleStale,
} from './version-watch'

describe('decideBundleState', () => {
  it('is unknown (not fresh) when daemon version is missing', () => {
    // This is the pin for the loop-guard regression: returning
    // `fresh` here would erase the reload marker on every mount
    // before health arrives, defeating the guard entirely.
    expect(decideBundleState(undefined, '0.1.0', null))
      .toEqual({ kind: 'unknown' })
  })

  it('preserves the marker through `unknown` (does not signal fresh)', () => {
    // Even with a marker present, an unknown daemon version stays
    // unknown rather than overriding the marker semantics.
    expect(decideBundleState(undefined, '0.1.0', '0.1.0'))
      .toEqual({ kind: 'unknown' })
  })

  it('is fresh when versions match', () => {
    expect(decideBundleState('0.1.0', '0.1.0', null))
      .toEqual({ kind: 'fresh' })
  })

  it('is stale on mismatch with no prior attempt', () => {
    expect(decideBundleState('0.1.1', '0.1.0', null))
      .toEqual({ kind: 'stale' })
  })

  it('is stuck when the marker matches this bundle (loop guard)', () => {
    // We already reloaded from this exact bundle once; the server is
    // still serving stale. Don't loop.
    expect(decideBundleState('0.1.1', '0.1.0', '0.1.0'))
      .toEqual({ kind: 'stuck' })
  })

  it('is stale when the marker is for a different bundle', () => {
    // Previously stuck on 0.1.0; this tab is now on 0.1.1 and daemon
    // moved to 0.1.2. The old marker is stale; take the fresh attempt.
    expect(decideBundleState('0.1.2', '0.1.1', '0.1.0'))
      .toEqual({ kind: 'stale' })
  })
})

describe('navigateWithReload', () => {
  beforeEach(() => {
    bundleStale.value = false
  })

  it('returns false and does not navigate when the bundle is fresh', () => {
    const calls: Array<[string, boolean]> = []
    const storage = makeStorage()
    const took = navigateWithReload('/foo', false, storage, '0.1.0', (u, r) => calls.push([u, r]))
    expect(took).toBe(false)
    expect(calls).toEqual([])
    expect(storage.getItem('gmux:reload-from')).toBe(null)
  })

  it('does a push-style reload (replace=false) and sets the marker', () => {
    bundleStale.value = true
    const calls: Array<[string, boolean]> = []
    const storage = makeStorage()
    const took = navigateWithReload('/foo', false, storage, '0.1.0', (u, r) => calls.push([u, r]))
    expect(took).toBe(true)
    expect(calls).toEqual([['/foo', false]])
    expect(storage.getItem('gmux:reload-from')).toBe('0.1.0')
  })

  it('forwards replace=true so auto-attach navigations do not grow the back stack', () => {
    // Pinning the fix for the dropped-replace bug Greptile flagged:
    // store.navigate(url, true) must not push a fresh history entry
    // just because the bundle happened to be stale at the time.
    bundleStale.value = true
    const calls: Array<[string, boolean]> = []
    const storage = makeStorage()
    navigateWithReload('/foo', true, storage, '0.1.0', (u, r) => calls.push([u, r]))
    expect(calls).toEqual([['/foo', true]])
  })
})

/** Minimal in-memory Storage-shaped object for tests. */
function makeStorage(): Pick<Storage, 'getItem' | 'setItem' | 'removeItem'> {
  const map = new Map<string, string>()
  return {
    getItem: (k) => map.get(k) ?? null,
    setItem: (k, v) => { map.set(k, v) },
    removeItem: (k) => { map.delete(k) },
  }
}
