import { familyStateOf, isProcessSession, type FamilyNode, type FamilyState } from './family'

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

/** The same, for a process rather than an agent.
 *
 * A process is a command an agent ran, and a command is only interesting
 * while it is the thing the agent is doing. An hour on from its last
 * output it is a line in a log, and the panel is not a log — which is
 * why processes also get only one row per agent (see `visibleFamilyRows`)
 * rather than one each. */
export const FAMILY_PROCESS_STALE_AFTER_MS = 60 * 60 * 1000

export interface LevelSplit {
  shown: readonly FamilyNode[]
  /** Present iff the level has rows the budget folded away. */
  /** The fold control's text, or null when the level has nothing
   * folded. Whether it's expanded and which level it toggles are the
   * caller's own `expanded` and `parentId` — restating them here only
   * created two ways to know one thing. */
  summary: string | null
}

export interface FamilyView {
  /** Rows to draw. */
  readonly visible: ReadonlySet<string>
  /** Rows that were allowed to compete for the budget — everyone when
   * nothing is filtered, and only the matches (plus the structure that
   * reaches them) when something is. `+N more` counts against this, so
   * a filter's folds never offer to expand what the filter excluded. */
  readonly universe: ReadonlySet<string>
}

export interface FamilyViewOptions {
  /** Your own spine, root to selection: never folded, never filtered
   * out. You are standing on it, and a map that omits where you are is
   * a map of somewhere else. */
  pinned?: ReadonlySet<string>
  /** Show only members in this state, or everyone. */
  filter?: FamilyState | null
  budget?: number
  floor?: number
  staleAfterMs?: number
  processStaleAfterMs?: number
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
  options: FamilyViewOptions = {},
): FamilyView {
  const {
    pinned = new Set<string>(),
    filter = null,
    budget = FAMILY_ROW_BUDGET,
    floor = FAMILY_ROW_FLOOR,
    staleAfterMs = FAMILY_STALE_AFTER_MS,
    processStaleAfterMs = FAMILY_PROCESS_STALE_AFTER_MS,
  } = options
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
  const processStaleBefore = familyNewest - processStaleAfterMs

  // A filter answers a question the panel's own triage was guessing at,
  // so the guessing stops: asked for the errors, you get every error,
  // stale or not, however many commands one agent ran. What survives is
  // the line budget, because a filter can still match three hundred
  // members.
  //
  // Structure comes along: an unmatched ancestor is how a match is
  // reachable, and a row you cannot see the parent of is a claim about
  // the family that the panel can't back up.
  const universe = new Set<string>([tree.session.id, ...pinned])
  if (filter) {
    for (const node of flat) {
      if (familyStateOf(node.session) !== filter) continue
      for (let id: string | undefined = node.session.id; id && !universe.has(id); id = parentOf.get(id)) {
        universe.add(id)
      }
    }
  } else {
    for (const node of flat) universe.add(node.session.id)
  }

  /** How many of a node's children could be drawn at all. Under a
   * filter this is not `children.length`: a level whose excluded
   * children are all hidden renders no summary (see `splitLevel`), so
   * charging a fold line for them would spend budget on a row that
   * never appears and push real matches out of the panel. Computed
   * once — recomputing it per candidate made the free-leaf pass
   * quadratic in a wide level. */
  const eligibleKids = new Map<string, number>()
  for (const node of flat) {
    eligibleKids.set(node.session.id,
      node.children.reduce((n, child) => n + (universe.has(child.session.id) ? 1 : 0), 0))
  }

  const visible = new Set<string>()
  const shownKids = new Map<string, number>()
  /** Commands currently drawn per agent, so the rest of an agent's stay
   * in the fold rather than pushing other branches off the panel. A
   * count, not a set: a speculative admission that the budget rejects
   * must leave no trace, or an agent with no visible command at all
   * would go on suppressing its own siblings. */
  const shownProcs = new Map<string, number>()
  /** Lines drawn so far: visible rows plus one per folded level. */
  let lines = 0
  const folds = (id: string) =>
    (eligibleKids.get(id) ?? 0) > (shownKids.get(id) ?? 0) ? 1 : 0

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
    // Every id here came from the walk, so the node is present: no
    // fallback, which would have quietly typed a process as an agent.
    if (parent !== undefined && isProcessSession(nodeById.get(id)!.session)) {
      shownProcs.set(parent, (shownProcs.get(parent) ?? 0) + 1)
    }
    lines += 1 + folds(id)
  }
  /** Exact inverse of `admit`, in reverse order of admission. */
  const withdraw = (id: string) => {
    visible.delete(id)
    lines -= 1 + folds(id)
    const parent = parentOf.get(id)
    if (parent === undefined) return
    if (isProcessSession(nodeById.get(id)!.session)) {
      shownProcs.set(parent, (shownProcs.get(parent) ?? 0) - 1)
    }
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

  /** `triage` is the ordinary pass: staleness applies, and an agent
   * shows one command. The top-up that follows drops both rules — it
   * runs only when the panel would otherwise be emptier than a glance,
   * and at that point a stale command beats a blank panel. */
  const fill = (ceiling: number, triage: boolean) => {
    for (const node of byRecency) {
      const id = node.session.id
      if (visible.has(id) || !universe.has(id)) continue
      // Stale branches fold whole rather than spending the budget on the
      // tail of finished work. They stay reachable behind `+N more`:
      // this is the panel declining to volunteer them, not hiding them.
      if (triage && (subtreeNewest.get(id) ?? 0) < staleBefore) continue
      // An agent's commands are a log, and the panel shows the top of
      // it: the one it is running now, while it is still running it.
      // The rest stay in the level's fold, one click away, instead of
      // spending a whole panel on one agent's shell history — the shape
      // of a family that is 248 processes to 8 agents.
      if (triage && isProcessSession(node.session)) {
        const parent = parentOf.get(id)
        if (parent !== undefined && (shownProcs.get(parent) ?? 0) > 0) continue
        if ((subtreeNewest.get(id) ?? 0) < processStaleBefore) continue
      }
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

  // Triage — staleness and one-command-per-agent — is the panel guessing
  // what you want to see. A filter is you saying it, so triage turns off
  // wholesale: asked for the errors, you get the two-day-old one on a
  // branch nothing else has touched, not a fold where it should be.
  fill(budget, !filter)
  // Then top the panel back up to the floor with whatever is left, most
  // recent first — a family that finished yesterday still gets a map.
  // Not while filtering: there the panel was asked a question, and
  // padding the answer with rows that don't match is answering a
  // different one.
  if (!filter && lines < floor) fill(Math.min(floor, budget), false)

  // Finally, draw every row that costs nothing: the last hidden leaf of
  // a level, whose summary occupies exactly the line the row would. No
  // rule above is worth a `+1 more` that saves no space and names
  // nobody, and admitting one can free the next, so this runs to a
  // fixed point. Line count cannot rise here — each of these retires
  // the summary it replaces.
  for (let changed = true; changed;) {
    changed = false
    for (const node of byRecency) {
      const id = node.session.id
      // "Leaf" means leaf *of the view*: under a filter, a node whose
      // children were all excluded raises no summary of its own, so it
      // is exactly as free as a childless one.
      if (visible.has(id) || (eligibleKids.get(id) ?? 0) > 0 || !universe.has(id)) continue
      const parent = parentOf.get(id)
      if (parent === undefined || !visible.has(parent)) continue
      if ((shownKids.get(parent) ?? 0) + 1 !== (eligibleKids.get(parent) ?? 0)) continue
      admit(id)
      changed = true
    }
  }
  return { visible, universe }
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
  view: FamilyView,
): LevelSplit {
  // `+N more` and `show fewer` both count against the universe, not the
  // level: under a filter, the rows it excluded were never on offer, so
  // counting them would advertise an expansion that contradicts what
  // was asked for.
  const eligible = nodes.filter(node => view.universe.has(node.session.id))
  const shown = eligible.filter(node => view.visible.has(node.session.id))
  const folded = eligible.length - shown.length
  // Expanded levels show everything eligible — but only offer `show
  // fewer` if collapsing would actually hide something. A filter
  // applied after an expansion can shrink a level to nothing folded,
  // and a control that visibly does nothing is worse than no control.
  //
  // Safe only because `visibleFamilyRows` knows nothing about
  // `expanded`: `folded` is therefore invariant under expanding, so
  // this can never strand a reader inside an expanded level with no
  // way back. Teach the budget about expansion and this becomes a
  // trap.
  if (expanded.has(parentId)) {
    return { shown: eligible, summary: folded > 0 ? 'show fewer' : null }
  }
  return { shown, summary: folded > 0 ? `+${folded} more` : null }
}
