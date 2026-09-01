import { describe, expect, it } from 'vitest'
import {
  childTrailTitle, descendantTree, familyActivityLabel, familyAncestors, familyIndex,
  familyMemberGlyph, familyRoot, familySegments, familyStateOf, hasFamily, isFamilyChild,
  projectFamily, promotionAction, promotionCopy, type FamilyActivity,
} from './family'
import { makeSession } from './test-helpers'

const agent = (id: string, parent?: string, extra = {}) => makeSession({
  id, cwd: '/p', title: id, parent_session_id: parent, launched_from_session_id: parent,
  semantic_agent: true, project_slug: 'p', ...extra,
})

describe('family overview state', () => {
  it('keeps process history out of agent attention', () => {
    const process = (extra = {}) => makeSession({ id: 'proc', cwd: '/p', adapter: 'shell', semantic_agent: false, ...extra })
    expect(familyStateOf(process({ alive: true, status: { active: true } }))).toBe('running')
    expect(familyStateOf(process({ alive: false, unread: true, status: { error: true } }))).toBeNull()
    expect(familyStateOf(process({ alive: true, unread: true, status: { active: false, error: true } }))).toBeNull()
  })

  it('retains the agent turn vocabulary and precedence', () => {
    expect(familyStateOf(agent('error', undefined, { alive: true, unread: true, status: { error: true } }))).toBe('error')
    expect(familyStateOf(agent('active', undefined, { alive: true, unread: true, status: { active: true } }))).toBe('active')
    expect(familyStateOf(agent('retry', undefined, { alive: true, unread: true, status: { active: true, error: true } }))).toBe('active')
    expect(familyStateOf(agent('acked-error', undefined, { alive: true, unread: false, status: { active: false, error: true } }))).toBeNull()
    expect(familyStateOf(agent('waiting', undefined, { unread: true }))).toBe('waiting')
  })
})

