/** Projection benchmarks over the exact worlds from `store-perf-fixture.ts`
 * (seed 1234). Run with `npx vitest bench src/store.bench.ts`.
 *
 * Two shapes per corpus size:
 *   - cold: fresh snapshot array → every projection rebuilt from scratch,
 *   - one-session mutation: the protocol-3 steady state (a full-list
 *     replacement that differs in a single row's unread bit).
 *
 * Reference numbers (i7-9700K, Node 24) before/after the slice-0
 * normalization — see report-optimize-frontend-projections.md:
 *   n=10k  mutation  1,514 ms → ~99 ms;  n=30k  16,026 ms → ~350 ms.
 */
import { bench, describe } from 'vitest'
import {
  _rawSessions, _setRawWorld, applySessionsSnapshot, familyActivityById, familyDotById, folders,
  homePartition, sessionsLoaded, sidebarActivity, sidebarSessions, unreadCount,
  urlPath, worldLoaded,
} from './store'
import { makeFixtureWorld } from './store-perf-fixture'
import type { Session } from './types'

/** Server-side re-encode: same values, all-new object identities. Cold and
 * snapshot benches must go through this — with structural sharing, replaying
 * the *same* row objects would short-circuit into the fast path and measure
 * nothing. The map itself costs ~2–6 ms @10k–30k and is charged to the
 * benches that use it (it models the per-snapshot decode allocation). */
function reencode(rows: readonly Session[]): Session[] {
  return rows.map(s => ({
    ...s,
    command: [...s.command],
    remotes: s.remotes ? { ...s.remotes } : s.remotes,
    status: s.status ? { ...s.status } : s.status,
  }))
}

function readAll(): unknown {
  return [
    sidebarSessions.value, folders.value, unreadCount.value, sidebarActivity.value,
    familyDotById.value, familyActivityById.value, homePartition.value,
  ]
}

for (const n of [1000, 10000, 30000]) {
  const w = makeFixtureWorld(1234, n)
  describe(`store projections, ${n} sessions (fixture seed=1234)`, () => {
    bench('cold snapshot → all projections', () => {
      _setRawWorld({
        projects: w.projects, peers: w.peers, peerProjects: w.peerProjects,
        health: { hostname: 'localbox' } as never,
      })
      sessionsLoaded.value = true
      worldLoaded.value = true
      urlPath.value = '/'
      // Fresh identities every iteration: a genuinely cold rebuild.
      _rawSessions.value = reencode(w.sessions)
      readAll()
    })

    let flip = 0
    bench('one-session unread flip → all projections', () => {
      const idx = w.sessions.findIndex(s => s.id === w.mutationTargetId)
      const next = _rawSessions.value.length === n ? _rawSessions.value.slice() : w.sessions.slice()
      next[idx] = { ...next[idx], unread: (flip++ & 1) === 0, unread_token: `flip-${flip}` }
      _rawSessions.value = next
      readAll()
    })

    // The full protocol-3 commit seam: a re-encoded wholesale snapshot with
    // one changed row, through applySessionsSnapshot (reconciliation cost
    // included). This is what one storm frame costs the browser post-parse.
    let snapFlip = 0
    bench('one-row flip via re-encoded snapshot commit', () => {
      const next = reencode(_rawSessions.value.length === n ? _rawSessions.value : w.sessions)
      const idx = next.findIndex(s => s.id === w.mutationTargetId)
      next[idx] = { ...next[idx], unread: (snapFlip++ & 1) === 0, unread_token: `snap-${snapFlip}` }
      applySessionsSnapshot(next)
      readAll()
    })

    bench('identical re-encoded snapshot commit (no-op reconcile)', () => {
      applySessionsSnapshot(reencode(_rawSessions.value.length === n ? _rawSessions.value : w.sessions))
      readAll()
    })

    // Structural churn defeats every fast path (row count changes → the
    // identity diff is null → full non-incremental rebuild). Pins that the
    // slow path did not regress under the added diff/reconcile attempts.
    bench('one-row remove/re-add via snapshot commit (full rebuild path)', () => {
      const cur = _rawSessions.value.length >= n - 1 ? _rawSessions.value : w.sessions
      const next = reencode(cur.length === n ? cur.slice(0, -1) : [...cur, w.sessions[n - 1]])
      applySessionsSnapshot(next)
      readAll()
    })
  })
}
