// Project hub page: sessions for one project grouped by host and cwd.
//
// Data shape comes from `buildProjectTopology` (pure function in types.ts).
// This module is render-only: no data fetching, no business logic. Callers
// pass the full sessions/projects/peers lists and a select callback.

import type { HostNode, PeerInfo, ProjectItem, Session } from './types'
import { buildProjectTopology, sessionPath } from './types'
import { LaunchButton } from './launcher'

interface ProjectHubProps {
  projectSlug: string
  sessions: Session[]
  projects: ProjectItem[]
  peers: PeerInfo[]
  onResume: (id: string) => void
  onCloseSession: (session: Session) => void
}

export function ProjectHub({ projectSlug, sessions, projects, peers, onResume, onCloseSession }: ProjectHubProps) {
  const project = projects.find(p => p.slug === projectSlug)
  const hosts = buildProjectTopology(projectSlug, sessions, projects, peers)

  const totalSessions = hosts.reduce(
    (n, h) => n + h.folders.reduce((m, f) => m + f.sessions.length, 0),
    0,
  )

  return (
    <div class="project-hub">
      <header class="project-hub-header">
        <h1 class="project-hub-title">{projectSlug}</h1>
        <div class="project-hub-subtitle">
          {project?.remote && <span class="project-hub-remote">{project.remote}</span>}
          {project?.remote && hosts.length > 0 && <span class="project-hub-dot">·</span>}
          {hosts.length > 0 && (
            <span>
              {totalSessions} session{totalSessions === 1 ? '' : 's'} across {hosts.length} host{hosts.length === 1 ? '' : 's'}
            </span>
          )}
        </div>
      </header>

      {hosts.length === 0
        ? <EmptyProject projectSlug={projectSlug} project={project} />
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

function EmptyProject({ projectSlug, project }: { projectSlug: string; project: ProjectItem | undefined }) {
  const launchCwd = project?.paths[0]
  return (
    <div class="project-hub-empty">
      <div class="project-hub-empty-title">No sessions yet in {projectSlug}</div>
      {launchCwd && (
        <div class="project-hub-empty-actions">
          <span class="path-mono">{launchCwd}</span>
          <LaunchButton cwd={launchCwd} storageKey={projectSlug} className="project-hub-empty-launcher" />
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

  // Only direct peers (or local) can launch today. Nested peers (devcontainers
  // on a spoke) would need hub → spoke → spoke chain-routing which isn't
  // implemented; disable launch buttons there rather than risk creating
  // sessions at the wrong host.
  const canLaunch = host.path.length <= 1
  // For launch, the peer prop is the root peer name (hub's direct peer) or
  // undefined for local.
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
                storageKey={projectSlug}
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
  // Mirror the sidebar: title is the primary label. Fall back to kind for
  // sessions that haven't reported a title yet.
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
        // Alive sessions: let click propagate to preact-iso for client-side nav.
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
