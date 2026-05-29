// Home dashboard: activity-first overview of every session, plus the
// list of configured projects. Replaces the old project-grid-only
// homepage. Three activity sections (Waiting / Active /
// Recent) surface what the user likely wants right now without
// asking them to scan the sidebar; the projects list below is the
// stable "wherever I'm working from" navigation surface.
//
// Section semantics, sort order, and the Recent floor/window/cap
// live in store.ts:partitionForHome. This file is presentation.

import { useCallback, useState } from 'preact/hooks'
import {
  health, folders, homePartition, dismissSession, projects,
  updateProjects, removeProject, removePeerReference,
} from './store'
import { HostSuffix } from './host-suffix'
import { SessionRow } from './session-row'
import { LaunchButton } from './launcher'
import { sessionPath } from './routing'
import { IconBell, type NotifPermission } from './sidebar'
import type { Folder, Session, ProjectItem } from './types'

/** Drag-to-reorder transient state. `from` is the index lifted off,
 *  `over` is the current insertion target. Set to null when nothing
 *  is being dragged. Lives in the component, not the store: it's
 *  view-only and reset on every mouse-up. */
interface DragState { from: number; over: number }

function reorder<T>(arr: readonly T[], from: number, to: number): T[] {
  const out = [...arr]
  const [moved] = out.splice(from, 1)
  out.splice(to, 0, moved)
  return out
}

export function Home({
  onManageProjects,
  notifPermission,
  onRequestNotifPermission,
}: {
  onManageProjects: () => void
  notifPermission: NotifPermission
  onRequestNotifPermission: () => void
}) {
  const healthVal = health.value
  const foldersVal = folders.value
  const projectsVal = projects.value
  const { needsAttention, running, recent } = homePartition.value

  // Drag-to-reorder state for the Projects list. `configured` is the
  // authoritative order; `dragItems` is the visual order during drag
  // (commit on drop via updateProjects). Folder index maps 1:1 to
  // projects.value index because buildProjectFolders preserves
  // projects.json order.
  const [drag, setDrag] = useState<DragState | null>(null)
  const dragItems = drag ? reorder(projectsVal, drag.from, drag.over) : projectsVal

  const handleDragStart = useCallback((i: number) => setDrag({ from: i, over: i }), [])
  const handleDragOver = useCallback((i: number) => {
    setDrag(prev => prev ? { ...prev, over: i } : null)
  }, [])
  const handleDragEnd = useCallback(() => {
    // Commit the reorder before clearing drag state. State-setter
    // updaters must stay pure (Preact may invoke them more than
    // once in strict / dev modes), so the side effect lives outside
    // the updater.
    if (drag && drag.from !== drag.over) {
      updateProjects(reorder(projectsVal, drag.from, drag.over))
    }
    setDrag(null)
  }, [drag, projectsVal])

  const handleRemove = useCallback((p: ProjectItem) => {
    if (p.peer) removePeerReference(p.peer, p.slug)
    else removeProject(p.slug)
  }, [])

  // Cheap session→folder lookup: the SessionRow needs a project name
  // and the folder's owning peer to build a correct href. Building a
  // map once is O(n+m) vs O(n*m) per row.
  const folderBySessionId = new Map<string, Folder>()
  for (const f of foldersVal) {
    for (const s of f.sessions) folderBySessionId.set(s.id, f)
  }

  const renderRow = (s: Session) => {
    const folder = folderBySessionId.get(s.id)
    // A session always belongs to a folder once stamps land (post
    // #228). Defensive fallback: if we somehow get a session
    // without a folder (mid-arrival race), skip rendering rather
    // than crashing the dashboard.
    if (!folder) return null
    return (
      <SessionRow
        key={s.id}
        session={s}
        href={sessionPath(folder.slug, s, folder.peer)}
        showProject
        projectName={folder.name}
        showHost
        onClose={() => dismissSession(s.id)}
      />
    )
  }

  const anyActivity = needsAttention.length + running.length + recent.length > 0

  return (
    <div class="page">
      <header class="hub-header">
        <div class="home-activity-header">
          <h2 class="hub-title">Activity</h2>
          <NotifPrompt
            permission={notifPermission}
            onRequest={onRequestNotifPermission}
          />
        </div>
      </header>
      {needsAttention.length > 0 && (
        <Section title="Waiting">
          {needsAttention.map(renderRow)}
        </Section>
      )}

      {running.length > 0 && (
        <Section title="Active">
          {running.map(renderRow)}
        </Section>
      )}

      {recent.length > 0 && (
        <Section title="Recent">
          {recent.map(renderRow)}
        </Section>
      )}

      <section class="home-projects-section">
        <h2 class="home-section-title">Projects</h2>
        {projectsVal.length > 0 ? (
          <div class="home-projects-list">
            {dragItems.map((p, i) => {
              // Match each project to its rendered folder (which
              // carries display-friendly fields like name and counts).
              // Folder key is `${peer ?? ''}::${slug}`; we match on that.
              const folderKey = `${p.peer ?? ''}::${p.slug}`
              const folder = foldersVal.find(f => f.key === folderKey)
              if (!folder) return null
              return (
                <ProjectRow
                  key={folderKey}
                  folder={folder}
                  project={p}
                  index={i}
                  dragging={drag !== null && drag.from === projectsVal.indexOf(p)}
                  dropTarget={drag !== null && drag.over === i && drag.from !== i}
                  onDragStart={handleDragStart}
                  onDragOver={handleDragOver}
                  onDragEnd={handleDragEnd}
                  onRemove={handleRemove}
                />
              )
            })}
          </div>
        ) : !anyActivity ? (
          <div class="home-empty-hint">
            No projects yet. <button class="home-empty-action" onClick={onManageProjects}>Add a project</button>
          </div>
        ) : null}
        <button class="home-add-project" onClick={onManageProjects}>
          + Add project
        </button>
      </section>

      <HomeFooter />
    </div>
  )
}