describe('task-family projection', () => {
  it('groups agent and process children of semantic parents unless promoted', () => {
    const root = agent('root')
    const child = agent('child', 'root')
    const shell = makeSession({ id: 'shell', cwd: '/p', parent_session_id: 'root', adapter: 'shell' })
    const promoted = agent('promoted', 'root', { parent_session_id: undefined })
    const sessions = [root, child, shell, promoted]
    expect(isFamilyChild(child, sessions)).toBe(true)
    expect(isFamilyChild(shell, sessions)).toBe(true)
    expect(isFamilyChild(promoted, sessions)).toBe(false)
    expect(familyRoot(child, sessions)).toBe(root)
    expect(familyRoot(shell, sessions)).toBe(root)
    expect(familyRoot(promoted, sessions)).toBe(promoted)
  })

  it.each([
    ['semantic child with shell parent', agent('child', 'shell'), makeSession({ id: 'shell', cwd: '/p', adapter: 'shell' })],
    ['shell child with shell parent', makeSession({ id: 'child', cwd: '/p', adapter: 'shell', parent_session_id: 'shell' }), makeSession({ id: 'shell', cwd: '/p', adapter: 'shell' })],
    ['semantic child with missing parent', agent('orphan', 'missing'), undefined],
    ['process child with missing parent', makeSession({ id: 'orphan-shell', cwd: '/p', adapter: 'shell', parent_session_id: 'missing' }), undefined],
  ])('keeps %s as a visible root', (_name, child, parent) => {
    const sessions = parent ? [parent, child] : [child]
    expect(isFamilyChild(child, sessions)).toBe(false)
    expect(familyRoot(child, sessions)).toBe(child)
    expect(descendantTree(child, sessions).session).toBe(child)
  })

  it('reprojects against a reassigned or cleared direct parent', () => {
    const first = agent('first')
    const second = agent('second')
    const child = agent('child', 'first')
    expect(familyRoot(child, [first, second, child])).toBe(first)

    const reparented = { ...child, parent_session_id: 'second' }
    expect(familyRoot(reparented, [first, second, reparented])).toBe(second)
    expect(isFamilyChild(reparented, [first, second, reparented])).toBe(true)

    const cleared = { ...child, parent_session_id: undefined }
    expect(familyRoot(cleared, [first, second, cleared])).toBe(cleared)
    expect(isFamilyChild(cleared, [first, second, cleared])).toBe(false)
  })

  it('shows family controls only when a real edge exists', () => {
    const root = agent('root')
    const child = agent('child', 'root')
    const orphan = agent('orphan', 'missing')
    expect(hasFamily(root, [root, child, orphan])).toBe(true)
    expect(hasFamily(child, [root, child, orphan])).toBe(true)
    expect(hasFamily(orphan, [root, child, orphan])).toBe(false)
  })

  it('fails malformed ancestry cycles open instead of hiding their members', () => {
    const a = agent('a', 'b')
    const b = agent('b', 'a')
    const descendant = agent('descendant', 'a')
    const snapshot = [a, b, descendant]
    expect(snapshot.map(s => isFamilyChild(s, snapshot))).toEqual([false, false, false])
    expect(snapshot.map(s => familyRoot(s, snapshot).id)).toEqual(['a', 'b', 'descendant'])
  })

  describe('promotionAction eligibility', () => {
    // The fixtures' cwd is /p; this catalog places them, so eligibility can
    // be probed independently of the placement gate (tested separately).
    const placed = [{ slug: 'p', match: [{ path: '/p' }] }]

    it('offers promote to a family child and names its parent', () => {
      const root = agent('root', undefined, { title: 'orchestrator' })
      const child = agent('child', 'root')
      const shell = makeSession({ id: 'shell', cwd: '/p', parent_session_id: 'root', adapter: 'shell', project_slug: 'p' })
      const snapshot = [root, child, shell]
      expect(promotionAction(child, snapshot, placed)).toEqual({ kind: 'promote', parent: root })
      // A process child is a family member too and can be promoted.
      expect(promotionAction(shell, snapshot, placed)).toEqual({ kind: 'promote', parent: root })
      // The root itself has nothing to promote or demote.
      expect(promotionAction(root, snapshot, placed)).toBeNull()
    })

    it('offers a daemon-stamped child when this viewer has that project', () => {
      const root = agent('root')
      const child = agent('child', 'root', { project_slug: 'stamped' })
      const project = [{ slug: 'stamped', match: [{ path: '/somewhere' }] }]
      expect(promotionAction(child, [root, child], project)).toEqual({ kind: 'promote', parent: root })
    })

    it('blocks a stamped child whose project is absent from this viewer', () => {
      const root = agent('root')
      const child = agent('child', 'root', { project_slug: 'stamped' })
      expect(promotionAction(child, [root, child], [])).toEqual({
        kind: 'promote', parent: root, blocked: 'no-project',
      })
    })

    it('blocks promote when no project would place the promoted row', () => {
      // The daemon has no placement for a session outside every project
      // (parentage never overrides matching, ADR 0026 §8): promoting would
      // strand it with no sidebar row and a dead URL. The action is offered
      // blocked — visible with the reason — never silently hidden.
      const root = agent('root', undefined, { title: 'orchestrator' })
      const child = agent('child', 'root', { project_slug: undefined })
      const action = promotionAction(child, [root, child], [])
      expect(action).toEqual({ kind: 'promote', parent: root, blocked: 'no-project' })
      const copy = promotionCopy(action!)
      expect(copy.label).toBe('Promote to root')
      expect(copy.note).toBe('Needs a project: no project contains this session’s folder, so it would have no row of its own. Add one in Settings → Projects.')
    })

    it('blocks return when the launch parent has no sidebar placement — visible, with the reason', () => {
      const root = agent('root', undefined, { project_slug: undefined, cwd: '/elsewhere' })
      const promoted = agent('promoted', 'root', { parent_session_id: undefined })
      expect(promotionAction(promoted, [root, promoted], [{ slug: 'p', match: [{ path: '/p' }] }]))
        .toEqual({ kind: 'demote', parent: root, blocked: 'no-project' })
    })

    it('offers demote to a promoted session whose family still exists', () => {
      const root = agent('root', undefined, { title: 'orchestrator' })
      const promoted = agent('promoted', 'root', { parent_session_id: undefined })
      expect(promotionAction(promoted, [root, promoted], placed)).toEqual({ kind: 'demote', parent: root })
    })

    it('hides demote when no valid local parent exists', () => {
      // Parent reconciled away: deletion repair normally clears the edge, but
      // a stale id must not offer a demote that rejoins nothing.
      const orphan = agent('orphan', 'gone', { parent_session_id: undefined })
      expect(promotionAction(orphan, [orphan], placed)).toBeNull()
      // Edge cleared entirely (post deletion repair).
      const cleared = agent('cleared', undefined, { parent_session_id: undefined })
      expect(promotionAction(cleared, [cleared], placed)).toBeNull()
      // Parent exists but is not a semantic agent: demoting would not rejoin
      // any presentation family.
      const shellParent = makeSession({ id: 'sh', cwd: '/p', adapter: 'shell' })
      const flagged = agent('flagged', 'sh', { parent_session_id: undefined })
      expect(promotionAction(flagged, [shellParent, flagged], placed)).toBeNull()
      // Malformed self-parent.
      const selfie = agent('selfie', 'selfie', { parent_session_id: undefined })
      expect(promotionAction(selfie, [selfie], placed)).toBeNull()
    })

    it('never offers mutations on peer-projected sessions', () => {
      // The daemon refuses promote/demote for sessions it does not own
      // (local_only) — network peers and Local/devcontainer peers alike.
      const root = agent('root', undefined, { peer: 'devbox' })
      const child = agent('child', 'root', { peer: 'devbox' })
      const promoted = agent('promoted', 'root', { peer: 'devbox', parent_session_id: undefined })
      const snapshot = [root, child, promoted]
      expect(promotionAction(child, snapshot, placed)).toBeNull()
      expect(promotionAction(promoted, snapshot, placed)).toBeNull()
    })

    it('offers nothing to plain roots, orphans and cycle members', () => {
      const solo = agent('solo')
      expect(promotionAction(solo, [solo], placed)).toBeNull()
      const orphan = agent('orphan', 'missing')
      expect(promotionAction(orphan, [orphan], placed)).toBeNull()
      const a = agent('a', 'b')
      const b = agent('b', 'a')
      expect(promotionAction(a, [a, b], placed)).toBeNull()
      expect(promotionAction(b, [a, b], placed)).toBeNull()
    })

    it('works for dead retained children when their stamped row is placeable', () => {
      const root = agent('root')
      const corpse = agent('corpse', 'root', { alive: false, resumable: true })
      expect(promotionAction(corpse, [root, corpse], placed)).toEqual({ kind: 'promote', parent: root })
    })

    it('blocks an unstamped retained corpse even when a match rule exists', () => {
      const root = agent('root')
      const corpse = agent('corpse', 'root', {
        project_slug: undefined, alive: false, resumable: true,
      })
      const action = promotionAction(corpse, [root, corpse], placed)
      expect(action).toEqual({ kind: 'promote', parent: root, blocked: 'no-project' })
      expect(promotionCopy(action!).note).toContain('no row of its own')
    })

    it('carries no note when the action is offerable — notes are for blockers', () => {
      const root = agent('root', undefined, { title: 'orchestrator' })
      const child = agent('child', 'root')
      const promote = promotionCopy(promotionAction(child, [root, child], placed)!)
      expect(promote.label).toBe('Promote to root')
      expect(promote.note).toBeUndefined()
      const promoted = agent('promoted', 'root', { parent_session_id: undefined })
      const demote = promotionCopy(promotionAction(promoted, [root, promoted], placed)!)
      expect(demote.label).toBe('Return to family')
      expect(demote.note).toBeUndefined()
    })

    it('pins the pending labels, both kinds', () => {
      const root = agent('root', undefined, { title: 'orchestrator' })
      const child = agent('child', 'root')
      const pendingPromote = promotionCopy(promotionAction(child, [root, child], placed)!, true)
      expect(pendingPromote.label).toBe('Promoting…')
      expect(pendingPromote.note).toBeUndefined()
      const promoted = agent('promoted', 'root', { parent_session_id: undefined })
      const pendingDemote = promotionCopy(promotionAction(promoted, [root, promoted], placed)!, true)
      expect(pendingDemote.label).toBe('Returning…')
      expect(pendingDemote.note).toBeUndefined()
    })
  })

  it('derives the breadcrumb ancestor spine, root first', () => {
    const root = agent('root')
    const parent = agent('parent', 'root')
    const child = agent('child', 'parent')
    const promoted = agent('promoted', 'parent', { parent_session_id: undefined })
    const snapshot = [root, parent, child, promoted]
    expect(familyAncestors(root, snapshot)).toEqual([])
    expect(familyAncestors(parent, snapshot).map(s => s.id)).toEqual(['root'])
    expect(familyAncestors(child, snapshot).map(s => s.id)).toEqual(['root', 'parent'])
    // Promotion severs the presentation edge: no crumbs, a plain title.
    expect(familyAncestors(promoted, snapshot)).toEqual([])
  })

  it('keeps a promoted agent full descendant subtree as a new family', () => {
    const root = agent('root')
    const promoted = agent('promoted', 'root', { parent_session_id: undefined })
    const grandchild = agent('grandchild', 'promoted')
    expect(descendantTree(root, [root, promoted, grandchild]).children).toEqual([])
    expect(descendantTree(promoted, [root, promoted, grandchild]).children[0]?.session).toBe(grandchild)
  })

  it('projects a child ancestor spine and only its own sibling level trees', () => {
    const root = agent('root')
    const aunt = agent('aunt', 'root')
    const parent = agent('parent', 'root')
    const selected = agent('selected', 'parent')
    const sibling = agent('sibling', 'parent')
    const niece = agent('niece', 'sibling')
    const p = projectFamily(selected, [root, aunt, parent, selected, sibling, niece])
    expect(p.ancestors.map(s => s.id)).toEqual(['root', 'parent'])
    // The whole family, from the root — including the branch the
    // selection isn't on. Standing deeper must never show you less.
    expect(p.tree.session.id).toBe('root')
    expect(p.tree.children.map(n => n.session.id).sort()).toEqual(['aunt', 'parent'])
    const parentNode = p.tree.children.find(n => n.session.id === 'parent')
    expect(parentNode?.children.map(n => n.session.id).sort()).toEqual(['selected', 'sibling'])
  })

  it('indexes a large snapshot once across projection callers', () => {
    const root = agent('root')
    const children = Array.from({ length: 500 }, (_, i) => agent(`child-${i}`, 'root'))
    const unrelated = Array.from({ length: 499 }, (_, i) => agent(`other-${i}`))
    let indexedReads = 0
    const snapshot = new Proxy([root, ...children, ...unrelated], {
      get(target, property, receiver) {
        if (typeof property === 'string' && /^\\d+$/.test(property)) indexedReads++
        return Reflect.get(target, property, receiver)
      },
    })

    expect(familyIndex(snapshot)).toBe(familyIndex(snapshot))
    expect(projectFamily(children[250], snapshot).tree.children).toHaveLength(500)
    expect(familyRoot(children[250], snapshot)).toBe(root)
    expect(hasFamily(root, snapshot)).toBe(true)
    expect(isFamilyChild(children[250], snapshot)).toBe(true)
    expect(familyAncestors(children[250], snapshot).map(s => s.id)).toEqual(['root'])
    // One indexed pass over 1,000 rows; old per-candidate Map construction
    // performed hundreds of thousands of indexed reads here.
    expect(indexedReads).toBeLessThanOrEqual(snapshot.length + 1)
  })
})

