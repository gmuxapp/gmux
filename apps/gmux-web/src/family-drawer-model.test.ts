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
  return projectFamily(root, snapshot).siblingTrees[0].children
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