/** Notification opt-in affordance, right-aligned in the Activity header.
 *  - 'default' : ghost pill inviting opt-in.
 *  - 'denied'  : icon-only muted bell with a tooltip pointing at browser
 *    settings (kept compact so it never crowds the header row).
 *  - 'granted' / 'unavailable' : render nothing (no opt-in needed, and a
 *    permission we cannot request is not actionable). */
function NotifPrompt({
  permission,
  onRequest,
}: {
  permission: NotifPermission
  onRequest: () => void
}) {
  if (permission === 'default') {
    return (
      <button class="notif-toggle" onClick={onRequest}>
        <IconBell /> Enable notifications
      </button>
    )
  }
  if (permission === 'denied') {
    return (
      <span
        class="notif-blocked"
        title="Notifications blocked in browser settings"
      >
        <IconBell muted />
      </span>
    )
  }
  return null
}

export function Section({
  title,
  children,
}: {
  title: string
  children: preact.ComponentChildren
}) {
  return (
    <section class="home-section">
      <h2 class="home-section-title">{title}</h2>
      <div class="home-section-body">{children}</div>
    </section>
  )
}

function ProjectRow({
  folder: f, project, index,
  dragging, dropTarget,
  onDragStart, onDragOver, onDragEnd, onRemove,
}: {
  folder: Folder
  project: ProjectItem
  index: number
  dragging: boolean
  dropTarget: boolean
  onDragStart: (i: number) => void
  onDragOver: (i: number) => void
  onDragEnd: () => void
  onRemove: (project: ProjectItem) => void
}) {
  // Counts mirror the legacy ProjectCard: "N alive" is the running
  // count, "N resumable" is dead-but-resurrectable. Empty projects
  // get a muted "no sessions" hint so the row isn't visually mute.
  const alive = f.sessions.filter(s => s.alive).length
  const resumable = f.sessions.filter(s => !s.alive && s.resumable).length
  const href = f.peer ? `/@${f.peer}/${f.slug}` : `/${f.slug}`
  const isReference = !!project.peer
  return (
    <div
      class={`home-project-row${dragging ? ' dragging' : ''}${dropTarget ? ' drop-target' : ''}`}
      draggable
      onDragStart={(e) => {
        e.dataTransfer!.effectAllowed = 'move'
        e.dataTransfer!.setData('text/plain', '')
        onDragStart(index)
      }}
      onDragOver={(e) => {
        e.preventDefault()
        e.dataTransfer!.dropEffect = 'move'
        onDragOver(index)
      }}
      // `onDrop` only consumes the event so the browser doesn't try
      // to navigate or search; the actual commit happens in
      // `onDragEnd`, which fires for both successful drops and
      // cancellations (Esc, release outside). Wiring both to the
      // same handler doubled the PUT /v1/projects call on every
      // successful drag.
      onDrop={(e) => { e.preventDefault() }}
      onDragEnd={onDragEnd}
    >
      <span class="home-project-drag" title="Drag to reorder" aria-hidden="true">⠿</span>
      <a class="home-project-link" href={href}>
        <div class="home-project-name">
          {f.name}
          <HostSuffix peer={f.peer} />
        </div>
        <div class="home-project-count">
          {alive > 0 && <span class="home-project-alive">{alive} alive</span>}
          {alive > 0 && resumable > 0 && <span class="home-project-rest"> · </span>}
          {resumable > 0 && <span class="home-project-rest">{resumable} resumable</span>}
          {alive === 0 && resumable === 0 && <span class="home-project-rest">no sessions</span>}
        </div>
      </a>
      <LaunchButton
        sessions={f.sessions}
        fallbackCwd={f.launchCwd ?? ''}
        peer={f.peer}
        className="home-project-launch"
      />
      <button
        class="home-project-remove"
        onClick={(e) => { e.preventDefault(); e.stopPropagation(); onRemove(project) }}
        title={isReference ? 'Remove reference' : 'Remove project'}
      >
        ×
      </button>
    </div>
  )
}

/** Page footer: daemon version, optionally followed by an inline
 *  update-available link. Renders nothing when the daemon is
 *  unreachable (the disconnect banner already covers that state).
 *
 *  Frontend version is not surfaced: a separate watcher auto-reloads
 *  the tab when the bundle goes stale relative to the daemon, so the
 *  user shouldn't see a long-lived mismatch. */
function HomeFooter() {
  const h = health.value
  if (!h?.version) return null
  return (
    <footer class="home-footer">
      gmux version {h.version}
      {h.update_available && (
        <>
          {' · '}
          <a class="home-footer-link" href="https://gmux.app/changelog/" target="_blank">
            version {h.update_available} available!
          </a>
        </>
      )}
    </footer>
  )
}
