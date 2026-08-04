import { describe, expect, it } from 'vitest'
import { descendantTree, familyIndex, familyNavigation, familyRoot, hasFamily, isFamilyChild, projectFamily } from './family'
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
    // One indexed pass over 1,000 rows; old per-candidate Map construction
    // performed hundreds of thousands of indexed reads here.
    expect(indexedReads).toBeLessThanOrEqual(snapshot.length + 1)
  })
})
