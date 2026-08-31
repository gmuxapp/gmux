/** Differential tests for the normalized (Map/Set-indexed) sidebar
 * projections against the pre-optimization reference algorithms.
 *
 * The production computeds (`sidebarSessions`, `folders`, `unreadCount`)
 * were rewritten from linear-scan-per-session (`Array.find` inside loops,
 * quadratic at scale) to shared `familyIndex` lookups. These tests pin the
 * rewrite to the old behavior: the reference implementations below are the
 * previous store code, verbatim modulo extraction into functions, and every
 * randomized world must project identically through both.
 *
 * Worlds come from the seeded generator in `store-perf-fixture.ts` (multi
 * host, family edges incl. malformed ones, unstamped strays, transitional
 * families, unresolvable stamps), crossed with the view state that the
 * projections read: tab filter, alive-only toggle, and selection (none /
 * root / family child / dismissed-by-filter).
 */
import { beforeEach, describe, expect, it } from 'vitest'
import { familyIndex, familyRoot, familyRootId, isFamilyChild, isProcessSession } from './family'
import { buildProjectFolders, type TemporaryPresentationPlacement } from './projects'
import { referencePresence } from './references'
import {
  _pendingMutations, _rawSessions, _setRawWorld, aliveOnly, filteredSessions, folders,
  localPeerNames, projects, peers, selected, selectedId, sessions, sessionsLoaded, setAliveOnly,
  sidebarSessions, unreadCount, urlHash, urlPath, urlSearch, worldLoaded,
} from './store'
import type { Folder, Session } from './types'
import { makeFixtureWorld, mulberry32 } from './store-perf-fixture'

// ── Reference implementations: the pre-optimization store code ──────────────

function referenceSidebarSessions(): Session[] {
  const sel = selectedId.value
  const onlyAlive = aliveOnly.value
  const base = filteredSessions.value.filter(s =>
    s.id === sel || (onlyAlive ? s.alive : s.alive || s.resumable),
  )
  const out = [...base]
  for (const candidate of base) {
    const rootId = familyRootId(candidate.id, sessions.value)
    const root = sessions.value.find(s => s.id === rootId)
    if (root && !out.some(s => s.id === root.id)) out.push(root)
  }
  if (sel) {
    const selectedSession = sessions.value.find(x => x.id === sel)
    if (selectedSession && !out.some(s => s.id === selectedSession.id)) out.push(selectedSession)
    const rootId = familyRootId(sel, sessions.value)
    const root = sessions.value.find(s => s.id === rootId)
    if (root && !out.some(s => s.id === root.id)) out.push(root)
  }
  return out
}

function referenceFoldersFrom(ss: Session[]): Folder[] {
  const forceVisible = new Set<string>()
  const temporaryPlacements = new Map<string, TemporaryPresentationPlacement>()
  const selectedRoot = familyRootId(selectedId.value, sessions.value)
  if (selectedRoot) forceVisible.add(selectedRoot)
  for (const candidate of ss) {
    if (isFamilyChild(candidate, sessions.value)) {
      const rootId = familyRootId(candidate.id, sessions.value)
      if (!rootId) continue
      forceVisible.add(rootId)
      const root = sessions.value.find(session => session.id === rootId)
      if (root?.project_slug || !candidate.project_slug) continue
      const sessionPeer = candidate.peer ?? ''
      const ownerPeer = sessionPeer && !localPeerNames.value.has(sessionPeer) ? sessionPeer : ''
      const resolves = projects.value.some(project =>
        project.slug === candidate.project_slug && (project.peer ?? '') === ownerPeer,
      )
      if (!resolves) continue
      const placement = { ownerPeer, slug: candidate.project_slug }
      const previous = temporaryPlacements.get(rootId)
      if (!previous || `${placement.ownerPeer}::${placement.slug}` < `${previous.ownerPeer}::${previous.slug}`) {
        temporaryPlacements.set(rootId, placement)
      }
    }
  }
  return buildProjectFolders(
    projects.value,
    ss.filter(s => !isFamilyChild(s, sessions.value)),
    (name) => localPeerNames.value.has(name),
    // Matches the store's private `_rawWorld.value.peerProjects` — the test
    // world round-trips it through `_setRawWorld`.
    worldPeerProjects,
    referencePresence(peers.value),
    forceVisible,
    temporaryPlacements,
  )
}

let worldPeerProjects: Record<string, { slug: string; launch_cwd?: string }[]> = {}

function referenceUnreadCount(): number {
  const sel = selectedId.value
  const childUnread = new Map<string, number>()
  for (const s of sessions.value) {
    if (s.id === sel || !s.unread || isProcessSession(s) || !isFamilyChild(s, sessions.value)) continue
    // The pre-optimization code read `index.rootById.get(s.id)?.id`;
    // `familyRootId` is that same lookup.
    const rootId = familyRootId(s.id, sessions.value)
    if (rootId) childUnread.set(rootId, (childUnread.get(rootId) ?? 0) + 1)
  }
  let n = 0
  for (const f of referenceFoldersFrom(filteredSessions.value)) {
    for (const s of f.sessions) {
      if (s.id !== sel && s.unread) n++
      n += childUnread.get(s.id) ?? 0
    }
  }
  return n
}

