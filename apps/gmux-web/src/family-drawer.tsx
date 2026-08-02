import { useEffect, useRef } from 'preact/hooks'
import { familyNavigation, projectFamily, type FamilyNode } from './family'
import { viewToPath } from './routing'
import { projects, sessions, tabHref } from './store'
import type { Session } from './types'

function hrefFor(session: Session): string | undefined {
  const path = viewToPath({ kind: 'session', sessionId: session.id }, projects.value, sessions.value)
  return path ? tabHref(path) : undefined
}

function FamilyRow({ node, selectedId, depth }: {
  node: FamilyNode
  selectedId: string
  depth: number
}) {
  const session = node.session
  const href = hrefFor(session)
  return (
    <li>
      <a
        class={`family-row${session.id === selectedId ? ' selected' : ''}${session.alive ? '' : ' inactive'}`}
        style={{ paddingLeft: `${12 + depth * 18}px` }}
        href={href}
        aria-current={session.id === selectedId ? 'page' : undefined}
        tabIndex={session.id === selectedId ? 0 : undefined}
      >
        <span class={`family-row-dot${session.status?.active ? ' active' : ''}`} aria-hidden="true" />
        <span class="family-row-title">{session.title}</span>
        <span class="family-row-kind">{session.adapter}</span>
      </a>
      {node.children.length > 0 && (
        <ul>{node.children.map(child => (
          <FamilyRow key={child.session.id} node={child} selectedId={selectedId} depth={depth + 1} />
        ))}</ul>
      )}
    </li>
  )
}

export function FamilyDrawer({ selected, onClose, triggerRef }: {
  selected: Session
  onClose: () => void
  triggerRef: { current: HTMLButtonElement | null }
}) {
  const drawerRef = useRef<HTMLDivElement>(null)
  const projection = projectFamily(selected, sessions.value)
  // Promotion intentionally defers provenance: promoted sessions present as
  // roots even though parent_session_id remains immutable on the wire.
  const navigation = familyNavigation(selected, sessions.value)

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
    <div id="agent-family-drawer" class="family-drawer" role="dialog" aria-modal="true" aria-label="Agent family" tabIndex={-1} ref={drawerRef}>
      <div class="family-drawer-heading">
        <strong>Agent family</strong>
        <div class="family-drawer-nav">
          {navigation.parent && navButton('Parent', navigation.parent)}
          {navigation.root && navButton('Root', navigation.root)}
          <button class="family-drawer-close" type="button" aria-label="Close agent family" onClick={() => {
            onClose()
            triggerRef.current?.focus()
          }}>×</button>
        </div>
      </div>
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
      <ul class="family-tree">
        {projection.siblingTrees.map(node => (
          <FamilyRow key={node.session.id} node={node} selectedId={selected.id} depth={0} />
        ))}
      </ul>
    </div>
  )
}