describe('flat panel projection', () => {
  const at = (minute: number) => `2026-08-04T10:${String(minute).padStart(2, '0')}:00Z`
  const member = (id: string, extra: Partial<Parameters<typeof makeSession>[0]> = {}) => makeSession({
    id, cwd: '/p', title: id, parent_session_id: 'root', semantic_agent: true,
    created_at: at(0), last_output_at: at(1), ...extra,
  })

  it('orders every children level by recency, like the sidebar activity feed', () => {
    const root = agent('root')
    const sessions = [
      root,
      member('old-working', { last_output_at: at(10), status: { active: true } }),
      member('newest-dead', { last_output_at: at(50), alive: false }),
      member('mid-idle', { last_output_at: at(30) }),
      member('created-only', { last_output_at: undefined, created_at: at(20) }),
    ]
    const rootNode = projectFamily(root, sessions).tree
    // Pure recency — status (working/dead/unread) must not reorder rows;
    // a session with no output yet sorts by creation time.
    expect(rootNode.children.map(n => n.session.id))
      .toEqual(['newest-dead', 'mid-idle', 'created-only', 'old-working'])
  })

})

describe('member row glyph', () => {
  const proc = (over = {}) => makeSession({
    id: 'p', cwd: '/p', title: 'tail -f', adapter: 'shell', parent_session_id: 'root', ...over,
  })
  const kid = (over = {}) => makeSession({
    id: 'k', cwd: '/p', title: 'kid', semantic_agent: true, parent_session_id: 'root', ...over,
  })

  it('gives a process one stable $ in every state, carrying lifecycle', () => {
    // Shape alone says “process”; agent attention never recolors it.
    // The one fact the $ does carry is lifecycle: running or not.
    expect(familyMemberGlyph(proc(), 'none')).toEqual({ kind: 'process', running: false })
    expect(familyMemberGlyph(proc(), 'working')).toEqual({ kind: 'process', running: false })
    expect(familyMemberGlyph(proc({ status: { active: true } }), 'none'))
      .toEqual({ kind: 'process', running: true })
    // Dead is never running, whatever the last status said.
    expect(familyMemberGlyph(proc({ status: { active: true }, alive: false }), 'none'))
      .toEqual({ kind: 'process', running: false })
  })

  it('keeps a named agent\'s attention on its dot', () => {
    for (const state of ['unread', 'error'] as const) {
      const glyph = familyMemberGlyph(kid(), state)
      expect(glyph).toEqual({ kind: 'dot', state })
    }
    expect(familyMemberGlyph(proc({ unread: true }), 'unread')).toEqual({ kind: 'process', running: false })
    expect(familyMemberGlyph(proc({ status: { error: true } }), 'error')).toEqual({ kind: 'process', running: false })
  })

  it('falls back to the branch only for an agent with nothing to say', () => {
    expect(familyMemberGlyph(kid(), 'none')).toEqual({ kind: 'branch' })
    expect(familyMemberGlyph(kid(), 'working')).toEqual({ kind: 'dot', state: 'working' })
  })
})

