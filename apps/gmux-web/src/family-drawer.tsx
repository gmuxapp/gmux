import { useEffect, useRef, useState } from 'preact/hooks'
import {
  familySegments, familyStateOf, isProcessSession, isRunningProcess, projectFamily,
  type FamilyNode, type FamilyState,
} from './family'
import {
  FAMILY_ROW_BUDGET, splitLevel, visibleFamilyRows, type FamilyView,
} from './family-drawer-model'
import { viewToPath } from './routing'
import { formatAge } from './session-row'
import {
  activityMap, cancelSession, familyActivityById, markSessionRead, ownDotState,
  projects, sessions, tabHref,
} from './store'
import { pushError } from './toasts'
import type { Session } from './types'

export function familyDrawerDotState(session: Session, selectedId: string) {
  return ownDotState(session, activityMap.value, selectedId)
}

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
  // Selection acknowledges only waiting attention; durable error remains.
  // Match every other navigation surface by muting selected waiting and
  // waiting-error states while retaining selected active/active-error status.
  const dot = familyDrawerDotState(session, selectedId)
  return (
    <li>
      <a
        class={`family-row${session.id === selectedId ? ' selected' : ''}${process ? ' process' : ''}`}
        style={{ paddingLeft: `${12 + depth * 18}px` }}
        href={hrefFor(session)}
        aria-current={session.id === selectedId ? 'page' : undefined}
      >
        {/* `$` is process identity; its one state is lifecycle — lit
          * while running, dimmed when finished. Agent attention never
          * recolors it. */}
        {process
          ? <span class={`family-proc${isRunningProcess(session) ? '' : ' done'}`} aria-hidden="true">$</span>
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
 * family, so “Interrupt all” reaches every matching agent, including
 * those behind `+N more`. Acting on the filter rather than the viewport
 * is the right behaviour — a fold is
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

/** Agent-state tallies plus one process-type control, all over the root's
 * descendants with the root excluded. Agent controls filter exactly the state
 * they count. The process control is deliberately different: its optional
 * number counts running processes, while pressing it opens all process
 * history. `all` is the absence of either question and carries no count. */
type FamilyFilter = FamilyState | 'processes'

function processCount(tree: FamilyNode): number {
  let count = 0
  const visit = (node: FamilyNode) => {
    if (isProcessSession(node.session)) count++
    for (const child of node.children) visit(child)
  }
  for (const child of tree.children) visit(child)
  return count
}

function CountsLine({ rootId, tree, filter, onFilter }: {
  rootId: string
  tree: FamilyNode
  filter: FamilyFilter | null
  onFilter: (state: FamilyFilter | null) => void
}) {
  const activity = familyActivityById.value.get(rootId)
  const processes = processCount(tree)
  const running = activity?.running ?? 0
  const tally = (state: FamilyFilter | null, active: boolean, children: preact.ComponentChildren,
    label?: string) => (
    <button
      key={state ?? 'all'}
      type="button"
      // A tally you can press is a filter; pressing the one that's on
      // turns it off, so the panel never traps you in a view.
      class={`family-count${state === 'error' || state === 'waiting' ? ' family-count-attention' : ''}${active ? ' active' : ''}`}
      aria-label={label}
      aria-pressed={active}
      onClick={() => onFilter(active ? null : state)}
    >
      {children}
    </button>
  )
  return (
    <div class="family-counts">
      {tally(null, filter === null, 'all')}
      {familySegments(activity).filter(segment => segment.state !== 'running').map(segment =>
        tally(segment.state, filter === segment.state, (
          <>
            <span class={`session-dot-indicator ${segment.dot}`} aria-hidden="true" />
            {segment.count} {segment.state}
          </>
        )))}
      {processes > 0 && tally('processes', filter === 'processes', (
        <>
          <span class={`family-proc family-proc-filter${running > 0 ? '' : ' quiet'}`} aria-hidden="true">$</span>
          {running > 0 ? `${running} running` : 'processes'}
        </>
      ), running > 0 ? `Processes, ${running} running` : 'Processes')}
    </div>
  )
}

interface ProcessEntry {
  session: Session
  parent: Session
}

function familyProcesses(tree: FamilyNode): ProcessEntry[] {
  const out: ProcessEntry[] = []
  const visit = (node: FamilyNode) => {
    for (const child of node.children) {
      if (isProcessSession(child.session)) out.push({ session: child.session, parent: node.session })
      visit(child)
    }
  }
  visit(tree)
  return out.sort((a, b) => {
    const at = a.session.last_output_at ?? a.session.created_at
    const bt = b.session.last_output_at ?? b.session.created_at
    return bt.localeCompare(at) || a.session.id.localeCompare(b.session.id)
  })
}

function ProcessRow({ entry, selectedId, now }: {
  entry: ProcessEntry
  selectedId: string
  now: number
}) {
  const { session, parent } = entry
  return (
    <li>
      <a
        class={`family-row process${session.id === selectedId ? ' selected' : ''}`}
        href={hrefFor(session)}
        aria-current={session.id === selectedId ? 'page' : undefined}
      >
        <span class={`family-proc${isRunningProcess(session) ? '' : ' done'}`} aria-hidden="true">$</span>
        <span class="family-row-title">{session.title}</span>
        <span class="family-process-parent">{parent.title}</span>
        <span class="family-row-age">{formatAge(session.last_output_at ?? session.created_at, now)}</span>
      </a>
    </li>
  )
}

function processSlice(entries: readonly ProcessEntry[], limit: number, selectedId: string): ProcessEntry[] {
  if (entries.length <= limit) return [...entries]
  const shown = entries.slice(0, limit)
  const selected = entries.find(entry => entry.session.id === selectedId)
  if (!selected || shown.includes(selected) || limit === 0) return shown
  // A selection beyond the recency slice is an explicit pin, not a row
  // silently substituted into the tail: lead with it, then keep recency order
  // among the remaining budget.
  return [selected, ...entries.slice(0, limit - 1)]
}

function ProcessSection({ name, entries, limit, expanded, selectedId, now, onToggle }: {
  name: 'Running' | 'Finished'
  entries: readonly ProcessEntry[]
  limit: number
  expanded: boolean
  selectedId: string
  now: number
  onToggle: () => void
}) {
  if (entries.length === 0) return null
  const foldable = entries.length > limit
  const open = expanded && foldable
  const shown = open ? [...entries] : processSlice(entries, limit, selectedId)
  const hidden = entries.length - shown.length
  return (
    <section class="family-process-section">
      <h3>{name} <span aria-hidden="true">·</span> {entries.length}</h3>
      <ul class="family-process-list">
        {shown.map(entry => <ProcessRow key={entry.session.id} entry={entry} selectedId={selectedId} now={now} />)}
        {foldable && (
          <li>
            <button type="button" class="family-more" aria-expanded={open} onClick={onToggle}>
              <span class="family-more-chevron" aria-hidden="true">{open ? '▴' : '▸'}</span>
              {open ? 'show fewer' : `+${hidden} more`}
            </button>
          </li>
        )}
      </ul>
    </section>
  )
}

function ProcessesView({ tree, selectedId, expanded, now, onToggle }: {
  tree: FamilyNode
  selectedId: string
  expanded: ReadonlySet<string>
  now: number
  onToggle: (key: string) => void
}) {
  const processes = familyProcesses(tree)
  const running = processes.filter(({ session }) => isRunningProcess(session))
  const finished = processes.filter(({ session }) => !isRunningProcess(session))
  // The selected process owns one budget line in its lifecycle section.
  // If running work would otherwise consume the whole budget, displace one
  // running row so a selected finished process never vanishes behind its fold.
  const selectedFinished = finished.some(({ session }) => session.id === selectedId)
  const runningLimit = Math.max(0, FAMILY_ROW_BUDGET - (selectedFinished ? 1 : 0))
  const initialRunning = Math.min(running.length, runningLimit)
  const finishedLimit = Math.max(0, FAMILY_ROW_BUDGET - initialRunning)
  return (
    <div class="family-processes">
      <ProcessSection name="Running" entries={running} limit={runningLimit}
        expanded={expanded.has('process:running')} selectedId={selectedId} now={now}
        onToggle={() => onToggle('process:running')} />
      <ProcessSection name="Finished" entries={finished} limit={finishedLimit}
        expanded={expanded.has('process:finished')} selectedId={selectedId} now={now}
        onToggle={() => onToggle('process:finished')} />
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
  const [filter, setFilter] = useState<FamilyFilter | null>(null)
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
      // Anything that declares itself a control of this drawer (the
      // header trigger, the sidebar indicator) toggles on its own
      // click — closing here on the pointerdown would flip the drawer
      // shut a beat early, and the control's click would reopen it.
      const el = target instanceof Element ? target : target.parentElement
      if (el?.closest('[aria-controls="agent-family-drawer"]')) return
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

  // Promotion is structural: promote clears parent_session_id on the wire,
  // so promoted sessions are roots here by that edge alone. The immutable
  // launched_from_session_id keeps the provenance “Return to family” uses.
  const projection = projectFamily(selected, sessions.value)
  const stateFilter = filter === 'processes' ? null : filter
  // Structural agent ancestors remain context under a state filter, but a
  // selected process does not: error/waiting/active are agent-only views.
  // The dedicated process view pins selected processes within its own budget.
  const pinSelected = !(stateFilter && isProcessSession(selected))
  const pinned = new Set([
    ...projection.ancestors.map(a => a.id),
    ...(pinSelected ? [selected.id] : []),
  ])
  const view = visibleFamilyRows(projection.tree, { pinned, filter: stateFilter })
  const action = stateFilter ? FAMILY_ACTIONS[stateFilter] : undefined
  const targets = stateFilter && action ? membersInState(projection.tree, stateFilter) : []
  // One clock for the whole paint, so sibling ages can't disagree.
  const now = Date.now()
  return (
    <div id="agent-family-drawer" class="family-drawer" role="dialog" aria-label="Session family" ref={panelRef}>
      <div class="family-drawer-head">
        <CountsLine rootId={projection.root.id} tree={projection.tree} filter={filter} onFilter={setFilter} />
        {/* Reserve the verb's column even when no filter owns one. The tally
          * then receives the same width before and after a press, so neither
          * it nor the drawer changes height when the button appears. */}
        <div class="family-action-slot">
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
      </div>
      <div class="family-drawer-scroll">
        {filter === 'processes'
          ? <ProcessesView tree={projection.tree} selectedId={selected.id} expanded={expanded} now={now} onToggle={toggle} />
          : (
            /* The root is a row, not a level: wrapping it in `LevelRows`
             * keyed the outer level by the root's own id — the same key
             * its children's level uses — so expanding the children put a
             * second, orphan `show fewer` under the whole tree. */
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
          )}
      </div>
    </div>
  )
}
