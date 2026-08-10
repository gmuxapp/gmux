import { useEffect, useRef, useState } from 'preact/hooks'
import { familyCounts, isProcessSession, projectFamily, type FamilyNode } from './family'
import { splitLevel, visibleFamilyRows } from './family-drawer-model'
import { viewToPath } from './routing'
import { formatAge } from './session-row'
import { activityMap, markSessionRead, projects, sessions, sessionDotState, tabHref } from './store'
import type { Session } from './types'

function hrefFor(session: Session): string | undefined {
  const path = viewToPath({ kind: 'session', sessionId: session.id }, projects.value, sessions.value)
  return path ? tabHref(path) : undefined
}

function FamilyRow({ node, selectedId, depth, expanded, visible, now, onToggle }: {
  node: FamilyNode
  selectedId: string
  depth: number
  expanded: ReadonlySet<string>
  /** The rows the panel's budget chose to draw. */
  visible: ReadonlySet<string>
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
        {process
          ? <span class={`family-row-proc${session.alive && session.status?.active ? ' working' : ''}`} aria-hidden="true">$</span>
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
            visible={visible}
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
function LevelRows({ nodes, parentId, selectedId, depth, expanded, visible, now, onToggle }: {
  nodes: readonly FamilyNode[]
  parentId: string
  selectedId: string
  depth: number
  expanded: ReadonlySet<string>
  visible: ReadonlySet<string>
  now: number
  onToggle: (key: string) => void
}) {
  const { shown, summary } = splitLevel(nodes, parentId, expanded, visible)
  return (
    <>
      {shown.map(node => (
        <FamilyRow
          key={node.session.id}
          node={node}
          selectedId={selectedId}
          depth={depth}
          expanded={expanded}
          visible={visible}
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
            aria-expanded={summary.expanded}
            onClick={() => onToggle(summary.key)}
          >
            {/* The summary sits below the rows it controls, so collapsing
              * takes them back upwards: `▴`, not the `▾` that would point
              * at the empty space where nothing is about to happen. */}
            <span class="family-more-chevron" aria-hidden="true">{summary.expanded ? '▴' : '▸'}</span>
            {summary.label}
          </button>
        </li>
      )}
    </>
  )
}

/** Every member with something outstanding, in one pass over the tree.
 * `markSessionRead` clears the error flag alongside the unread one, so
 * both belong here — the button answers the counts line, and the counts
 * line counts both. */
function unreadMembers(tree: FamilyNode): Session[] {
  const out: Session[] = []
  const visit = (node: FamilyNode) => {
    if (node.session.unread || node.session.status?.error) out.push(node.session)
    for (const child of node.children) visit(child)
  }
  visit(tree)
  return out
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
 * that means subagents thinking or shells running. `total` gets no
 * glyph: it isn't a state. */
function CountsLine({ tree }: { tree: FamilyNode }) {
  const counts = familyCounts([tree])
  const segments: {
    key: string
    dot?: string
    process?: boolean
    count: number
    label: string
    cls?: string
  }[] = []
  // Same precedence the dot itself resolves by: error, then active,
  // then waiting. Processes follow the agents they belong to.
  if (counts.error > 0) {
    segments.push({ key: 'error', dot: 'error', count: counts.error, label: 'error', cls: 'attention' })
  }
  if (counts.workingAgents > 0) {
    segments.push({ key: 'active', dot: 'working', count: counts.workingAgents, label: 'active' })
  }
  if (counts.workingProcesses > 0) {
    // `running`, not `active`: one turn model (ADR 0023), but a command
    // runs where an agent works, and this codebase already says
    // "running process" in the sidebar's own summary.
    segments.push({ key: 'running', process: true, count: counts.workingProcesses, label: 'running' })
  }
  if (counts.unread > 0) {
    segments.push({ key: 'waiting', dot: 'unread', count: counts.unread, label: 'waiting', cls: 'attention' })
  }
  segments.push({ key: 'total', count: counts.total, label: 'total' })
  return (
    <div class="family-counts">
      {segments.map(segment => (
        <span
          key={segment.key}
          class={`family-count${segment.cls ? ` family-count-${segment.cls}` : ''}`}
        >
          {segment.dot && <span class={`session-dot-indicator ${segment.dot}`} aria-hidden="true" />}
          {segment.process && <span class="family-row-proc working" aria-hidden="true">$</span>}
          {segment.count} {segment.label}
        </span>
      ))}
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
  const visible = visibleFamilyRows(projection.tree, pinned)
  const outstanding = unreadMembers(projection.tree)
  // One clock for the whole paint, so sibling ages can't disagree.
  const now = Date.now()
  return (
    <div id="agent-family-drawer" class="family-drawer" role="dialog" aria-label="Session family" ref={panelRef}>
      <div class="family-drawer-head">
        <CountsLine tree={projection.tree} />
        {outstanding.length > 0 && (
          <button
            type="button"
            class="family-mark-read"
            // Iterating is the whole implementation: `markSessionRead` is
            // optimistic and token-bound, so the panel clears instantly
            // and a member that speaks again mid-flight keeps its dot.
            onClick={() => { for (const session of outstanding) markSessionRead(session.id) }}
          >
            Mark all read
          </button>
        )}
      </div>
      <div class="family-drawer-scroll">
        <ul class="family-tree">
          <LevelRows
            nodes={[projection.tree]}
            parentId={projection.root.id}
            selectedId={selected.id}
            depth={0}
            expanded={expanded}
            visible={visible}
            now={now}
            onToggle={toggle}
          />
        </ul>
      </div>
    </div>
  )
}
