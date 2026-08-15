import { useEffect, useRef, useState } from 'preact/hooks'
import {
  familySegments, familyStateOf, isProcessSession, projectFamily,
  type FamilyNode, type FamilyState,
} from './family'
import { splitLevel, visibleFamilyRows, type FamilyView } from './family-drawer-model'
import { viewToPath } from './routing'
import { formatAge } from './session-row'
import {
  activityMap, cancelSession, familyActivityById, killSession, markSessionRead,
  projects, sessions, sessionDotState, tabHref,
} from './store'
import { pushError } from './toasts'
import type { Session } from './types'

function hrefFor(session: Session): string | undefined {
  const path = viewToPath({ kind: 'session', sessionId: session.id }, projects.value, sessions.value)
  return path ? tabHref(path) : undefined
}

function FamilyRow({ node, selectedId, depth, expanded, view, now, onToggle }: {
  node: FamilyNode
  selectedId: string
  depth: number
  expanded: ReadonlySet<string>
  /** What the panel's budget chose to draw, and what it chose from. */
  view: FamilyView
  /** Render-time clock, passed down so every row in one paint agrees. */
  now: number
  onToggle: (key: string) => void
}) {
  const session = node.session
  const process = isProcessSession(session)
  // No selection muting here, unlike the sidebar: the panel is a map of
  // the family, and a map that blanks the row you're standing on would
  // claim "1 error" in the counts line with no errored row in sight.
  // (Viewing a session clears its unread/error flags server-side anyway,
  // so the dot settles on its own a beat later.)
  const dot = sessionDotState(session, activityMap.value)
  return (
    <li>
      <a
        class={`family-row${session.id === selectedId ? ' selected' : ''}${process ? ' process' : ''}`}
        style={{ paddingLeft: `${12 + depth * 18}px` }}
        href={hrefFor(session)}
        aria-current={session.id === selectedId ? 'page' : undefined}
      >
        {/* A process keeps its `$` shape and carries state as colour —
          * the glyph column is the only place this row's state appears,
          * so a `$` that can't say `unread` would hide a member the
          * tally counts and the `waiting` filter shows. */}
        {process
          ? <span class={`family-proc ${dot}`} aria-hidden="true">$</span>
          : <span class={`session-dot-indicator ${dot}`} aria-hidden="true" />}
        <span class="family-row-title">{session.title}</span>
        {/* Levels are ordered by this timestamp, so it has to be on
          * screen: sorting by an invisible key reads as no order at
          * all. It replaces the adapter label, which repeated the same
          * word down the whole panel and duplicated the `$` glyph. */}
        <span class="family-row-age">{formatAge(session.last_output_at ?? session.created_at, now)}</span>
      </a>
      {node.children.length > 0 && (
        <ul>
          <LevelRows
            nodes={node.children}
            parentId={session.id}
            selectedId={selectedId}
            depth={depth + 1}
            expanded={expanded}
            view={view}
            now={now}
            onToggle={onToggle}
          />
        </ul>
      )}
    </li>
  )
}

/** One children level: capped rows plus a two-state summary row
 * (`+N more` / `show fewer`) keyed per parent. */
function LevelRows({ nodes, parentId, selectedId, depth, expanded, view, now, onToggle }: {
  nodes: readonly FamilyNode[]
  parentId: string
  selectedId: string
  depth: number
  expanded: ReadonlySet<string>
  view: FamilyView
  now: number
  onToggle: (key: string) => void
}) {
  const { shown, summary } = splitLevel(nodes, parentId, expanded, view)
  return (
    <>
      {shown.map(node => (
        <FamilyRow
          key={node.session.id}
          node={node}
          selectedId={selectedId}
          depth={depth}
          expanded={expanded}
          view={view}
          now={now}
          onToggle={onToggle}
        />
      ))}
      {summary && (
        <li>
          <button
            type="button"
            class="family-more"
            style={{ paddingLeft: `${12 + depth * 18}px` }}
            aria-expanded={expanded.has(parentId)}
            onClick={() => onToggle(parentId)}
          >
            {/* The summary sits below the rows it controls, so collapsing
              * takes them back upwards: `▴`, not the `▾` that would point
              * at the empty space where nothing is about to happen. */}
            <span class="family-more-chevron" aria-hidden="true">{expanded.has(parentId) ? '▴' : '▸'}</span>
            {summary}
          </button>
        </li>
      )}
    </>
  )
}

/** Every member the given filter matches — the twin of
 * `familyActivityById`, which counts what this collects. They agree
 * because both start from the same family membership with the root
 * excluded and neither filters further; a filter added to one alone
 * would put a number on a button that touches a different set.
 *
 * The same `familyStateOf` rule the tally counts by, over the same
 * population it counts: the
 * root's descendants, never the root. The tally, the verb's label, and
 * the verb's targets must quote one number, and the root is not the
 * family's to act on — you act on the root by visiting it, which is
 * usually where you are standing. */
