import { describe, expect, it } from 'vitest'
import {
  FAMILY_PROCESS_STALE_AFTER_MS, FAMILY_ROW_BUDGET, FAMILY_ROW_FLOOR, FAMILY_STALE_AFTER_MS,
  splitLevel, visibleFamilyRows,
} from './family-drawer-model'
import { projectFamily, type FamilyNode } from './family'
import { makeSession } from './test-helpers'

const at = (minute: number) => `2026-08-04T10:${String(minute).padStart(2, '0')}:00Z`
const root = makeSession({
  id: 'root', cwd: '/p', title: 'root', semantic_agent: true,
  created_at: at(0), last_output_at: at(1),
})
const child = (id: string, minute: number, parent = 'root') => makeSession({
  id, cwd: '/p', title: id, parent_session_id: parent, semantic_agent: true,
  created_at: at(0), last_output_at: at(minute),
})

function rootChildren(count: number) {
  const snapshot = [root, ...Array.from({ length: count }, (_, i) => child(`c-${i}`, 59 - i))]
  return projectFamily(root, snapshot).tree
}

/** Every line the panel would draw, walking the budget the way the
 * component does. Summary rows are lines too — that is the whole point
 * of the budget — so they are counted here as `+N more`. */
function renderedLines(tree: FamilyNode, visible: ReadonlySet<string>): string[] {
  const out: string[] = []
  const walk = (node: FamilyNode) => {
    out.push(node.session.id)
    const { shown, summary } = splitLevel(node.children, node.session.id, new Set(), visible)
    for (const kid of shown) walk(kid)
    if (summary) out.push(`${node.session.id}:${summary.label}`)
  }
  walk(tree)
  return out
}

describe('the panel draws at most a screenful, whatever the family looks like', () => {
  it('shows a small family whole, with no summary rows', () => {
    const tree = rootChildren(6)
    const visible = visibleFamilyRows(tree)
    expect(renderedLines(tree, visible)).toHaveLength(7)
    expect(splitLevel(tree.children, 'root', new Set(), visible).summary).toBeNull()
  })

  it('bounds a wide family and folds the rest into "+N more"', () => {
    const tree = rootChildren(40)
    const visible = visibleFamilyRows(tree)
    const rows = renderedLines(tree, visible)
    expect(rows).toHaveLength(FAMILY_ROW_BUDGET)
    const { shown, summary } = splitLevel(tree.children, 'root', new Set(), visible)
    expect(summary).toEqual({ key: 'root', expanded: false, label: `+${40 - shown.length} more` })
  })

  it('bounds a deep family too, where a per-level cap bounded nothing', () => {
    // A chain nine deep with three children each: the shape that used to
    // render two hundred rows under a fifteen-per-level cap.
    const snapshot = [root]
    let parents = ['root']
    for (let depth = 0; depth < 9; depth++) {
      const next: string[] = []
      for (const parent of parents) {
        for (let i = 0; i < 3; i++) {
          const id = `d${depth}-${parent}-${i}`
          snapshot.push(child(id, 59 - depth, parent))
          next.push(id)
        }
      }
      parents = next.slice(0, 2) // keep the fixture from exploding
    }
    const tree = projectFamily(root, snapshot).tree
    expect(renderedLines(tree, visibleFamilyRows(tree)).length).toBeLessThanOrEqual(FAMILY_ROW_BUDGET)
  })

  it('ranks across branches, so a stale branch loses to newer work elsewhere', () => {
    // Two only-children: one recent, one stale. A per-level cap never
    // folded either — each was the sole row of its level.
    const snapshot = [
      root,
      child('busy', 59), child('busy-kid', 58, 'busy'),
      child('stale', 5), child('stale-kid', 4, 'stale'),
    ]
    const tree = projectFamily(root, snapshot).tree
    const rows = renderedLines(tree, visibleFamilyRows(tree, new Set(), 4))
    expect(rows).toContain('busy')
    expect(rows).toContain('busy-kid')
    // The stale branch folds whole, which a per-level cap could never
    // do: each of these is an only child, alone on its level, and a cap
    // per level never folds a level of one.
    expect(rows).not.toContain('stale')
    expect(rows).not.toContain('stale-kid')
    expect(rows).toContain('root:+1 more')
    expect(rows).toHaveLength(4)
  })
})

