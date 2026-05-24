// Home dashboard: activity-first overview of every session, plus the
// list of configured projects. Replaces the old project-grid-only
// homepage. Three activity sections (Waiting / Active /
// Recent) surface what the user likely wants right now without
// asking them to scan the sidebar; the projects list below is the
// stable "wherever I'm working from" navigation surface.
//
// Section semantics, sort order, and the Recent floor/window/cap
// live in store.ts:partitionForHome. This file is presentation.

import { health, folders, homePartition, dismissSession } from './store'
import { HostSuffix } from './host-suffix'
import { SessionRow } from './session-row'
import { LaunchButton } from './launcher'
import { sessionPath } from './routing'
import type { Folder, Session } from './types'

export function Home({ onManageProjects }: { onManageProjects: () => void }) {
  const healthVal = health.value
  const foldersVal = folders.value
  const { needsAttention, running, recent } = homePartition.value

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
        <h2 class="hub-title">Activity</h2>
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
        {foldersVal.length > 0 ? (
          <div class="home-projects-list">
            {foldersVal.map(f => <ProjectRow key={f.key} folder={f} />)}
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

function Section({
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

function ProjectRow({ folder: f }: { folder: Folder }) {
  // Counts mirror the legacy ProjectCard: "N alive" is the running
  // count, "N resumable" is dead-but-resurrectable. Empty projects
  // get a muted "no sessions" hint so the row isn't visually mute.
  const alive = f.sessions.filter(s => s.alive).length
  const resumable = f.sessions.filter(s => !s.alive && s.resumable).length
  const href = f.peer ? `/@${f.peer}/${f.slug}` : `/${f.slug}`
  return (
    <div class="home-project-row">
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