function membersInState(tree: FamilyNode, state: FamilyState): Session[] {
  const out: Session[] = []
  const visit = (node: FamilyNode) => {
    if (familyStateOf(node.session) === state) out.push(node.session)
    for (const child of node.children) visit(child)
  }
  for (const child of tree.children) visit(child)
  return out
}

/** The one bulk action each filter's members admit. There is no ambient
 * action: a verb only appears once its filter is on, so the panel is
 * already showing that state and nothing else before the button exists.
 *
 * The destructive verbs carry their target count, because the rows on
 * screen are NOT the whole story: the line budget still folds a big
 * family, so "Stop all" under a filter matching 200 processes reaches
 * every one of them, including those behind `+N more`. Acting on the
 * filter rather than the viewport is the right behaviour — a fold is
 * the panel's economy, not a selection — but then the blast radius has
 * to be in the label you are about to click.
 *
 * `error` shares the waiting verb because acknowledging is all you can
 * do in bulk to an error — `markSessionRead` clears the error flag — and
 * error's precedence means an errored member never surfaces under
 * `waiting`. There is no action for `all`: no single verb answers
 * everything. */
type FamilyAction = {
  label: (n: number) => string
  /** Returns false for a member that failed, so `runBulk` can report
   * once at the end. `markSessionRead` returns nothing: it is
   * optimistic and fire-and-forget, so it has no failure to count. */
  run: (id: string) => unknown
}
const FAMILY_ACTIONS: Partial<Record<FamilyState, FamilyAction>> = {
  waiting: { label: () => 'Mark all read', run: markSessionRead },
  error: { label: () => 'Mark all read', run: markSessionRead },
  running: { label: n => `Stop all ${n}`, run: id => killSession(id, { quiet: true }) },
  active: { label: n => `Interrupt all ${n}`, run: id => cancelSession(id, { quiet: true }) },
}

/** Run a bulk verb over a family's worth of members.
 *
 * Bounded concurrency: a family runs to several hundred, and firing
 * that many fetches at once buys nothing (the browser queues them per
 * host anyway) while making the failure report arrive in one lump at
 * the end regardless. Failures are counted, not toasted individually —
 * see `quiet` in the store — so a daemon that is simply down produces
 * one line instead of two hundred. */
async function runBulk(action: FamilyAction, targets: readonly Session[]): Promise<void> {
  const queue = [...targets]
  let failed = 0
  const worker = async () => {
    for (let next = queue.shift(); next !== undefined; next = queue.shift()) {
      if ((await action.run(next.id)) === false) failed++
    }
  }
  await Promise.all(Array.from({ length: Math.min(8, queue.length) }, worker))
  if (failed > 0) pushError(`${failed} of ${targets.length} did not respond`)
}

/** The header's tally, in the turn model's own words (ADR 0023: a
 * session is active, idle, or waiting on you) and the sidebar's own
 * dots. `working` is the CSS token for the active dot; the fact behind
 * it is `status.active`, so it reads `active` here. `unread` is how the
 * wire spells "waiting on you", shortened to `waiting` because it sits
 * beside three other segments.
 *
 * Each segment names a state and shows the glyph the rows use for it, so
 * the header and the tree can be read against each other without
 * translating — including the `$`, because a family is routinely mostly
 * processes and "3 active" reads very differently depending on whether
 * that means subagents thinking or shells running. `all` gets no glyph
 * or count: it isn't a state, it's the absence of the question — and it
 * goes first because it's the position the panel opens in. The family's
 * size was the one fact the old `total` carried, and members mostly
 * accumulate; the folds' `+N more` already says how much lies below.
 *
 * Counted over the root's descendants, root excluded — the standard
 * family numbers, shared with the header pill and the sidebar line.
 * The root's own dot is on its pinned row directly beneath: it speaks
 * for itself, and counting it here would make this tally the one place
 * quoting a different number for the same dots. */
function CountsLine({ rootId, filter, onFilter }: {
  rootId: string
  filter: FamilyState | null
  onFilter: (state: FamilyState | null) => void
}) {
  const activity = familyActivityById.value.get(rootId)
  const tally = (state: FamilyState | null, active: boolean, children: preact.ComponentChildren) => (
    <button
      key={state ?? 'all'}
      type="button"
      // A tally you can press is a filter; pressing the one that's on
      // turns it off, so the panel never traps you in a view.
      class={`family-count${state === 'error' || state === 'waiting' ? ' family-count-attention' : ''}${active ? ' active' : ''}`}
      aria-pressed={active}
      onClick={() => onFilter(active ? null : state)}
    >
      {children}
    </button>
  )
  return (
    <div class="family-counts">
      {tally(null, filter === null, 'all')}
      {familySegments(activity).map(segment => tally(segment.state, filter === segment.state, (
        <>
          {segment.dot
            ? <span class={`session-dot-indicator ${segment.dot}`} aria-hidden="true" />
            : <span class="family-proc working" aria-hidden="true">$</span>}
          {/* The state's own name is the label: `running`, not a second
            * `active` — one turn model (ADR 0023), but a command runs
            * where an agent works. */}
          {segment.count} {segment.state}
        </>
      )))}
    </div>
  )
}