describe('the row you are on is never behind the fold', () => {
  it('renders the whole spine of a deep selection, quiet ancestors included', () => {
    // The realistic shape: an orchestrator with many subagents goes quiet
    // while the one you're in does the talking, so its parent ranks last
    // on recency — exactly the row a budget would spend elsewhere.
    const kids = Array.from({ length: 40 }, (_, i) => child(`mid-${i}`, 59 - i))
    const quietParent = kids[kids.length - 1]
    // Quiet itself, too: you're reading it, not running it. Nothing
    // about this branch earns its way onto the screen on recency.
    const selected = makeSession({
      id: 'selected', cwd: '/p', title: 'selected', semantic_agent: true,
      parent_session_id: quietParent.id, created_at: at(0), last_output_at: at(3),
    })
    const projection = projectFamily(selected, [root, ...kids, selected])
    const pinned = new Set([...projection.ancestors.map(a => a.id), selected.id])
    const rows = renderedLines(projection.tree, visibleFamilyRows(projection.tree, pinned))
    expect(rows).toContain(quietParent.id)
    expect(rows).toContain('selected')
    // Without the pin, the spine — and with it the selected row — goes.
    const unpinned = renderedLines(projection.tree, visibleFamilyRows(projection.tree))
    expect(unpinned).not.toContain(quietParent.id)
  })

  it('spends budget on the spine rather than widening past it', () => {
    const kids = Array.from({ length: 40 }, (_, i) => child(`mid-${i}`, 59 - i))
    const quiet = kids[kids.length - 1]
    const selected = makeSession({
      id: 'selected', cwd: '/p', title: 'selected', semantic_agent: true,
      parent_session_id: quiet.id, created_at: at(0), last_output_at: at(2),
    })
    const projection = projectFamily(selected, [root, ...kids, selected])
    const pinned = new Set([...projection.ancestors.map(a => a.id), selected.id])
    const visible = visibleFamilyRows(projection.tree, pinned)
    expect(renderedLines(projection.tree, visible)).toHaveLength(FAMILY_ROW_BUDGET)
  })
})

describe('level rows', () => {
  it('keeps recency order rather than floating kept rows to the top', () => {
    const tree = rootChildren(40)
    const nodes = tree.children
    const visible = visibleFamilyRows(tree, new Set([nodes[nodes.length - 1].session.id]))
    const { shown } = splitLevel(nodes, 'root', new Set(), visible)
    const order = shown.map(n => nodes.findIndex(x => x.session.id === n.session.id))
    expect(order).toEqual([...order].sort((a, b) => a - b))
    expect(shown[shown.length - 1].session.id).toBe(nodes[nodes.length - 1].session.id)
  })

  it('expands two-state per parent: everything plus "show fewer"', () => {
    const tree = rootChildren(40)
    const visible = visibleFamilyRows(tree)
    const { shown, summary } = splitLevel(tree.children, 'root', new Set(['root']), visible)
    expect(shown).toHaveLength(40)
    expect(summary).toEqual({ key: 'root', expanded: true, label: 'show fewer' })
    // Another parent's expansion state does not leak into this level.
    expect(splitLevel(tree.children, 'root', new Set(['other-parent']), visible).shown.length)
      .toBeLessThan(40)
  })
})

describe('finished work folds itself away', () => {
  const day = (hour: number, id: string, parent?: string) => makeSession({
    id, cwd: '/p', title: id, parent_session_id: parent, semantic_agent: true,
    created_at: '2026-08-04T00:00:00Z',
    last_output_at: new Date(Date.parse('2026-08-04T20:00:00Z') - hour * 3600_000).toISOString(),
  })

  it('folds a branch that finished long before the rest of the family', () => {
    // Plenty of budget left; the branch is dropped for being over, not
    // for being in the way. Enough live siblings to clear the floor, so
    // the fold is staleness talking and not the top-up.
    const live = Array.from({ length: 10 }, (_, i) => day(0.1 * i, `fresh-${i}`, 'root'))
    // Two of them: folding one lone leaf would save no line, so the
    // panel would rightly draw it instead of counting it.
    const snapshot = [day(0, 'root'), ...live, day(20, 'yesterday', 'root'), day(21, 'earlier', 'root')]
    const tree = projectFamily(snapshot[0], snapshot).tree
    const rows = renderedLines(tree, visibleFamilyRows(tree))
    expect(rows).toContain('fresh-0')
    expect(rows).not.toContain('yesterday')
    expect(rows).toContain('root:+2 more')
  })

  it('keeps an old family whole, because it is only old by the clock', () => {
    // Everything stopped days ago. Staleness is measured from the
    // family's own newest output, so nothing here is stale relative to
    // anything else and the panel still shows the family.
    const snapshot = [day(72, 'root'), day(72.2, 'a', 'root'), day(73, 'b', 'root')]
    const tree = projectFamily(snapshot[0], snapshot).tree
    const rows = renderedLines(tree, visibleFamilyRows(tree))
    expect(rows).toEqual(expect.arrayContaining(['root', 'a', 'b']))
  })

  it('judges a branch by its liveliest member, not by its head', () => {
    // The orchestrator went quiet hours ago; its subagent is mid-
    // sentence. Judging the parent on its own output would fold both.
    const snapshot = [day(0, 'root'), day(20, 'quiet', 'root'), day(0.1, 'talking', 'quiet')]
    const tree = projectFamily(snapshot[0], snapshot).tree
    const rows = renderedLines(tree, visibleFamilyRows(tree))
    expect(rows).toEqual(expect.arrayContaining(['quiet', 'talking']))
  })

  it('keeps your own spine however stale it is', () => {
    const snapshot = [day(0, 'root'), day(40, 'old', 'root'), day(41, 'older', 'old')]
    const projection = projectFamily(snapshot[2], snapshot)
    const pinned = new Set([...projection.ancestors.map(a => a.id), 'older'])
    const rows = renderedLines(projection.tree, visibleFamilyRows(projection.tree, pinned))
    expect(rows).toEqual(expect.arrayContaining(['old', 'older']))
  })

  it('folds nothing when the window is wider than the family', () => {
    const snapshot = [day(0, 'root'), day(5, 'a', 'root'), day(20, 'b', 'root')]
    const tree = projectFamily(snapshot[0], snapshot).tree
    const wide = visibleFamilyRows(tree, new Set(), FAMILY_ROW_BUDGET, 48 * 3600_000)
    expect(renderedLines(tree, wide)).toEqual(expect.arrayContaining(['a', 'b']))
    // …and the default window is narrower than that, or the constant is
    // doing nothing.
    expect(FAMILY_STALE_AFTER_MS).toBeLessThan(48 * 3600_000)
  })
})

