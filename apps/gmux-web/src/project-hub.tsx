// Project hub page: sessions for one project grouped by host and cwd.
//
// Data shape comes from `buildProjectTopology` (pure function in types.ts).
// This module is render-only: no data fetching, no business logic.
// Reads sessions/projects/peers from the store.

import type { HostNode, Session, ProjectItem } from './types'
import { buildProjectTopology, sessionPath } from './types'
import { LaunchButton } from './launcher'
import { sessions, projects, peers } from './store'

function projectRemote(p: ProjectItem | undefined): string | undefined {
  return p?.match.find(r => r.remote)?.remote
}

function projectFirstPath(p: ProjectItem | undefined): string | undefined {
  return p?.match.find(r => r.path)?.path
}

interface ProjectHubProps {
  projectSlug: string
  onResume: (id: string) => void
  onCloseSession: (session: Session) => void
}

export function ProjectHub({ projectSlug, onResume, onCloseSession }: ProjectHubProps) {
  const projectsVal = projects.value
  const project = projectsVal.find(p => p.slug === projectSlug)
  const hosts = buildProjectTopology(projectSlug, sessions.value, projectsVal, peers.value)
  const remote = projectRemote(project)

  const totalSessions = hosts.reduce(
    (n, h) => n + h.folders.reduce((m, f) => m + f.sessions.length, 0),
    0,
  )

  return (
    <div class="project-hub">
      <header class="project-hub-header">
        <h1 class="project-hub-title">{projectSlug}</h1>
        <div class="project-hub-subtitle">
          {remote && <span class="project-hub-remote">{remote}</span>}
          {remote && hosts.length > 0 && <span class="project-hub-dot">·</span>}
          {hosts.length > 0 && (
            <span>
              {totalSessions} session{totalSessions === 1 ? '' : 's'} across {hosts.length} host{hosts.length === 1 ? '' : 's'}
            </span>
          )}
        </div>
      </header>

      {hosts.length === 0
        ? <EmptyProject projectSlug={projectSlug} launchCwd={projectFirstPath(project)} />
        : hosts.map(host => (
            <HostSection
              key={host.path.join('\0') || '(local)'}
              host={host}
              projectSlug={projectSlug}
              onResume={onResume}
              onCloseSession={onCloseSession}
            />
          ))}
    </div>
  )
}

function EmptyProject({ projectSlug, launchCwd }: { projectSlug: string; launchCwd?: string }) {
  return (
    <div class="project-hub-empty">
      <div class="project-hub-empty-title">No sessions yet in {projectSlug}</div>
      {launchCwd && (
        <div class="project-hub-empty-actions">
          <span class="path-mono">{launchCwd}</span>
          <LaunchButton cwd={launchCwd} className="project-hub-empty-launcher" />
        </div>
      )}
    </div>
  )
}

function HostSection({
  host, projectSlug, onResume, onCloseSession,
}: { host: HostNode; projectSlug: string; onResume: (id: string) => void; onCloseSession: (session: Session) => void }) {
  const sessionCount = host.folders.reduce((n, f) => n + f.sessions.length, 0)
  const folderCount = host.folders.length
  const countText = folderCount > 1
    ? `${sessionCount} session${sessionCount === 1 ? '' : 's'} · ${folderCount} folders`
    : `${sessionCount} session${sessionCount === 1 ? '' : 's'}`

  const canLaunch = host.path.length <= 1
  const launchPeer = host.path.length === 1 ? host.path[0] : undefined

  return (
    <section class="host-section">
      <div class="host-section-header">
        <span class={`host-status ${host.status}`} />
        <HostPath path={host.path} />
        {host.meta && <span class="host-meta">{host.meta}</span>}
        <span class="host-session-count">{countText}</span>
      </div>
      {host.folders.map(folder => (
        <div class="folder-row" key={folder.cwd}>
          <div class="folder-row-info">
            <div class="path-mono folder-row-path">{folder.cwd || '~'}</div>
            <div class="host-folder-cards">
              {folder.sessions.map(s => (
                <SessionCard
                  key={s.id}
                  session={s}
                  projectSlug={projectSlug}
                  onResume={onResume}
                  onClose={() => onCloseSession(s)}
                />
              ))}
            </div>
          </div>
          {canLaunch && (
            <div class="folder-launchers">
              <LaunchButton
                cwd={folder.cwd || undefined}
                peer={launchPeer}
                className="folder-row-launch"
              />
            </div>
          )}
        </div>
      ))}
    </section>
  )
}

function HostPath({ path }: { path: string[] }) {
  if (path.length === 0) {
    return (
      <div class="host-path">
        <span class="host-segment leaf">local</span>
      </div>
    )
  }
  return (
    <div class="host-path">
      {path.map((seg, i) => {
        const isLeaf = i === path.length - 1
        return (
          <>
            {i > 0 && <span class="host-arrow">›</span>}
            <span class={`host-segment ${isLeaf ? 'leaf' : 'ancestor'}`}>{seg}</span>
          </>
        )
      })}
    </div>
  )
}

function SessionCard({
  session, projectSlug, onResume, onClose,
}: { session: Session; projectSlug: string; onResume: (id: string) => void; onClose: () => void }) {
  const dotClass = session.alive ? '' : 'dead'
  const sleeping = !session.alive && session.resumable
  const name = session.title || session.kind
  const href = sessionPath(projectSlug, session)
  return (
    <a
      class="session-card"
      href={href}
      onClick={(e) => {
        if (sleeping) {
          e.preventDefault()
          onResume(session.id)
        }
      }}
    >
      <span class={`session-card-dot ${dotClass}`} />
      <span class="session-card-name">{name}</span>
      <button
        class="session-card-close"
        onClick={(e) => { e.preventDefault(); e.stopPropagation(); onClose() }}
        title={session.alive ? 'Kill session' : 'Dismiss'}
      >
        ×
      </button>
    </a>
  )
}
