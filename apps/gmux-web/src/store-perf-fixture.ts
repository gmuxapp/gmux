/** Deterministic synthetic worlds for the store-projection differential and
 * performance tests (`store-projections.test.ts`, `store.bench.ts`).
 *
 * Mirrors the corpus shape from the frontend-sync assessment probes:
 * multiple hosts (one Local devcontainer peer), ~50 projects plus peer
 * references, ~10% task-family children (with grandchildren), retained-dead
 * sessions, unread flags, unstamped strays, unresolvable stamps, and
 * transitional families (stamped child under an unstamped root — the
 * temporary-placement path in `foldersFrom`).
 *
 * Everything is seeded (Mulberry32) so a fixture is exactly reproducible:
 * the same (seed, n) pair always yields byte-identical worlds, which lets
 * perf numbers in reports/benches reference "fixture seed=S n=N" precisely.
 */
import type { PeerInfo, ProjectItem, Session } from './types'

export interface FixtureWorld {
  sessions: Session[]
  projects: ProjectItem[]
  peers: PeerInfo[]
  peerProjects: Record<string, { slug: string; launch_cwd?: string }[]>
  /** A mid-list family child id, for selection-dependent projections. */
  selectedChildId: string
  /** A root session id whose unread bit the mutation benches flip. */
  mutationTargetId: string
}

/** Mulberry32 — tiny, high-quality-enough seeded PRNG. */
export function mulberry32(seed: number): () => number {
  let a = seed >>> 0
  return () => {
    a = (a + 0x6D2B79F5) | 0
    let t = Math.imul(a ^ (a >>> 15), 1 | a)
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}

const HOSTS = ['', '', '', 'hostA', 'hostB', 'hostC', 'devbox'] as const

export function makeFixtureWorld(seed: number, n: number): FixtureWorld {
  const rnd = mulberry32(seed)
  const pick = <T,>(arr: readonly T[]): T => arr[Math.floor(rnd() * arr.length)]

  const projectCount = Math.max(5, Math.min(50, Math.floor(n / 20)))
  const projects: ProjectItem[] = []
  for (let i = 0; i < projectCount; i++) {
    projects.push({ slug: `p${i}`, match: [{ path: `~/work/p${i}` }] })
  }
  // References to peer-owned projects (hostA/hostB own a few slugs each).
  for (const peer of ['hostA', 'hostB']) {
    for (let i = 0; i < 4; i++) {
      projects.push({ slug: `${peer.toLowerCase()}-r${i}`, peer, node_id: `node-${peer}` })
    }
  }
  const peers: PeerInfo[] = [
    { name: 'hostA', url: 'http://a', status: 'connected', session_count: 0, node_id: 'node-hostA' } as PeerInfo,
    { name: 'hostB', url: 'http://b', status: 'connected', session_count: 0, node_id: 'node-hostB' } as PeerInfo,
    { name: 'hostC', url: 'http://c', status: 'disconnected', session_count: 0 },
    { name: 'devbox', url: 'http://d', status: 'connected', session_count: 0, local: true },
  ]
  const peerProjects: FixtureWorld['peerProjects'] = {
    hostA: [0, 1, 2].map(i => ({ slug: `hosta-r${i}`, launch_cwd: `/peer/a${i}` })),
    // hosta-r3 / hostb-r3 stay dangling (connected peer, slug removed upstream).
    hostB: [0, 1, 2].map(i => ({ slug: `hostb-r${i}`, launch_cwd: `/peer/b${i}` })),
  }

  const baseMs = Date.parse('2026-01-01T00:00:00Z')
  const iso = (offsetSec: number) => new Date(baseMs + offsetSec * 1000).toISOString()

  const sessions: Session[] = []
  const roots: Session[] = []
  const mk = (id: string, over: Partial<Session>): Session => ({
    id,
    created_at: iso(Math.floor(rnd() * 86400 * 14)),
    command: ['pi'],
    cwd: `~/work/p${Math.floor(rnd() * projectCount)}`,
    adapter: rnd() < 0.7 ? 'pi' : 'shell',
    alive: true,
    pid: 1,
    exit_code: null,
    started_at: iso(0),
    exited_at: null,
    title: `session ${id}`,
    subtitle: '',
    status: null,
    unread: false,
    socket_path: '/tmp/s.sock',
    ...over,
  })

  for (let i = 0; i < n; i++) {
    const id = `s${i}`
    const host = pick(HOSTS)
    const asChild = roots.length > 0 && rnd() < 0.12
    const alive = rnd() < 0.7
    const resumable = !alive && rnd() < 0.5
    const unread = rnd() < 0.15
    const over: Partial<Session> = {
      alive,
      resumable,
      unread,
      unread_token: unread ? `tok-${id}` : undefined,
      exited_at: alive ? null : iso(Math.floor(rnd() * 86400 * 14)),
      last_output_at: rnd() < 0.9 ? iso(Math.floor(rnd() * 86400 * 14)) : undefined,
      status: rnd() < 0.5 ? { active: rnd() < 0.5, error: rnd() < 0.1 } : null,
      semantic_agent: rnd() < 0.6 ? true : undefined,
    }
    if (host) over.peer = host
    if (asChild) {
      // Parent must share the child's host for realistic families, but a few
      // malformed cross-host edges keep the cycle/fail-open paths honest.
      const parent = rnd() < 0.95
        ? roots[Math.floor(rnd() * roots.length)]
        : sessions[Math.floor(rnd() * sessions.length)]
      over.parent_session_id = parent.id
      if ((parent.peer ?? '') === host || rnd() < 0.3) {
        // Most children are unstamped; some carry a stamp while their root
        // does not (transitional family → temporary placement), and some
        // carry a stamp that resolves to no project at all.
        const r = rnd()
        if (r < 0.25) over.project_slug = `p${Math.floor(rnd() * projectCount)}`
        else if (r < 0.3) over.project_slug = 'no-such-project'
      }
    } else {
      // Roots: mostly stamped; ~10% unstamped (discovery-only), a few stamped
      // with slugs the viewer doesn't know.
      const r = rnd()
      if (r < 0.82) {
        if (host === 'hostA' || host === 'hostB') {
          over.project_slug = `${host.toLowerCase()}-r${Math.floor(rnd() * 4)}`
        } else {
          over.project_slug = `p${Math.floor(rnd() * projectCount)}`
        }
        over.project_index = Math.floor(rnd() * 10)
      } else if (r < 0.88) {
        over.project_slug = 'unknown-slug'
      }
    }
    const s = mk(id, over)
    sessions.push(s)
    if (!asChild && s.semantic_agent && rnd() < 0.5) roots.push(s)
  }

  const firstChild = sessions.find(s => s.parent_session_id)
  const firstStampedRoot = sessions.find(s => !s.parent_session_id && s.project_slug?.startsWith('p'))
  return {
    sessions,
    projects,
    peers,
    peerProjects,
    selectedChildId: firstChild?.id ?? sessions[0]?.id ?? '',
    mutationTargetId: firstStampedRoot?.id ?? sessions[0]?.id ?? '',
  }
}