describe('the floor', () => {
  const hoursAgo = (hour: number, id: string, parent?: string) => makeSession({
    id, cwd: '/p', title: id, parent_session_id: parent, semantic_agent: true,
    created_at: '2026-08-04T00:00:00Z',
    last_output_at: new Date(Date.parse('2026-08-04T20:00:00Z') - hour * 3600_000).toISOString(),
  })

  it('still maps a family whose work all finished before the window', () => {
    // The root spoke recently; everything it did is hours older. Pure
    // staleness would leave one row and a "+N more" — true, and no use
    // to anyone trying to get back to a session.
    const kids = Array.from({ length: 12 }, (_, i) => hoursAgo(10 + i * 0.1, `k-${i}`, 'root'))
    const snapshot = [hoursAgo(0, 'root'), ...kids]
    const tree = projectFamily(snapshot[0], snapshot).tree
    const rows = renderedLines(tree, visibleFamilyRows(tree))
    expect(rows.length).toBeGreaterThanOrEqual(FAMILY_ROW_FLOOR)
    // Topped up most-recent-first, like everything else here.
    expect(rows).toContain('k-0')
    expect(rows).not.toContain('k-11')
  })

  it('does not pad a family that is simply small', () => {
    const snapshot = [hoursAgo(0, 'root'), hoursAgo(0.1, 'only', 'root')]
    const tree = projectFamily(snapshot[0], snapshot).tree
    expect(renderedLines(tree, visibleFamilyRows(tree))).toEqual(['root', 'only'])
  })

  it('never lifts the panel past the budget', () => {
    const kids = Array.from({ length: 60 }, (_, i) => hoursAgo(10 + i * 0.01, `k-${i}`, 'root'))
    const tree = projectFamily(kids[0], [hoursAgo(0, 'root'), ...kids]).tree
    const rows = renderedLines(tree, visibleFamilyRows(tree, new Set(), 5, FAMILY_STALE_AFTER_MS, 40))
    expect(rows.length).toBeLessThanOrEqual(5)
  })
})

