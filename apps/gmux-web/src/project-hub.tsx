// Project hub page: sessions for one project grouped by host and cwd.
//
// Data shape comes from `buildProjectTopology` (pure function in types.ts).
// This module is render-only: no data fetching, no business logic.
// Reads sessions/projects/peers from the store.

import type { Session, ProjectItem } from './types'
import type { HostNode } from './projects'
import { buildProjectTopology } from './projects'
import { sessionPath } from './routing'
import { LaunchButton } from './launcher'
import { sessions, projects, peers, peerAppearance } from './store'

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
    <div class="home">
      <section>
        <h2 class="home-section-title">{projectSlug}</h2>
        {(remote || totalSessions > 0) && (
          <div class="hub-meta">
            {remote && <span class="hub-remote">{remote}</span>}
            {remote && totalSessions > 0 && ' · '}
            {totalSessions > 0 && (
              <span>
                {totalSessions} session{totalSessions === 1 ? '' : 's'}
                {hosts.length > 1 && <> across {hosts.length} hosts</>}
              </span>
            )}
          </div>
        )}
      </section>

      {hosts.length === 0
        ? <EmptyProject projectSlug={projectSlug} launchCwd={projectFirstPath(project)} />
        : hosts.map(host => (
            <HostGroup
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
    <div class="hub-empty">
      <span>No sessions yet in {projectSlug}</span>
      {launchCwd && (
        <>
          <span class="hub-empty-path">{launchCwd}</span>
          <LaunchButton cwd={launchCwd} className="hub-empty-launch" />
        </>
      )}
    </div>
  )
}

function HostGroup({
  host, projectSlug, onResume, onCloseSession,
}: { host: HostNode; projectSlug: string; onResume: (id: string) => void; onCloseSession: (session: Session) => void }) {
  const sessionCount = host.folders.reduce((n, f) => n + f.sessions.length, 0)
  const canLaunch = host.path.length <= 1
  const launchPeer = host.path.length === 1 ? host.path[0] : undefined

  return (
    <section class="hub-host">
      <div class="hub-host-header">
        <span class={`hub-host-dot ${host.status}`} />
        {host.path.length > 0 && (() => {
          const name = host.path[0]
          const a = peerAppearance.value.get(name)
          return <span class="session-peer-label" style={a && { color: a.color, background: a.bg }}>{a?.label ?? name[0].toUpperCase()}</span>
        })()}
        <span class="hub-host-name"><HostPath path={host.path} /></span>
        <span class="hub-host-count">
          {sessionCount} session{sessionCount === 1 ? '' : 's'}
        </span>
      </div>
      {host.folders.map(folder => (
        <div class="hub-folder" key={folder.cwd}>
          <div class="hub-folder-path">{folder.cwd || '~'}</div>
          <div class="hub-folder-sessions">
            {folder.sessions.map(s => (
              <SessionCard
                key={s.id}
                session={s}
                projectSlug={projectSlug}
                onResume={onResume}
                onClose={() => onCloseSession(s)}
              />
            ))}
            {canLaunch && (
              <LaunchButton
                cwd={folder.cwd || undefined}
                peer={launchPeer}
                className="hub-folder-launch"
              />
            )}
          </div>
        </div>
      ))}
    </section>
  )
}

function HostPath({ path }: { path: string[] }) {
  if (path.length === 0) return <>local</>
  return (
    <>
      {path.map((seg, i) => (
        <>{i > 0 && <span class="hub-host-arrow">›</span>}{seg}</>
      ))}
    </>
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
