import { describe, expect, it } from 'vitest'
import {
  bucketedFamily, descendantTree, familyAgentCount, familyBucket, familyIndex, familyNavigation,
  familyRoot, FAMILY_GROUP_CAPS, hasFamily, isFamilyChild, projectFamily,
  type FamilyBucketGroup,
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

  it('reprojects against the reassigned direct parent', () => {
    const first = agent('first')
    const second = agent('second')
    const child = agent('child', 'first')
    expect(familyRoot(child, [first, second, child])).toBe(first)

    const reparented = { ...child, parent_session_id: 'second' }
    expect(familyRoot(reparented, [first, second, reparented])).toBe(second)
    expect(isFamilyChild(reparented, [first, second, reparented])).toBe(true)

    const cleared = { ...child, parent_session_id: undefined }
    expect(familyRoot(cleared, [first, second, cleared])).toBe(cleared)
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

  it('derives non-duplicated parent/root header navigation', () => {
    const root = agent('root')
    const parent = agent('parent', 'root')
    const child = agent('child', 'parent')
    expect(familyNavigation(parent, [root, parent])).toEqual({ parent: root, root: undefined })
    expect(familyNavigation(child, [root, parent, child])).toEqual({ parent, root })
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
    // Bucketing works off the projected trees plus the shared index; it
    // must not rescan the snapshot array per node.
    expect(bucketedFamily(children[250], snapshot).groups.length).toBeGreaterThan(0)
    // One indexed pass over 1,000 rows; old per-candidate Map construction
    // performed hundreds of thousands of indexed reads here.
    expect(indexedReads).toBeLessThanOrEqual(snapshot.length + 1)
  })
})

describe('bucketed drawer projection', () => {
  const at = (minute: number) => `2026-08-04T10:${String(minute).padStart(2, '0')}:00Z`
  const member = (id: string, extra: Partial<Parameters<typeof makeSession>[0]> = {}) => makeSession({
    id, cwd: '/p', title: id, parent_session_id: 'root', semantic_agent: true,
    created_at: at(0), last_output_at: at(1), ...extra,
  })
  const flat = (group: FamilyBucketGroup) => group.nodes.map(n => n.session.id)

  it('caps groups at ∞/20/10/3', () => {
    expect(FAMILY_GROUP_CAPS).toEqual({ attention: Infinity, working: 20, idle: 10, finished: 3 })
  })

  it('derives a session own bucket from the sessionDotState precedence', () => {
    expect(familyBucket(member('e', { status: { active: true, error: true } }))).toBe('attention')
    expect(familyBucket(member('u', { unread: true }))).toBe('attention')
    expect(familyBucket(member('w', { status: { active: true } }))).toBe('working')
    // Dot precedence (working > unread) carries over: still-active work
    // isn't "waiting on you" yet even if output is unseen.
    expect(familyBucket(member('wu', { status: { active: true }, unread: true }))).toBe('working')
    expect(familyBucket(member('i'))).toBe('idle')
    // Unread is not alive-gated: unseen output demands attention even
    // after the session died.
    expect(familyBucket(member('du', { alive: false, unread: true }))).toBe('attention')
    // Error/working are alive-gated (as in sessionDotState); a dead,
    // fully-viewed session is finished no matter how it ended.
    expect(familyBucket(member('d', { alive: false, status: { active: true, error: true } }))).toBe('finished')
  })

  it('partitions a children list into attention/working/idle/finished order', () => {
    const root = agent('root')
    const sessions = [
      root,
      member('idle-1'),
      member('dead-1', { alive: false }),
      member('working-1', { status: { active: true } }),
      member('unread-1', { unread: true }),
    ]
    const projection = bucketedFamily(sessions[0], sessions)
    // Top level: the root tree is the single (idle-bucketed) node…
    expect(projection.groups.map(g => g.bucket)).toEqual(['attention'])
    const rootNode = projection.groups[0].nodes[0]
    expect(rootNode.session.id).toBe('root')
    // …whose children carry the four buckets in display order.
    expect(rootNode.groups.map(g => g.bucket)).toEqual(['attention', 'working', 'idle', 'finished'])
    expect(rootNode.groups.map(flat)).toEqual([['unread-1'], ['working-1'], ['idle-1'], ['dead-1']])
    expect(projection.counts).toEqual({ attention: 1, working: 1, idle: 2, finished: 1, processes: 0 })
  })

  it('hoists a node to its subtree highest-urgency bucket', () => {
    const root = agent('root')
    const deadParent = member('dead-parent', { alive: false })
    const workingGrandchild = member('grandchild', {
      parent_session_id: 'dead-parent', status: { active: true },
    })
    const deadSibling = member('dead-sibling', { alive: false })
    const projection = bucketedFamily(root, [root, deadParent, workingGrandchild, deadSibling])
    const rootNode = projection.groups[0].nodes[0]
    // The dead parent sorts as working — its subtree contains active work —
    // while the plain dead sibling stays in finished.
    expect(rootNode.groups.map(g => g.bucket)).toEqual(['working', 'finished'])
    expect(rootNode.groups.map(flat)).toEqual([['dead-parent'], ['dead-sibling']])
    // Counts stay status-truth: the hoisted parent still tallies as finished.
    expect(projection.counts).toEqual({ attention: 0, working: 1, idle: 1, finished: 2, processes: 0 })
  })

  it('sorts within a bucket: agents before processes, recency, natural title', () => {
    const root = agent('root')
    const proc = (id: string, extra = {}) => member(id, { semantic_agent: undefined, adapter: 'shell', ...extra })
    const sessions = [
      root,
      proc('proc-new', { last_output_at: at(50) }),
      member('agent-old', { last_output_at: at(10) }),
      member('swarm-10', { last_output_at: at(20) }),
      member('swarm-9', { last_output_at: at(20) }),
      member('swarm-100', { last_output_at: at(20) }),
    ]
    const rootNode = bucketedFamily(root, sessions).groups[0].nodes[0]
    expect(rootNode.groups).toHaveLength(1)
    // Recency beats title; equal recency reads in natural numeric order;
    // the process sinks below every agent despite being the most recent.
    expect(flat(rootNode.groups[0])).toEqual(['swarm-9', 'swarm-10', 'swarm-100', 'agent-old', 'proc-new'])
    expect(rootNode.groups[0].nodes.map(n => n.process)).toEqual([false, false, false, false, true])
  })

  it('projects the ancestor spine and counts only the sibling-level trees', () => {
    const root = agent('root')
    const parent = member('parent')
    const selected = member('selected', { parent_session_id: 'parent' })
    const sibling = member('sibling', { parent_session_id: 'parent', alive: false })
    const projection = bucketedFamily(selected, [root, parent, selected, sibling])
    expect(projection.ancestors.map(s => s.id)).toEqual(['root', 'parent'])
    expect(projection.groups.map(g => g.bucket)).toEqual(['idle', 'finished'])
    // Ancestors are breadcrumb context, not counted rows.
    expect(projection.counts).toEqual({ attention: 0, working: 0, idle: 1, finished: 1, processes: 0 })
  })

  it('counts alive agents for the pill, excluding processes and other families', () => {
    const root = agent('root')
    const child = member('child')
    const dead = member('dead', { alive: false })
    const proc = member('proc', { semantic_agent: undefined, adapter: 'shell' })
    const stranger = agent('stranger')
    const sessions = [root, child, dead, proc, stranger]
    expect(familyAgentCount(child, sessions)).toBe(2)
    expect(familyAgentCount(root, sessions)).toBe(2)
    expect(familyAgentCount(stranger, sessions)).toBe(1)
  })
})