describe("an agent's commands are a log, not a level", () => {
  const ago = (minutes: number, id: string, parent: string | undefined, process: boolean) => makeSession({
    id, cwd: '/p', title: id, parent_session_id: parent, semantic_agent: !process,
    created_at: '2026-08-04T00:00:00Z',
    last_output_at: new Date(Date.parse('2026-08-04T20:00:00Z') - minutes * 60_000).toISOString(),
  })

  it('shows an agent the command it is running, not the ten before it', () => {
    // The observed shape: one agent with a wall of near-identical shell
    // lines, drowning every other branch in the family.
    const shells = Array.from({ length: 10 }, (_, i) => ago(2 + i, `sh-${i}`, 'busy', true))
    const others = Array.from({ length: 5 }, (_, i) => ago(3 + i, `agent-${i}`, 'root', false))
    const snapshot = [ago(0, 'root', undefined, false), ago(1, 'busy', 'root', false), ...shells, ...others]
    const tree = projectFamily(snapshot[0], snapshot).tree
    const rows = renderedLines(tree, visibleFamilyRows(tree))
    expect(rows).toContain('sh-0')
    expect(rows.filter(r => r.startsWith('sh-') && !r.includes(':'))).toHaveLength(1)
    // The freed lines go to the rest of the family, which is the point.
    expect(rows).toEqual(expect.arrayContaining(['agent-0', 'agent-4']))
    // Nothing is lost: the remainder is one click away on its own level.
    expect(rows).toContain('busy:+9 more')
  })

  it('gives each agent its own command', () => {
    const snapshot = [
      ago(0, 'root', undefined, false),
      ago(1, 'a', 'root', false), ago(2, 'a-sh', 'a', true),
      ago(3, 'b', 'root', false), ago(4, 'b-sh', 'b', true),
    ]
    const tree = projectFamily(snapshot[0], snapshot).tree
    expect(renderedLines(tree, visibleFamilyRows(tree))).toEqual(
      expect.arrayContaining(['a-sh', 'b-sh']),
    )
  })

  it('drops a command that stopped talking, well before an agent would', () => {
    const stale = FAMILY_PROCESS_STALE_AFTER_MS / 60_000 + 30
    // Enough live siblings that the floor's top-up never runs; this is
    // triage talking, not an empty panel being padded.
    const live = Array.from({ length: 10 }, (_, i) => ago(1 + i * 0.1, `live-${i}`, 'root', false))
    const snapshot = [
      ago(0, 'root', undefined, false),
      ...live,
      ago(stale, 'quiet-agent', 'root', false),
      // Two stale commands, so folding them is a real saving; one alone
      // would cost the same line folded or drawn, and be drawn.
      ago(stale, 'old-command', 'root', true),
      ago(stale + 1, 'older-command', 'root', true),
    ]
    const tree = projectFamily(snapshot[0], snapshot).tree
    const rows = renderedLines(tree, visibleFamilyRows(tree))
    // Same age, different verdict: the agent is still worth a line at
    // ninety minutes, the finished command is not.
    expect(FAMILY_PROCESS_STALE_AFTER_MS).toBeLessThan(FAMILY_STALE_AFTER_MS)
    expect(rows).toContain('quiet-agent')
    expect(rows).not.toContain('old-command')
  })

  it('still maps a family that is nothing but commands', () => {
    // 248 processes to 8 agents is a real family here. One command per
    // agent would leave this one two rows deep, so the floor's top-up
    // drops that rule too: a command log beats a blank panel.
    const shells = Array.from({ length: 30 }, (_, i) => ago(2 + i * 0.1, `sh-${i}`, 'root', true))
    const tree = projectFamily(shells[0], [ago(0, 'root', undefined, false), ...shells]).tree
    expect(renderedLines(tree, visibleFamilyRows(tree)).length).toBeGreaterThanOrEqual(FAMILY_ROW_FLOOR)
  })
})

describe('a summary that saves no space is not drawn', () => {
  const ago = (minutes: number, id: string, parent: string | undefined, process = false) => makeSession({
    id, cwd: '/p', title: id, parent_session_id: parent, semantic_agent: !process,
    created_at: '2026-08-04T00:00:00Z',
    last_output_at: new Date(Date.parse('2026-08-04T20:00:00Z') - minutes * 60_000).toISOString(),
  })

  it('names the last hidden command instead of counting it', () => {
    // Two commands under one agent: the one-command rule would hide the
    // second behind a "+1 more" that occupies the very line the command
    // would have — costing the same and saying less.
    const snapshot = [
      ago(0, 'root', undefined), ago(1, 'agent', 'root'),
      ago(2, 'sh-a', 'agent', true), ago(3, 'sh-b', 'agent', true),
    ]
    const tree = projectFamily(snapshot[0], snapshot).tree
    const rows = renderedLines(tree, visibleFamilyRows(tree))
    expect(rows).toEqual(['root', 'agent', 'sh-a', 'sh-b'])
    expect(rows.some(r => r.includes('more'))).toBe(false)
  })

  it('still counts a hidden subtree, which is not the same bargain', () => {
    // The lone hidden row here has children of its own, so drawing it
    // raises a summary underneath: two lines for what a summary says in
    // one. The count stays.
    const stale = FAMILY_STALE_AFTER_MS / 60_000 + 60
    const live = Array.from({ length: 10 }, (_, i) => ago(1 + i * 0.1, `live-${i}`, 'root'))
    const snapshot = [
      ago(0, 'root', undefined), ...live,
      ago(stale, 'old', 'root'), ago(stale + 1, 'older', 'old'), ago(stale + 2, 'oldest', 'old'),
    ]
    const tree = projectFamily(snapshot[0], snapshot).tree
    const rows = renderedLines(tree, visibleFamilyRows(tree))
    expect(rows).toContain('root:+1 more')
    expect(rows).not.toContain('old')
  })
})
