import { useEffect, useReducer, useRef } from 'preact/hooks'
import { familyIndex, type FamilyBucketGroup, type FamilyBucketNode } from './family'
import { splitGroup, syncDrawer, toggleDrawerGroup, type DrawerModel } from './family-drawer-model'
import { viewToPath } from './routing'
import { activityMap, projects, sessions, sessionDotState, tabHref } from './store'
import type { Session } from './types'

function hrefFor(session: Session): string | undefined {
  const path = viewToPath({ kind: 'session', sessionId: session.id }, projects.value, sessions.value)
  return path ? tabHref(path) : undefined
}

function FamilyRow({ node, selectedId, depth, expanded, onToggle }: {
  node: FamilyBucketNode
  selectedId: string
  depth: number
  expanded: ReadonlySet<string>
  onToggle: (key: string) => void
}) {
  // Structure (order, buckets, membership) is frozen while the drawer is
  // open; each row still reads the live session so dots and titles update
  // in place without reshuffling the list under the cursor.
  const session = familyIndex(sessions.value).byId.get(node.session.id) ?? node.session
  const href = hrefFor(session)
  const rawDot = sessionDotState(session, activityMap.value)
  const dot = session.id === selectedId && (rawDot === 'error' || rawDot === 'unread') ? 'none' : rawDot
  return (
    <li>
      <a
        class={`family-row${session.id === selectedId ? ' selected' : ''}${session.alive ? '' : ' inactive'}${node.process ? ' process' : ''}`}
        style={{ paddingLeft: `${12 + depth * 18}px` }}
        href={href}
        aria-current={session.id === selectedId ? 'page' : undefined}
        tabIndex={session.id === selectedId ? 0 : undefined}
      >
        {node.process
          ? <span class={`family-row-proc${session.alive && session.status?.active ? ' working' : ''}`} aria-hidden="true">$</span>
          : <span class={`session-dot-indicator ${dot}`} aria-hidden="true" />}
        <span class="family-row-title">{session.title}</span>
        <span class="family-row-kind">{session.adapter}</span>
      </a>
      {node.groups.length > 0 && (
        <ul>
          {node.groups.map(group => (
            <GroupRows
              key={group.bucket}
              group={group}
              parentId={node.session.id}
              selectedId={selectedId}
              depth={depth + 1}
              expanded={expanded}
              onToggle={onToggle}
            />
          ))}
        </ul>
      )}
    </li>
  )
}

/** One bucket group inside a children list: capped rows plus a two-state
 * summary row (`+N finished` / `show fewer`) keyed per (parent, bucket). */
function GroupRows({ group, parentId, selectedId, depth, expanded, onToggle }: {
  group: FamilyBucketGroup
  parentId: string
  selectedId: string
  depth: number
  expanded: ReadonlySet<string>
  onToggle: (key: string) => void
}) {
  const { shown, summary } = splitGroup(group, parentId, expanded)
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

function CountsLine({ counts }: { counts: DrawerModel['projection']['counts'] }) {
  const segments: { text: string; cls?: string }[] = []
  if (counts.attention > 0) segments.push({ text: `${counts.attention} need attention`, cls: 'attention' })
  if (counts.working > 0) segments.push({ text: `${counts.working} working` })
  if (counts.idle > 0) segments.push({ text: `${counts.idle} idle` })
  if (counts.finished > 0) segments.push({ text: `${counts.finished} finished` })
  if (counts.processes > 0) segments.push({ text: `${counts.processes} ${counts.processes === 1 ? 'process' : 'processes'}`, cls: 'processes' })
  if (segments.length === 0) return null
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

export function FamilyDrawer({ selected, onClose, triggerRef }: {
  selected: Session
  onClose: () => void
  triggerRef: { current: HTMLButtonElement | null }
}) {
  const drawerRef = useRef<HTMLDivElement>(null)
  const [, forceRender] = useReducer((n: number) => n + 1, 0)

  // All freeze/expansion behavior lives in the pure drawer model (see
  // family-drawer-model.ts): ordinary live updates keep the projection
  // frozen; selection changes and group toggles re-project from current
  // facts. `peek` keeps the projection itself unsubscribed; rows read
  // sessions.value and repaint live. Expansion resets when the drawer
  // closes because this component unmounts with it.
  // Promotion intentionally defers provenance: promoted sessions present
  // as roots even though parent_session_id remains immutable on the wire.
  const modelRef = useRef<DrawerModel | null>(null)
  modelRef.current = syncDrawer(modelRef.current, selected, sessions.peek())
  const model = modelRef.current
  const toggle = (key: string) => {
    modelRef.current = toggleDrawerGroup(modelRef.current!, selected, sessions.peek(), key)
    forceRender(0)
  }

  useEffect(() => {
    const drawer = drawerRef.current
    drawer?.focus()
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault()
        onClose()
        triggerRef.current?.focus()
        return
      }
      if (event.key !== 'Tab' || !drawer) return
      const focusable = [...drawer.querySelectorAll<HTMLElement>('a[href], button:not([disabled])')]
      if (focusable.length === 0) { event.preventDefault(); drawer.focus(); return }
      const first = focusable[0]
      const last = focusable[focusable.length - 1]
      if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus() }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus() }
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [onClose, triggerRef])

  // Main App normally focuses the terminal after session navigation. While
  // this modal remains open, reclaim focus onto the newly-selected row so
  // keyboard users can traverse the family without the drawer closing or
  // focus escaping behind it.
  useEffect(() => {
    let inner = 0
    const outer = requestAnimationFrame(() => {
      inner = requestAnimationFrame(() => {
        drawerRef.current?.querySelector<HTMLElement>('[aria-current="page"]')?.focus()
      })
    })
    return () => { cancelAnimationFrame(outer); cancelAnimationFrame(inner) }
  }, [selected.id])

  const navButton = (label: string, session: Session) => {
    const href = hrefFor(session)
    return href ? <a class="family-nav-button" href={href}>{label}</a> : null
  }

  const { projection, navigation } = model
  return (
    <div id="agent-family-drawer" class="family-drawer" role="dialog" aria-modal="true" aria-label="Agents" tabIndex={-1} ref={drawerRef}>
      <div class="family-drawer-head">
        <div class="family-drawer-heading">
          <strong>Agents</strong>
          <div class="family-drawer-nav">
            {navigation.parent && navButton('Parent', navigation.parent)}
            {navigation.root && navButton('Root', navigation.root)}
            <button class="family-drawer-close" type="button" aria-label="Close agents" onClick={() => {
              onClose()
              triggerRef.current?.focus()
            }}>×</button>
          </div>
        </div>
        <CountsLine counts={projection.counts} />
        {projection.ancestors.length > 0 && (
          <ol class="family-spine" aria-label="Ancestors">
            {projection.ancestors.map((ancestor, index) => (
              <li key={ancestor.id}>
                <a href={hrefFor(ancestor)}>{ancestor.title}</a>
                {index < projection.ancestors.length - 1 && <span aria-hidden="true">›</span>}
              </li>
            ))}
          </ol>
        )}
      </div>
      <div class="family-drawer-scroll">
        <ul class="family-tree">
          {projection.groups.map(group => (
            <GroupRows
              key={group.bucket}
              group={group}
              parentId={projection.ancestors[projection.ancestors.length - 1]?.id ?? ''}
              selectedId={selected.id}
              depth={0}
              expanded={model.expanded}
              onToggle={toggle}
            />
          ))}
        </ul>
      </div>
    </div>
  )
}
