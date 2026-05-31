// Home dashboard: activity-first overview of every session. Three
// activity sections (Waiting / Active / Recent) surface what the user
// likely wants right now. Project configuration (add / reorder /
// remove) lives in Settings → Projects; project navigation lives in
// the sidebar. The home page is purely an overview surface.
//
// Section semantics, sort order, and the recency-bucket boundaries
// live in store.ts:partitionForHome. This file is presentation.

import {
  health, folders, homePartition, dismissSession,
} from './store'
import { SessionRow } from './session-row'
import { sessionPath } from './routing'
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
  const { needsAttention, running, buckets } = homePartition.value

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

  const anyActivity = needsAttention.length > 0 || running.length > 0 || buckets.length > 0

  return (
    <div class="page">
      <header class="mb-5">
        <div class="flex items-center justify-between gap-3">
          <h2 class="text-lg font-semibold text-text m-0">Activity</h2>
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

      {buckets.map(b => (
        <Section key={b.label} title={b.label}>
          {b.sessions.map(renderRow)}
        </Section>
      ))}

      {!anyActivity && (
        hasProjects ? (
          <div class="text-[13px] text-text-muted py-4">Nothing active right now.</div>
        ) : (
          <div class="text-[13px] text-text-muted py-4">
            No projects yet. <button class="bg-transparent border-0 text-accent cursor-pointer p-0 underline underline-offset-[2px] font-[inherit]" onClick={onManageProjects}>Add a project</button>
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
      <button class="inline-flex items-center gap-1.5 shrink-0 py-1 px-2.5 border border-border rounded bg-transparent text-text-muted text-[12px] font-[Source_Sans_3] whitespace-nowrap cursor-pointer transition-colors duration-150 hover:border-accent hover:text-text-secondary" onClick={onRequest}>
        <IconBell /> Enable notifications
      </button>
    )
  }
  if (permission === 'denied') {
    return (
      <span
        class="inline-flex items-center shrink-0 text-text-muted cursor-default"
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
    <section class="mb-6">
      <h2 class="text-[11px] font-semibold uppercase tracking-[0.06em] text-text-muted mb-3">{title}</h2>
      <div class="flex flex-col gap-1.5">{children}</div>
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
    <footer class="mt-auto pt-4 border-t border-border text-[11px] text-text-muted leading-[1.5]">
      gmux version {h.version}
      {h.update_available && (
        <>
          {' · '}
          <a class="font-[inherit] text-accent bg-transparent border-0 p-0 cursor-pointer underline underline-offset-[2px] hover:text-text" href="https://gmux.app/changelog/" target="_blank">
            version {h.update_available} available!
          </a>
        </>
      )}
    </footer>
  )
}
