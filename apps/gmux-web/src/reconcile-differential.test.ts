/** Differential/property tests: the incremental (structurally shared,
 * memo-fast-pathed) projections must produce exactly the same *values* as a
 * non-incremental recompute, across randomized mutation sequences applied
 * through the production snapshot path (`applySessionsSnapshot`, i.e. what a
 * protocol-3 ready commit calls), including row removal, insertion, reorder,
 * family moves, peer merges, and full epoch resets through the protocol-3
 * begin/batch/ready staging.
 *
 * Every step re-encodes ALL rows (fresh object identities, like the server
 * does), so the equality fast path — not accidental identity — is what's
 * under test. The reference (`_uncachedProjections`) runs the same pure
 * building blocks with a freshly built family index and no memos.
 *
 * A second suite pins the identity/rerender contract: an identical
 * re-encoded snapshot must not change any projection identity (zero signal
 * recomputation propagates), and a one-row flip must change only the
 * affected slices.
 */

import type { Session as ProtocolSession } from '@gmux/protocol'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { familyIndex } from './family'
import {
  _pendingMutations, _rawSessions, _setRawWorld, _uncachedProjections,appendSessionsBootstrap, 
  applySessionsSnapshot, beginSessionsBootstrap,
  type DayBucket, familyActivityById, familyDotById, folders, homePartition,
  readySessionsBootstrap, resetSessionsTransport, sessions, sessionsLoaded,
  sidebarActivity, sidebarSessions, unreadCount, urlHash, urlPath, urlSearch,
  worldLoaded,
} from './store'
import { makeFixtureWorld, mulberry32 } from './store-perf-fixture'
import type { Folder, Session } from './types'

;(globalThis as { window?: unknown }).window ??= globalThis

// Pinned clock: partition buckets are day-relative, and the differential
// comparison recomputes them at "now". Mid-January keeps the fixture's
// 14-day activity window spread across today/named/dated buckets.
const NOW = Date.parse('2026-01-12T15:00:00Z')

beforeEach(() => {
  vi.useFakeTimers({ now: NOW })
  resetSessionsTransport()
  _rawSessions.value = []
  _pendingMutations.value = []
  _setRawWorld({ projects: [], peers: [], peerProjects: {} })
  sessionsLoaded.value = true
  worldLoaded.value = true
  urlPath.value = '/'
  urlSearch.value = ''
  urlHash.value = ''
})

afterEach(() => {
  vi.useRealTimers()
})

/** Server-side re-encode: same values, all-new identities. */
function reencode(s: Session): Session {
  return {
    ...s,
    command: [...s.command],
    remotes: s.remotes ? { ...s.remotes } : s.remotes,
    status: s.status ? { ...s.status } : s.status,
  }
}

const bucketsComparable = (bs: DayBucket[]) =>
  bs.map(b => ({ label: b.label, kind: b.kind, sessions: b.sessions }))

const foldersComparable = (fs: Folder[]) =>
  fs.map(f => ({ ...f }))

function expectProjectionsMatchUncached() {
  const ref = _uncachedProjections(Date.now())
  // Deep value equality against the uncached recompute. Same snapshot row
  // objects flow through both, so element identity holds too.
  expect(sidebarSessions.value).toEqual(ref.sidebarSessions)
  for (let i = 0; i < ref.sidebarSessions.length; i++) {
    expect(sidebarSessions.value[i]).toBe(ref.sidebarSessions[i])
  }
  expect(foldersComparable(folders.value)).toEqual(foldersComparable(ref.folders))
  expect(unreadCount.value).toBe(ref.unreadCount)
  expect(bucketsComparable(sidebarActivity.value)).toEqual(bucketsComparable(ref.sidebarActivity))
  expect(bucketsComparable(homePartition.value)).toEqual(bucketsComparable(ref.homePartition))
  // The patched family index must describe exactly the current snapshot.
  const idx = familyIndex(sessions.value)
  for (const s of sessions.value) expect(idx.byId.get(s.id)).toBe(s)
  void familyDotById.value
  void familyActivityById.value
}

