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
  _rawSessions, _setRawWorld, familyActivityById, familyDotById, folders,
  homePartition, sessionsLoaded, sidebarActivity, sidebarSessions, unreadCount,
  urlPath, worldLoaded,
} from './store'
import { makeFixtureWorld } from './store-perf-fixture'

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
      _rawSessions.value = w.sessions.slice()
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
  })
}