describe('family activity line', () => {
  const activity = (over: Partial<FamilyActivity> = {}): FamilyActivity =>
    ({ error: 0, waiting: 0, active: 0, running: 0, ...over })

  it('has no segments for an idle family, or for one with no entry at all', () => {
    expect(familySegments(activity())).toEqual([])
    // A quiet family is absent from the activity map entirely, and that
    // absence is the same fact as "nothing to report".
    expect(familySegments(undefined)).toEqual([])
  })

  it('gives every state a segment', () => {
    for (const state of ['error', 'waiting', 'active', 'running'] as const) {
      expect(familySegments(activity({ [state]: 1 })).map(s => s.state)).toEqual([state])
    }
  })

  it('orders segments attention-first, and drops the zeros', () => {
    // One display order for every surface: the members that need you
    // (error, waiting) before the ambient work (active, running), so
    // what survives a narrow clip is what matters.
    expect(familySegments(activity({ running: 3, waiting: 2, error: 1, active: 4 })).map(s => s.state))
      .toEqual(['error', 'waiting', 'active', 'running'])
    expect(familySegments(activity({ running: 3, error: 1 })).map(s => s.state))
      .toEqual(['error', 'running'])
    // The glyph table is the vocabulary: dots for states that have
    // them, and the running state's null dot is the `$`.
    expect(familySegments(activity({ running: 1 }))[0].dot).toBeNull()
    expect(familySegments(activity({ waiting: 1 }))[0].dot).toBe('unread')
  })

  it('spells the glyph row out for screen readers, attention first', () => {
    expect(familyActivityLabel(activity({ error: 1, waiting: 2, active: 1, running: 3 })))
      .toBe('In this family: 1 member with an error, 2 members waiting on you, 1 active subagent, 3 running processes')
  })

  it('omits zero states from the label', () => {
    expect(familyActivityLabel(activity({ waiting: 1 })))
      .toBe('In this family: 1 member waiting on you')
  })
})