/** The family panel: a non-modal popover anchored under the header's family
 * trigger, matching the ⋮ menu's behavior — closes on an outside
 * pointerdown and Escape, no focus trap. Clicking a row navigates without closing it so a
 * family can be traversed in place.
 *
 * Shows the whole family from the root, wherever you're standing, with
 * every level ordered by recency — the same rule as the sidebar's
 * activity feed, and the same stability bar: rows move only when new
 * output arrives, never because you acted on one. */
export function FamilyDrawer({ selected, onClose, triggerRef }: {
  selected: Session
  onClose: () => void
  triggerRef: { current: HTMLButtonElement | null }
}) {
  const panelRef = useRef<HTMLDivElement>(null)
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set())
  // Per-open, like the expansion state beside it: a filter is a way of
  // looking at the family right now, not a preference about it.
  const [filter, setFilter] = useState<FamilyState | null>(null)
  const [busy, setBusy] = useState(false)
  const toggle = (key: string) => {
    setExpanded(prev => {
      const next = new Set(prev)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
  }

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      event.preventDefault()
      onClose()
      triggerRef.current?.focus()
    }
    // pointerdown, not mousedown: the terminal's touch handlers
    // preventDefault on several gesture paths, which suppresses the
    // browser's synthesized mouse cascade — so a tap on the terminal
    // never reached a mousedown listener and the panel stayed open over
    // the session you were tapping back into. pointerdown fires ahead of
    // touchstart and can't be cancelled by it.
    const onPointerDown = (event: PointerEvent) => {
      const target = event.target as Node
      if (panelRef.current?.contains(target)) return
      if (triggerRef.current?.contains(target)) return // trigger's own click toggles
      onClose()
    }
    document.addEventListener('keydown', onKey)
    document.addEventListener('pointerdown', onPointerDown)
    return () => {
      document.removeEventListener('keydown', onKey)
      document.removeEventListener('pointerdown', onPointerDown)
    }
  }, [onClose, triggerRef])

  // Main App normally focuses the terminal after session navigation. While
  // the panel is open, reclaim focus onto the newly-selected row so keyboard
  // users can traverse the family without focus escaping behind it.
  useEffect(() => {
    let inner = 0
    const outer = requestAnimationFrame(() => {
      inner = requestAnimationFrame(() => {
        const row = panelRef.current?.querySelector<HTMLElement>('[aria-current="page"]')
        // Whole-family scope means your row can open below the fold, so
        // bring it into view before taking focus (focus alone would
        // scroll it to an arbitrary edge).
        row?.scrollIntoView({ block: 'nearest' })
        row?.focus({ preventScroll: true })
      })
    })
    return () => { cancelAnimationFrame(outer); cancelAnimationFrame(inner) }
  }, [selected.id])

  // Promotion intentionally defers provenance: promoted sessions present as
  // roots even though parent_session_id remains immutable on the wire.
  const projection = projectFamily(selected, sessions.value)
  // Your own path, root to selection: the budget may fold anything but this.
  const pinned = new Set([...projection.ancestors.map(a => a.id), selected.id])
  const view = visibleFamilyRows(projection.tree, { pinned, filter })
  const action = filter ? FAMILY_ACTIONS[filter] : undefined
  const targets = filter && action ? membersInState(projection.tree, filter) : []
  // One clock for the whole paint, so sibling ages can't disagree.
  const now = Date.now()
  return (
    <div id="agent-family-drawer" class="family-drawer" role="dialog" aria-label="Session family" ref={panelRef}>
      <div class="family-drawer-head">
        <CountsLine rootId={projection.root.id} filter={filter} onFilter={setFilter} />
        {action && targets.length > 0 && (
          <button
            type="button"
            class="family-mark-read"
            // Disabled while in flight: the verbs are individually
            // idempotent-or-tolerant, but a second click would re-run
            // the whole family and double any toast it earns.
            disabled={busy}
            onClick={() => {
              setBusy(true)
              runBulk(action, targets).finally(() => { setBusy(false) })
            }}
          >
            {action.label(targets.length)}
          </button>
        )}
      </div>
      <div class="family-drawer-scroll">
        {/* The root is a row, not a level: wrapping it in `LevelRows`
          * keyed the outer level by the root's own id — the same key
          * its children's level uses — so expanding the children put a
          * second, orphan `show fewer` under the whole tree. It could
          * never fold anyway; the root is admitted before anything
          * competes for the budget. */}
        <ul class="family-tree">
          <FamilyRow
            node={projection.tree}
            selectedId={selected.id}
            depth={0}
            expanded={expanded}
            view={view}
            now={now}
            onToggle={toggle}
          />
        </ul>
      </div>
    </div>
  )
}
