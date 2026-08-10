import { useEffect, useRef, useState } from 'preact/hooks'
import { familyCounts, isProcessSession, projectFamily, type FamilyNode } from './family'
import { splitLevel, visibleFamilyRows } from './family-drawer-model'
import { viewToPath } from './routing'
import { formatAge } from './session-row'
import { activityMap, projects, sessions, sessionDotState, tabHref } from './store'
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

function CountsLine({ tree }: { tree: FamilyNode }) {
  const counts = familyCounts([tree])
  const segments: { text: string; cls?: string }[] = []
  if (counts.error > 0) segments.push({ text: `${counts.error} error`, cls: 'attention' })
  if (counts.working > 0) segments.push({ text: `${counts.working} working` })
  if (counts.unread > 0) segments.push({ text: `${counts.unread} unread`, cls: 'attention' })
  segments.push({ text: `${counts.total} total` })
  return (
    <div class="family-counts">
      {segments.map((segment, index) => (
        <span key={segment.text} class={segment.cls ? `family-count-${segment.cls}` : undefined}>
          {index > 0 && <span class="family-count-sep" aria-hidden="true"> · </span>}
          {segment.text}
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
  // One clock for the whole paint, so sibling ages can't disagree.
  const now = Date.now()
  return (
    <div id="agent-family-drawer" class="family-drawer" role="dialog" aria-label="Session family" ref={panelRef}>
      <div class="family-drawer-head">
        <CountsLine tree={projection.tree} />
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