// ── Harness ──────────────────────────────────────────────────────────────────

function loadWorld(seed: number, n: number) {
  const w = makeFixtureWorld(seed, n)
  worldPeerProjects = w.peerProjects
  _rawSessions.value = w.sessions
  _setRawWorld({
    projects: w.projects,
    peers: w.peers,
    peerProjects: w.peerProjects,
    health: { hostname: 'localbox' } as never,
  })
  sessionsLoaded.value = true
  worldLoaded.value = true
  return w
}

/** Select a session through the URL, the way the app does. Returns whether
 * the selection actually resolved (fixture worlds don't guarantee every
 * shape is addressable). */
function selectViaUrl(s: Session | undefined): boolean {
  if (!s || s.peer) return false
  const root = familyRoot(s, sessions.value)
  if (root.peer || !root.project_slug || !projects.value.some(p => !p.peer && p.slug === root.project_slug)) return false
  urlPath.value = `/${root.project_slug}/${s.adapter}/~${s.id}`
  return selectedId.value === s.id
}

function foldersComparable(fs: Folder[]) {
  return fs.map(f => ({
    key: f.key,
    launchCwd: f.launchCwd,
    missing: f.missing,
    unresolved: f.unresolved,
    sessions: f.sessions.map(s => s.id),
  }))
}

function expectProjectionsMatchReference() {
  const actual = sidebarSessions.value
  const reference = referenceSidebarSessions()
  expect(actual.map(s => s.id)).toEqual(reference.map(s => s.id))
  // Row *identity* must hold too (`toBe` per element, not deep equality):
  // both projections must hand out the exact snapshot session objects, so
  // downstream identity-keyed caches and === comparisons keep working.
  for (let i = 0; i < actual.length; i++) expect(actual[i]).toBe(reference[i])
  expect(foldersComparable(folders.value)).toEqual(foldersComparable(referenceFoldersFrom(sidebarSessions.value)))
  expect(unreadCount.value).toBe(referenceUnreadCount())
  // `selected` is load-bearing for the whole session view: when the URL
  // addresses a session, it must be the exact snapshot object, never null.
  const sel = selectedId.value
  if (sel) expect(selected.value).toBe(sessions.value.find(s => s.id === sel) ?? null)
  else expect(selected.value).toBeNull()
}

// `selected` mirrors itself onto `window.__gmuxSession` for debugging; give
// the node test environment a window object so reading it is possible.
;(globalThis as { window?: unknown }).window ??= globalThis

beforeEach(() => {
  _rawSessions.value = []
  _setRawWorld({ projects: [], peers: [], peerProjects: {} })
  _pendingMutations.value = []
  sessionsLoaded.value = false
  worldLoaded.value = false
  urlPath.value = '/'
  urlSearch.value = ''
  urlHash.value = ''
  setAliveOnly(false)
})

