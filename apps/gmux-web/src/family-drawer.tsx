import { useEffect, useRef, useState } from 'preact/hooks'
import {
  bucketedFamily, familyIndex, familyNavigation, FAMILY_GROUP_CAPS,
  type BucketedFamilyProjection, type FamilyBucket, type FamilyBucketGroup,
  type FamilyBucketNode, type FamilyNavigation,
} from './family'
import { viewToPath } from './routing'
import { activityMap, projects, sessions, sessionDotState, tabHref } from './store'
import type { Session } from './types'

function hrefFor(session: Session): string | undefined {
  const path = viewToPath({ kind: 'session', sessionId: session.id }, projects.value, sessions.value)
  return path ? tabHref(path) : undefined
}

/** Wording for a collapsed group's summary row. Agents get the bucket noun,
 * processes their own, so `+547 finished · 12 processes done` reads as two
 * facts instead of one blurred count. */
function processNoun(count: number): string {
  return count === 1 ? 'process' : 'processes'
}

function summaryLabel(bucket: FamilyBucket, agents: number, processes: number): string {
  const parts: string[] = []
  if (agents > 0) {
    parts.push(bucket === 'working' ? `+${agents} more working` : `+${agents} ${bucket}`)
  }
  if (processes > 0) {
    parts.push(`${agents > 0 ? '' : '+'}${processes} ${processNoun(processes)}${bucket === 'finished' ? ' done' : ''}`)
  }
  return parts.join(' · ')
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
  // Dead rows carry no dot: their group position already says "finished",
  // and a wall of never-viewed dead children must not glow "unread".
  const rawDot = session.alive ? sessionDotState(session, activityMap.value) : 'none'
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
  const key = `${parentId}:${group.bucket}`
  const isExpanded = expanded.has(key)
  const cap = FAMILY_GROUP_CAPS[group.bucket]
  const shown = isExpanded ? group.nodes : group.nodes.slice(0, cap)
  const hidden = group.nodes.slice(shown.length)
  const collapsible = group.nodes.length > cap
  const hiddenAgents = hidden.filter(n => !n.process).length
  const hiddenProcesses = hidden.length - hiddenAgents
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
      {collapsible && (
        <li>
          <button
            type="button"
            class="family-more"
            style={{ paddingLeft: `${12 + depth * 18}px` }}
            aria-expanded={isExpanded}
            onClick={() => onToggle(key)}
          >
            <span class="family-more-chevron" aria-hidden="true">{isExpanded ? '▾' : '▸'}</span>
            {isExpanded ? 'show fewer' : summaryLabel(group.bucket, hiddenAgents, hiddenProcesses)}
          </button>
        </li>
      )}
    </>
  )
}

function CountsLine({ counts }: { counts: BucketedFamilyProjection['counts'] }) {
  const segments: { text: string; cls?: string }[] = []
  if (counts.attention > 0) segments.push({ text: `${counts.attention} need attention`, cls: 'attention' })
  if (counts.working > 0) segments.push({ text: `${counts.working} working` })
  if (counts.idle > 0) segments.push({ text: `${counts.idle} idle` })
  if (counts.finished > 0) segments.push({ text: `${counts.finished} finished` })
  if (counts.processes > 0) segments.push({ text: `${counts.processes} ${processNoun(counts.processes)}`, cls: 'processes' })
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
  // Two-state expansion per (parent, bucket); resets when the drawer
  // closes because this component unmounts with it.
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set())
  const toggle = (key: string) => setExpanded(prev => {
    const next = new Set(prev)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    return next
  })

  // Freeze the projection (ordering, bucketing, membership) for as long as
  // the same session stays selected: live status updates repaint rows in
  // place but never re-sort the list while the user is reading it. `peek`
  // avoids subscribing the projection itself; rows subscribe for repaints.
  // Promotion intentionally defers provenance: promoted sessions present
  // as roots even though parent_session_id remains immutable on the wire.
  const frozenRef = useRef<{ selectedId: string; projection: BucketedFamilyProjection; navigation: FamilyNavigation } | null>(null)
  if (frozenRef.current?.selectedId !== selected.id) {
    const snapshot = sessions.peek()
    frozenRef.current = {
      selectedId: selected.id,
      projection: bucketedFamily(selected, snapshot),
      navigation: familyNavigation(selected, snapshot),
    }
  }
  const { projection, navigation } = frozenRef.current

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
              expanded={expanded}
              onToggle={toggle}
            />
          ))}
        </ul>
      </div>
    </div>
  )
}
