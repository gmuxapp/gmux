import { describe, expect, it } from 'vitest'
import { splitGroup, syncDrawer, toggleDrawerGroup, groupKey } from './family-drawer-model'
import type { Session } from './types'
import { makeSession } from './test-helpers'

const at = (minute: number) => `2026-08-04T10:${String(minute).padStart(2, '0')}:00Z`
const root = makeSession({
  id: 'root', cwd: '/p', title: 'root', semantic_agent: true,
  created_at: at(0), last_output_at: at(1),
})
const child = (id: string, extra: Partial<Session> = {}) => makeSession({
  id, cwd: '/p', title: id, parent_session_id: 'root', semantic_agent: true,
  created_at: at(0), last_output_at: at(1), ...extra,
})
const procChild = (id: string, extra: Partial<Session> = {}) => makeSession({
  id, cwd: '/p', title: id, parent_session_id: 'root', adapter: 'shell',
  created_at: at(0), last_output_at: at(1), ...extra,
})

/** The root-selected drawer has one top-level group containing the root
 * node; its children groups are what the drawer body renders. */
function childGroups(model: ReturnType<typeof syncDrawer>) {
  return model.projection.groups[0].nodes[0].groups
}

describe('drawer model: frozen ordering with explicit refresh', () => {
  it('ignores ordinary live updates but re-projects on group toggle', () => {
    const mover = child('mover')
    const before = [root, mover, child('idle-friend')]
    const model = syncDrawer(null, root, before)
    expect(childGroups(model).map(g => g.bucket)).toEqual(['idle'])

    // Live update: `mover` starts working. An ordinary re-render syncs
    // against the new snapshot and MUST return the same frozen model —
    // no auto-reorder under the cursor.
    const after = [root, { ...mover, status: { active: true } }, child('idle-friend')]
    const synced = syncDrawer(model, root, after)
    expect(synced).toBe(model)
    expect(childGroups(synced).map(g => g.bucket)).toEqual(['idle'])

    // Explicit user action: toggling any group re-projects from current
    // facts — `mover` now leads a working group.
    const toggled = toggleDrawerGroup(synced, root, after, groupKey('root', 'idle'))
    expect(toggled).not.toBe(synced)
    expect(childGroups(toggled).map(g => g.bucket)).toEqual(['working', 'idle'])
    expect(childGroups(toggled)[0].nodes.map(n => n.session.id)).toEqual(['mover'])
  })

  it('tracks expansion per (parent, bucket) across toggles and selection changes', () => {
    const snapshot = [root, ...Array.from({ length: 5 }, (_, i) => child(`dead-${i}`, { alive: false }))]
    let model = syncDrawer(null, root, snapshot)
    const key = groupKey('root', 'finished')
    model = toggleDrawerGroup(model, root, snapshot, key)
    expect(model.expanded.has(key)).toBe(true)
    // Selecting another family member re-projects but keeps expansion.
    const other = snapshot[1]
    const reselected = syncDrawer(model, other, snapshot)
    expect(reselected).not.toBe(model)
    expect(reselected.expanded.has(key)).toBe(true)
    // Toggling again collapses back (two-state, no increments).
    const collapsed = toggleDrawerGroup(reselected, other, snapshot, key)
    expect(collapsed.expanded.has(key)).toBe(false)
  })
})

describe('drawer model: rendered caps and summary rows', () => {
  it('applies the 20/10/3 caps to what is actually shown', () => {
    const snapshot = [
      root,
      ...Array.from({ length: 25 }, (_, i) => child(`work-${i}`, { status: { active: true } })),
      ...Array.from({ length: 14 }, (_, i) => child(`idle-${i}`)),
      ...Array.from({ length: 10 }, (_, i) => child(`dead-${i}`, { alive: false })),
    ]
    const model = syncDrawer(null, root, snapshot)
    const groups = childGroups(model)
    expect(groups.map(g => g.bucket)).toEqual(['working', 'idle', 'finished'])
    const splits = groups.map(g => splitGroup(g, 'root', model.expanded))
    expect(splits.map(s => s.shown.length)).toEqual([20, 10, 3])
    expect(splits.map(s => s.summary?.label)).toEqual(['+5 more working', '+4 idle', '+7 finished'])
  })

  it('never caps attention', () => {
    const snapshot = [root, ...Array.from({ length: 30 }, (_, i) => child(`u-${i}`, { unread: true }))]
    const model = syncDrawer(null, root, snapshot)
    const [attention] = childGroups(model)
    expect(attention.bucket).toBe('attention')
    const split = splitGroup(attention, 'root', model.expanded)
    expect(split.shown.length).toBe(30)
    expect(split.summary).toBeNull()
  })

  it('shows everything plus "show fewer" once expanded', () => {
    const snapshot = [root, ...Array.from({ length: 9 }, (_, i) => child(`dead-${i}`, { alive: false }))]
    let model = syncDrawer(null, root, snapshot)
    const key = groupKey('root', 'finished')
    model = toggleDrawerGroup(model, root, snapshot, key)
    const split = splitGroup(childGroups(model)[0], 'root', model.expanded)
    expect(split.shown.length).toBe(9)
    expect(split.summary).toEqual({ key, expanded: true, label: 'show fewer' })
  })

  it('words mixed hidden agents and processes as two facts', () => {
    // Agents sort before processes within a bucket, so the 3 visible
    // finished rows are agents; hidden = 4 agents + 2 processes.
    const snapshot = [
      root,
      ...Array.from({ length: 7 }, (_, i) => child(`dead-${i}`, { alive: false })),
      ...Array.from({ length: 2 }, (_, i) => procChild(`proc-${i}`, { alive: false })),
    ]
    const model = syncDrawer(null, root, snapshot)
    const split = splitGroup(childGroups(model)[0], 'root', model.expanded)
    expect(split.shown.every(n => !n.process)).toBe(true)
    expect(split.summary?.label).toBe('+4 finished · 2 processes done')
  })

  it('gives process-only remainders their own noun', () => {
    const done = [root, ...Array.from({ length: 5 }, (_, i) => procChild(`proc-${i}`, { alive: false }))]
    const doneModel = syncDrawer(null, root, done)
    expect(splitGroup(childGroups(doneModel)[0], 'root', doneModel.expanded).summary?.label)
      .toBe('+2 processes done')

    const running = [root, ...Array.from({ length: 12 }, (_, i) => procChild(`proc-${i}`))]
    const runningModel = syncDrawer(null, root, running)
    // Live shells report no agent status; they land in idle (cap 10).
    expect(splitGroup(childGroups(runningModel)[0], 'root', runningModel.expanded).summary?.label)
      .toBe('+2 processes')
  })
})