describe('normalized projections ≡ reference algorithms (property)', () => {
  const seeds = [1, 2, 3, 5, 8, 13, 21, 42]
  for (const seed of seeds) {
    it(`randomized world seed=${seed}`, () => {
      const rnd = mulberry32(seed * 7919)
      const n = 50 + Math.floor(rnd() * 450)
      const w = loadWorld(seed, n)

      // 1. no selection, no filter
      expectProjectionsMatchReference()

      // 2. alive-only toggle
      setAliveOnly(true)
      expectProjectionsMatchReference()
      setAliveOnly(false)

      // 3. tab filter narrowed to one project, then one host
      urlSearch.value = '?filter=p1'
      expectProjectionsMatchReference()
      urlSearch.value = '?filter=*%40hostA'
      expectProjectionsMatchReference()
      urlSearch.value = ''

      // 4. select a family child (root row must stand in), then its root
      const child = w.sessions.find(s => s.parent_session_id && !s.peer)
      if (selectViaUrl(child)) {
        expectProjectionsMatchReference()
        urlSearch.value = '?filter=p2'
        expectProjectionsMatchReference()
        setAliveOnly(true)
        expectProjectionsMatchReference()
        setAliveOnly(false)
        urlSearch.value = ''
      }
      const root = w.sessions.find(s => !s.parent_session_id && !s.peer && s.project_slug?.startsWith('p'))
      if (selectViaUrl(root)) expectProjectionsMatchReference()

      // 5. random single-session mutations (unread flips, deaths, title runs)
      for (let i = 0; i < 20; i++) {
        const idx = Math.floor(rnd() * w.sessions.length)
        const next = _rawSessions.value.slice()
        const target = next[idx]
        const kind = rnd()
        next[idx] = kind < 0.5
          ? { ...target, unread: !target.unread, unread_token: `flip-${i}` }
          : kind < 0.8
            ? { ...target, alive: false, resumable: rnd() < 0.5, exited_at: '2026-01-02T00:00:00Z' }
            : { ...target, title: `mut ${i}` }
        _rawSessions.value = next
        expectProjectionsMatchReference()
      }

      // 6. removal and addition
      _rawSessions.value = _rawSessions.value.filter((_, i) => i % 17 !== 3)
      expectProjectionsMatchReference()
    })
  }

  it('selected hands out the exact snapshot session for the addressed URL', () => {
    loadWorld(3, 200)
    // A local stamped root is always URL-addressable; the fixture guarantees
    // plenty. A projection mutant that loses the session (returns null or a
    // copy) must fail here deterministically, not only via random worlds.
    const root = sessions.value.find(s =>
      !s.peer && !s.parent_session_id && /^p\d+$/.test(s.project_slug ?? '')
      && projects.value.some(p => !p.peer && p.slug === s.project_slug))
    expect(root).toBeDefined()
    expect(selectViaUrl(root)).toBe(true)
    expect(selected.value).toBe(root)
    expect(sidebarSessions.value).toContain(root)

    // Family-child selection: exact child object, root row stands in.
    const child = sessions.value.find(s => s.parent_session_id && selectViaUrl(s))
    if (child) {
      expect(selected.value).toBe(child)
      const childRoot = familyRoot(child, sessions.value)
      expect(sidebarSessions.value).toContain(childRoot)
    }

    urlPath.value = '/'
    expect(selected.value).toBeNull()
  })

  it('degenerate worlds: empty, single, all-children-of-one-root', () => {
    loadWorld(99, 0)
    expectProjectionsMatchReference()

    loadWorld(99, 1)
    expectProjectionsMatchReference()

    const w = loadWorld(4, 60)
    const root: Session = { ...w.sessions[0], id: 'root', parent_session_id: undefined, semantic_agent: true, peer: undefined, project_slug: 'p1', unread: true }
    const kids = Array.from({ length: 40 }, (_, i): Session => ({
      ...w.sessions[i % w.sessions.length],
      id: `kid${i}`,
      parent_session_id: i === 0 ? 'root' : `kid${i - 1}`,
      semantic_agent: true,
      peer: undefined,
      project_slug: i % 3 === 0 ? 'p2' : undefined,
      unread: i % 2 === 0,
    }))
    _rawSessions.value = [root, ...kids]
    expectProjectionsMatchReference()
  })
})

describe('projection performance regression (fixture seed=1234)', () => {
  /** One-session mutation must not trigger quadratic rework. Calibration
   * (2019 desktop i7-9700K, 24 repeated best-of-3 runs): optimized 29–37 ms,
   * pre-optimization O(N²) algorithms 1,200–1,487 ms. 400 ms is >10× headroom
   * over the optimized cost for slow CI machines while sitting 3× below the
   * old implementation's floor, so a reintroduced O(N²) scan fails hard. */
  it('one-session mutation derived cost at 10k sessions stays linear-ish', () => {
    const w = loadWorld(1234, 10000)
    const readAll = () => [sidebarSessions.value, folders.value, unreadCount.value]
    readAll() // cold build outside the measured window
    let best = Infinity
    for (let run = 0; run < 3; run++) {
      const idx = w.sessions.findIndex(s => s.id === w.mutationTargetId)
      const next = _rawSessions.value.slice()
      next[idx] = { ...next[idx], unread: !next[idx].unread, unread_token: `flip-${run}` }
      _rawSessions.value = next
      const t0 = performance.now()
      readAll()
      best = Math.min(best, performance.now() - t0)
    }
    expect(best).toBeLessThan(400)
  }, 120000)

  it('computeds share one family index per snapshot (no duplicate index builds)', () => {
    // `familyIndex` is WeakMap-cached per session-array identity. Reading
    // every projection must reuse one FamilyIndex object per snapshot, and
    // signal memoization must keep projection identity stable until the
    // snapshot array changes.
    loadWorld(7, 500)
    const before = familyIndex(sessions.value)
    const a = sidebarSessions.value
    const b = folders.value
    const c = unreadCount.value
    expect(familyIndex(sessions.value)).toBe(before)
    expect(sidebarSessions.value).toBe(a)
    expect(folders.value).toBe(b)
    expect(unreadCount.value).toBe(c)
    // An array swap with identical row objects is patched incrementally
    // (reconcile.ts): the previous index is re-keyed onto the new array
    // rather than rebuilt — still exactly one live index.
    _rawSessions.value = _rawSessions.value.slice()
    const after = familyIndex(sessions.value)
    expect(after).toBe(before)
    void [sidebarSessions.value, folders.value, unreadCount.value]
    expect(familyIndex(sessions.value)).toBe(after)
    // A genuine row replacement (new object, changed family fact) rebuilds.
    const next = _rawSessions.value.slice()
    next[0] = { ...next[0], parent_session_id: next[0].parent_session_id ? undefined : next[1]?.id }
    _rawSessions.value = next
    expect(familyIndex(sessions.value)).not.toBe(after)
  })
})