let epoch = 1000

/** Full epoch reset through the protocol-3 staging path: begin → batches →
 * ready, exactly like an SSE reconnect replaying the world. */
function commitViaProtocol(rows: Session[]) {
  beginSessionsBootstrap(3, ++epoch)
  const mid = Math.floor(rows.length / 2)
  appendSessionsBootstrap(epoch, rows.slice(0, mid) as unknown as ProtocolSession[], 1)
  appendSessionsBootstrap(epoch, rows.slice(mid) as unknown as ProtocolSession[], 1)
  expect(readySessionsBootstrap(epoch)).toBe(true)
}

describe('incremental projections ≡ uncached recompute (randomized sequences)', () => {
  // CI budget: every step runs the full uncached oracle (O(N·projections))
  // plus deep toEqual diffs, and CI runners measured ~15–25× slower than a
  // dev machine (seed=29 took 17 s on CI at the original N=150–400 while
  // running ~0.6 s locally). Coverage lives in the *operation mix* and step
  // count, not in N — every structural shape (families, peers, transitional
  // placements, strays) exists in the fixture well below N=100 — so the
  // worlds are kept small (N≈60–160, still multi-host with dozens of
  // families) and each seed gets an explicit 30 s timeout: ~120 ms locally
  // × 25 CI factor ≈ 3 s, a 10× margin.
  const DIFF_TEST_TIMEOUT_MS = 30_000
  for (const seed of [11, 29, 47, 101]) {
    it(`mutation sequence seed=${seed}`, { timeout: DIFF_TEST_TIMEOUT_MS }, () => {
      const rnd = mulberry32(seed * 104729)
      const n = 60 + Math.floor(rnd() * 100)
      const w = makeFixtureWorld(seed, n)
      _setRawWorld({
        projects: w.projects, peers: w.peers, peerProjects: w.peerProjects,
        health: { hostname: 'localbox' } as never,
      })
      applySessionsSnapshot(w.sessions.map(reencode))
      expectProjectionsMatchUncached()

      let rows = sessions.value.slice()
      const pickIdx = () => Math.floor(rnd() * rows.length)
      let counter = 0

      for (let step = 0; step < 50; step++) {
        // Every step ships a full re-encoded snapshot (protocol semantics).
        rows = rows.map(reencode)
        const op = rnd()
        const i = pickIdx()
        const t = rows[i]
        if (op < 0.2) {
          // content-only flips: unread, title, status
          const r = rnd()
          rows[i] = r < 0.4
            ? { ...t, unread: !t.unread, unread_token: `tok-${counter++}` }
            : r < 0.7
              ? { ...t, title: `title ${counter++}`, subtitle: `sub ${counter}` }
              : { ...t, status: { active: rnd() < 0.5, error: rnd() < 0.3 } }
        } else if (op < 0.3) {
          // activity bump (moves partition buckets / ordering)
          rows[i] = { ...t, last_output_at: new Date(NOW - Math.floor(rnd() * 12 * 86400_000)).toISOString() }
        } else if (op < 0.4) {
          // lifecycle: death/resurrection
          rows[i] = t.alive
            ? { ...t, alive: false, resumable: rnd() < 0.5, status: null, exited_at: new Date(NOW).toISOString() }
            : { ...t, alive: true, resumable: false, exited_at: null }
        } else if (op < 0.5) {
          // family move: reparent to a random row, or promote to root
          const parent = rows[pickIdx()]
          rows[i] = rnd() < 0.3
            ? { ...t, parent_session_id: undefined }
            : { ...t, parent_session_id: parent.id }
        } else if (op < 0.58) {
          rows[i] = { ...t, semantic_agent: t.semantic_agent ? undefined : true }
        } else if (op < 0.68) {
          // peer merge/move: reassign owning host, as a hub picking up a
          // spoke's rows (or dropping them back to local)
          const peers = [undefined, 'hostA', 'hostB', 'devbox'] as const
          rows[i] = { ...t, peer: peers[Math.floor(rnd() * peers.length)] }
        } else if (op < 0.76) {
          // project restamp / reorder within folder
          rows[i] = rnd() < 0.5
            ? { ...t, project_slug: `p${Math.floor(rnd() * 10)}`, project_index: Math.floor(rnd() * 10) }
            : { ...t, project_index: Math.floor(rnd() * 10) }
        } else if (op < 0.84) {
          // removal
          rows.splice(i, 1)
        } else if (op < 0.9) {
          // insertion (new session appears)
          rows.splice(Math.floor(rnd() * (rows.length + 1)), 0,
            { ...reencode(t), id: `new-${seed}-${counter++}`, parent_session_id: undefined })
        } else if (op < 0.96) {
          // reorder: server-side placement shuffle
          for (let k = rows.length - 1; k > 0; k--) {
            const j = Math.floor(rnd() * (k + 1))
            ;[rows[k], rows[j]] = [rows[j], rows[k]]
          }
        } else {
          // epoch reset: full replay through begin/batch/ready staging
          resetSessionsTransport()
          commitViaProtocol(rows)
          expectProjectionsMatchUncached()
          rows = sessions.value.slice()
          continue
        }
        applySessionsSnapshot(rows.slice())
        expectProjectionsMatchUncached()
        rows = sessions.value.slice()
      }
    })
  }
})

