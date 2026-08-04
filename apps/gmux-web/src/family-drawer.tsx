import { useEffect, useRef, useState } from 'preact/hooks'
import { familyCounts, isProcessSession, projectFamily, type FamilyNode } from './family'
import { splitLevel } from './family-drawer-model'
import { viewToPath } from './routing'
import { activityMap, projects, sessions, sessionDotState, tabHref } from './store'
import type { Session } from './types'

function hrefFor(session: Session): string | undefined {
  const path = viewToPath({ kind: 'session', sessionId: session.id }, projects.value, sessions.value)
  return path ? tabHref(path) : undefined
}

function FamilyRow({ node, selectedId, depth, expanded, onToggle }: {
  node: FamilyNode
  selectedId: string
  depth: number
  expanded: ReadonlySet<string>
  onToggle: (key: string) => void
}) {
  const session = node.session
  const process = isProcessSession(session)
  const rawDot = sessionDotState(session, activityMap.value)
  const dot = session.id === selectedId && (rawDot === 'error' || rawDot === 'unread') ? 'none' : rawDot
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
        <span class="family-row-kind">{session.adapter}</span>
      </a>
      {node.children.length > 0 && (
        <ul>
          <LevelRows
            nodes={node.children}
            parentId={session.id}
            selectedId={selectedId}
            depth={depth + 1}
            expanded={expanded}
            onToggle={onToggle}
          />
        </ul>
      )}
    </li>
  )
}

/** One children level: capped rows plus a two-state summary row
 * (`+N more` / `show fewer`) keyed per parent. */
function LevelRows({ nodes, parentId, selectedId, depth, expanded, onToggle }: {
  nodes: readonly FamilyNode[]
  parentId: string
  selectedId: string
  depth: number
  expanded: ReadonlySet<string>
  onToggle: (key: string) => void
}) {
  const { shown, summary } = splitLevel(nodes, parentId, expanded)
  return (
    <>
      {shown.map(node => (
        <FamilyRow
          key={node.session.id}
          node={node}
          selectedId={selectedId}
          depth={depth}
          expanded={expanded}
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
            <span class="family-more-chevron" aria-hidden="true">{summary.expanded ? '▾' : '▸'}</span>
            {summary.label}
          </button>
        </li>
      )}
    </>
  )
}

function CountsLine({ trees }: { trees: readonly FamilyNode[] }) {
  const counts = familyCounts(trees)
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
 * trigger, matching the ⋮ menu's behavior — closes on outside mousedown and
 * Escape, no focus trap. Clicking a row navigates without closing it so a
 * family can be traversed in place. Renders live (sidebar activity-mode
 * semantics): rows re-sort only when new output arrives, which is the same
 * stability bar the sidebar meets. */
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
    const onMouseDown = (event: MouseEvent) => {
      const target = event.target as Node
      if (panelRef.current?.contains(target)) return
      if (triggerRef.current?.contains(target)) return // trigger's own click toggles
      onClose()
    }
    document.addEventListener('keydown', onKey)
    document.addEventListener('mousedown', onMouseDown)
    return () => {
      document.removeEventListener('keydown', onKey)
      document.removeEventListener('mousedown', onMouseDown)
    }
  }, [onClose, triggerRef])

  // Main App normally focuses the terminal after session navigation. While
  // the panel is open, reclaim focus onto the newly-selected row so keyboard
  // users can traverse the family without focus escaping behind it.
  useEffect(() => {
    let inner = 0
    const outer = requestAnimationFrame(() => {
      inner = requestAnimationFrame(() => {
        panelRef.current?.querySelector<HTMLElement>('[aria-current="page"]')?.focus()
      })
    })
    return () => { cancelAnimationFrame(outer); cancelAnimationFrame(inner) }
  }, [selected.id])

  // Promotion intentionally defers provenance: promoted sessions present as
  // roots even though parent_session_id remains immutable on the wire.
  const projection = projectFamily(selected, sessions.value)
  return (
    <div id="agent-family-drawer" class="family-drawer" role="dialog" aria-label="Session family" ref={panelRef}>
      <div class="family-drawer-head">
        <CountsLine trees={projection.siblingTrees} />
      </div>
      <div class="family-drawer-scroll">
        <ul class="family-tree">
          <LevelRows
            nodes={projection.siblingTrees}
            parentId={projection.ancestors[projection.ancestors.length - 1]?.id ?? projection.root.id}
            selectedId={selected.id}
            depth={0}
            expanded={expanded}
            onToggle={toggle}
          />
        </ul>
      </div>
    </div>
  )
}