describe('childTrailTitle', () => {
  const root = agent('root')
  const mid = agent('mid', 'root')
  const leaf = agent('leaf', 'mid')

  it('reads root › … › child for a deep descendant', () => {
    const sessions = [root, mid, leaf]
    expect(childTrailTitle(root, familyAncestors(leaf, sessions), leaf)).toBe('root › mid › leaf')
  })

  it('collapses to root › child for a direct child', () => {
    const sessions = [root, mid]
    expect(childTrailTitle(root, familyAncestors(mid, sessions), mid)).toBe('root › mid')
  })

  it('never repeats the root when the spine already starts with it', () => {
    // familyAncestors is root-first; the trail must not print it twice.
    expect(childTrailTitle(root, [root], mid)).toBe('root › mid')
  })
})

describe('familyIndex incremental patching (structural sharing)', () => {
  it('re-keys the cache on patch: the old array must rebuild, never alias the patched index', () => {
    // The in-place patch mutates the previous snapshot's index maps to
    // describe the *new* array. The WeakMap entry for the old array is
    // deleted at that moment; if a mutant drops that delete, asking for the
    // old array's index again would hand back the patched (aliased) maps,
    // whose rows belong to the new snapshot (stale reads). Unreachable from today's
    // production call sites (they always pass the live sessions.value), but
    // pinned here so a future call-site refactor can't silently regress.
    const root = agent('root')
    const child = agent('child', 'root', { unread: false })
    const prev = [root, child]
    const prevIndex = familyIndex(prev)
    expect(prevIndex.byId.get('child')).toBe(child)

    // Replaced-rows-only successor with family facts intact → patched, and
    // the same index object is re-keyed onto the new array.
    const childFlipped = { ...child, unread: true, unread_token: 'tok' }
    const next = [root, childFlipped]
    const nextIndex = familyIndex(next)
    expect(nextIndex).toBe(prevIndex)
    expect(nextIndex.byId.get('child')).toBe(childFlipped)

    // Asking for the old array again must yield an index that describes
    // exactly its own rows. (Mechanically it may be the same object patched
    // back — the ping-pong re-patch — which is fine; what the deleted cache
    // entry guarantees is that it can never be served *stale*, still holding
    // the new snapshot's rows. A mutant dropping the delete returns the
    // aliased index with `childFlipped` here and fails.)
    const rebuilt = familyIndex(prev)
    expect(rebuilt.byId.get('child')).toBe(child)
    expect(rebuilt.rootById.get('child')).toBe(root)

    // And the ping-pong stays coherent: querying the new array again after
    // the old one was re-indexed still yields an index of the new rows.
    expect(familyIndex(next).byId.get('child')).toBe(childFlipped)
  })

  it('family-fact changes rebuild instead of patching', () => {
    const root = agent('root')
    const child = agent('child', 'root')
    const prev = [root, child]
    const prevIndex = familyIndex(prev)
    const next = [root, { ...child, parent_session_id: undefined }]
    const nextIndex = familyIndex(next)
    expect(nextIndex).not.toBe(prevIndex)
    expect(nextIndex.childIds.has('child')).toBe(false)
  })
})
