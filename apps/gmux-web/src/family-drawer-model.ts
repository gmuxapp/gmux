import type { FamilyNode } from './family'

/** When a member last said anything, as a timestamp. */
function activityOf(node: FamilyNode): number {
  return Date.parse(node.session.last_output_at || node.session.created_at) || 0
}

/** How many rows the panel will draw before folding the rest behind
 * per-level `+N more` summaries.
 *
 * A budget for the whole tree, not a cap per level: real families are
 * 70–600 members and deep, so a per-level cap bounds nothing — fifteen
 * rows a level across nine levels is still two hundred rows. Budgeting
 * the tree bounds the panel by construction, whatever shape the family
 * turns out to have.
 *
 * Twenty-five is roughly a screenful; past that you are scrolling a map
 * instead of reading one. */
export const FAMILY_ROW_BUDGET = 25

/** The panel never folds itself below this many lines.
 *
 * Staleness is a cliff by nature: a family whose last burst ended a
 * little over the window shows nothing but its root, which is a true
 * statement about liveness and a useless map for getting anywhere. The
 * floor keeps the most recent members on screen even when all of them
 * are finished work. */
export const FAMILY_ROW_FLOOR = 8

/** How far back from a family's own newest activity a branch may be and
 * still be worth a line, in milliseconds.
 *
 * Measured from the family rather than the clock, because families are
 * bursty: one does all its work inside an afternoon, so an absolute
 * cutoff either keeps every row or empties the panel depending on which
 * side of the line that afternoon fell. Relative to the family's newest
 * output, both a family working now and one that stopped on Tuesday
 * show the same thing — the tail of what it was doing — and a family
 * that has been quiet throughout stays whole, because everything in it
 * is equally recent by its own standard.
 *
 * Six hours is about a working session: long enough to keep the branch
 * you had running before lunch, short enough to fold yesterday's. */
export const FAMILY_STALE_AFTER_MS = 6 * 60 * 60 * 1000

export interface LevelSplit {
  shown: readonly FamilyNode[]
  /** Present iff the level has rows the budget folded away. */
  summary: { key: string; expanded: boolean; label: string } | null
}

/** Choose the rows worth the panel's budget: the most recent members of
 * the family, plus whatever structure it takes to reach them.
 *
 * Recency does the triage that status buckets used to — unread members
 * have recent output by definition, so they surface — and doing it
 * across the whole family rather than per level is what makes an old
 * branch fold: it loses to newer work everywhere else in the tree, not
 * merely to its own siblings.
 *
 * Deliberately *not* an age threshold. Families turn out to be bursty:
 * one does all its work inside a couple of hours, so a cutoff anywhere
 * near that burst either keeps every row or hides every row, and the
 * two families on either side of the line look nothing alike. A rank
 * cuts the same noise without a number that means something different
 * to every family.
 *
 * `pinned` is your own spine, root to selection; it is seeded before
 * anything competes for the budget, so the row you are standing on and
 * the path that explains it are never the rows that get folded.
 *
 * The budget counts *lines*, not members: a `+N more` occupies the
 * screen exactly like the row it replaces, so folding is only ever a
 * saving when it hides two rows or more. Charging the fold makes that
 * arithmetic automatic — a member that completes its level and has no
 * children of its own is free, because drawing it retires the summary
 * that stood in for it. */
