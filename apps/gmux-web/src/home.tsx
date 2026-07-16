// Home dashboard: activity-first overview of every session. Three
// activity sections (Waiting / Active / Recent) surface what the user
// likely wants right now. Project configuration (add / reorder /
// remove) lives in Settings → Projects; project navigation lives in
// the sidebar. The home page is purely an overview surface.
//
// Day-bucket grouping, sort order, and the label boundaries live in
// store.ts:partitionByDay. This file is presentation.

import {
  health, folders, homePartition, dismissSession, tabHref,
} from './store'
import { SessionRow } from './session-row'
import { sessionPath } from './routing'
import { cwdBadge } from './cwd-format'
import { IconBell, type NotifPermission } from './sidebar'
import type { Folder, Session } from './types'

export function Home({
  onManageProjects,
  notifPermission,
  onRequestNotifPermission,
}: {
  onManageProjects: () => void
  notifPermission: NotifPermission
  onRequestNotifPermission: () => void
}) {
  const foldersVal = folders.value
  const hasProjects = folders.value.length > 0
  const buckets = homePartition.value

  // Cheap session→folder lookup: the SessionRow needs a project name
  // and the folder's owning peer to build a correct href. Building a
  // map once is O(n+m) vs O(n*m) per row.
  const folderBySessionId = new Map<string, Folder>()
  for (const f of foldersVal) {
    for (const s of f.sessions) folderBySessionId.set(s.id, f)
  }

  // Sessions with no folder can't be rendered (no project name / href)
  // and would otherwise leave a section heading with no rows during the
  // brief window after a daemon restart where recovered sessions arrive
  // before their project stamp does. Drop them here so a section only
  // shows when it has rows to render.
  // Home shows only the named-day window; the dated tail (8+ days ago)
  // lives in the sidebar's full Activity list. `placed()` drops folderless
  // sessions so a day heading never renders with no rows (the brief
  // post-restart window where recovered sessions arrive unstamped).
  const placed = (arr: readonly Session[]) => arr.filter(s => folderBySessionId.has(s.id))
  const shown = buckets
    .filter(b => b.kind !== 'dated')
    .map(b => ({ label: b.label, sessions: placed(b.sessions) }))
    .filter(b => b.sessions.length > 0)

  const renderRow = (s: Session) => {
    const folder = folderBySessionId.get(s.id)
    // Guarded by `placed()` above; the fallback stays as a defensive
    // guard against a mid-arrival race.
    if (!folder) return null
    // Surface the session's cwd only when it strays from the project's
    // canonical folder (a subfolder or worktree/grove workspace);
    // sessions at the project root stay quiet.
    const cwd = cwdBadge(s.cwd, folder.launchCwd)
    return (
      <SessionRow
        key={s.id}
        session={s}
        href={tabHref(sessionPath(folder.slug, s, folder.peer))}
        showProject
        projectName={folder.name}
        showHost
        showCwd={!!cwd}
        cwdLabel={cwd ?? undefined}
        onClose={() => dismissSession(s.id)}
      />
    )
  }

  const anyActivity = shown.length > 0

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
      {shown.map(b => (
        <Section key={b.label ?? 'today'} title={b.label ?? undefined}>
          {b.sessions.map(renderRow)}
        </Section>
      ))}

      {!anyActivity && (
        hasProjects ? (
          <div class="home-empty-hint">Nothing active right now.</div>
        ) : (
          <div class="home-empty-hint">
            No projects yet. <button class="home-empty-action" onClick={onManageProjects}>Add a project</button>
          </div>
        )
      )}

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
  title?: string
  children: preact.ComponentChildren
}) {
  return (
    <section class="home-section">
      {title && <h2 class="home-section-title">{title}</h2>}
      <div class="home-section-body">{children}</div>
    </section>
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
          <a class="home-footer-link" href="https://gmux.app/changelog/" target="_blank" rel="noopener">
            version {h.update_available} available!
          </a>
        </>
      )}
    </footer>
  )
}