describe('identity preservation (rerender/recompute granularity)', () => {
  function load(n: number) {
    const w = makeFixtureWorld(1234, n)
    _setRawWorld({
      projects: w.projects, peers: w.peers, peerProjects: w.peerProjects,
      health: { hostname: 'localbox' } as never,
    })
    applySessionsSnapshot(w.sessions.map(reencode))
    return w
  }

  it('an identical re-encoded snapshot is a complete no-op: zero recomputation propagates', () => {
    load(1500)
    const before = {
      raw: _rawSessions.value,
      sidebar: sidebarSessions.value,
      folders: folders.value,
      unread: unreadCount.value,
      activity: sidebarActivity.value,
      home: homePartition.value,
      dots: familyDotById.value,
      index: familyIndex(sessions.value),
    }
    // Count actual signal recomputations via subscriptions.
    let notified = 0
    const disposers = [sidebarSessions, folders, sidebarActivity, homePartition].map(sig =>
      sig.subscribe(() => { notified++ }))
    notified = 0 // discard the initial subscription callbacks
    applySessionsSnapshot(_rawSessions.value.map(reencode))
    expect(_rawSessions.value).toBe(before.raw) // array identity preserved
    expect(sidebarSessions.value).toBe(before.sidebar)
    expect(folders.value).toBe(before.folders)
    expect(unreadCount.value).toBe(before.unread)
    expect(sidebarActivity.value).toBe(before.activity)
    expect(homePartition.value).toBe(before.home)
    expect(familyDotById.value).toBe(before.dots)
    expect(familyIndex(sessions.value)).toBe(before.index)
    expect(notified).toBe(0)
    for (const d of disposers) d()
  })

  it('a one-row flip changes only the affected projection slices', () => {
    const w = load(1500)
    const beforeRaw = _rawSessions.value
    const beforeFolders = folders.value
    const beforeSidebar = sidebarSessions.value
    const beforeActivity = sidebarActivity.value
    const beforeIndex = familyIndex(sessions.value)

    const next = _rawSessions.value.map(reencode)
    const idx = next.findIndex(s => s.id === w.mutationTargetId)
    next[idx] = { ...next[idx], unread: !next[idx].unread, unread_token: 'flip' }
    applySessionsSnapshot(next)

    // Raw: every row but the flipped one keeps identity.
    const raw = _rawSessions.value
    expect(raw).not.toBe(beforeRaw)
    let changedRows = 0
    for (let i = 0; i < raw.length; i++) if (raw[i] !== beforeRaw[i]) changedRows++
    expect(changedRows).toBe(1)
    const newRow = raw[idx]

    // Family index is patched, not rebuilt (unread is not a family fact).
    expect(familyIndex(sessions.value)).toBe(beforeIndex)

    // Sidebar list: replaced-only substitution.
    const sidebar = sidebarSessions.value
    let changedSidebar = 0
    for (let i = 0; i < sidebar.length; i++) if (sidebar[i] !== beforeSidebar[i]) changedSidebar++
    expect(changedSidebar).toBe(1)

    // Folders: only the folder containing the flipped row gets a new object;
    // every other Folder (and its sessions array) keeps identity, so
    // identity-memoized folder components skip N-1 folders.
    const fs = folders.value
    expect(fs.length).toBe(beforeFolders.length)
    const changedFolders = fs.filter((f, i) => f !== beforeFolders[i])
    expect(changedFolders.length).toBe(1)
    expect(changedFolders[0].sessions).toContain(newRow)

    // Day buckets: only the bucket holding the flipped row changed.
    const bs = sidebarActivity.value
    const changedBuckets = bs.filter((b, i) => b !== beforeActivity[i])
    expect(changedBuckets.length).toBe(1)
    expect(changedBuckets[0].sessions).toContain(newRow)
  })

  it('identical re-broadcast after midnight refreshes day-partition labels', () => {
    // Reviewer-minimized regression (review-pr-518.md F2): with structural
    // sharing, a byte-identical snapshot commits as an identity no-op, so
    // day-relative labels must depend on an explicit clock input instead of
    // riding on array-identity churn. Production trigger: SSE reconnect
    // replaying an unchanged world after the machine slept across local
    // midnight. Failed at 15575d6f; passed at parent d893a120.
    load(40)
    const labelsBefore = {
      activity: sidebarActivity.value.map(b => b.label),
      home: homePartition.value.map(b => b.label),
    }
    vi.setSystemTime(NOW + 24 * 60 * 60 * 1000)
    applySessionsSnapshot(_rawSessions.value.map(reencode))
    // The oracle recomputes at the new clock; the incremental path must agree.
    const ref = _uncachedProjections(Date.now())
    expect(bucketsComparable(sidebarActivity.value)).toEqual(bucketsComparable(ref.sidebarActivity))
    expect(bucketsComparable(homePartition.value)).toEqual(bucketsComparable(ref.homePartition))
    // And the day actually moved: yesterday's bucket appears, labels shift.
    expect(sidebarActivity.value.map(b => b.label)).not.toEqual(labelsBefore.activity)
    expect(homePartition.value.map(b => b.label)).not.toEqual(labelsBefore.home)
    expect(sidebarActivity.value.some(b => b.label === 'Yesterday')).toBe(true)
    // Everything not day-relative stays a no-op: rows and folders keep identity.
    const raw = _rawSessions.value
    applySessionsSnapshot(raw.map(reencode))
    expect(_rawSessions.value).toBe(raw)
  })

  it('fact changes disable the fast path (a mutant reusing structure must fail)', () => {
    load(800)
    const beforeFolders = folders.value
    // Restamp one row into a different project: folder membership must move.
    const next = _rawSessions.value.map(reencode)
    const i = next.findIndex(s => !s.peer && s.project_slug === 'p1' && !s.parent_session_id && s.alive)
    expect(i).toBeGreaterThanOrEqual(0)
    next[i] = { ...next[i], project_slug: 'p2', project_index: 0 }
    applySessionsSnapshot(next)
    const ref = _uncachedProjections(Date.now())
    expect(foldersComparable(folders.value)).toEqual(foldersComparable(ref.folders))
    expect(folders.value.find(f => f.key === '::p2')!.sessions.map(s => s.id))
      .not.toEqual(beforeFolders.find(f => f.key === '::p2')!.sessions.map(s => s.id))
  })
})