export function visibleFamilyRows(
  tree: FamilyNode,
  pinned: ReadonlySet<string> = new Set(),
  budget: number = FAMILY_ROW_BUDGET,
  staleAfterMs: number = FAMILY_STALE_AFTER_MS,
  floor: number = FAMILY_ROW_FLOOR,
): ReadonlySet<string> {
  const parentOf = new Map<string, string>()
  const nodeById = new Map<string, FamilyNode>()
  const flat: FamilyNode[] = []
  /** Newest output anywhere in a node's subtree: what decides whether a
   * branch is stale. Judging a parent by its own output alone would
   * fold the quiet orchestrator whose subagent is mid-sentence. */
  const subtreeNewest = new Map<string, number>()
  const walk = (node: FamilyNode): number => {
    flat.push(node)
    nodeById.set(node.session.id, node)
    let newest = activityOf(node)
    for (const child of node.children) {
      parentOf.set(child.session.id, node.session.id)
      newest = Math.max(newest, walk(child))
    }
    subtreeNewest.set(node.session.id, newest)
    return newest
  }
  walk(tree)
  const familyNewest = subtreeNewest.get(tree.session.id) ?? 0
  const staleBefore = familyNewest - staleAfterMs

  const visible = new Set<string>()
  const shownKids = new Map<string, number>()
  /** Lines drawn so far: visible rows plus one per folded level. */
  let lines = 0
  const folds = (id: string) =>
    (nodeById.get(id)?.children.length ?? 0) > (shownKids.get(id) ?? 0) ? 1 : 0

  /** Draw one row whose parent is already drawn, keeping the line count
   * honest: it may retire its parent's summary and may raise one of its
   * own. */
  const admit = (id: string) => {
    const parent = parentOf.get(id)
    if (parent !== undefined) {
      const before = folds(parent)
      shownKids.set(parent, (shownKids.get(parent) ?? 0) + 1)
      lines -= before - folds(parent)
    }
    visible.add(id)
    lines += 1 + folds(id)
  }
  /** Exact inverse of `admit`, in reverse order of admission. */
  const withdraw = (id: string) => {
    visible.delete(id)
    lines -= 1 + folds(id)
    const parent = parentOf.get(id)
    if (parent === undefined) return
    const before = folds(parent)
    shownKids.set(parent, (shownKids.get(parent) ?? 0) - 1)
    lines += folds(parent) - before
  }

  // The root, then your own spine, before anything competes for a line.
  admit(tree.session.id)
  for (const node of flat) {
    if (node.session.id !== tree.session.id && pinned.has(node.session.id)) admit(node.session.id)
  }

  // Levels arrive in recency order (`projectFamily` sorts them), so a
  // depth-first flatten is already ordered by recency within each
  // branch; sort across branches to rank the family as a whole.
  const byRecency = [...flat].sort((a, b) =>
    activityOf(b) - activityOf(a) || a.session.id.localeCompare(b.session.id))

  /** `triage` is the ordinary pass, where staleness applies. The top-up
   * that follows drops it: it runs only when the panel would otherwise
   * be emptier than a glance, and at that point a finished branch beats
   * a blank panel. */
  const fill = (ceiling: number, triage: boolean) => {
    for (const node of byRecency) {
      const id = node.session.id
      if (visible.has(id)) continue
      // Stale branches fold whole rather than spending the budget on the
      // tail of finished work. They stay reachable behind `+N more`:
      // this is the panel declining to volunteer them, not hiding them.
      if (triage && (subtreeNewest.get(id) ?? 0) < staleBefore) continue
      // A row costs its own line plus every ancestor still missing: an
      // unreachable row would be a lie about the shape of the family.
      const spine: string[] = []
      for (let cursor: string | undefined = id; cursor && !visible.has(cursor); cursor = parentOf.get(cursor)) {
        spine.push(cursor)
      }
      spine.reverse()
      // Costing a spine exactly means walking it: an ancestor's summary
      // is retired by the very child that follows it. So admit, then
      // take it back if the whole path didn't fit.
      for (const ancestor of spine) admit(ancestor)
      if (lines > ceiling) {
        for (const ancestor of [...spine].reverse()) withdraw(ancestor)
      }
    }
  }

  fill(budget, true)
  // Then top the panel back up to the floor with whatever is left, most
  // recent first — a family that finished yesterday still gets a map.
  if (lines < floor) fill(Math.min(floor, budget), false)
  return visible
}

/** Apply the budget to one children level: what the panel renders.
 *
 * Expansion is keyed per parent id and two-state — expanding shows the
 * whole level plus `show fewer`, never an incremental ladder. Rows keep
 * the recency order they arrived in; the budget only decides which of
 * them survive. */
export function splitLevel(
  nodes: readonly FamilyNode[],
  parentId: string,
  expanded: ReadonlySet<string>,
  visible: ReadonlySet<string>,
): LevelSplit {
  if (expanded.has(parentId)) {
    return { shown: nodes, summary: { key: parentId, expanded: true, label: 'show fewer' } }
  }
  const shown = nodes.filter(node => visible.has(node.session.id))
  if (shown.length === nodes.length) return { shown, summary: null }
  return {
    shown,
    summary: { key: parentId, expanded: false, label: `+${nodes.length - shown.length} more` },
  }
}
