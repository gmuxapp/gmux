import { describe, expect, it } from 'vitest'
import {
  descendantTree, familyAncestors, familyCounts, familyIndex,
  childTrailTitle, familyActivityLabel, familyRoot, hasFamily, hasFamilyActivity,
  isFamilyChild, NO_FAMILY_ACTIVITY, projectFamily,
} from './family'
import { makeSession } from './test-helpers'

const agent = (id: string, parent?: string, extra = {}) => makeSession({
  id, cwd: '/p', title: id, parent_session_id: parent, semantic_agent: true, ...extra,
})

describe('task-family projection', () => {
  it('groups agent and process children of semantic parents unless promoted', () => {
    const root = agent('root')
    const child = agent('child', 'root')
    const shell = makeSession({ id: 'shell', cwd: '/p', parent_session_id: 'root', adapter: 'shell' })
    const promoted = agent('promoted', 'root', { promoted_to_root: true })
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

  it('derives the breadcrumb ancestor spine, root first', () => {
    const root = agent('root')
    const parent = agent('parent', 'root')
    const child = agent('child', 'parent')
    const promoted = agent('promoted', 'parent', { promoted_to_root: true })
    const snapshot = [root, parent, child, promoted]
    expect(familyAncestors(root, snapshot)).toEqual([])
    expect(familyAncestors(parent, snapshot).map(s => s.id)).toEqual(['root'])
    expect(familyAncestors(child, snapshot).map(s => s.id)).toEqual(['root', 'parent'])
    // Promotion severs the presentation edge: no crumbs, a plain title.
    expect(familyAncestors(promoted, snapshot)).toEqual([])
  })

  it('keeps a promoted agent full descendant subtree as a new family', () => {
    const root = agent('root')
    const promoted = agent('promoted', 'root', { promoted_to_root: true })
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
    expect(p.siblingTrees.map(n => n.session.id).sort()).toEqual(['selected', 'sibling'])
    expect(p.siblingTrees.flatMap(n => n.children).map(n => n.session.id)).toEqual(['niece'])
    expect(p.siblingTrees.some(n => n.session.id === 'aunt')).toBe(false)
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
    expect(projectFamily(children[250], snapshot).siblingTrees).toHaveLength(500)
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
    const rootNode = projectFamily(root, sessions).siblingTrees[0]
    // Pure recency — status (working/dead/unread) must not reorder rows;
    // a session with no output yet sorts by creation time.
    expect(rootNode.children.map(n => n.session.id))
      .toEqual(['newest-dead', 'mid-idle', 'created-only', 'old-working'])
  })

  it('tallies the counts line by dot precedence over all panel members', () => {
    const root = agent('root')
    const sessions = [
      root,
      member('working', { status: { active: true } }),
      member('working-unread', { status: { active: true }, unread: true }),
      member('errored', { status: { active: true, error: true } }),
      member('dead-unread', { alive: false, unread: true }),
      member('grandchild-unread', { parent_session_id: 'working', unread: true }),
      member('proc-working', { semantic_agent: undefined, adapter: 'shell', status: { active: true } }),
      member('dead-viewed', { alive: false }),
    ]
    const trees = projectFamily(root, sessions).siblingTrees
    // Each member counted once under its highest-precedence state: the
    // working-unread agent is working (dot precedence), the dead unread
    // one is unread (not alive-gated), processes count like anyone else,
    // and the root + quiet members only land in the total.
    expect(familyCounts(trees)).toEqual({ error: 1, working: 3, unread: 2, total: 8 })
  })

  it('counts only the sibling-level trees for a nested selection', () => {
    const root = agent('root')
    const parent = member('parent')
    const selected = member('selected', { parent_session_id: 'parent' })
    const sibling = member('sibling', { parent_session_id: 'parent', alive: false })
    const projection = projectFamily(selected, [root, parent, selected, sibling])
    expect(projection.ancestors.map(s => s.id)).toEqual(['root', 'parent'])
    // Ancestors are breadcrumb context in the header, not counted rows.
    expect(familyCounts(projection.siblingTrees)).toEqual({ error: 0, working: 0, unread: 0, total: 2 })
  })
})

describe('family activity line', () => {
  const activity = (over = {}) => ({ ...NO_FAMILY_ACTIVITY, ...over })

  it('shows nothing for an idle family', () => {
    expect(hasFamilyActivity(NO_FAMILY_ACTIVITY)).toBe(false)
  })

  it('shows for any single non-zero state', () => {
    expect(hasFamilyActivity(activity({ error: 1 }))).toBe(true)
    expect(hasFamilyActivity(activity({ unread: 1 }))).toBe(true)
    expect(hasFamilyActivity(activity({ workingAgents: 1 }))).toBe(true)
    expect(hasFamilyActivity(activity({ workingProcesses: 1 }))).toBe(true)
  })

  it('spells the glyph row out for screen readers, attention first', () => {
    expect(familyActivityLabel(activity({ error: 1, unread: 2, workingAgents: 1, workingProcesses: 3 })))
      .toBe('Also in this family: 1 member with an error, 2 unread members, 1 working subagent, 3 running processes')
  })

  it('omits zero states from the label', () => {
    expect(familyActivityLabel(activity({ unread: 1 })))
      .toBe('Also in this family: 1 unread member')
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
