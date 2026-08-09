import { describe, expect, it } from 'vitest'
import { FAMILY_LEVEL_CAP, splitLevel } from './family-drawer-model'
import { projectFamily } from './family'
import { makeSession } from './test-helpers'

const at = (minute: number) => `2026-08-04T10:${String(minute).padStart(2, '0')}:00Z`
const root = makeSession({
  id: 'root', cwd: '/p', title: 'root', semantic_agent: true,
  created_at: at(0), last_output_at: at(1),
})
const child = (id: string, minute: number) => makeSession({
  id, cwd: '/p', title: id, parent_session_id: 'root', semantic_agent: true,
  created_at: at(0), last_output_at: at(minute),
})

function rootChildren(count: number) {
  const snapshot = [root, ...Array.from({ length: count }, (_, i) => child(`c-${i}`, 59 - i))]
  return projectFamily(root, snapshot).tree.children
}

describe('panel level split (cap + summary row)', () => {
  it('shows small levels in full with no summary row', () => {
    const nodes = rootChildren(FAMILY_LEVEL_CAP)
    expect(splitLevel(nodes, 'root', new Set())).toEqual({ shown: nodes, summary: null })
  })

  it('caps an over-full level at the most recent 15 behind "+N more"', () => {
    const nodes = rootChildren(40)
    const { shown, summary } = splitLevel(nodes, 'root', new Set())
    expect(shown.map(n => n.session.id)).toEqual(nodes.slice(0, 15).map(n => n.session.id))
    expect(summary).toEqual({ key: 'root', expanded: false, label: '+25 more' })
  })

  it('expands two-state per parent: everything plus "show fewer"', () => {
    const nodes = rootChildren(40)
    const { shown, summary } = splitLevel(nodes, 'root', new Set(['root']))
    expect(shown).toHaveLength(40)
    expect(summary).toEqual({ key: 'root', expanded: true, label: 'show fewer' })
    // Another parent's expansion state does not leak into this level.
    expect(splitLevel(nodes, 'root', new Set(['other-parent'])).shown).toHaveLength(15)
  })
})

describe('the row you are on is never behind the fold', () => {
  it('keeps a pinned member that recency would have folded away', () => {
    const nodes = rootChildren(40)
    const stale = nodes[nodes.length - 1].session.id // oldest, deep below the cap
    const { shown, summary } = splitLevel(nodes, 'root', new Set(), new Set([stale]))
    expect(shown.map(n => n.session.id)).toContain(stale)
    // A pinned row spends a slot rather than widening the level, so the
    // panel's height doesn't jump around as you navigate.
    expect(shown).toHaveLength(FAMILY_LEVEL_CAP)
    expect(summary).toEqual({ key: 'root', expanded: false, label: '+25 more' })
  })

  it('keeps recency order rather than floating the pinned row to the top', () => {
    const nodes = rootChildren(40)
    const stale = nodes[nodes.length - 1].session.id
    const { shown } = splitLevel(nodes, 'root', new Set(), new Set([stale]))
    const order = shown.map(n => nodes.findIndex(x => x.session.id === n.session.id))
    expect(order).toEqual([...order].sort((a, b) => a - b))
    expect(shown[shown.length - 1].session.id).toBe(stale)
  })

  it('renders the whole spine of a deep selection, quiet ancestors included', () => {
    // The realistic shape: an orchestrator with many subagents goes quiet
    // while the one you're in does the talking, so its parent ranks last
    // on recency — exactly the row the cap would drop.
    const kids = Array.from({ length: 20 }, (_, i) => child(`mid-${i}`, 59 - i))
    const quietParent = kids[kids.length - 1]
    const selected = makeSession({
      id: 'selected', cwd: '/p', title: 'selected', semantic_agent: true,
      parent_session_id: quietParent.id, created_at: at(0), last_output_at: at(59),
    })
    const snapshot = [root, ...kids, selected]
    const projection = projectFamily(selected, snapshot)
    const pinned = new Set([...projection.ancestors.map(a => a.id), selected.id])
    const { shown } = splitLevel(projection.tree.children, 'root', new Set(), pinned)
    expect(shown.map(n => n.session.id)).toContain(quietParent.id)
    // Without the pin the spine — and with it the selected row — vanishes.
    const unpinned = splitLevel(projection.tree.children, 'root', new Set())
    expect(unpinned.shown.map(n => n.session.id)).not.toContain(quietParent.id)
  })
})
